// This file wires the F5 API types into a Kubernetes runtime.Scheme.
// The Scheme uses this info to know which Go types belong to which
// API group/version ("f5.extensions.gardener.cloud/v1alpha1").

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1" // common K8s metadata helpers
	"k8s.io/apimachinery/pkg/runtime"             // runtime.Scheme
	"k8s.io/apimachinery/pkg/runtime/schema"      // GroupVersion, GroupResource, ...
)

// -----------------------------------------------------------------------------
// Group / Version metadata
// -----------------------------------------------------------------------------

// GroupName is the API group name for all F5 CRDs in this package.
const GroupName = "f5.extensions.gardener.cloud"

// SchemeGroupVersion is the combination of API group and version
// that identifies this package's types (f5.extensions.gardener.cloud/v1alpha1).
var SchemeGroupVersion = schema.GroupVersion{
	Group:   GroupName,
	Version: "v1alpha1",
}

// -----------------------------------------------------------------------------
// Scheme registration helpers
// -----------------------------------------------------------------------------

// SchemeBuilder collects functions that know how to register our types
// with a runtime.Scheme.
var SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

// AddToScheme is what callers (e.g. main or init in another package) will use
// to register all F5 v1alpha1 types into their Scheme instance.
var AddToScheme = SchemeBuilder.AddToScheme

// addKnownTypes tells the Scheme about our CRD Go types.
// This must include every top-level object + its list type.
func addKnownTypes(scheme *runtime.Scheme) error {
	// Register the F5LoadBalancerConfig and its list type.
	scheme.AddKnownTypes(SchemeGroupVersion,
		&F5LoadBalancerConfig{},
		&F5LoadBalancerConfigList{},
	)

	// Also register the group/version itself so the scheme can map between
	// "f5.extensions.gardener.cloud/v1alpha1" <-> Go types.
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)

	return nil
}

// Kind is a helper that returns a GroupKind for this API group.
func Kind(kind string) schema.GroupKind {
	return SchemeGroupVersion.WithKind(kind).GroupKind()
}

// Resource is a helper that returns a GroupResource for this API group.
func Resource(resource string) schema.GroupResource {
	return SchemeGroupVersion.WithResource(resource).GroupResource()
}
