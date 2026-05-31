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
	dnsOn := &egressv1.MemberDNS{Enabled: true}
	cases := []struct {
		name string
		spec egressv1.EgressGroupSpec
		want bool
	}{
		{"default off", egressv1.EgressGroupSpec{}, false},
		{"defaults on with dns", egressv1.EgressGroupSpec{DNS: dnsOn}, true},
		{"explicit true", egressv1.EgressGroupSpec{MirrorRoutes: ptr.To(true)}, true},
		{"explicit false beats dns", egressv1.EgressGroupSpec{DNS: dnsOn, MirrorRoutes: ptr.To(false)}, false},
		{"exit node disables", egressv1.EgressGroupSpec{DNS: dnsOn, ExitNode: &egressv1.ExitNodeRef{NodeID: "x"}}, false},
	}
	for _, c := range cases {
		if got := c.spec.MirrorRoutesEnabled(); got != c.want {
			t.Errorf("%s: MirrorRoutesEnabled = %v, want %v", c.name, got, c.want)
		}
	}
}
