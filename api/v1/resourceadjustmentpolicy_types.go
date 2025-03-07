/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// ResourceAdjustmentPolicySpec defines the desired state of ResourceAdjustmentPolicy
type ResourceAdjustmentPolicySpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	TargetSelector TargetSelector `json:"targetSelector"`
	Policies       Policies       `json:"policies"`
	Logging        Logging        `json:"logging"`
	Exclusions     Exclusions     `json:"exclusions"`
}

// TargetSelector specifies the labels and namespaces to match resources
type TargetSelector struct {
	MatchLabels map[string]string `json:"matchLabels"`
	Namespaces  []string          `json:"namespaces"`
}

// Exclusions specifies resources to exclude from adjustments
type Exclusions struct {
	ExcludedPods []PodReference `json:"excludedPods,omitempty"` // List of pods to exclude
}

// PodReference specifies a pod's name and namespace to be excluded
type PodReference struct {
	Namespace string `json:"namespace"`
	PodName   string `json:"podName"`
}

// Policies defines the resource adjustment policies
type Policies struct {
	MetricsSources       []string             `json:"metricsSources"`
	PromURL              string               `json:"promUrl"`
	PulseURL             string               `json:"pulseUrl"`
	Frequency            string               `json:"frequency"`
	AutoApply            bool                 `json:"autoApply"`
	LookbackDuration     string               `json:"lookbackDuration"`
	PidController        PidController        `json:"pidController"`
	CPURecommendation    CPURecommendation    `json:"cpuRecommendation"`
	MemoryRecommendation MemoryRecommendation `json:"memoryRecommendation"`
	Normalization        Normalization        `json:"normalization"`
}

// Pid controller configuration
type PidController struct {
	PropertionalGain              string `json:"propertionalGain"`
	IntegralGain                  string `json:"integralGain"`
	DerivativeGain                string `json:"derivativeGain"`
	AntiWindUpGain                string `json:"antiWindUpGain"`
	IntegralDischargeTimeConstant string `json:"integralDischargeTimeConstant"`
	LowPassTimeConstant           string `json:"lowPassTimeConstant"`
	MaxOutput                     string `json:"maxOutput"`
	MinOutput                     string `json:"minOutput"`
}

// CPURecommendation specifies policies for CPU adjustments
type CPURecommendation struct {
	SampleInterval    string `json:"sampleInterval"`
	RequestPercentile string `json:"requestPercentile"`
	MarginFraction    string `json:"marginFraction"`
	TargetUtilization string `json:"targetUtilization"`
	HistoryLength     string `json:"historyLength"`
}

// MemoryRecommendation specifies policies for memory adjustments
type MemoryRecommendation struct {
	SampleInterval    string `json:"sampleInterval"`
	RequestPercentile string `json:"requestPercentile"`
	MarginFraction    string `json:"marginFraction"`
	TargetUtilization string `json:"targetUtilization"`
	HistoryLength     string `json:"historyLength"`
	OOMProtection     bool   `json:"oomProtection"`
	OOMBumpRatio      string `json:"oomBumpRatio"`
}

// Normalization specifies normalization policies
type Normalization struct {
	Enabled bool   `json:"enabled"`
	Config  string `json:"config"`
}

// Logging defines logging configurations
type Logging struct {
	Level           string `json:"level"`
	EnableAuditLogs bool   `json:"enableAuditLogs"`
}

// ResourceAdjustmentPolicyStatus defines the observed state of ResourceAdjustmentPolicy
type ResourceAdjustmentPolicyStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	Containers []ContainerStatus  `json:"containers,omitempty"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ContainerStatus holds detailed status for each monitored container.
type ContainerStatus struct {
	Namespace            string                `json:"namespace"`            // Namespace of the container
	PodName              string                `json:"podName"`              // Name of the pod
	ContainerName        string                `json:"containerName"`        // Name of the container
	LastUpdated          metav1.Time           `json:"lastUpdated"`          // Timestamp of the last update
	CurrentCPU           ResourceUsage         `json:"currentCPU"`           // Current CPU details
	CurrentMemory        ResourceUsage         `json:"currentMemory"`        // Current memory details
	CPURecommendation    RecommendationDetails `json:"cpuRecommendation"`    // CPU recommendation details
	MemoryRecommendation RecommendationDetails `json:"memoryRecommendation"` // Memory recommendation details
	NodeSelectionResult  NodeSelectionResult   `json:"nodeSelectionResult"`  // Node selection result
	OOMEvents            []OOMEvent            `json:"oomEvents,omitempty"`  // List of OOM events for this container
}

// ResourceUsage holds details about current resource usage and limits.
type ResourceUsage struct {
	Request string `json:"request,omitempty"`
	Limit   string `json:"limit,omitempty"` // Current resource limit (e.g., "1.0 cores", "512 Mi")
	Usage   string `json:"usage,omitempty"` // Current resource usage (e.g., "0.173 cores", "10548964 bytes")
}

// NodeSelectionResult contains the decision about pod placement
type NodeSelectionResult struct {
	NeedsMigration bool   `json:"needsMigration"` // Flag indicating if the pod needs to be migrated
	TargetNode     string `json:"targetNode"`     // Name of the target node for migration
}

// RecommendationDetails holds the base and adjusted recommendations for a resource.
type RecommendationDetails struct {
	BaseRecommendation     string `json:"baseRecommendation,omitempty"`     // Base recommendation (e.g., "0.2 cores")
	AdjustedRecommendation string `json:"adjustedRecommendation,omitempty"` // Adjusted recommendation (e.g., "0.3 cores")
	NeedToApply            bool   `json:"needToApply,omitempty"`            // Flag indicating if the recommendation needs to be applied
}

// OOMEvent holds details of an Out-of-Memory event.
type OOMEvent struct {
	Timestamp   metav1.Time `json:"timestamp"`   // Timestamp of the OOM event
	Reason      string      `json:"reason"`      // Reason for the OOM (if available)
	Description string      `json:"description"` // Description or details about the OOM event
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// ResourceAdjustmentPolicy is the Schema for the resourceadjustmentpolicies API
type ResourceAdjustmentPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ResourceAdjustmentPolicySpec   `json:"spec,omitempty"`
	Status ResourceAdjustmentPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ResourceAdjustmentPolicyList contains a list of ResourceAdjustmentPolicy
type ResourceAdjustmentPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ResourceAdjustmentPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ResourceAdjustmentPolicy{}, &ResourceAdjustmentPolicyList{})
}
