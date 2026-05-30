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

// memberAddr4 derives a stable /16 link-local v4 address for the member's ts0.
func memberAddr4(podIP string) string {
	h := podHash(podIP)
	b2 := byte(h>>8) % 254 // 0..253
	b3 := byte(h) % 254
	if b2 == 0 {
		b2 = 1
	}
	if b3 == 0 {
		b3 = 2 // skip .0.1 (gateway); any non-1 is fine, and b2>=1 already avoids .0.x
	}
	return fmt.Sprintf("169.254.%d.%d/16", b2, b3)
}

// memberAddr6 derives a stable /64 v6 address for the member's ts0 (>=::1:0000 so never ::1).
func memberAddr6(podIP string) string {
	return fmt.Sprintf("fd96:7467::%x:%x/64", 1+(podHash(podIP)>>16), uint16(podHash(podIP))|1)
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

// Wire connects a member pod to its node-local gateway: a veth pair with the member
// end (ts0) in the pod netns routing tailnet CIDRs at the gateway bridge, and the
// gateway end enslaved to tgbr0 in the gateway netns. Idempotent.
func Wire(info netinfo.PodNetInfo, gwNsPath string, routes []string, exit *ExitOpts) error {
	if err := ensureGatewayBridge(gwNsPath); err != nil {
		return err
	}
	memberName, gwName := hostVethNames(info.PodIP)

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

	// Move ends into their namespaces.
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
		return fmt.Errorf("move member end: %w", err)
	}
	if err := netlink.LinkSetNsFd(gwLink, int(gwH)); err != nil {
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

	// Member side: rename to ts0, assign dual-stack link addrs (so return traffic is
	// symmetric), up, then route tailnet CIDRs via the same-family gateway bridge IP.
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
		// Remove a stale ts0 from a previous wiring (e.g. re-wire after gateway restart).
		if old, e := netlink.LinkByName(podIf); e == nil {
			_ = netlink.LinkDel(old)
		}
		l, err := netlink.LinkByName(memberName)
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
		if err := netlink.AddrReplace(l, addr4); err != nil {
			return fmt.Errorf("member v4 addr: %w", err)
		}
		if err := netlink.AddrReplace(l, addr6); err != nil {
			return fmt.Errorf("member v6 addr: %w", err)
		}
		if err := netlink.LinkSetUp(l); err != nil {
			return err
		}
		idx := l.Attrs().Index
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
