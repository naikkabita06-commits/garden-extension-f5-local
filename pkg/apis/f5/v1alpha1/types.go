package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// -----------------------------------------------------------------------------
// Top‑level CRD type
// -----------------------------------------------------------------------------

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
//
// F5LoadBalancerConfig is the root object of our CRD.
// Each instance represents "how this tenant/Shoot should be wired to CMP + CIS".
type F5LoadBalancerConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   F5LoadBalancerConfigSpec   `json:"spec,omitempty"`
	Status F5LoadBalancerConfigStatus `json:"status,omitempty"`
}

// -----------------------------------------------------------------------------
// Spec: desired configuration
// -----------------------------------------------------------------------------

// F5LoadBalancerConfigSpec contains all user‑provided config fields.
//
// It is intentionally focused on CMP + CIS configuration, not raw BIG‑IP
// objects like pools/monitors/virtual servers.
type F5LoadBalancerConfigSpec struct {
	// CcpApiEndpoint is the CMP LBaaS / CCP base URL used for control‑plane LB.
	CcpApiEndpoint string `json:"ccpApiEndpoint"`

	// TenantOrPartition is the CMP tenant or BIG‑IP partition to use.
	TenantOrPartition string `json:"tenantOrPartition"`

	// CredentialsSecretRef points to a Secret with CMP/CIS credentials.
	//
	// It is optional because some modes (e.g. application LB disabled and CMP
	// provisioning disabled) don't need any credentials.
	CredentialsSecretRef *corev1.SecretReference `json:"credentialsSecretRef,omitempty"`

	// ControlPlaneVIP is the VIP used for the Shoot's kube‑apiserver.
	ControlPlaneVIP string `json:"controlPlaneVIP"`

	// ControlPlaneReady is an optional override that indicates whether the
	// control-plane VIP/VS has been provisioned and configured externally (e.g.
	// by CMP out-of-band).
	//
	// When set:
	//   - true: the controller treats the control-plane LB as Ready
	//   - false: the controller treats the control-plane LB as NotReady
	//
	// When omitted (nil), the controller falls back to its dev-stub behavior.
	ControlPlaneReady *bool `json:"controlPlaneReady,omitempty"`

	// EnablePerShootControlPlaneVIP toggles per-Shoot dedicated F5 VIP for the kube-apiserver
	// (Mechanism B). When false (default), control-plane access uses the shared Seed Ingress
	// VIP via Istio SNI routing, identical to vanilla Gardener behaviour. When true, the
	// controller calls CMP to allocate a dedicated VIP per Shoot and programs a Virtual Server
	// directly to that Shoot's kube-apiserver NodePort — bypassing Istio entirely.
	EnablePerShootControlPlaneVIP bool `json:"enablePerShootControlPlaneVIP,omitempty"`

	// EnableApplicationLB toggles CIS‑based application load balancers for Shoots.
	EnableApplicationLB bool `json:"enableApplicationLB"`

	// CIS holds configuration for deploying CIS into Shoots (application plane).
	// It is optional; when omitted, only control‑plane LB is managed.
	CIS *CISConfig `json:"cis,omitempty"`

	// CMP LBaaS v2.1 provisioning fields.
	// These are required when using the CMP LBaaS flow (LBService → VIP → VirtualServer).

	// FlavorID is the CMP LB flavor ID to use when creating an LB Service.
	FlavorID int32 `json:"flavorId,omitempty"`

	// NetworkID is the CMP network/subnet ID for the LB Service.
	NetworkID string `json:"networkId,omitempty"`

	// VPCID is the CMP VPC ID for the LB Service.
	VPCID string `json:"vpcId,omitempty"`

	// VPCName is the CMP VPC name for the LB Service.
	VPCName string `json:"vpcName,omitempty"`

	// RoutingAlgorithm is the load-balancing algorithm for virtual servers (e.g. "round_robin").
	RoutingAlgorithm string `json:"routingAlgorithm,omitempty"`

	// MonitorInterval is the health-check interval in seconds for virtual server nodes.
	MonitorInterval int32 `json:"monitorInterval,omitempty"`
}

// CISConfig contains the knobs required to run CIS in Shoots.
type CISConfig struct {
	// Image is the CIS container image, e.g. "f5networks/k8s-bigip-ctlr:<tag>".
	Image string `json:"image"`

	// BridgeImage is an optional image for a small Shoot-side controller that
	// translates Services of type LoadBalancer into Ingresses understood by CIS,
	// and mirrors the desired VIP into Service status.
	//
	// When empty, only the native CIS triggers (Ingress/ConfigMap) are supported.
	BridgeImage string `json:"bridgeImage,omitempty"`

	// BigIPURL is the BIG‑IP management URL CIS talks to.
	BigIPURL string `json:"bigipUrl"`

	// Partition is the BIG‑IP partition CIS should use for application LBs.
	Partition string `json:"partition"`

	// ExtraArgs is a generic, implementation‑defined bag for CIS tuning flags.
	ExtraArgs []string `json:"extraArgs,omitempty"`
}

