package wiring

import (
	"net/netip"
	"testing"
)

// Verify the shared constants are stable and non-colliding (the same invariants the
// old agent's injector_linux_test.go checked, now against the wiring package so both
// the CNI plugin and the agent share the same derivation).
func TestMemberAddrFamilies(t *testing.T) {
	gw4 := netip.MustParseAddr(GwIP4)
	gw6 := netip.MustParseAddr(GwIP6)
	link4 := netip.MustParsePrefix(GwCIDR4)
	link6 := netip.MustParsePrefix(GwCIDR6)

	seen4 := map[netip.Addr]string{}
	seen6 := map[netip.Addr]string{}
	for a := 0; a < 32; a++ {
		for b := 2; b < 255; b++ {
			ip := netip.AddrFrom4([4]byte{10, 244, byte(a), byte(b)}).String()

			p4, err := netip.ParsePrefix(MemberAddr4(ip))
			if err != nil {
				t.Fatalf("MemberAddr4(%s) bad: %v", ip, err)
			}
			p6, err := netip.ParsePrefix(MemberAddr6(ip))
			if err != nil {
				t.Fatalf("MemberAddr6(%s) bad: %v", ip, err)
			}
			if !link4.Contains(p4.Addr()) {
				t.Fatalf("v4 %s not in link range %s", p4, link4)
			}
			if !link6.Contains(p6.Addr()) {
				t.Fatalf("v6 %s not in link range %s", p6, link6)
			}
			if p4.Addr() == gw4 {
				t.Fatalf("member v4 %s collides with gateway %s (pod %s)", p4.Addr(), gw4, ip)
			}
			if p6.Addr() == gw6 {
				t.Fatalf("member v6 %s collides with gateway %s (pod %s)", p6.Addr(), gw6, ip)
			}
			if prev, ok := seen4[p4.Addr()]; ok && prev != ip {
				t.Fatalf("v4 collision %s: pods %s and %s", p4.Addr(), prev, ip)
			}
			if prev, ok := seen6[p6.Addr()]; ok && prev != ip {
				t.Fatalf("v6 collision %s: pods %s and %s", p6.Addr(), prev, ip)
			}
			seen4[p4.Addr()] = ip
			seen6[p6.Addr()] = ip
		}
	}
}

func TestRouteSetFamilies(t *testing.T) {
	got := RouteSet()
	var have4, have6 bool
	for _, c := range got {
		switch c {
		case "100.64.0.0/10":
			have4 = true
		case "fd7a:115c:a1e0::/48":
			have6 = true
		}
		if _, err := netip.ParsePrefix(c); err != nil {
			t.Fatalf("route %q is not a valid prefix: %v", c, err)
		}
	}
	if !have4 {
		t.Error("RouteSet missing v4 CGNAT 100.64.0.0/10")
	}
	if !have6 {
		t.Error("RouteSet missing v6 ULA fd7a:115c:a1e0::/48")
	}
	if len(got) != 2 {
		t.Errorf("expected exactly CGNAT + ULA, got %v", got)
	}
}

func TestHostVethNames(t *testing.T) {
	member, gw := HostVethNames("10.244.1.5")
	if len(member) > 15 || len(gw) > 15 {
		t.Fatalf("veth names too long: %s(%d) %s(%d)", member, len(member), gw, len(gw))
	}
	if member == gw {
		t.Fatal("member and gw names are the same")
	}
	// Deterministic
	m2, g2 := HostVethNames("10.244.1.5")
	if member != m2 || gw != g2 {
		t.Fatal("HostVethNames is not deterministic")
	}
	// Different IPs produce different names
	m3, _ := HostVethNames("10.244.1.6")
	if m3 == member {
		t.Fatal("different pods got the same member veth name")
	}
}

func TestIPNet(t *testing.T) {
	if IPNet("not-a-cidr") != nil {
		t.Fatal("IPNet should return nil for invalid CIDR")
	}
	n := IPNet("100.64.0.0/10")
	if n == nil || n.String() != "100.64.0.0/10" {
		t.Fatalf("IPNet returned wrong value: %v", n)
	}
}
