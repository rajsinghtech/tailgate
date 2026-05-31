//go:build e2e

// Subnet-router live-traffic e2e: an in-process tsnet node ADVERTISES a CIDR (auto-approved
// via the tailnet ACL) and serves any IP in it through a fallback TCP handler. A member of
// a mode=subnet EgressGroup reaches a backend IP INSIDE that CIDR (not the router's own
// node IP) THROUGH the gateway split tunnel — proving accepted-route forwarding end to end.
// The gateway routes forwarded member traffic into the TUN (fwmark), tailscaled matches the
// approved route to the subnet-router peer, and the router's handler echoes the dst IP.
package e2e

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"tailscale.com/ipn"
	"tailscale.com/tsnet"

	egressv1 "github.com/rajsinghtech/tailgate/api/v1alpha1"
)

const subnetCIDR = "10.50.0.0/16"
const subnetTargetIP = "10.50.1.1" // inside the CIDR, NOT the router's tailnet IP

func TestSubnetRouterReachability(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	cfg, cs, kc := clients(t)

	// 1. tailnet with autoApprovers so the subnet-router's advertised route auto-approves.
	tn := newTailnetClient(t)
	eg, err := tn.Create(ctx, "tailgate-subnet-"+time.Now().UTC().Format("150405"))
	must(t, err, "create tailnet")
	t.Cleanup(func() { _ = eg.Close(context.Background()) })
	must(t, eg.ApplyACL(ctx, []byte(`{
	  "tagOwners": {
	    "tag:ci": ["autogroup:admin"],
	    "tag:egress-subnet": ["autogroup:admin"],
	    "tag:subnet-router": ["autogroup:admin"]
	  },
	  "autoApprovers": { "routes": { "`+subnetCIDR+`": ["tag:subnet-router"] } },
	  "grants": [
	    {"src":["*"],"dst":["*"],"ip":["*"]},
	    {"src":["*"],"dst":["`+subnetCIDR+`"],"via":["tag:subnet-router"],"ip":["*"]}
	  ]
	}`)), "acl")
	upsertSecretFor(t, ctx, kc, eg, "subnet")
	restartOperator(t, ctx, cs)

	// 2. in-process tsnet subnet router advertising subnetCIDR, serving any IP in it.
	rkey, err := eg.MintAuthKey(ctx, []string{"tag:subnet-router"})
	must(t, err, "mint router authkey")
	startSubnetRouter(t, ctx, rkey, "tailgate-subnet-router", subnetCIDR, "router-ok")

	// 3. mode=subnet EgressGroup steering subnetCIDR onto members.
	name := "subnet"
	forceCleanEG(t, ctx, cs, kc, name)
	must(t, kc.Create(ctx, &egressv1.EgressGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: egressv1.EgressGroupSpec{
			Routes:   []string{subnetCIDR},
			Selector: egressv1.EgressSelector{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"egress": name}}},
		},
	}), "create egressgroup")
	t.Cleanup(func() { deleteEGWait(kc, name) })
	waitGatewayReady(t, ctx, cs, name)

	member := runPod(t, ctx, cs, "subnet-member", map[string]string{"egress": name})
	other := runPod(t, ctx, cs, "subnet-nonmember", map[string]string{"app": "web"})
	t.Cleanup(func() {
		_ = cs.CoreV1().Pods("default").Delete(context.Background(), member, *metav1.NewDeleteOptions(0))
		_ = cs.CoreV1().Pods("default").Delete(context.Background(), other, *metav1.NewDeleteOptions(0))
	})

	// 4. member reaches a backend INSIDE the advertised CIDR through the gateway split
	// tunnel; the dst-echo body proves it was forwarded as subnet-routed traffic.
	want := "router-ok:" + subnetTargetIP
	if err := wait.PollUntilContextTimeout(ctx, 4*time.Second, 150*time.Second, true, func(ctx context.Context) (bool, error) {
		out, _, _ := execPod(ctx, cfg, cs, member, []string{"curl", "-sS", "-m", "6", "http://" + subnetTargetIP + "/"})
		t.Logf("member curl subnet -> %q", strings.TrimSpace(out))
		return strings.TrimSpace(out) == want, nil
	}); err != nil {
		t.Fatalf("member never reached the subnet-routed backend %s through the gateway: %v", subnetTargetIP, err)
	}
	t.Logf("PASS: member reached subnet-routed %s through the gateway (split tunnel)", subnetTargetIP)

	// 5. negative control: a non-member has no ts0/route and must NOT reach it.
	if out, _, _ := execPod(ctx, cfg, cs, other, []string{"curl", "-sS", "-m", "6", "http://" + subnetTargetIP + "/"}); strings.TrimSpace(out) == want {
		t.Fatal("non-member reached the subnet-routed backend — selection is broken")
	}
	t.Log("PASS: non-member correctly denied the subnet route")
}

// startSubnetRouter brings up an in-process tsnet node that ADVERTISES advRoute and serves
// "<body>:<dstIP>" for ANY TCP flow to an IP inside advRoute via the fallback handler
// (netstack hands us the accepted conn before its std-dialer forwarder runs). The route
// auto-approves via the tailnet ACL autoApprovers, so the gateway sees it with no admin click.
func startSubnetRouter(t *testing.T, ctx context.Context, authkey, hostname, advRoute, body string) {
	t.Helper()
	dir, _ := os.MkdirTemp("", "e2e-router-*")
	s := &tsnet.Server{Hostname: hostname, Ephemeral: true, AuthKey: authkey, Dir: dir}
	t.Cleanup(func() { _ = s.Close() })
	pfx := netip.MustParsePrefix(advRoute)

	s.RegisterFallbackTCPHandler(func(src, dst netip.AddrPort) (func(net.Conn), bool) {
		if !pfx.Contains(dst.Addr()) {
			return nil, false
		}
		return func(c net.Conn) {
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			_, _ = c.Read(make([]byte, 1024)) // drain the request line/headers
			payload := fmt.Sprintf("%s:%s", body, dst.Addr())
			fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(payload), payload)
		}, true
	})

	// A throwaway listener triggers Start (mirrors startPeer); NOT on the advertised CIDR.
	ln, err := s.Listen("tcp", ":9")
	must(t, err, "router listen")
	t.Cleanup(func() { _ = ln.Close() })
	lc, err := s.LocalClient()
	must(t, err, "router localclient")

	_, err = lc.EditPrefs(ctx, &ipn.MaskedPrefs{
		Prefs:              ipn.Prefs{AdvertiseRoutes: []netip.Prefix{pfx}},
		AdvertiseRoutesSet: true,
	})
	must(t, err, "router advertise route")

	must(t, wait.PollUntilContextTimeout(ctx, time.Second, 60*time.Second, true, func(ctx context.Context) (bool, error) {
		st, err := lc.Status(ctx)
		if err != nil || st.Self == nil {
			return false, nil
		}
		for _, a := range st.Self.TailscaleIPs {
			if a.Is4() {
				return true, nil
			}
		}
		return false, nil
	}), "router get IP")
}
