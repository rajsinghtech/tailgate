//go:build e2e

// App-connector live-traffic e2e (CONSUME, not create). An in-process tsnet node is granted
// the app-connector capability for "github.com" via the tailnet ACL (nodeAttrs.app) and
// advertises GitHub's published CIDR (140.82.112.0/20) — auto-approved. A member of an
// EgressGroup that steers that CIDR reaches a GitHub IP THROUGH the gateway -> connector,
// which intercepts it (serves "appc-ok:<ip>"). A direct (non-member) pod instead reaches the
// real internet — proving the connector route genuinely diverted the member's GitHub traffic.
//
// Note on faithfulness: a full DNS-observation connector (client resolves github.com via the
// connector's PeerAPI DoH, connector then advertises the resolved /32) is gated on an
// experimental capability that isn't reliably deliverable on a vanilla ephemeral tailnet, so
// this test uses the connector's predetermined app.routes (advertised immediately on cap
// parse) — the same route-consume datapath, exercised deterministically.
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

const githubCIDR = "140.82.112.0/20" // GitHub's published range (github.com lives here)
const githubIP = "140.82.112.3"      // a representative IP inside it

func TestAppConnectorReachability(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	cfg, cs, kc := clients(t)

	// 1. tailnet with an app-connector grant for github.com + autoApprovers for its route.
	tn := newTailnetClient(t)
	eg, err := tn.Create(ctx, "tailgate-appc-"+time.Now().UTC().Format("150405"))
	must(t, err, "create tailnet")
	t.Cleanup(func() { _ = eg.Close(context.Background()) })
	must(t, eg.ApplyACL(ctx, []byte(`{
	  "tagOwners": {
	    "tag:ci": ["autogroup:admin"],
	    "tag:k8s": ["autogroup:admin"],
	    "tag:app-connector": ["autogroup:admin"]
	  },
	  "autoApprovers": { "routes": { "`+githubCIDR+`": ["tag:app-connector"] } },
	  "nodeAttrs": [
	    {
	      "target": ["*"],
	      "app": {
	        "tailscale.com/app-connectors": [
	          { "name": "github", "connectors": ["tag:app-connector"], "presetAppID": "github" }
	        ]
	      }
	    }
	  ],
	  "grants": [
	    {"src":["*"],"dst":["*"],"ip":["*"]},
	    {"src":["*"],"dst":["`+githubCIDR+`"],"via":["tag:app-connector"],"ip":["*"]}
	  ]
	}`)), "acl")
	upsertSecretFor(t, ctx, kc, eg, "appc")
	restartOperator(t, ctx, cs)

	// 2. in-process tsnet app connector (advertises github CIDR via the app config; serves it).
	ckey, err := eg.MintAuthKey(ctx, []string{"tag:app-connector"})
	must(t, err, "mint connector authkey")
	startAppConnector(t, ctx, ckey, "tailgate-github-connector", githubCIDR, "appc-ok")

	// 3. EgressGroup steering the github CIDR onto members (gateway accepts the route).
	name := "appc"
	forceCleanEG(t, ctx, cs, kc, name)
	must(t, kc.Create(ctx, &egressv1.EgressGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: egressv1.EgressGroupSpec{
			Selector: egressv1.EgressSelector{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"egress": name}}},
		},
	}), "create egressgroup")
	t.Cleanup(func() { deleteEGWait(kc, name) })
	waitGatewayReady(t, ctx, cs, name)

	member := runPod(t, ctx, cs, "appc-member", map[string]string{"egress": name})
	other := runPod(t, ctx, cs, "appc-nonmember", map[string]string{"app": "web"})
	t.Cleanup(func() {
		_ = cs.CoreV1().Pods("default").Delete(context.Background(), member, *metav1.NewDeleteOptions(0))
		_ = cs.CoreV1().Pods("default").Delete(context.Background(), other, *metav1.NewDeleteOptions(0))
	})

	// 4. member's traffic to a GitHub IP is intercepted by the connector through the gateway.
	want := "appc-ok:" + githubIP
	if err := wait.PollUntilContextTimeout(ctx, 4*time.Second, 150*time.Second, true, func(ctx context.Context) (bool, error) {
		out, _, _ := execPod(ctx, cfg, cs, member, []string{"curl", "-sS", "-m", "6", "http://" + githubIP + "/"})
		t.Logf("member curl github-IP -> %q", strings.TrimSpace(out))
		return strings.TrimSpace(out) == want, nil
	}); err != nil {
		t.Fatalf("member's GitHub-bound traffic was not routed through the app connector: %v", err)
	}
	t.Logf("PASS: member's traffic to %s was intercepted by the github app connector (split tunnel)", githubIP)

	// 5. control: a non-member is NOT steered through the connector — it reaches the real
	// internet (or nothing), but NEVER the connector's synthetic response.
	out, _, _ := execPod(ctx, cfg, cs, other, []string{"curl", "-sS", "-m", "6", "http://" + githubIP + "/"})
	if strings.TrimSpace(out) == want {
		t.Fatal("non-member also hit the app connector — the route was not gated by group membership")
	}
	t.Logf("PASS: non-member was NOT routed through the connector (got %q)", strings.TrimSpace(out))
}

// startAppConnector brings up an in-process tsnet node with the app-connector pref enabled.
// The tailnet ACL grants it the connector capability + the predetermined route (servedCIDR),
// which it advertises immediately; its fallback handler serves "<body>:<dstIP>" for any IP in
// that CIDR. Mirrors startSubnetRouter but opts in via AppConnector instead of AdvertiseRoutes.
func startAppConnector(t *testing.T, ctx context.Context, authkey, hostname, servedCIDR, body string) {
	t.Helper()
	dir, _ := os.MkdirTemp("", "e2e-appc-*")
	s := &tsnet.Server{Hostname: hostname, Ephemeral: true, AuthKey: authkey, Dir: dir}
	t.Cleanup(func() { _ = s.Close() })
	pfx := netip.MustParsePrefix(servedCIDR)

	s.RegisterFallbackTCPHandler(func(src, dst netip.AddrPort) (func(net.Conn), bool) {
		if !pfx.Contains(dst.Addr()) {
			return nil, false
		}
		return func(c net.Conn) {
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			_, _ = c.Read(make([]byte, 1024))
			payload := fmt.Sprintf("%s:%s", body, dst.Addr())
			fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(payload), payload)
		}, true
	})

	ln, err := s.Listen("tcp", ":9")
	must(t, err, "connector listen")
	t.Cleanup(func() { _ = ln.Close() })
	lc, err := s.LocalClient()
	must(t, err, "connector localclient")

	_, err = lc.EditPrefs(ctx, &ipn.MaskedPrefs{
		Prefs:           ipn.Prefs{AppConnector: ipn.AppConnectorPrefs{Advertise: true}},
		AppConnectorSet: true,
	})
	must(t, err, "connector enable app-connector pref")

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
	}), "connector get IP")

	// diagnostic: what routes did the preset connector advertise (predetermined vs DNS-observed)?
	_ = wait.PollUntilContextTimeout(ctx, 3*time.Second, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		pr, err := lc.GetPrefs(ctx)
		if err != nil {
			return false, nil
		}
		if len(pr.AdvertiseRoutes) > 0 {
			t.Logf("preset connector advertised routes: %v", pr.AdvertiseRoutes)
			return true, nil
		}
		return false, nil
	})
}
