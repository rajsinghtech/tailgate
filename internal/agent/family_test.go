package agent

import (
	"net/netip"
	"testing"
)

// routeSet must always carry both the v4 CGNAT range and the v6 ULA so a member reaches
// the tailnet on either family regardless of the cluster's primary IP family. (Accepted
// subnet/app-connector routes are added on top by route-mirroring, not by routeSet.)
func TestRouteSetFamilies(t *testing.T) {
	got := routeSet()

	var have4CGNAT, haveULA bool
	for _, c := range got {
		switch c {
		case "100.64.0.0/10":
			have4CGNAT = true
		case "fd7a:115c:a1e0::/48":
			haveULA = true
		}
		// every entry must parse as a prefix
		if _, err := netip.ParsePrefix(c); err != nil {
			t.Fatalf("route %q is not a valid prefix: %v", c, err)
		}
	}
	if !have4CGNAT {
		t.Error("routeSet missing v4 CGNAT 100.64.0.0/10")
	}
	if !haveULA {
		t.Error("routeSet missing v6 ULA fd7a:115c:a1e0::/48")
	}
	if len(got) != 2 {
		t.Errorf("expected exactly CGNAT + ULA, got %v", got)
	}
}
