package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	ConditionReady          = "Ready"
	NodeClassHashAnnotation = "karpenter.vultr.com/nodeclass-hash"

	// NodeClassHashVersionAnnotation records which revision of the hash
	// function produced NodeClassHashAnnotation. Drift is only evaluated
	// between a NodeClaim and a NodeClass that agree on this value.
	NodeClassHashVersionAnnotation = "karpenter.vultr.com/nodeclass-hash-version"

	// TerminationFinalizer keeps a VultrNodeClass around until every NodeClaim
	// using it has been terminated. Karpenter core ships no NodeClass
	// controller, so without it a NodeClass can be deleted out from under
	// running nodes and their NodeClaims can no longer be resolved.
	TerminationFinalizer = "karpenter.vultr.com/termination"
)

// VultrNodeClassSpec contains provider-specific configuration used to create Vultr instances.
type VultrNodeClassSpec struct {
	// Region is the Vultr region ID, e.g. syd.
	// +kubebuilder:validation:MinLength=1
	Region string `json:"region"`

	// OSID is the Vultr operating system ID.
	// +optional
	OSID *int `json:"osID,omitempty"`

	// SnapshotID selects an account snapshot instead of an OS image.
	// +optional
	SnapshotID string `json:"snapshotID,omitempty"`

	// Plan is an optional hard constraint. Normally NodePool requirements select plans.
	// +optional
	Plan string `json:"plan,omitempty"`

	// SSHKeyIDs are Vultr SSH key IDs installed on the instance.
	// +optional
	SSHKeyIDs []string `json:"sshKeyIDs,omitempty"`

	// VPCIDs are Vultr VPC IDs attached to the instance.
	// +optional
	VPCIDs []string `json:"vpcIDs,omitempty"`

	// KubernetesVersion is the Kubernetes minor version used to install kubeadm and kubelet.
	// Use the same minor version as the control plane, for example v1.35.
	// +kubebuilder:validation:Pattern=`^v?1\.[0-9]+(?:\.[0-9]+)?$`
	KubernetesVersion string `json:"kubernetesVersion"`

	// ClusterEndpoint is the externally reachable Kubernetes API server endpoint,
	// for example https://k8s.example.com:6443.
	// +kubebuilder:validation:MinLength=1
	ClusterEndpoint string `json:"clusterEndpoint"`

	// CACertHash optionally overrides the CA hash calculated from kube-system/kube-root-ca.crt.
	// Format: sha256:<64 hex characters>.
	// +optional
	CACertHash string `json:"caCertHash,omitempty"`

	// ExtraUserData is an optional shell script executed after kubeadm join succeeds.
	// It is intended for provider-specific node customization, not cluster bootstrap.
	// +optional
	ExtraUserData string `json:"extraUserData,omitempty"`

	// EnableIPv6 controls whether Vultr assigns IPv6 networking.
	// +optional
	EnableIPv6 *bool `json:"enableIPv6,omitempty"`
}

type VultrNodeClassStatus struct {
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ResolvedPlanIDs is the set of Vultr plans currently reported as available
	// in the configured region. It is informational and may change independently
	// of the NodeClass spec.
	// +optional
	ResolvedPlanIDs []string `json:"resolvedPlanIDs,omitempty"`

	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=vnc
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Region",type="string",JSONPath=".spec.region"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type VultrNodeClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              VultrNodeClassSpec   `json:"spec,omitempty"`
	Status            VultrNodeClassStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type VultrNodeClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VultrNodeClass `json:"items"`
}
