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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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

	gw := gatewayPod(t, ctx, cs, name)
	node := gw.Spec.NodeName
	if node == "" {
		t.Fatal("gateway pod has no nodeName")
	}

	// kind node names are already clean DNS labels, so sanitizeLabel(node) == node here.
	want := "tailgate-gw-" + name + "-" + node
	out, serr, err := execPod(ctx, cfg, cs, gw.Name, []string{"tailscale", "status", "--json"})
	must(t, err, "tailscale status --json: "+serr)
	var st struct {
		Self struct{ HostName string }
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("parse status json: %v", err)
	}
	if st.Self.HostName != want {
		t.Fatalf("gateway Self.HostName = %q, want %q (node suffix missing)", st.Self.HostName, want)
	}
	t.Logf("PASS: gateway registered as %q (traceable to group=%s node=%s)", st.Self.HostName, name, node)
}
