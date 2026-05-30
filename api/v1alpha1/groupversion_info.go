// Package v1alpha1 contains the EgressGroup API for tailgate.
// +kubebuilder:object:generate=true
// +groupName=tailscale.rajsingh.info
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the API group/version for tailgate.
	GroupVersion = schema.GroupVersion{Group: "tailscale.rajsingh.info", Version: "v1alpha1"}
	// SchemeBuilder registers the API types.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
	// AddToScheme adds the types to a runtime.Scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func init() { SchemeBuilder.Register(&EgressGroup{}, &EgressGroupList{}) }
