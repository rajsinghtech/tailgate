package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// EgressSelector picks the pods whose egress is steered through the group gateway.
type EgressSelector struct {
	// +optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`
	// +optional
	PodSelector *metav1.LabelSelector `json:"podSelector,omitempty"`
}

// ExitNodeRef selects a tailnet exit node to route all member egress through. The
// gateway is a client that USES an exit node; it never offers itself as one. The surface
// mirrors the native `tailscale set --exit-node`: pin a node, or say "auto".
type ExitNodeRef struct {
	// Name selects the exit node using the same value space as the native
	// `tailscale set --exit-node`: a tailnet IP, a MagicDNS name, a StableNodeID,
	// or "auto" / "auto:any" to let an eligible exit node be picked automatically.
	// Empty clears the exit node. (The declarative tailscaled config cannot carry
	// "auto", so for "auto" the operator resolves a concrete node and re-resolves on
	// a periodic requeue for failover; an explicit ref is passed through verbatim.)
	// +optional
	Name string `json:"name,omitempty"`
	// AllowLANAccess keeps the member's direct LAN/cluster reachability while the exit
	// node carries the default route. It is a property of the exit-node *client* (the
	// gateway), mapping to tailscaled's AllowLANWhileUsingExitNode
	// (== `tailscale up --exit-node-allow-lan-access`).
	// +optional
	AllowLANAccess bool `json:"allowLANAccess,omitempty"`
}

// MemberDNS makes matched member pods native tailnet DNS clients. When Enabled, the
// operator's mutating webhook injects dnsPolicy=None + a dnsConfig that points primarily at
// the gateway's MagicDNS (100.100.100.100) — which already serves the whole tailnet
// namespace (MagicDNS *.ts.net, split-DNS domains, app-connector names, global forwarding) —
// and keeps the in-cluster resolver as a secondary for cluster.local. Off unless Enabled.
type MemberDNS struct {
	// Enabled turns on resolv.conf injection for matched members.
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// ClusterDNS is the in-cluster resolver ClusterIP used as the secondary nameserver for
	// cluster.local. Auto-detected from the kube-dns/CoreDNS Service when empty.
	// +optional
	ClusterDNS string `json:"clusterDNS,omitempty"`

	// SearchDomains overrides the injected search list. Defaults to
	// [<ns>.svc.cluster.local, svc.cluster.local, cluster.local].
	// +optional
	SearchDomains []string `json:"searchDomains,omitempty"`

	// Ndots for the injected resolv.conf options. Defaults to 5 (standard Kubernetes).
	// +optional
	Ndots *int32 `json:"ndots,omitempty"`
}

// EgressGroupSpec is the desired state of an EgressGroup.
type EgressGroupSpec struct {
	// Selector picks member pods (gateway-side selection; no Multus needed).
	Selector EgressSelector `json:"selector"`

	// AcceptRoutes makes the gateway accept the subnet-router and app-connector routes
	// advertised on the tailnet (--accept-routes), and the agent steers those onto member
	// pods — so members reach everything the gateway's tag is granted to reach. Defaults to
	// true; set false to restrict members to CGNAT peers only. To reach a specific subset,
	// scope the gateway's tag with grants (the tailnet policy layer), not a per-CIDR list.
	// Hot-reloadable. We dial, not serve: the gateway never ADVERTISES routes.
	// +optional
	AcceptRoutes *bool `json:"acceptRoutes,omitempty"`

	// ExitNode routes all member egress (default route) through a chosen tailnet exit
	// node. Optional; hot-reloadable. Selecting one puts 0.0.0.0/0 + ::/0 on member
	// pods (in a dedicated policy table, never the pod main table).
	// +optional
	ExitNode *ExitNodeRef `json:"exitNode,omitempty"`

	// DNS makes matched member pods native tailnet DNS clients (see MemberDNS).
	// +optional
	DNS *MemberDNS `json:"dns,omitempty"`

	// Tags for the gateway tailnet node. The OAuth client must own these. Defaults to
	// ["tag:k8s"] (the Tailscale operator's convention) when unset.
	// +optional
	Tags []string `json:"tags,omitempty"`
}

// AcceptRoutesEnabled reports whether the gateway accepts advertised subnet-router and
// app-connector routes (--accept-routes). Defaults to true.
func (s *EgressGroupSpec) AcceptRoutesEnabled() bool {
	if s.AcceptRoutes != nil {
		return *s.AcceptRoutes
	}
	return true
}

// MirrorRoutesEnabled reports whether the agent steers the gateway's accepted routes onto
// members — on whenever the gateway accepts routes, so a member reaches everything the gateway
// can (the native-client behaviour); off under ExitNode (the full tunnel already covers it).
// This is internal plumbing for the gateway/member netns split, not a user-facing knob.
func (s *EgressGroupSpec) MirrorRoutesEnabled() bool {
	return s.ExitNode == nil && s.AcceptRoutesEnabled()
}

// EgressGroupStatus is the observed state of an EgressGroup.
type EgressGroupStatus struct {
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	GatewayHostname string `json:"gatewayHostname,omitempty"`
	// +optional
	MatchedPods int32 `json:"matchedPods,omitempty"`
	// DNSInjected is the number of member pods whose DNS the webhook mutated for native
	// tailnet resolution.
	// +optional
	DNSInjected int32 `json:"dnsInjected,omitempty"`
	// ResolvedExitNode is the concrete exit node the gateway was pinned to — an echo of a
	// static NodeID, or the node the operator auto-selected.
	// +optional
	ResolvedExitNode string `json:"resolvedExitNode,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=eg
// +kubebuilder:printcolumn:name="Pods",type=integer,JSONPath=`.status.matchedPods`
// +kubebuilder:printcolumn:name="DNS",type=boolean,JSONPath=`.spec.dns.enabled`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// EgressGroup is a set of pods that egress onto the tailnet through one shared gateway.
type EgressGroup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              EgressGroupSpec   `json:"spec,omitempty"`
	Status            EgressGroupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// EgressGroupList is a list of EgressGroup.
type EgressGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EgressGroup `json:"items"`
}
