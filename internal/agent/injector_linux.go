//go:build linux

package agent

import (
	"fmt"
	"hash/fnv"
	"net"
	"runtime"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"

	"github.com/rajsinghtech/tailgate/internal/netinfo"
)

const (
	gwBridge = "tgbr0"
	// The veth link to the gateway is a private dual-stack link (NOT the tailnet ranges),
	// independent of the cluster's primary CNI family — so a v4-only cluster still carries
	// v6 (ULA / 4via6) tailnet traffic to the gateway.
	gwIP4   = "169.254.0.1"
	gwCIDR4 = "169.254.0.1/16"
	gwIP6   = "fd96:7467::1"
	gwCIDR6 = "fd96:7467::1/64"
	podIf   = "ts0"
	// tunMTU matches the gateway's tailscale0 MTU (Tailscale's standard 1280). The member
	// egresses all tailnet traffic through that TUN, so ts0 must carry the same MTU. With the
	// default 1500 the member negotiates a 1460 TCP MSS and large segments (e.g. a TLS
	// ServerHello) blackhole on the smaller tunnel — PMTUD is unreliable over relayed/DERP
	// exit-node paths, so this surfaced as full-tunnel egress hanging on HTTPS while ICMP/DNS
	// worked. Setting ts0=1280 makes the member a correct 1280-MTU tailnet client.
	tunMTU = 1280
)

func podHash(podIP string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(podIP))
	return h.Sum32()
}

// hostVethNames returns deterministic host-side veth names for a pod (<=15 chars).
func hostVethNames(podIP string) (member, gw string) {
	s := fmt.Sprintf("%08x", podHash(podIP))
	return "tgm" + s, "tgg" + s
}

// memberAddr4 derives a stable /16 link address for the member's ts0. For a v4 pod IP it
// uses the pod IP's last two octets, which are unique across a node's pod CIDR — so members
// sharing a gateway never collide (a hash-into-/16 would birthday-collide). Falls back to a
// hash for v6-only pods.
func memberAddr4(podIP string) string {
	b2, b3 := addrOctets(podIP)
	if b2 == 0 && b3 <= 1 {
		b3 = 2 // avoid 169.254.0.0 and .0.1 (the gateway bridge)
	}
	return fmt.Sprintf("169.254.%d.%d/16", b2, b3)
}

// memberAddr6 derives the matching /64 link address (host = 1:<two octets>, never ::1).
func memberAddr6(podIP string) string {
	b2, b3 := addrOctets(podIP)
	return fmt.Sprintf("fd96:7467::1:%x/64", uint16(b2)<<8|uint16(b3))
}

// addrOctets returns the last two octets of a v4 pod IP (unique per node), or two hash bytes
// for a non-v4 (v6-only) pod IP.
func addrOctets(podIP string) (byte, byte) {
	if ip := net.ParseIP(podIP); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			return v4[2], v4[3]
		}
	}
	h := podHash(podIP)
	return byte(h >> 8), byte(h)
}

func withNetNS(path string, fn func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	orig, err := netns.Get()
	if err != nil {
		return err
	}
	defer func() { _ = netns.Set(orig); orig.Close() }()
	h, err := netns.GetFromPath(path)
	if err != nil {
		return fmt.Errorf("open netns %s: %w", path, err)
	}
	defer h.Close()
	if err := netns.Set(h); err != nil {
		return fmt.Errorf("enter netns %s: %w", path, err)
	}
	return fn()
}

