//go:build linux

package agent

import (
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"

	"github.com/rajsinghtech/tailgate/internal/netinfo"
	"github.com/rajsinghtech/tailgate/internal/wiring"
)

// Aliases for the test file (which references these names directly). The wiring
// package is the single source of truth; these exists so the address-derivation
// test compiles without knowing about the wiring package.
var (
	gwIP4   = wiring.GwIP4
	gwCIDR4 = wiring.GwCIDR4
	gwIP6   = wiring.GwIP6
	gwCIDR6 = wiring.GwCIDR6
)

func memberAddr4(podIP string) string { return wiring.MemberAddr4(podIP) }
func memberAddr6(podIP string) string { return wiring.MemberAddr6(podIP) }

// ipnet is a local alias for wiring.IPNet (used by the exit-tunnel applier).
func ipnet(cidr string) *net.IPNet { return wiring.IPNet(cidr) }

// ensureGatewayBridge creates tgbr0 (169.254.200.1/24) in the gateway netns if absent.
func ensureGatewayBridge(gwNsPath string) error {
	return wiring.WithNetNS(gwNsPath, func() error {
		if _, err := netlink.LinkByName(wiring.GwBridge); err == nil {
			return nil
		}
		br := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: wiring.GwBridge}}
		if err := netlink.LinkAdd(br); err != nil {
			return fmt.Errorf("add bridge: %w", err)
		}
		for _, cidr := range []string{wiring.GwCIDR4, wiring.GwCIDR6} {
			addr, err := netlink.ParseAddr(cidr)
			if err != nil {
				return err
			}
			if err := netlink.AddrReplace(br, addr); err != nil {
				return fmt.Errorf("addr bridge %s: %w", cidr, err)
			}
		}
		return netlink.LinkSetUp(br)
	})
}

// Wire connects a member pod to its node-local gateway. It handles three states:
//
//   - WireNone: ts0 is absent in the pod netns (non-gVisor pod, or the CNI plugin
//     couldn't pre-wire). The agent creates the full veth pair, moves both ends,
//     and configures the member side from scratch.
//   - WireCNI: ts0 already exists in the pod netns (the CNI plugin created it at
//     ADD time for gVisor), but the gateway peer is still on the host. The agent
//     moves only the peer into the gateway netns and enslaves it to the bridge,
//     then refreshes member-side routes. ts0 is never touched (ifindex preserved).
//   - WireFull: both ends are in place (agent restart). Only routes + exit tunnel
//     are re-applied. All Replace ops — non-disruptive.
//
// Idempotent: an agent restart starts with an empty wiring map and re-Wires every
// member, but WireFull preserves the veth so live egress never flaps.
func Wire(info netinfo.PodNetInfo, gwNsPath string, routes, stale []string, exit *ExitOpts) error {
	if err := ensureGatewayBridge(gwNsPath); err != nil {
		return err
	}
	_, gwName := wiring.HostVethNames(info.PodIP)
	state := wiring.CheckWireState(info.Netns, gwNsPath, gwName)

	switch state {
	case wiring.WireCNI:
		// CNI created ts0 and left the gw peer on the host. Move it into the
		// gateway netns, enslave to bridge, bring up. Don't touch ts0.
		if err := moveGwPeerToGateway(gwNsPath, gwName); err != nil {
			return fmt.Errorf("adopt cni wiring: %w", err)
		}
		// Fall through to member-side route refresh below.

	case wiring.WireNone:
		// No ts0 — full creation. The CNI plugin didn't pre-wire (non-gVisor pod,
		// creds not ready, or no matching group at ADD time). Create the veth pair
		// and configure the member side now, leaving the gw peer on the host for
		// the move below.
		if err := wiring.SetupMember(info.PodIP, info.Netns, routes); err != nil {
			return err
		}
		// Now move the gw peer into the gateway netns.
		if err := moveGwPeerToGateway(gwNsPath, gwName); err != nil {
			return fmt.Errorf("move gw peer: %w", err)
		}

	case wiring.WireFull:
		// Both ends in place — just refresh routes + exit tunnel below.
	}

	// Member side: refresh routes (mirrored changes, stale withdrawal) and exit
	// tunnel. All Replace ops — safe on an existing ts0 regardless of who created it.
	gw4, gw6 := net.ParseIP(wiring.GwIP4), net.ParseIP(wiring.GwIP6)
	return wiring.WithNetNS(info.Netns, func() error {
		l, err := netlink.LinkByName(wiring.PodIf)
		if err != nil {
			return err
		}
		idx := l.Attrs().Index
		// Withdraw routes no longer mirrored from the gateway.
		for _, c := range stale {
			if dst := ipnet(c); dst != nil {
				_ = netlink.RouteDel(&netlink.Route{LinkIndex: idx, Dst: dst})
			}
		}
		for _, c := range routes {
			dst := ipnet(c)
			if dst == nil {
				continue
			}
			gw := gw4
			if dst.IP.To4() == nil {
				gw = gw6
			}
			if err := netlink.RouteReplace(&netlink.Route{LinkIndex: idx, Dst: dst, Gw: gw}); err != nil {
				return fmt.Errorf("route %s: %w", c, err)
			}
		}
		if exit != nil {
			if err := installExitTunnel(idx, gw4, gw6, exit.ClusterCIDRs); err != nil {
				return err
			}
		} else {
			removeExitTunnel()
		}
		return nil
	})
}

