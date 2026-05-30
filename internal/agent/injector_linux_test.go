//go:build linux

package agent

import (
	"net/netip"
	"testing"
)

// The member's ts0 gets a dual-stack link address derived from the pod IP. Verify both
// families parse, sit in the right private link ranges, never collide with the gateway,
// and are collision-free across a large spread of pod IPs (stable per-IP hashing).
func TestMemberAddrFamilies(t *testing.T) {
	gw4 := netip.MustParseAddr(gwIP4)
	gw6 := netip.MustParseAddr(gwIP6)
	link4 := netip.MustParsePrefix(gwCIDR4)
	link6 := netip.MustParsePrefix(gwCIDR6)

	seen4 := map[netip.Addr]string{}
	seen6 := map[netip.Addr]string{}
	for a := 0; a < 256; a++ {
		for b := 0; b < 16; b++ {
			ip := netip.AddrFrom4([4]byte{10, byte(a), byte(b), 7}).String()

			p4, err := netip.ParsePrefix(memberAddr4(ip))
			if err != nil {
				t.Fatalf("memberAddr4(%s) bad: %v", ip, err)
			}
			p6, err := netip.ParsePrefix(memberAddr6(ip))
			if err != nil {
				t.Fatalf("memberAddr6(%s) bad: %v", ip, err)
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