// -----------------------------------------------------------------------------
// Status: observed state
// -----------------------------------------------------------------------------

// F5LoadBalancerConfigStatus describes what the controller observes / has done.
//
// For Story 1 this is intentionally minimal; later stories will fill and
// interpret these fields more precisely.
type F5LoadBalancerConfigStatus struct {
	// VIP is the VIP actually configured on CMP/F5 for the control‑plane LB.
	VIP string `json:"vip,omitempty"`

	// VirtualServerName is the name/ID of the control‑plane VS on CMP/F5.
	VirtualServerName string `json:"virtualServerName,omitempty"`

	// LBServiceID is the CMP LB Service ID created for this control-plane LB.
	LBServiceID string `json:"lbServiceId,omitempty"`

	// VIPPortID is the CMP VIP port ID allocated for this control-plane LB.
	VIPPortID string `json:"vipPortId,omitempty"`

	// VirtualServerID is the CMP Virtual Server ID for this control-plane LB.
	VirtualServerID string `json:"virtualServerId,omitempty"`

	// Conditions represent the current state of the F5 configuration.
	// Typical conditions include:
	//   - Ready
	//   - Progressing
	//   - Error
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// -----------------------------------------------------------------------------
// List type
// -----------------------------------------------------------------------------

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
//
// F5LoadBalancerConfigList is the list wrapper used by the API server.
type F5LoadBalancerConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []F5LoadBalancerConfig `json:"items"`
}

// -----------------------------------------------------------------------------
// Deep copy implementations
// -----------------------------------------------------------------------------
//
// NOTE: These replace the previous shallow-copy stubs. Run `make generate` in
// future to replace these with the controller-gen generated zz_generated.deepcopy.go.

// DeepCopyInto copies all fields of F5LoadBalancerConfig into out, handling all
// pointer, slice, and nested struct fields correctly.
func (c *F5LoadBalancerConfig) DeepCopyInto(out *F5LoadBalancerConfig) {
	*out = *c
	out.TypeMeta = c.TypeMeta
	c.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	c.Spec.DeepCopyInto(&out.Spec)
	c.Status.DeepCopyInto(&out.Status)
}

// DeepCopyObject implements runtime.Object for F5LoadBalancerConfig.
func (c *F5LoadBalancerConfig) DeepCopyObject() runtime.Object {
	if c == nil {
		return nil
	}
	out := new(F5LoadBalancerConfig)
	c.DeepCopyInto(out)
	return out
}

// Hub marks F5LoadBalancerConfig v1alpha1 as the CRD conversion hub version.
// When a second API version (e.g. v1beta1) is introduced, the controller-runtime
// conversion webhook will use this type as the intermediate representation and
// route ConvertTo/ConvertFrom calls through it. Until then this is a no-op that
// satisfies the conversion.Hub interface so the webhook can be wired up in advance.
func (*F5LoadBalancerConfig) Hub() {}

// DeepCopyInto copies all fields of F5LoadBalancerConfigSpec into out.
func (s *F5LoadBalancerConfigSpec) DeepCopyInto(out *F5LoadBalancerConfigSpec) {
	*out = *s
	if s.CredentialsSecretRef != nil {
		in, outRef := &s.CredentialsSecretRef, &out.CredentialsSecretRef
		*outRef = new(corev1.SecretReference)
		**outRef = **in
	}
	if s.ControlPlaneReady != nil {
		in, outRef := &s.ControlPlaneReady, &out.ControlPlaneReady
		*outRef = new(bool)
		**outRef = **in
	}
	if s.CIS != nil {
		in, outRef := &s.CIS, &out.CIS
		*outRef = new(CISConfig)
		(*in).DeepCopyInto(*outRef)
	}
}

// DeepCopyInto copies all fields of CISConfig into out.
func (c *CISConfig) DeepCopyInto(out *CISConfig) {
	*out = *c
	if c.ExtraArgs != nil {
		out.ExtraArgs = make([]string, len(c.ExtraArgs))
		copy(out.ExtraArgs, c.ExtraArgs)
	}
}

// DeepCopyInto copies all fields of F5LoadBalancerConfigStatus into out.
func (s *F5LoadBalancerConfigStatus) DeepCopyInto(out *F5LoadBalancerConfigStatus) {
	*out = *s
	if s.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(s.Conditions))
		for i := range s.Conditions {
			s.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

// DeepCopyInto copies all fields of F5LoadBalancerConfigList into out.
func (c *F5LoadBalancerConfigList) DeepCopyInto(out *F5LoadBalancerConfigList) {
	*out = *c
	out.TypeMeta = c.TypeMeta
	c.ListMeta.DeepCopyInto(&out.ListMeta)
	if c.Items != nil {
		out.Items = make([]F5LoadBalancerConfig, len(c.Items))
		for i := range c.Items {
			c.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

// DeepCopyObject implements runtime.Object for F5LoadBalancerConfigList.
func (c *F5LoadBalancerConfigList) DeepCopyObject() runtime.Object {
	if c == nil {
		return nil
	}
	out := new(F5LoadBalancerConfigList)
	c.DeepCopyInto(out)
	return out
}