// moveGwPeerToGateway moves the host-side gateway veth peer (gwName) into the
// gateway netns, enslaves it to tgbr0, and brings it up. Called when the CNI
// plugin left the peer on the host (WireCNI) or after the agent created the
// full pair (WireNone).
func moveGwPeerToGateway(gwNsPath, gwName string) error {
	// If the peer is already in the gateway netns, nothing to do.
	peerInGw := false
	_ = wiring.WithNetNS(gwNsPath, func() error {
		if _, err := netlink.LinkByName(gwName); err == nil {
			peerInGw = true
		}
		return nil
	})
	if peerInGw {
		return nil
	}
	// Find the peer on the host netns.
	gwLink, err := netlink.LinkByName(gwName)
	if err != nil {
		return fmt.Errorf("gw peer %s not on host: %w", gwName, err)
	}
	// Open the gateway netns handle and move the link there (from the host netns).
	h, err := netns.GetFromPath(gwNsPath)
	if err != nil {
		return fmt.Errorf("open gw netns: %w", err)
	}
	defer h.Close()
	if err := netlink.LinkSetNsFd(gwLink, int(h)); err != nil {
		return fmt.Errorf("move gw peer: %w", err)
	}
	// Gateway side: enslave to bridge, up.
	return wiring.WithNetNS(gwNsPath, func() error {
		l, err := netlink.LinkByName(gwName)
		if err != nil {
			return err
		}
		br, err := netlink.LinkByName(wiring.GwBridge)
		if err != nil {
			return err
		}
		if err := netlink.LinkSetMaster(l, br); err != nil {
			return err
		}
		return netlink.LinkSetUp(l)
	})
}

// Unwire removes the member's veth (deleting either end removes the pair).
func Unwire(info netinfo.PodNetInfo, gwNsPath string) error {
	_, gwName := wiring.HostVethNames(info.PodIP)
	// The gateway end lives in the gateway netns.
	err := wiring.WithNetNS(gwNsPath, func() error {
		if l, e := netlink.LinkByName(gwName); e == nil {
			return netlink.LinkDel(l)
		}
		return nil
	})
	// Best-effort: also try the member netns (in case the move half-failed).
	_ = wiring.WithNetNS(info.Netns, func() error {
		if l, e := netlink.LinkByName(wiring.PodIf); e == nil {
			_ = netlink.LinkDel(l)
		}
		return nil
	})
	// Also clean up any host-side peer (CNI left it before the agent moved it).
	wiring.DeleteHostPeer(info.PodIP)
	return err
}
