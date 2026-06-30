// Package wiring holds the shared datapath primitives used by both the CNI plugin
// (which creates ts0 in the pod netns at CNI ADD time, before the sandbox boots) and
// the agent (which moves the gateway-side peer into the gateway netns and keeps
// routes fresh). Everything here is platform-independent; the netlink/netns calls
// live in netns_linux.go.
package wiring

import (
	"fmt"
	"hash/fnv"
	"net"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	egressv1 "github.com/rajsinghtech/tailgate/api/v1alpha1"
)

const (
	GwBridge = "tgbr0"
	// The veth link to the gateway is a private dual-stack link (NOT the tailnet ranges),
	// independent of the cluster's primary CNI family — so a v4-only cluster still carries
	// v6 (ULA / 4via6) tailnet traffic to the gateway.
	GwIP4   = "169.254.0.1"
	GwCIDR4 = "169.254.0.1/16"
	GwIP6   = "fd96:7467::1"
	GwCIDR6 = "fd96:7467::1/64"
	PodIf   = "ts0"
	// TunMTU matches the gateway's tailscale0 MTU (Tailscale's standard 1280). The member
	// egresses all tailnet traffic through that TUN, so ts0 must carry the same MTU. With the
	// default 1500 the member negotiates a 1460 TCP MSS and large segments (e.g. a TLS
	// ServerHello) blackhole on the smaller tunnel — PMTUD is unreliable over relayed/DERP
	// exit-node paths, so this surfaced as full-tunnel egress hanging on HTTPS while ICMP/DNS
	// worked. Setting ts0=1280 makes the member a correct 1280-MTU tailnet client.
	TunMTU = 1280
)

// PodHash returns a stable FNV-1a hash of the pod IP, used for deterministic veth names.
func PodHash(podIP string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(podIP))
	return h.Sum32()
}

// HostVethNames returns deterministic host-side veth names for a pod (<=15 chars).
// The "member" end is the host-side peer of ts0 (left on the host by CNI ADD until
// the agent moves it into the gateway netns). The "gw" end is what gets enslaved to
// tgbr0 inside the gateway netns.
func HostVethNames(podIP string) (member, gw string) {
	s := fmt.Sprintf("%08x", PodHash(podIP))
	return "tgm" + s, "tgg" + s
}

// MemberAddr4 derives a stable /16 link address for the member's ts0. For a v4 pod IP it
// uses the pod IP's last two octets, which are unique across a node's pod CIDR — so members
// sharing a gateway never collide (a hash-into-/16 would birthday-collide). Falls back to a
// hash for v6-only pods.
func MemberAddr4(podIP string) string {
	b2, b3 := AddrOctets(podIP)
	if b2 == 0 && b3 <= 1 {
		b3 = 2 // avoid 169.254.0.0 and .0.1 (the gateway bridge)
	}
	return fmt.Sprintf("169.254.%d.%d/16", b2, b3)
}

// MemberAddr6 derives the matching /64 link address (host = 1:<two octets>, never ::1).
func MemberAddr6(podIP string) string {
	b2, b3 := AddrOctets(podIP)
	return fmt.Sprintf("fd96:7467::1:%x/64", uint16(b2)<<8|uint16(b3))
}

// AddrOctets returns the last two octets of a v4 pod IP (unique per node), or two hash bytes
// for a non-v4 (v6-only) pod IP.
func AddrOctets(podIP string) (byte, byte) {
	if ip := net.ParseIP(podIP); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			return v4[2], v4[3]
		}
	}
	h := PodHash(podIP)
	return byte(h >> 8), byte(h)
}

// RouteSet is the base tailnet CIDRs every member routes at its gateway: the CGNAT v4 range
// and the Tailscale IPv6 ULA (covers MagicDNS, peer ULAs and 4via6 mappings under
// fd7a:115c:a1e0::/48). The gateway's accepted subnet-router / app-connector routes are added
// on top by route-mirroring. Both families are routed even on a single-family cluster — the
// veth link is dual-stack.
func RouteSet() []string {
	return []string{"100.64.0.0/10", "fd7a:115c:a1e0::/48"}
}

// IPNet parses a CIDR string into a *net.IPNet, returning nil on error.
func IPNet(cidr string) *net.IPNet {
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil
	}
	return n
}

// MatchGroup returns the name of the first EgressGroup whose selector matches pod,
// or "" if none. Namespace selection is matched against nsLabels (the pod namespace's
// labels), which the caller supplies.
func MatchGroup(pod *corev1.Pod, nsLabels map[string]string, groups []egressv1.EgressGroup) string {
	for i := range groups {
		g := &groups[i]
		sel := g.Spec.Selector
		if sel.NamespaceSelector != nil {
			s, err := metav1.LabelSelectorAsSelector(sel.NamespaceSelector)
			if err != nil || !s.Matches(labels.Set(nsLabels)) {
				continue
			}
		}
		if sel.PodSelector != nil {
			s, err := metav1.LabelSelectorAsSelector(sel.PodSelector)
			if err != nil || !s.Matches(labels.Set(pod.Labels)) {
				continue
			}
		} else if sel.NamespaceSelector == nil {
			// empty selector matches nothing (avoid grabbing every pod by accident)
			continue
		}
		return g.Name
	}
	return ""
}
