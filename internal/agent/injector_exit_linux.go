//go:build linux

package agent

import (
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
)

// Linux applier for the exit-node plan in injector_exit.go. Programs policy rules in the
// member's own netns (so they only affect this pod).

// installExitTunnel programs the member netns (call from within withNetNS) for full-tunnel
// egress through the gateway, carving cluster CIDRs back to the main table.
func installExitTunnel(memberIdx int, gw4, gw6 net.IP, clusterCIDRs []string) error {
	def4 := &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}
	def6 := &net.IPNet{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)}
	if err := netlink.RouteReplace(&netlink.Route{Table: exitTable, LinkIndex: memberIdx, Dst: def4, Gw: gw4}); err != nil {
		return fmt.Errorf("exit v4 default: %w", err)
	}
	if err := netlink.RouteReplace(&netlink.Route{Table: exitTable, LinkIndex: memberIdx, Dst: def6, Gw: gw6}); err != nil {
		return fmt.Errorf("exit v6 default: %w", err)
	}

	prio := exitCarvePrio
	for _, d := range exitRulePlan(clusterCIDRs) {
		r := netlink.NewRule()
		if d.ToMain {
			dst := ipnet(d.CIDR)
			if dst == nil {
				continue
			}
			r.Dst = dst
			r.Table = mainTable
			r.Priority = prio
			r.Family = familyOf(dst.IP)
			prio++
		} else {
			// catch-all for both families
			for _, fam := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
				cr := netlink.NewRule()
				cr.Table = exitTable
				cr.Priority = exitFromPrio
				cr.Family = fam
				_ = netlink.RuleDel(cr)
				if err := netlink.RuleAdd(cr); err != nil {
					return fmt.Errorf("exit catch-all rule (family %d): %w", fam, err)
				}
			}
			continue
		}
		_ = netlink.RuleDel(r)
		if err := netlink.RuleAdd(r); err != nil {
			return fmt.Errorf("exit carve %s: %w", d.CIDR, err)
		}
	}
	return nil
}

// removeExitTunnel best-effort clears any exit-node rules in the current netns (used when
// a pod leaves an exit-node group while still alive). No-op if none exist.
func removeExitTunnel() {
	for _, fam := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		rules, err := netlink.RuleList(fam)
		if err != nil {
			continue
		}
		for i := range rules {
			if rules[i].Table == exitTable || (rules[i].Priority >= exitCarvePrio && rules[i].Priority <= exitFromPrio) {
				_ = netlink.RuleDel(&rules[i])
			}
		}
	}
}

func familyOf(ip net.IP) int {
	if ip.To4() != nil {
		return netlink.FAMILY_V4
	}
	return netlink.FAMILY_V6
}
