// Package agent is the node-local DaemonSet: it watches EgressGroups + Pods, and for
// each member pod wires a veth from the pod into its node-local group gateway and
// installs routes so the pod reaches the tailnet through the gateway. Selection is
// data (informer-driven), not network plumbing.
package agent

import (
	"github.com/rajsinghtech/tailgate/internal/wiring"
)

// routeSet is the base tailnet CIDRs every member routes at its gateway. Delegates to
// wiring.RouteSet (shared with the CNI plugin, which needs the same route set at ADD time).
func routeSet() []string {
	return wiring.RouteSet()
}
