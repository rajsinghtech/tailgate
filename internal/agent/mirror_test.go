//go:build linux

package agent

import (
	"reflect"
	"testing"

	"k8s.io/utils/ptr"

	egressv1 "github.com/rajsinghtech/tailgate/api/v1alpha1"
)

func TestOverlapsCluster(t *testing.T) {
	a := &Agent{ClusterCIDRs: []string{"10.244.0.0/16", "10.96.0.0/12"}}
	cases := map[string]bool{
		"192.168.169.0/24":   false, // app subnet, no overlap
		"104.119.184.202/32": false,
		"10.244.1.5/32":      true, // inside pod CIDR
		"10.96.0.10/32":      true, // inside service CIDR
		"10.0.0.0/8":         true, // covers the cluster ranges
		"not-a-cidr":         true, // unparseable -> never steer
	}
	for cidr, want := range cases {
		if got := a.overlapsCluster(cidr); got != want {
			t.Errorf("overlapsCluster(%q) = %v, want %v", cidr, got, want)
		}
	}
}

func TestSubtractStrings(t *testing.T) {
	got := subtractStrings([]string{"a", "b", "c"}, []string{"b"})
	if want := []string{"a", "c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("subtractStrings = %v, want %v", got, want)
	}
	if got := subtractStrings(nil, []string{"a"}); got != nil {
		t.Fatalf("subtractStrings(nil,...) = %v, want nil", got)
	}
}

func TestMirrorRoutesEnabled(t *testing.T) {
	cases := []struct {
		name string
		spec egressv1.EgressGroupSpec
		want bool
	}{
		{"default on (accepts routes by default)", egressv1.EgressGroupSpec{}, true},
		{"acceptRoutes off disables", egressv1.EgressGroupSpec{AcceptRoutes: ptr.To(false)}, false},
		{"exit node disables", egressv1.EgressGroupSpec{ExitNode: &egressv1.ExitNodeRef{Name: "x"}}, false},
		{"exit node disables even with accept routes", egressv1.EgressGroupSpec{AcceptRoutes: ptr.To(true), ExitNode: &egressv1.ExitNodeRef{Name: "x"}}, false},
	}
	for _, c := range cases {
		if got := c.spec.MirrorRoutesEnabled(); got != c.want {
			t.Errorf("%s: MirrorRoutesEnabled = %v, want %v", c.name, got, c.want)
		}
	}
}
