//go:build e2e

// MagicDNS-through-the-gateway e2e: a member pod pointed at 100.100.100.100 resolves a
// tailnet peer's MagicDNS name via the SHARED gateway's tailscaled resolver (forwarded +
// MASQUERADEd into the gateway netns) and reaches it BY NAME. Proves DNS (forced on) works
// for members without each pod running tailscaled.
package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"

	egressv1 "github.com/rajsinghtech/tailgate/api/v1alpha1"
)

func TestMagicDNSThroughGateway(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	cfg, cs, kc := clients(t)

	// 1. tailnet (MagicDNS on by default) + operator pointed at it.
	tn := newTailnetClient(t)
	eg, err := tn.Create(ctx, "tailgate-dns-"+time.Now().UTC().Format("150405"))
	must(t, err, "create tailnet")
	t.Cleanup(func() { _ = eg.Close(context.Background()) })
	must(t, eg.ApplyACL(ctx, []byte(`{"tagOwners":{"tag:ci":["autogroup:admin"],"tag:k8s": ["autogroup:admin"]},"grants":[{"src":["*"],"dst":["*"],"ip":["*"]}]}`)), "acl")
	upsertSecretFor(t, ctx, kc, eg, "dns")
	restartOperator(t, ctx, cs)

	// 2. tsnet peer; capture its MagicDNS FQDN.
	key, err := eg.MintAuthKey(ctx, []string{"tag:ci"})
	must(t, err, "mint peer authkey")
	_, _, fqdn := startPeer(t, ctx, key, "tailgate-dns-target")
	if fqdn == "" {
		t.Fatal("peer has no MagicDNS name")
	}
	t.Logf("peer MagicDNS name: %s", fqdn)

	// 3. EgressGroup + a member pod whose ONLY resolver is the gateway's 100.100.100.100.
	name := "dns"
	forceCleanEG(t, ctx, cs, kc, name)
	must(t, kc.Create(ctx, &egressv1.EgressGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: egressv1.EgressGroupSpec{
			Selector: egressv1.EgressSelector{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"egress": name}}},
		},
	}), "create egressgroup")
	t.Cleanup(func() { deleteEGWait(kc, name) })

	member := runPodQuad100(t, ctx, cs, "dns-member", map[string]string{"egress": name})
	waitGatewayReady(t, ctx, cs, name)
	t.Cleanup(func() {
		_ = cs.CoreV1().Pods("default").Delete(context.Background(), member, *metav1.NewDeleteOptions(0))
	})

	// 4. resolve+reach the peer BY NAME through the gateway (retry for tunnel/DNS warmup).
	if err := wait.PollUntilContextTimeout(ctx, 4*time.Second, 120*time.Second, true, func(ctx context.Context) (bool, error) {
		out, _, _ := execPod(ctx, cfg, cs, member, []string{"curl", "-sS", "-m", "6", "http://" + fqdn + "/"})
		t.Logf("member curl by-name -> %q", strings.TrimSpace(out))
		return strings.TrimSpace(out) == "ok", nil
	}); err != nil {
		// surface what the resolver returned, for diagnosis
		res, _, _ := execPod(ctx, cfg, cs, member, []string{"getent", "hosts", fqdn})
		t.Fatalf("member never reached peer by MagicDNS name through gateway: %v (getent: %q)", err, strings.TrimSpace(res))
	}
	t.Log("PASS: member resolved + reached the peer by MagicDNS name through the gateway's 100.100.100.100")
}

// runPodQuad100 creates a netshoot pod whose only nameserver is the tailnet MagicDNS
// resolver (100.100.100.100), reachable via the member's gateway. dnsPolicy None lets us
// override the cluster resolver entirely so the test isolates the quad-100 path.
func runPodQuad100(t *testing.T, ctx context.Context, cs *kubernetes.Clientset, name string, labels map[string]string) string {
	t.Helper()
	_ = cs.CoreV1().Pods("default").Delete(ctx, name, *metav1.NewDeleteOptions(0))
	ndots := "1"
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: labels},
		Spec: corev1.PodSpec{
			DNSPolicy: corev1.DNSNone,
			DNSConfig: &corev1.PodDNSConfig{
				Nameservers: []string{"100.100.100.100"},
				Options:     []corev1.PodDNSConfigOption{{Name: "ndots", Value: &ndots}},
			},
			Containers: []corev1.Container{{
				Name: "c", Image: "nicolaka/netshoot", Command: []string{"sleep", "3600"},
			}},
		},
	}
	must(t, wait.PollUntilContextTimeout(ctx, time.Second, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		_, err := cs.CoreV1().Pods("default").Create(ctx, p, metav1.CreateOptions{})
		return err == nil || apierrors.IsAlreadyExists(err), nil
	}), "create pod "+name)
	must(t, wait.PollUntilContextTimeout(ctx, 2*time.Second, 90*time.Second, true, func(ctx context.Context) (bool, error) {
		got, err := cs.CoreV1().Pods("default").Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		for _, c := range got.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				return true, nil
			}
		}
		return false, nil
	}), "pod ready "+name)
	return name
}
