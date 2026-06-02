//go:build e2e

// Per-node hostname e2e: the gateway DaemonSet shares one rendered config (and thus one
// Hostname), so without intervention every per-node gateway would register under the same
// tailnet name. The entrypoint suffixes the hostname with the node (from the downward-API
// NODE_NAME) so each device is traceable to <egressgroup>+<node>. This asserts the gateway's
// actual tailscaled Self hostname is tailgate-gw-<group>-<node>.
package e2e

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	egressv1 "github.com/rajsinghtech/tailgate/api/v1alpha1"
)

func TestGatewayHostnamePerNode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	cfg, cs, kc := clients(t)

	tn := newTailnetClient(t)
	eg, err := tn.Create(ctx, "tailgate-hn-"+time.Now().UTC().Format("150405"))
	must(t, err, "create tailnet")
	t.Cleanup(func() { _ = eg.Close(context.Background()) })
	must(t, eg.ApplyACL(ctx, []byte(`{
	  "tagOwners": {"tag:ci": ["autogroup:admin"], "tag:k8s": ["autogroup:admin"]},
	  "grants": [{"src":["*"],"dst":["*"],"ip":["*"]}]
	}`)), "acl")
	upsertSecretFor(t, ctx, kc, eg, "hn")
	restartOperator(t, ctx, cs)

	name := "hn"
	forceCleanEG(t, ctx, cs, kc, name)
	must(t, kc.Create(ctx, &egressv1.EgressGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: egressv1.EgressGroupSpec{
			Selector: egressv1.EgressSelector{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"egress": name}}},
		},
	}), "create egressgroup")
	t.Cleanup(func() { deleteEGWait(kc, name) })
	waitGatewayReady(t, ctx, cs, name)

	// Re-fetch the current gateway pod each iteration (the DaemonSet can briefly roll a pod
	// during operator settling) and tolerate transient exec errors. kind node names are clean
	// DNS labels, so sanitizeLabel(node) == node and we can assert exact equality.
	var last string
	err = wait.PollUntilContextTimeout(ctx, 4*time.Second, 120*time.Second, true, func(ctx context.Context) (bool, error) {
		pods, e := cs.CoreV1().Pods(tgNS).List(ctx, metav1.ListOptions{LabelSelector: "tailgate.dev/group=" + name})
		if e != nil {
			return false, nil
		}
		var pod *corev1.Pod
		for i := range pods.Items {
			if pods.Items[i].DeletionTimestamp == nil {
				pod = &pods.Items[i]
				break
			}
		}
		if pod == nil || pod.Spec.NodeName == "" {
			return false, nil
		}
		out, _, e := execIn(ctx, cfg, cs, tgNS, pod.Name, "gateway", []string{"tailscale", "--socket=/var/run/tailscale/tailscaled.sock", "status", "--json"})
		if e != nil {
			return false, nil // pod momentarily gone / not yet up
		}
		var st struct {
			Self struct{ HostName string }
		}
		if json.Unmarshal([]byte(out), &st) != nil {
			return false, nil
		}
		last = st.Self.HostName
		return last == "tailgate-gw-"+name+"-"+pod.Spec.NodeName, nil
	})
	if err != nil {
		t.Fatalf("gateway Self.HostName never became tailgate-gw-%s-<node> (last seen %q): %v", name, last, err)
	}
	t.Logf("PASS: gateway registered as %q (traceable to group=%s + node)", last, name)
}
