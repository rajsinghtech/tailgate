//go:build linux

package wiring

import (
	"fmt"
	"net"
	"runtime"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// WireState describes how far along a member's veth wiring is, so the agent
// knows whether to create from scratch, adopt a CNI-created ts0, or just
// refresh routes.
type WireState int

const (
	WireNone WireState = iota // ts0 absent in the pod netns — full creation needed
	WireCNI                   // ts0 exists, peer still on the host — move peer into gateway
	WireFull                  // ts0 exists, peer already in the gateway — routes-only refresh
)

// WithNetNS enters the namespace at path and runs fn inside it, restoring the
// original namespace on return. The calling goroutine is locked to an OS thread
// for the duration (namespace switches are thread-local).
func WithNetNS(path string, fn func() error) error {
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

// CheckWireState inspects the pod and gateway netns to determine the wiring state.
// memberName is the deterministic host-side peer name from HostVethNames.
func CheckWireState(memberNs, gwNsPath, gwName string) WireState {
	ts0Exists := false
	_ = WithNetNS(memberNs, func() error {
		if _, err := netlink.LinkByName(PodIf); err == nil {
			ts0Exists = true
		}
		return nil
	})
	if !ts0Exists {
		return WireNone
	}
	peerInGw := false
	_ = WithNetNS(gwNsPath, func() error {
		if _, err := netlink.LinkByName(gwName); err == nil {
			peerInGw = true
		}
		return nil
	})
	if peerInGw {
		return WireFull
	}
	return WireCNI
}

// SetupMember creates the ts0 interface inside the pod netns at CNI ADD time —
// BEFORE the sandbox boots. This is required for gVisor, which scrapes the netns
// for interfaces + addresses + routes only at sandbox start and does not
// hot-plug interfaces added afterward.
//
// It creates a veth pair in the host netns, moves one end into the pod netns
// (renaming it to ts0), assigns the dual-stack link-local address, sets the MTU,
// and installs the base tailnet routes (100.64.0.0/10, fd7a:115c:a1e0::/48) toward
// the gateway bridge IP. The other end (the gateway peer) is left on the host
// for the agent to move into the gateway netns later.
//
// Idempotent: if ts0 already exists (e.g. a CNI ADD retry), it re-applies
// addresses/routes only and leaves the peer in place.
func SetupMember(podIP, podNetns string, routes []string) error {
	memberName, gwName := HostVethNames(podIP)

	// Does ts0 already exist in the pod netns? (CNI ADD retry, or the agent
	// raced ahead for a non-gVisor pod.)
	ts0Exists := false
	if err := WithNetNS(podNetns, func() error {
		if _, err := netlink.LinkByName(PodIf); err == nil {
			ts0Exists = true
		}
		return nil
	}); err != nil {
		return err
	}

	if !ts0Exists {
		// Clean any stale host-side veth from a prior partial attempt.
		if l, err := netlink.LinkByName(memberName); err == nil {
			_ = netlink.LinkDel(l)
		}
		// Create the veth pair in the host netns. The "member" end will be
		// moved into the pod; the "gw" end stays on the host for the agent.
		veth := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: memberName}, PeerName: gwName}
		if err := netlink.LinkAdd(veth); err != nil {
			return fmt.Errorf("add veth: %w", err)
		}
		memberLink, err := netlink.LinkByName(memberName)
		if err != nil {
			return err
		}
		// Move only the member end into the pod netns. The gw peer stays on host.
		h, err := netns.GetFromPath(podNetns)
		if err != nil {
			_ = netlink.LinkDel(veth) // both ends still on host — clean up
			return fmt.Errorf("open pod netns: %w", err)
		}
		defer h.Close()
		if err := netlink.LinkSetNsFd(memberLink, int(h)); err != nil {
			_ = netlink.LinkDel(veth)
			return fmt.Errorf("move member end: %w", err)
		}
	}

	// Member side: (rename freshly-moved end → ts0 if new), assign addresses,
	// set MTU, bring up, install routes. All Replace ops — idempotent on retry.
	addr4, err := netlink.ParseAddr(MemberAddr4(podIP))
	if err != nil {
		return err
	}
	addr6, err := netlink.ParseAddr(MemberAddr6(podIP))
	if err != nil {
		return err
	}
	gw4, gw6 := net.ParseIP(GwIP4), net.ParseIP(GwIP6)

	return WithNetNS(podNetns, func() error {
		var l netlink.Link
		if ts0Exists {
			l, err = netlink.LinkByName(PodIf)
			if err != nil {
				return err
			}
		} else {
			// Remove a stale ts0 from a previous wiring, then rename the moved end.
			if old, e := netlink.LinkByName(PodIf); e == nil {
				_ = netlink.LinkDel(old)
			}
			l, err = netlink.LinkByName(memberName)
			if err != nil {
				return err
			}
			if err := netlink.LinkSetName(l, PodIf); err != nil {
				return err
			}
			l, err = netlink.LinkByName(PodIf)
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
		if l.Attrs().MTU != TunMTU {
			if err := netlink.LinkSetMTU(l, TunMTU); err != nil {
				return fmt.Errorf("member ts0 mtu: %w", err)
			}
		}
		if err := netlink.LinkSetUp(l); err != nil {
			return err
		}
		idx := l.Attrs().Index
		for _, c := range routes {
			dst := IPNet(c)
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
		return nil
	})
}

// DeleteHostPeer removes the host-side gateway peer veth (named by gwName) if it
// is still lingering on the host — useful when the pod is deleted (CNI DEL)
// before the agent moved the peer into the gateway netns.
func DeleteHostPeer(podIP string) {
	_, gwName := HostVethNames(podIP)
	DeleteHostLink(gwName)
}

// DeleteHostLink removes a host-netns link by name if present.
func DeleteHostLink(name string) {
	if name == "" {
		return
	}
	if l, err := netlink.LinkByName(name); err == nil {
		_ = netlink.LinkDel(l)
	}
}

// LinkExistsOnHost reports whether a link with the given name exists in the
// current (host) netns.
func LinkExistsOnHost(name string) bool {
	_, err := netlink.LinkByName(name)
	return err == nil
}
