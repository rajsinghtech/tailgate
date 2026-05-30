package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// EgressMode selects what the per-group gateway makes reachable.
type EgressMode string

const (
	// ModeCGNAT: reach tailnet peers (100.64.0.0/10 + ULA).
	ModeCGNAT EgressMode = "cgnat"
	// ModeSubnet: reach advertised subnet-router CIDRs (plus CGNAT).
	ModeSubnet EgressMode = "subnet"
)

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

// EgressGroupSpec is the desired state of an EgressGroup.
type EgressGroupSpec struct {
	// Selector picks member pods (gateway-side selection; no Multus needed).
	Selector EgressSelector `json:"selector"`

	// Mode selects what the gateway makes reachable.
	// +kubebuilder:validation:Enum=cgnat;subnet
	// +kubebuilder:default=cgnat
	Mode EgressMode `json:"mode,omitempty"`

	// Datapath: kernel (full-fat TUN, default) or userspace (netstack).
	// +kubebuilder:validation:Enum=kernel;userspace
	// +kubebuilder:default=kernel
	Datapath string `json:"datapath,omitempty"`

	// Attach: how member pods reach the gateway. MVP supports "routed".
	// +kubebuilder:validation:Enum=routed
	// +kubebuilder:default=routed
	Attach string `json:"attach,omitempty"`

	// Routes are the tailnet-reachable CIDRs to steer through the gateway onto member
	// pods — private subnet-router ranges and/or public app-connector ranges. This
	// drives member-side route programming only; the gateway accepts routes via
	// AcceptRoutes. CGNAT (100.64.0.0/10) and the IPv6 ULA are always steered and need
	// not be listed. We dial, not serve: the gateway never ADVERTISES routes.
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

	// Tailnet is informational for the MVP (the gateway joins whatever tailnet the
	// operator's OAuth credentials belong to).
	// +optional
	Tailnet string `json:"tailnet,omitempty"`

	// Replicas of the gateway Deployment. Pod churn never touches these.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	Replicas int32 `json:"replicas,omitempty"`

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
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=eg
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Attach",type=string,JSONPath=`.spec.attach`
// +kubebuilder:printcolumn:name="Pods",type=integer,JSONPath=`.status.matchedPods`
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
