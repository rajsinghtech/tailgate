//go:build linux

package netfilter

import (
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/vishvananda/netlink"
)

// SetupPolicyRouting steers fwmark'd member traffic into the TUN and pins the gateway's own
// tailnet routes, all via netlink (no `ip rule`/`ip route` shell-outs). Idempotent: the
// fwmark rules are deleted-then-added and the routes use Replace. Call only after the TUN
// (tunIf) exists.
func SetupPolicyRouting(fwmark uint32, table int, tunIf string) error {
	link, err := netlink.LinkByName(tunIf)
	if err != nil {
		return fmt.Errorf("tun %s not ready: %w", tunIf, err)
	}
	idx := link.Attrs().Index
	fullMask := uint32(0xffffffff)

	// fwmark <mark> -> lookup <table>, both families.
	for _, fam := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		r := netlink.NewRule()
		r.Mark = fwmark
		r.Mask = &fullMask
		r.Table = table
		r.Priority = 1000
		r.Family = fam
		_ = netlink.RuleDel(r)
		if err := netlink.RuleAdd(r); err != nil {
			return fmt.Errorf("fwmark rule (family %d): %w", fam, err)
		}
	}

	// default route via the TUN in <table>, both families — so marked member traffic exits
	// the TUN and tailscaled routes each destination per its netmap.
	def4 := &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}
	def6 := &net.IPNet{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)}
	if err := netlink.RouteReplace(&netlink.Route{Table: table, LinkIndex: idx, Dst: def4}); err != nil {
		return fmt.Errorf("table %d v4 default: %w", table, err)
	}
	if err := netlink.RouteReplace(&netlink.Route{Table: table, LinkIndex: idx, Dst: def6}); err != nil {
		return fmt.Errorf("table %d v6 default: %w", table, err)
	}

	// the gateway's OWN egress to tailnet peers (CGNAT v4 + Tailscale ULA v6, covering
	// MagicDNS/4via6) goes out the TUN via the main table.
	for _, c := range []string{"100.64.0.0/10", "fd7a:115c:a1e0::/48"} {
		_, dst, err := net.ParseCIDR(c)
		if err != nil {
			return err
		}
		if err := netlink.RouteReplace(&netlink.Route{LinkIndex: idx, Dst: dst}); err != nil {
			return fmt.Errorf("main route %s: %w", c, err)
		}
	}
	return nil
}

// EnableForwardingAndRelaxRPFilter enables IPv4/IPv6 forwarding and disables reverse-path
// filtering on every interface via /proc/sys. Member egress is policy-routed asymmetrically
// (reply arrives on the TUN while the main-table route to that source is via eth0), so
// strict rp_filter would drop replies as martians. Effective rp_filter = max(all, per-iface),
// so every existing iface must be zeroed, not just `all`. Best-effort per iface.
func EnableForwardingAndRelaxRPFilter() error {
	if err := writeProcSys("/proc/sys/net/ipv4/ip_forward", "1"); err != nil {
		return err
	}
	if err := writeProcSys("/proc/sys/net/ipv6/conf/all/forwarding", "1"); err != nil {
		return err
	}
	matches, _ := filepath.Glob("/proc/sys/net/ipv4/conf/*/rp_filter")
	for _, p := range matches {
		_ = writeProcSys(p, "0") // best-effort across all/default/eth0/...
	}
	return nil
}

func writeProcSys(path, val string) error {
	if err := os.WriteFile(path, []byte(val), 0o644); err != nil {
		return fmt.Errorf("write %s=%s: %w", path, val, err)
	}
	return nil
}
