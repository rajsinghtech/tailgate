//go:build e2e

// Route-mirroring e2e: with MirrorRoutes enabled and NO spec.routes, a member still reaches an
// app-connector CIDR — because the gateway accepts the connector's advertised route, publishes
// it to /run/tailgate/<group>.routes, and the agent steers it onto the member's ts0. This is
// the "egress follows resolution" half: whatever the gateway can reach, members can reach,
// without anyone listing CIDRs in spec.routes.
package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"

	egressv1 "github.com/rajsinghtech/tailgate/api/v1alpha1"
)

func TestMirrorRoutes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Minute)
	defer cancel()
	cfg, cs, kc := clients(t)

	tn := newTailnetClient(t)
	eg, err := tn.Create(ctx, "tailgate-mirror-"+time.Now().UTC().Format("150405"))
	must(t, err, "create tailnet")
	t.Cleanup(func() { _ = eg.Close(context.Background()) })
	must(t, eg.ApplyACL(ctx, []byte(`{
	  "tagOwners": {
	    "tag:ci": ["autogroup:admin"],
	    "tag:egress-mirror": ["autogroup:admin"],
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
	upsertSecretFor(t, ctx, kc, eg, "mirror")
	restartOperator(t, ctx, cs)

	ckey, err := eg.MintAuthKey(ctx, []string{"tag:app-connector"})
	must(t, err, "mint connector authkey")
	startAppConnector(t, ctx, ckey, "tailgate-mirror-connector", githubCIDR, "mirror-ok")

	// EgressGroup with NO routes — MirrorRoutes steers whatever the gateway accepts.
	name := "mirror"
	forceCleanEG(t, ctx, cs, kc, name)
	must(t, kc.Create(ctx, &egressv1.EgressGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: egressv1.EgressGroupSpec{
			MirrorRoutes: ptr.To(true),
			Selector:     egressv1.EgressSelector{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"egress": name}}},
		},
	}), "create egressgroup")
	t.Cleanup(func() { deleteEGWait(kc, name) })
	waitGatewayReady(t, ctx, cs, name)

	member := runPod(t, ctx, cs, "mirror-member", map[string]string{"egress": name})
	t.Cleanup(func() { _ = cs.CoreV1().Pods("default").Delete(context.Background(), member, *metav1.NewDeleteOptions(0)) })

	want := "mirror-ok:" + githubIP
	if err := wait.PollUntilContextTimeout(ctx, 4*time.Second, 200*time.Second, true, func(ctx context.Context) (bool, error) {
		out, _, _ := execPod(ctx, cfg, cs, member, []string{"curl", "-sS", "-m", "6", "http://" + githubIP + "/"})
		t.Logf("member curl github-IP -> %q", strings.TrimSpace(out))
		return strings.TrimSpace(out) == want, nil
	}); err != nil {
		r, _, _ := execPod(ctx, cfg, cs, member, []string{"ip", "route", "get", githubIP})
		t.Fatalf("member did not reach the connector via a MIRRORED route (no spec.routes set): %v (route get: %q)", err, strings.TrimSpace(r))
	}
	t.Logf("PASS: member reached %s through a route the agent MIRRORED from the gateway netmap — no spec.routes", githubIP)
}
