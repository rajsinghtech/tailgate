// Package agent is the node-local DaemonSet: it watches EgressGroups + Pods, and for
// each member pod wires a veth from the pod into its node-local group gateway and
// installs routes so the pod reaches the tailnet through the gateway. Selection is
// data (informer-driven), not network plumbing.
package agent

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	egressv1 "github.com/rajsinghtech/tailgate/api/v1alpha1"
)

// matchGroup returns the name of the first EgressGroup whose selector matches pod,
// or "" if none. Namespace selection is matched against nsLabels (the pod namespace's
// labels), which the caller supplies.
func matchGroup(pod *corev1.Pod, nsLabels map[string]string, groups []egressv1.EgressGroup) string {
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

// routeSet is the tailnet CIDRs a member of g must route at its gateway. The CGNAT v4
// range and the Tailscale IPv6 ULA (covers MagicDNS, peer ULAs and 4via6 mappings under
// fd7a:115c:a1e0::/48) are always included; advertised subnet CIDRs are added for
// mode=subnet. v6 CIDRs are routed even on a v4-only cluster — the veth link is dual-stack.
func routeSet(g *egressv1.EgressGroup) []string {
	out := []string{"100.64.0.0/10", "fd7a:115c:a1e0::/48"}
	out = append(out, g.Spec.Routes...)
	return out
}
