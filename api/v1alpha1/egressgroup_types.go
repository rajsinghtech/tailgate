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
// gateway is a client that USES an exit node; it never offers itself as one.
type ExitNodeRef struct {
	// NodeID is the exit node's MagicDNS name, StableID, or tailnet IP.
	NodeID string `json:"nodeID"`
	// AllowLANAccess keeps direct LAN/cluster reachability while the exit node carries
	// the default route.
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

	// Routes are the tailnet-reachable CIDRs to steer through the gateway onto member
	// pods — private subnet-router ranges and/or public app-connector ranges. CGNAT
	// (100.64.0.0/10) and the IPv6 ULA are always steered and need not be listed. A
	// non-empty Routes is the implicit "subnet" signal. We dial, not serve: the gateway
	// never ADVERTISES routes.
	// +optional
	Routes []string `json:"routes,omitempty"`

	// AcceptRoutes makes the gateway accept subnet-router and app-connector routes
	// advertised on the tailnet (--accept-routes). Defaults to true — a client egress
	// gateway normally wants whatever the tailnet makes reachable. Hot-reloadable.
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

	// Tags for the gateway tailnet node. Defaults to ["tag:egress-<name>"].
	// +optional
	Tags []string `json:"tags,omitempty"`
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
	AdvertisedRoutes string `json:"advertisedRoutes,omitempty"`
	// +optional
	MatchedPods int32 `json:"matchedPods,omitempty"`
	// DNSInjected is the number of member pods whose DNS the webhook mutated for native
	// tailnet resolution.
	// +optional
	DNSInjected int32 `json:"dnsInjected,omitempty"`
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
