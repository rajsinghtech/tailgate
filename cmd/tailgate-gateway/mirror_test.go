//go:build linux

package main

import (
	"net/netip"
	"reflect"
	"testing"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/types/key"
	"tailscale.com/types/views"
)

func peerWith(cidrs ...string) *ipnstate.PeerStatus {
	pfxs := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		pfxs = append(pfxs, netip.MustParsePrefix(c))
	}
	s := views.SliceOf(pfxs)
	return &ipnstate.PeerStatus{AllowedIPs: &s}
}

// reachableRoutes keeps only the advertised subnet-router + app-connector CIDRs — not the
// peers' own CGNAT/ULA host routes (already steered) and not the exit node's default route.
func TestReachableRoutes(t *testing.T) {
	st := &ipnstate.Status{Peer: map[key.NodePublic]*ipnstate.PeerStatus{
		key.NewNode().Public(): peerWith("100.64.0.5/32", "fd7a:115c:a1e0::5/128", "192.168.169.0/24"), // subnet router
		key.NewNode().Public(): peerWith("100.64.0.6/32", "104.119.184.202/32"),                        // app-connector /32
		key.NewNode().Public(): peerWith("100.64.0.7/32", "0.0.0.0/0", "::/0"),                         // exit node
		key.NewNode().Public(): peerWith("100.64.0.8/32"),                                              // plain peer
	}}
	got := reachableRoutes(st)
	want := []string{"104.119.184.202/32", "192.168.169.0/24"} // sorted; CGNAT/ULA/default dropped
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reachableRoutes = %v, want %v", got, want)
	}
}