// ensureGatewayBridge creates tgbr0 (169.254.200.1/24) in the gateway netns if absent.
func ensureGatewayBridge(gwNsPath string) error {
	return withNetNS(gwNsPath, func() error {
		if _, err := netlink.LinkByName(gwBridge); err == nil {
			return nil
		}
		br := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: gwBridge}}
		if err := netlink.LinkAdd(br); err != nil {
			return fmt.Errorf("add bridge: %w", err)
		}
		for _, cidr := range []string{gwCIDR4, gwCIDR6} {
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

// alreadyWired reports whether the member is already stitched to THIS gateway: ts0
// present in the member netns AND its peer (the gw-side veth) present in the current
// gateway netns. A gateway pod restart changes gwNsPath, so the peer is absent there and
// this returns false (forcing a re-stitch onto the new gateway); a mere agent restart
// leaves both ends in place, so it returns true and Wire stays non-destructive.
func alreadyWired(memberNs, gwNsPath, gwName string) bool {
	have := false
	if err := withNetNS(memberNs, func() error {
		if _, e := netlink.LinkByName(podIf); e == nil {
			have = true
		}
		return nil
	}); err != nil || !have {
		return false
	}
	peer := false
	if err := withNetNS(gwNsPath, func() error {
		if _, e := netlink.LinkByName(gwName); e == nil {
			peer = true
		}
		return nil
	}); err != nil {
		return false
	}
	return peer
}

// Wire connects a member pod to its node-local gateway: a veth pair with the member
// end (ts0) in the pod netns routing tailnet CIDRs at the gateway bridge, and the
// gateway end enslaved to tgbr0 in the gateway netns. Idempotent and non-disruptive:
// if the member is already stitched to this gateway the veth is left in place and only
// the addrs/routes are re-applied (all Replace ops), so an agent restart — which starts
// with an empty wiring map and re-Wires every member — never flaps live egress.
func Wire(info netinfo.PodNetInfo, gwNsPath string, routes, stale []string, exit *ExitOpts) error {
	if err := ensureGatewayBridge(gwNsPath); err != nil {
		return err
	}
	memberName, gwName := hostVethNames(info.PodIP)
	adopted := alreadyWired(info.Netns, gwNsPath, gwName)

	if !adopted {
		// Clean any stale host-side veth from a prior attempt, then create the pair in
		// the agent's (host) netns.
		if l, err := netlink.LinkByName(memberName); err == nil {
			_ = netlink.LinkDel(l)
		}
		veth := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: memberName}, PeerName: gwName}
		if err := netlink.LinkAdd(veth); err != nil {
			return fmt.Errorf("add veth: %w", err)
		}
		memberLink, err := netlink.LinkByName(memberName)
		if err != nil {
			return err
		}
		gwLink, err := netlink.LinkByName(gwName)
		if err != nil {
			return err
		}

		// Move ends into their namespaces; on any partial failure delete the pair so a
		// half-moved veth never lingers in the host netns.
		memberH, err := netns.GetFromPath(info.Netns)
		if err != nil {
			_ = netlink.LinkDel(veth)
			return fmt.Errorf("open member netns: %w", err)
		}
		defer memberH.Close()
		gwH, err := netns.GetFromPath(gwNsPath)
		if err != nil {
			_ = netlink.LinkDel(veth)
			return fmt.Errorf("open gw netns: %w", err)
		}
		defer gwH.Close()
		if err := netlink.LinkSetNsFd(memberLink, int(memberH)); err != nil {
			_ = netlink.LinkDel(veth) // both ends still in host netns
			return fmt.Errorf("move member end: %w", err)
		}
		if err := netlink.LinkSetNsFd(gwLink, int(gwH)); err != nil {
			_ = netlink.LinkDel(gwLink) // member end already moved; deleting the gw end drops the pair
			return fmt.Errorf("move gw end: %w", err)
		}

		// Gateway side: enslave to bridge, up.
		if err := withNetNS(gwNsPath, func() error {
			l, err := netlink.LinkByName(gwName)
			if err != nil {
				return err
			}
			br, err := netlink.LinkByName(gwBridge)
			if err != nil {
				return err
			}
			if err := netlink.LinkSetMaster(l, br); err != nil {
				return err
			}
			return netlink.LinkSetUp(l)
		}); err != nil {
			return fmt.Errorf("gw side: %w", err)
		}
	}

	// Member side: (rename the freshly-moved end to ts0 if new), assign dual-stack link
	// addrs (so return traffic is symmetric), up, then route tailnet CIDRs via the
	// same-family gateway bridge IP. All Replace ops — safe to re-run on an adopted wiring.
	gw4, gw6 := net.ParseIP(gwIP4), net.ParseIP(gwIP6)
	addr4, err := netlink.ParseAddr(memberAddr4(info.PodIP))
	if err != nil {
		return err
	}
	addr6, err := netlink.ParseAddr(memberAddr6(info.PodIP))
	if err != nil {
		return err
	}
	return withNetNS(info.Netns, func() error {
		var l netlink.Link
		if adopted {
			// Live ts0 already present — reuse it, never delete it.
			l, err = netlink.LinkByName(podIf)
			if err != nil {
				return err
			}
		} else {
			// Remove a stale ts0 from a previous wiring, then rename the freshly-moved end.
			if old, e := netlink.LinkByName(podIf); e == nil {
				_ = netlink.LinkDel(old)
			}
			l, err = netlink.LinkByName(memberName)
			if err != nil {
				return err
			}
			if err := netlink.LinkSetName(l, podIf); err != nil {
				return err
			}
			l, err = netlink.LinkByName(podIf)
			if err != nil {
				return err
			}
		}
		if err := netlink.AddrReplace(l, addr4); err != nil {
			return fmt.Errorf("member v4 addr: %w", err)
		}
		if err := netlink.AddrReplace(l, addr6); err != nil {
			return fmt.Errorf("member v6 addr: %w", err)
		}
		// Match the gateway's tailscale0 MTU so member TCP negotiates an MSS that fits the
		// tunnel (no large-segment blackhole over relayed exit-node paths). Idempotent.
		if l.Attrs().MTU != tunMTU {
			if err := netlink.LinkSetMTU(l, tunMTU); err != nil {
				return fmt.Errorf("member ts0 mtu: %w", err)
			}
		}
		if err := netlink.LinkSetUp(l); err != nil {
			return err
		}
		idx := l.Attrs().Index
		// Withdraw routes no longer mirrored from the gateway (e.g. an app-connector /32 the
		// gateway stopped advertising). Best-effort: ignore if already gone.
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
				gw = gw6 // v6 CIDR -> v6 gateway
			}
			if err := netlink.RouteReplace(&netlink.Route{LinkIndex: idx, Dst: dst, Gw: gw}); err != nil {
				return fmt.Errorf("route %s: %w", c, err)
			}
		}
		if exit != nil {
			// Full-tunnel: send everything-but-cluster through the gateway to the exit node.
			if err := installExitTunnel(idx, gw4, gw6, exit.ClusterCIDRs); err != nil {
				return err
			}
		} else {
			// Not (or no longer) an exit-node member: clear any stale exit rules.
			removeExitTunnel()
		}
		return nil
	})
}

// Unwire removes the member's veth (deleting either end removes the pair).
func Unwire(info netinfo.PodNetInfo, gwNsPath string) error {
	_, gwName := hostVethNames(info.PodIP)
	// The gateway end lives in the gateway netns.
	err := withNetNS(gwNsPath, func() error {
		if l, e := netlink.LinkByName(gwName); e == nil {
			return netlink.LinkDel(l)
		}
		return nil
	})
	// Best-effort: also try the member netns (in case the move half-failed).
	_ = withNetNS(info.Netns, func() error {
		if l, e := netlink.LinkByName(podIf); e == nil {
			_ = netlink.LinkDel(l)
		}
		return nil
	})
	return err
}

func ipnet(cidr string) *net.IPNet {
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil
	}
	return n
}
