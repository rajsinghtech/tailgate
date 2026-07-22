//go:build e2e

// Native-DNS webhook e2e: an EgressGroup with dns.enabled makes the operator's mutating
// webhook inject a tailnet resolver (100.100.100.100 primary, cluster DNS secondary) into
// matched member pods — no manual dnsConfig. The injected DNS then resolves + reaches a peer
// by MagicDNS name through the shared gateway. A non-member pod is left untouched.
package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	egressv1 "github.com/rajsinghtech/tailgate/api/v1alpha1"
)

func TestNativeDNSWebhook(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	cfg, cs, kc := clients(t)

	tn := newTailnetClient(t)
	eg, err := tn.Create(ctx, "tailgate-dnswh-"+time.Now().UTC().Format("150405"))
	must(t, err, "create tailnet")
	t.Cleanup(func() { _ = eg.Close(context.Background()) })
	must(t, eg.ApplyACL(ctx, []byte(`{"tagOwners":{"tag:ci":["autogroup:admin"],"tag:k8s": ["autogroup:admin"]},"grants":[{"src":["*"],"dst":["*"],"ip":["*"]}]}`)), "acl")
	upsertSecretFor(t, ctx, kc, eg, "dnswh")
	restartOperator(t, ctx, cs)

	key, err := eg.MintAuthKey(ctx, []string{"tag:ci"})
	must(t, err, "mint peer authkey")
	_, _, fqdn := startPeer(t, ctx, key, "tailgate-dnswh-target")
	if fqdn == "" {
		t.Fatal("peer has no MagicDNS name")
	}

	name := "dnswh"
	forceCleanEG(t, ctx, cs, kc, name)
	must(t, kc.Create(ctx, &egressv1.EgressGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: egressv1.EgressGroupSpec{
			Selector: egressv1.EgressSelector{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"egress": name}}},
			DNS:      &egressv1.MemberDNS{Enabled: true},
		},
	}), "create egressgroup")
	t.Cleanup(func() { deleteEGWait(kc, name) })

	// member is a NORMAL pod (no manual dnsConfig); the webhook must inject native DNS.
	member := runPod(t, ctx, cs, "dnswh-member", map[string]string{"egress": name})
	waitGatewayReady(t, ctx, cs, name)
	t.Cleanup(func() { _ = cs.CoreV1().Pods("default").Delete(context.Background(), member, *metav1.NewDeleteOptions(0)) })

	got, err := cs.CoreV1().Pods("default").Get(ctx, member, metav1.GetOptions{})
	must(t, err, "get member pod")
	if got.Spec.DNSPolicy != corev1.DNSNone || got.Spec.DNSConfig == nil ||
		len(got.Spec.DNSConfig.Nameservers) == 0 || got.Spec.DNSConfig.Nameservers[0] != "100.100.100.100" {
		t.Fatalf("webhook did not inject native DNS: policy=%q config=%+v", got.Spec.DNSPolicy, got.Spec.DNSConfig)
	}
	t.Logf("PASS: webhook injected dnsConfig nameservers=%v searches=%v", got.Spec.DNSConfig.Nameservers, got.Spec.DNSConfig.Searches)

	// the injected DNS actually resolves + reaches the peer by name through the gateway.
	if err := wait.PollUntilContextTimeout(ctx, 4*time.Second, 120*time.Second, true, func(ctx context.Context) (bool, error) {
		out, _, _ := execPod(ctx, cfg, cs, member, []string{"curl", "-sS", "-m", "6", "http://" + fqdn + "/"})
		return strings.TrimSpace(out) == "ok", nil
	}); err != nil {
		res, _, _ := execPod(ctx, cfg, cs, member, []string{"getent", "hosts", fqdn})
		t.Fatalf("member never reached peer via webhook-injected DNS: %v (getent: %q)", err, strings.TrimSpace(res))
	}
	t.Log("PASS: webhook-injected native DNS resolved + reached the peer by MagicDNS name")

	// negative: a non-member pod must NOT be DNS-injected.
	other := runPod(t, ctx, cs, "dnswh-nonmember", map[string]string{"app": "web"})
	t.Cleanup(func() { _ = cs.CoreV1().Pods("default").Delete(context.Background(), other, *metav1.NewDeleteOptions(0)) })
	og, err := cs.CoreV1().Pods("default").Get(ctx, other, metav1.GetOptions{})
	must(t, err, "get non-member pod")
	if og.Spec.DNSPolicy == corev1.DNSNone {
		t.Fatal("non-member pod was wrongly DNS-injected by the webhook")
	}
	t.Log("PASS: non-member pod left untouched")
}
