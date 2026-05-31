//go:build e2e

// Exit-node AUTO-selection e2e: an EgressGroup with exitNode.auto (narrowed to tag:exit-node)
// makes the OPERATOR resolve a concrete exit node from the tailnet Devices API and pin it.
// Proves the resolution wires through to status.resolvedExitNode, the gateway commits the
// exit node, and the member gets full-tunnel plumbing — all without a hand-typed nodeID.
package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	egressv1 "github.com/rajsinghtech/tailgate/api/v1alpha1"
)

func TestExitNodeAutoSelect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cfg, cs, kc := clients(t)

	tn := newTailnetClient(t)
	eg, err := tn.Create(ctx, "tailgate-exitauto-"+time.Now().UTC().Format("150405"))
	must(t, err, "create tailnet")
	t.Cleanup(func() { _ = eg.Close(context.Background()) })
	must(t, eg.ApplyACL(ctx, []byte(`{
	  "tagOwners": {
	    "tag:ci": ["autogroup:admin"],
	    "tag:egress-exitauto": ["autogroup:admin"],
	    "tag:exit-node": ["autogroup:admin"]
	  },
	  "autoApprovers": { "exitNode": ["tag:exit-node"] },
	  "grants": [
	    {"src":["*"],"dst":["*"],"ip":["*"]},
	    {"src":["*"],"dst":["autogroup:internet"],"ip":["*"]}
	  ]
	}`)), "acl")
	upsertSecretFor(t, ctx, kc, eg, "exitauto")
	restartOperator(t, ctx, cs)

	// a real kernel-mode tailscaled exit node tagged tag:exit-node (advertises 0/0, auto-approved).
	ekey, err := eg.MintAuthKey(ctx, []string{"tag:exit-node"})
	must(t, err, "mint exit authkey")
	exitCGNAT, _ := startExitNodePod(t, ctx, cfg, cs, ekey)
	t.Logf("exit node cgnat=%s", exitCGNAT)

	// EgressGroup with NO nodeID — the operator must auto-resolve the tag:exit-node node.
	name := "exitauto"
	forceCleanEG(t, ctx, cs, kc, name)
	must(t, kc.Create(ctx, &egressv1.EgressGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: egressv1.EgressGroupSpec{
			ExitNode: &egressv1.ExitNodeRef{Auto: true, Tag: "tag:exit-node"},
			Selector: egressv1.EgressSelector{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"egress": name}}},
		},
	}), "create egressgroup")
	t.Cleanup(func() { deleteEGWait(kc, name) })

	// the operator resolved a concrete node and recorded it in status.
	if err := wait.PollUntilContextTimeout(ctx, 4*time.Second, 120*time.Second, true, func(ctx context.Context) (bool, error) {
		var got egressv1.EgressGroup
		if err := kc.Get(ctx, client.ObjectKey{Name: name}, &got); err != nil {
			return false, nil
		}
		return got.Status.ResolvedExitNode != "", nil
	}); err != nil {
		t.Fatalf("operator never auto-resolved an exit node into status.resolvedExitNode: %v", err)
	}
	var got egressv1.EgressGroup
	must(t, kc.Get(ctx, client.ObjectKey{Name: name}, &got), "get eg")
	if got.Status.ResolvedExitNode != exitCGNAT {
		t.Fatalf("resolvedExitNode = %q, want the exit node %q", got.Status.ResolvedExitNode, exitCGNAT)
	}
	t.Logf("PASS: operator auto-resolved exit node %s (no nodeID set)", got.Status.ResolvedExitNode)

	waitGatewayReady(t, ctx, cs, name)

	// the gateway committed the auto-selected exit node.
	gw := gatewayPod(t, ctx, cs, name)
	must(t, wait.PollUntilContextTimeout(ctx, 4*time.Second, 200*time.Second, true, func(ctx context.Context) (bool, error) {
		return gatewayExitNodeID(t, ctx, cfg, cs, gw.Name) != "", nil
	}), "gateway committed the auto-selected exit node")
	t.Log("PASS: gateway committed the auto-selected exit node")

	// member-side full-tunnel plumbing is installed (table 7717) — i.e. the exit node took effect.
	member := runPod(t, ctx, cs, "exitauto-member", map[string]string{"egress": name})
	t.Cleanup(func() { _ = cs.CoreV1().Pods("default").Delete(context.Background(), member, *metav1.NewDeleteOptions(0)) })
	if err := wait.PollUntilContextTimeout(ctx, 3*time.Second, 60*time.Second, true, func(ctx context.Context) (bool, error) {
		rules, _, _ := execPod(ctx, cfg, cs, member, []string{"ip", "rule"})
		rt, _, _ := execPod(ctx, cfg, cs, member, []string{"ip", "route", "show", "table", "7717"})
		return strings.Contains(rules, "7717") && strings.Contains(rt, "default"), nil
	}); err != nil {
		t.Fatalf("member full-tunnel plumbing not installed for the auto-selected exit node: %v", err)
	}
	t.Log("PASS: member full-tunnel routing installed via the auto-selected exit node")
}
