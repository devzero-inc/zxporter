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

// ImageAnalysisResultSpec defines the desired state of ImageAnalysisResult
type ImageAnalysisResultSpec struct {
	// ImageRef is the full image reference including tag (e.g. "docker.io/library/nginx:1.25.3")
	ImageRef string `json:"imageRef"`

	// ImageDigest is the image digest (e.g. "sha256:a1b2c3d4e5f6...")
	ImageDigest string `json:"imageDigest"`

	// ImageSizeBytes is the total size of the image in bytes
	ImageSizeBytes int64 `json:"imageSizeBytes,omitempty"`

	// ImageSource describes how the image was acquired for analysis
	ImageSource ImageSourceInfo `json:"imageSource"`

	// Analysis contains the dive analysis results
	Analysis ImageAnalysis `json:"analysis"`

	// WorkloadSummary is a high-level summary of workloads using this image
	WorkloadSummary WorkloadSummary `json:"workloadSummary,omitempty"`

	// WorkloadReferences lists the workloads using this image (capped at 200, sorted by replica count desc)
	// +optional
	WorkloadReferences []WorkloadReference `json:"workloadReferences,omitempty"`
}

// ImageSourceInfo describes how the image was acquired for analysis
type ImageSourceInfo struct {
	// Type is the image acquisition method: "local-containerd", "remote-pull", or "failed"
	// +kubebuilder:validation:Enum=local-containerd;remote-pull;failed
	Type string `json:"type"`

	// NodeName is the node where the analysis job ran
	NodeName string `json:"nodeName"`

	// ExportDurationMs is how long it took to export/pull the image in milliseconds
	ExportDurationMs int64 `json:"exportDurationMs,omitempty"`

	// BatchJobName is the name of the batch job that analyzed this image
	BatchJobName string `json:"batchJobName,omitempty"`
}

// ImageAnalysis contains the dive analysis results
type ImageAnalysis struct {
	// Efficiency is the image efficiency ratio as a string (e.g. "0.9516")
	// Stored as string for cross-language CRD compatibility. Range: "0.0" to "1.0".
	Efficiency string `json:"efficiency"`

	// WastedBytes is the total wasted space in bytes
	WastedBytes int64 `json:"wastedBytes"`

	// UserWastedPercent is the percentage of user image space that is wasted as a string (e.g. "0.016")
	// Stored as string for cross-language CRD compatibility. Range: "0.0" to "1.0".
	UserWastedPercent string `json:"userWastedPercent"`

	// Passed indicates whether the image meets the configured efficiency thresholds
	Passed bool `json:"passed"`

	// LayerCount is the total number of layers in the image
	LayerCount int `json:"layerCount"`

	// Layers contains details for each image layer
	// +optional
	Layers []LayerInfo `json:"layers,omitempty"`

	// FileAnalysis contains aggregate file statistics across all layers
	// +optional
	FileAnalysis *FileAnalysis `json:"fileAnalysis,omitempty"`

	// WastedFiles lists the top files contributing to wasted space (capped at 50)
	// +optional
	WastedFiles []WastedFile `json:"wastedFiles,omitempty"`
}

// LayerInfo contains details about a single image layer
type LayerInfo struct {
	// Index is the layer position (0 = base layer)
	Index int `json:"index"`

	// Digest is the layer digest
	Digest string `json:"digest,omitempty"`

	// SizeBytes is the layer size in bytes
	SizeBytes int64 `json:"sizeBytes"`

	// Command is the Dockerfile instruction that created this layer (truncated to 200 chars)
	Command string `json:"command,omitempty"`
}

// FileAnalysis contains aggregate file statistics
type FileAnalysis struct {
	// TotalFiles is the total number of files in the image
	TotalFiles int `json:"totalFiles,omitempty"`

	// ModifiedFiles is the count of files modified across layers
	ModifiedFiles int `json:"modifiedFiles,omitempty"`

	// AddedFiles is the count of files added across layers
	AddedFiles int `json:"addedFiles,omitempty"`

	// RemovedFiles is the count of files removed across layers
	RemovedFiles int `json:"removedFiles,omitempty"`
}

// WastedFile identifies a file or directory contributing to wasted space
type WastedFile struct {
	// Path is the file or directory path
	Path string `json:"path"`

	// SizeBytes is the wasted space from this file in bytes
	SizeBytes int64 `json:"sizeBytes"`
}

// WorkloadSummary provides a high-level summary of workloads using this image
type WorkloadSummary struct {
	// TotalContainers is the total number of container instances using this image across the cluster
	TotalContainers int `json:"totalContainers,omitempty"`

	// TotalWorkloads is the count of unique workloads (Deployments, StatefulSets, etc.) using this image
	TotalWorkloads int `json:"totalWorkloads,omitempty"`

	// NodesRunningImage is the count of nodes that have containers running this image
	NodesRunningImage int `json:"nodesRunningImage,omitempty"`

	// Namespaces lists the namespaces where this image is used
	Namespaces []string `json:"namespaces,omitempty"`
}

// WorkloadReference identifies a workload that uses this image
type WorkloadReference struct {
	// Namespace is the workload's namespace
	Namespace string `json:"namespace"`

	// WorkloadType is the kind of workload (e.g. "Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob")
	WorkloadType string `json:"workloadType"`

	// WorkloadName is the name of the workload
	WorkloadName string `json:"workloadName"`

	// WorkloadUID is the UID of the workload
	WorkloadUID string `json:"workloadUID"`

	// ContainerNames lists the container names within the workload that use this image
	ContainerNames []string `json:"containerNames,omitempty"`

	// Replicas is the current replica count of the workload
	Replicas int32 `json:"replicas,omitempty"`
}

// ImageAnalysisResultStatus defines the observed state of ImageAnalysisResult
type ImageAnalysisResultStatus struct {
	// Phase is the current state of the analysis
	// +kubebuilder:validation:Enum=Pending;Analyzing;Completed;Failed
	Phase string `json:"phase,omitempty"`

	// AnalyzedAt is when the image was analyzed
	AnalyzedAt metav1.Time `json:"analyzedAt,omitempty"`

	// AnalysisDurationSeconds is how long the dive analysis took
	AnalysisDurationSeconds int `json:"analysisDurationSeconds,omitempty"`

	// Message contains additional information about the current phase (e.g. error details)
	Message string `json:"message,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Cluster
//+kubebuilder:printcolumn:name="Image",type="string",JSONPath=".spec.imageRef",priority=0
//+kubebuilder:printcolumn:name="Efficiency",type="string",JSONPath=".spec.analysis.efficiency",priority=0
//+kubebuilder:printcolumn:name="Passed",type="boolean",JSONPath=".spec.analysis.passed",priority=0
//+kubebuilder:printcolumn:name="Source",type="string",JSONPath=".spec.imageSource.type",priority=1
//+kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase",priority=0
//+kubebuilder:printcolumn:name="Analyzed At",type="date",JSONPath=".status.analyzedAt",priority=0

// ImageAnalysisResult is the Schema for the imageanalysisresults API.
// It stores dive analysis results for a container image including efficiency metrics,
// layer breakdown, wasted space details, and references to workloads using the image.
type ImageAnalysisResult struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ImageAnalysisResultSpec   `json:"spec,omitempty"`
	Status ImageAnalysisResultStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// ImageAnalysisResultList contains a list of ImageAnalysisResult
type ImageAnalysisResultList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ImageAnalysisResult `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ImageAnalysisResult{}, &ImageAnalysisResultList{})
}
