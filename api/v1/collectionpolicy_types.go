// api/v1/collectionpolicy_types.go
package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CollectionPolicySpec defines the desired state of CollectionPolicy
type CollectionPolicySpec struct {
	// TargetSelector specifies which resources to collect
	TargetSelector TargetSelector `json:"targetSelector,omitempty"`

	// Exclusions specifies resources to exclude from collection
	Exclusions Exclusions `json:"exclusions,omitempty"`

	// Policies defines collection behavior
	Policies Policies `json:"policies,omitempty"`
}

// TargetSelector specifies which resources to collect
type TargetSelector struct {
	// Namespaces to watch (empty means all)
	Namespaces []string `json:"namespaces,omitempty"`

	// Labels to select
	Labels map[string]string `json:"labels,omitempty"`
}

// Exclusions specifies resources to exclude from collection
type Exclusions struct {
	// ExcludedNamespaces are namespaces to exclude from collection
	ExcludedNamespaces []string `json:"excludedNamespaces,omitempty"`

	// ExcludedPods are pods to exclude from collection
	ExcludedPods []ExcludedPod `json:"excludedPods,omitempty"`

	// ExcludedLabels are label selectors to exclude
	ExcludedLabels map[string]string `json:"excludedLabels,omitempty"`

	// ExcludedNodes are nodes to exclude from collection
	ExcludedNodes []string `json:"excludedNodes,omitempty"`
}

// ExcludedPod identifies a pod to exclude
type ExcludedPod struct {
	// Namespace is the pod's namespace
	Namespace string `json:"namespace"`

	// PodName is the pod's name
	PodName string `json:"podName"`
}

// Policies defines collection behavior
type Policies struct {
	// PulseURL is the URL of the Pulse service
	PulseURL string `json:"pulseURL,omitempty"`

	// Frequency is how often to collect resource usage metrics
	Frequency string `json:"frequency,omitempty"`

	// BufferSize is the size of the sender buffer
	BufferSize int `json:"bufferSize,omitempty"`
}

// CollectionPolicyStatus defines the observed state of CollectionPolicy
type CollectionPolicyStatus struct {
	// Conditions represents the latest available observations of the policy's state
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// CollectedResources is the count of resources being collected
	CollectedResources int `json:"collectedResources,omitempty"`

	// LastUpdated is when the policy was last updated
	LastUpdated metav1.Time `json:"lastUpdated,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// CollectionPolicy is the Schema for the collectionpolicies API
type CollectionPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CollectionPolicySpec   `json:"spec,omitempty"`
	Status CollectionPolicyStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// CollectionPolicyList contains a list of CollectionPolicy
type CollectionPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CollectionPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CollectionPolicy{}, &CollectionPolicyList{})
}
