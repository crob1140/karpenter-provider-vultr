package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	ConditionReady          = "Ready"
	NodeClassHashAnnotation = "karpenter.vultr.com/nodeclass-hash"
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

	// UserData is base64-encoded user-data passed to Vultr. If omitted, the provider generates bootstrap data.
	// +optional
	UserData string `json:"userData,omitempty"`

	// EnableIPv6 controls whether Vultr assigns IPv6 networking.
	// +optional
	EnableIPv6 *bool `json:"enableIPv6,omitempty"`
}

type VultrNodeClassStatus struct {
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

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
