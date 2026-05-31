package controller

import (
	"os"
	"path/filepath"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"tailscale.com/ipn/conffile"

	egressv1 "github.com/rajsinghtech/tailgate/api/v1alpha1"
)

func exitID(eg *egressv1.EgressGroup) string {
	if eg.Spec.ExitNode != nil {
		return eg.Spec.ExitNode.NodeID
	}
	return ""
}

// The rendered config must parse via the real conffile loader — which validates the
// "alpha0" version and rejects unknown fields (DisallowUnknownFields). This catches any
// field-name/casing drift or an accidental tags field before it ever hits a gateway.
func TestRenderGatewayConfigRoundTrips(t *testing.T) {
	tt := true
	ff := false
	cases := map[string]*egressv1.EgressGroup{
		"defaults": {ObjectMeta: metav1.ObjectMeta{Name: "alpha"}},
		"accept-routes-off": {
			ObjectMeta: metav1.ObjectMeta{Name: "beta"},
			Spec:       egressv1.EgressGroupSpec{AcceptRoutes: &ff},
		},
		"exit-node": {
			ObjectMeta: metav1.ObjectMeta{Name: "gamma"},
			Spec: egressv1.EgressGroupSpec{
				AcceptRoutes: &tt,
				ExitNode:     &egressv1.ExitNodeRef{NodeID: "exit-1.tail1234.ts.net", AllowLANAccess: true},
			},
		},
	}

	for name, eg := range cases {
		t.Run(name, func(t *testing.T) {
			b, err := renderGatewayConfig(eg, exitID(eg))
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			path := filepath.Join(t.TempDir(), "tailscaled.json")
			if err := os.WriteFile(path, b, 0o600); err != nil {
				t.Fatal(err)
			}
			c, err := conffile.Load(path)
			if err != nil {
				t.Fatalf("conffile.Load rejected rendered config: %v\nconfig was: %s", err, b)
			}
			// DNS is forced on regardless of spec.
			if !c.Parsed.AcceptDNS.EqualBool(true) {
				t.Errorf("acceptDNS not forced on: %q", c.Parsed.AcceptDNS)
			}
			// Node must come up.
			if c.Parsed.Enabled.EqualBool(false) {
				t.Error("Enabled should be true")
			}
			// Authkey referenced by file, never inlined.
			if c.Parsed.AuthKey == nil || *c.Parsed.AuthKey != "file:"+gwAuthKeyPath {
				t.Errorf("authkey ref = %v, want file:%s", c.Parsed.AuthKey, gwAuthKeyPath)
			}
			// Netfilter must be off so tailscaled doesn't fight our MASQUERADE.
			if c.Parsed.NetfilterMode == nil || *c.Parsed.NetfilterMode != "off" {
				t.Errorf("netfilterMode = %v, want off", c.Parsed.NetfilterMode)
			}
			// Exit node round-trips when set.
			if eg.Spec.ExitNode != nil {
				if c.Parsed.ExitNode == nil || *c.Parsed.ExitNode != eg.Spec.ExitNode.NodeID {
					t.Errorf("exitNode = %v, want %s", c.Parsed.ExitNode, eg.Spec.ExitNode.NodeID)
				}
				if !c.Parsed.AllowLANWhileUsingExitNode.EqualBool(true) {
					t.Errorf("allowLANWhileUsingExitNode = %q, want true", c.Parsed.AllowLANWhileUsingExitNode)
				}
			}
			// AcceptRoutes honors the spec.
			wantAR := eg.Spec.AcceptRoutes == nil || *eg.Spec.AcceptRoutes
			if c.Parsed.AcceptRoutes.EqualBool(true) != wantAR {
				t.Errorf("acceptRoutes = %q, want %v", c.Parsed.AcceptRoutes, wantAR)
			}
		})
	}
}
