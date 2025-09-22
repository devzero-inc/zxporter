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

	// ExcludedNodes are nodes to exclude from collection
	ExcludedNodes []string `json:"excludedNodes,omitempty"`

	// ExcludedLabels are label selectors to exclude
	ExcludedLabels map[string]string `json:"excludedLabels,omitempty"`

	// Workload Exclusions
	ExcludedPods                   []ExcludedPod                   `json:"excludedPods,omitempty"`
	ExcludedDeployments            []ExcludedDeployment            `json:"excludedDeployments,omitempty"`
	ExcludedStatefulSets           []ExcludedStatefulSet           `json:"excludedStatefulSets,omitempty"`
	ExcludedDaemonSets             []ExcludedDaemonSet             `json:"excludedDaemonSets,omitempty"`
	ExcludedReplicationControllers []ExcludedReplicationController `json:"excludedReplicationControllers,omitempty"`
	ExcludedJobs                   []ExcludedJob                   `json:"excludedJobs,omitempty"`
	ExcludedCronJobs               []ExcludedCronJob               `json:"excludedCronJobs,omitempty"`

	// Service-Related Exclusions
	ExcludedServices        []ExcludedService       `json:"excludedServices,omitempty"`
	ExcludedEndpoints       []ExcludedEndpoint      `json:"excludedEndpoints,omitempty"`
	ExcludedIngresses       []ExcludedIngress       `json:"excludedIngresses,omitempty"`
	ExcludedIngressClasses  []string                `json:"excludedIngressClasses,omitempty"`
	ExcludedNetworkPolicies []ExcludedNetworkPolicy `json:"excludedNetworkPolicies,omitempty"`

	// Configuration Exclusions
	ExcludedConfigMaps      []ExcludedConfigMap      `json:"excludedConfigMaps,omitempty"`
	ExcludedSecrets         []ExcludedSecret         `json:"excludedSecrets,omitempty"`
	ExcludedServiceAccounts []ExcludedServiceAccount `json:"excludedServiceAccounts,omitempty"`

	// Storage Exclusions
	ExcludedPVCs           []ExcludedPVC `json:"excludedPVCs,omitempty"`
	ExcludedPVs            []string      `json:"excludedPVs,omitempty"`
	ExcludedStorageClasses []string      `json:"excludedStorageClasses,omitempty"`

	// RBAC Exclusions
	ExcludedRoles               []ExcludedRole        `json:"excludedRoles,omitempty"`
	ExcludedRoleBindings        []ExcludedRoleBinding `json:"excludedRoleBindings,omitempty"`
	ExcludedClusterRoles        []string              `json:"excludedClusterRoles,omitempty"`
	ExcludedClusterRoleBindings []string              `json:"excludedClusterRoleBindings,omitempty"`

	// Resource Management Exclusions
	ExcludedResourceQuotas []ExcludedResourceQuota `json:"excludedResourceQuotas,omitempty"`
	ExcludedLimitRanges    []ExcludedLimitRange    `json:"excludedLimitRanges,omitempty"`
	ExcludedHPAs           []ExcludedHPA           `json:"excludedHPAs,omitempty"`
	ExcludedVPAs           []ExcludedVPA           `json:"excludedVPAs,omitempty"`
	ExcludedPDBs           []ExcludedPDB           `json:"excludedPDBs,omitempty"`
	ExcludedPSPs           []string                `json:"excludedPSPs,omitempty"`

	// Custom Resource Exclusions
	ExcludedCRDs            []string                 `json:"excludedCRDs,omitempty"`
	ExcludedCustomResources []ExcludedCustomResource `json:"excludedCustomResources,omitempty"`
	ExcludedCRDGroups       []string                 `json:"excludedCRDGroups,omitempty"`

	// Event Exclusions
	ExcludedEvents []ExcludedEvent `json:"excludedEvents,omitempty"`

	// Keda Exclusions
	ExcludedScaledObjects []ExcludedScaledObject `json:"excludedScaledObjects,omitempty"`
	ExcludedScaledJobs    []ExcludedScaledJob    `json:"excludedScaledJobs,omitempty"`

	// CSI Exclusions
	ExcludedCSIDrivers           []string                     `json:"excludedCSIDrivers,omitempty"`
	ExcludedCSIStorageCapacities []ExcludedCSIStorageCapacity `json:"excludedCSIStorageCapacities,omitempty"`
	ExcludedVolumeAttachments    []string                     `json:"excludedVolumeAttachments,omitempty"`
}

// Common exclusion patterns

// NamespacedResource represents the common fields for namespace-scoped resources
type NamespacedResource struct {
	// Namespace is the resource's namespace
	Namespace string `json:"namespace"`

	// Name is the resource's name
	Name string `json:"name"`
}

// Workload exclusion types

// ExcludedPod identifies a pod to exclude
type ExcludedPod struct {
	// Namespace is the pod's namespace
	Namespace string `json:"namespace"`

	// Name is the pod's name
	Name string `json:"name"`
}

// ExcludedDeployment identifies a deployment to exclude
type ExcludedDeployment struct {
	// Namespace is the deployment's namespace
	Namespace string `json:"namespace"`

	// Name is the deployment's name
	Name string `json:"name"`
}

// ExcludedStatefulSet identifies a statefulset to exclude
type ExcludedStatefulSet struct {
	// Namespace is the statefulset's namespace
	Namespace string `json:"namespace"`

	// Name is the statefulset's name
	Name string `json:"name"`
}

// ExcludedDaemonSet identifies a daemonset to exclude
type ExcludedDaemonSet struct {
	// Namespace is the daemonset's namespace
	Namespace string `json:"namespace"`

	// Name is the daemonset's name
	Name string `json:"name"`
}

// ExcludedReplicationController identifies a replication controller to exclude
type ExcludedReplicationController struct {
	// Namespace is the replication controller's namespace
	Namespace string `json:"namespace"`

	// Name is the replication controller's name
	Name string `json:"name"`
}

// ExcludedJob identifies a job to exclude
type ExcludedJob struct {
	// Namespace is the job's namespace
	Namespace string `json:"namespace"`

	// Name is the job's name
	Name string `json:"name"`
}

// ExcludedCronJob identifies a cronjob to exclude
type ExcludedCronJob struct {
	// Namespace is the cronjob's namespace
	Namespace string `json:"namespace"`

	// Name is the cronjob's name
	Name string `json:"name"`
}

// Service-related exclusion types

// ExcludedService identifies a service to exclude
type ExcludedService struct {
	// Namespace is the service's namespace
	Namespace string `json:"namespace"`

	// Name is the service's name
	Name string `json:"name"`
}

// ExcludedEndpoint identifies an endpoint to exclude
type ExcludedEndpoint struct {
	// Namespace is the endpoint's namespace
	Namespace string `json:"namespace"`

	// Name is the endpoint's name
	Name string `json:"name"`
}

// ExcludedIngress identifies an ingress to exclude
type ExcludedIngress struct {
	// Namespace is the ingress's namespace
	Namespace string `json:"namespace"`

	// Name is the ingress's name
	Name string `json:"name"`
}

// ExcludedNetworkPolicy identifies a network policy to exclude
type ExcludedNetworkPolicy struct {
	// Namespace is the network policy's namespace
	Namespace string `json:"namespace"`

	// Name is the network policy's name
	Name string `json:"name"`
}

// Configuration exclusion types

// ExcludedConfigMap identifies a configmap to exclude
type ExcludedConfigMap struct {
	// Namespace is the configmap's namespace
	Namespace string `json:"namespace"`

	// Name is the configmap's name
	Name string `json:"name"`
}

// ExcludedSecret identifies a secret to exclude
type ExcludedSecret struct {
	// Namespace is the secret's namespace
	Namespace string `json:"namespace"`

	// Name is the secret's name
	Name string `json:"name"`
}

// ExcludedServiceAccount identifies a service account to exclude
type ExcludedServiceAccount struct {
	// Namespace is the service account's namespace
	Namespace string `json:"namespace"`

	// Name is the service account's name
	Name string `json:"name"`
}

// Storage exclusion types

// ExcludedPVC identifies a persistent volume claim to exclude
type ExcludedPVC struct {
	// Namespace is the persistent volume claim's namespace
	Namespace string `json:"namespace"`

	// Name is the persistent volume claim's name
	Name string `json:"name"`
}

// RBAC exclusion types

// ExcludedRole identifies a role to exclude
type ExcludedRole struct {
	// Namespace is the role's namespace
	Namespace string `json:"namespace"`

	// Name is the role's name
	Name string `json:"name"`
}

// ExcludedRoleBinding identifies a role binding to exclude
type ExcludedRoleBinding struct {
	// Namespace is the role binding's namespace
	Namespace string `json:"namespace"`

	// Name is the role binding's name
	Name string `json:"name"`
}

// Resource management exclusion types

// ExcludedResourceQuota identifies a resource quota to exclude
type ExcludedResourceQuota struct {
	// Namespace is the resource quota's namespace
	Namespace string `json:"namespace"`

	// Name is the resource quota's name
	Name string `json:"name"`
}

// ExcludedLimitRange identifies a limit range to exclude
type ExcludedLimitRange struct {
	// Namespace is the limit range's namespace
	Namespace string `json:"namespace"`

	// Name is the limit range's name
	Name string `json:"name"`
}

// ExcludedHPA identifies a horizontal pod autoscaler to exclude
type ExcludedHPA struct {
	// Namespace is the horizontal pod autoscaler's namespace
	Namespace string `json:"namespace"`

	// Name is the horizontal pod autoscaler's name
	Name string `json:"name"`
}

// ExcludedVPA identifies a vertical pod autoscaler to exclude
type ExcludedVPA struct {
	// Namespace is the vertical pod autoscaler's namespace
	Namespace string `json:"namespace"`

	// Name is the vertical pod autoscaler's name
	Name string `json:"name"`
}

// ExcludedPDB identifies a pod disruption budget to exclude
type ExcludedPDB struct {
	// Namespace is the pod disruption budget's namespace
	Namespace string `json:"namespace"`

	// Name is the pod disruption budget's name
	Name string `json:"name"`
}

// Custom resource exclusion types

// ExcludedCustomResource identifies a custom resource to exclude
type ExcludedCustomResource struct {
	// Group is the API group of the custom resource
	Group string `json:"group"`

	// Version is the API version of the custom resource
	Version string `json:"version"`

	// Kind is the kind of the custom resource
	Kind string `json:"kind"`

	// Namespace is the custom resource's namespace
	Namespace string `json:"namespace"`

	// Name is the custom resource's name
	Name string `json:"name"`
}

// Event exclusion types

// ExcludedEvent identifies an event to exclude
type ExcludedEvent struct {
	// Namespace is the event's namespace
	Namespace string `json:"namespace"`

	// Name of the event
	Name string `json:"name"`
}

// Keda exclusion types

// ExcludedScaledObject identifies a KEDA scaled object to exclude
type ExcludedScaledObject struct {
	// Namespace is the scaled object's namespace
	Namespace string `json:"namespace"`

	// Name is the scaled object
	Name string `json:"name"`
}

// ExcludedScaledJob identifies a KEDA scaled job to exclude
type ExcludedScaledJob struct {
	// Namespace is the scaled job's namespace
	Namespace string `json:"namespace"`

	// Name is the scaled job
	Name string `json:"name"`
}

// ExcludedCSIStorageCapacity identifies a CSIStorageCapacity to exclude
type ExcludedCSIStorageCapacity struct {
	// Namespace is the CSIStorageCapacity's namespace
	Namespace string `json:"namespace"`

	// Name is the CSIStorageCapacity's name
	Name string `json:"name"`
}

// Policies defines collection behavior
type Policies struct {
	// KubeContextName is the name of the current context being used to apply the installation yaml
	KubeContextName string `json:"kubeContextName,omitempty"`

	// DakrURL is the URL of the Dakr service
	DakrURL string `json:"dakrURL,omitempty"`

	// ClusterToken is the token used to authenticate as a cluster
	ClusterToken string `json:"clusterToken,omitempty"`

	// PrometheusURL is the URL of the Prometheus server to query for metrics
	// If not provided, defaults to in-cluster Prometheus at "http://prometheus-service.monitoring.svc.cluster.local:8080"
	// +optional
	PrometheusURL string `json:"prometheusURL,omitempty"`

	// DisableNetworkIOMetrics disables collection of container network and I/O metrics from Prometheus
	// These metrics include network throughput, packet rates, and disk I/O operations
	// Default is false, meaning metrics are collected by default
	// +optional
	DisableNetworkIOMetrics bool `json:"disableNetworkIOMetrics,omitempty"`

	// DisableGpuMetrics disables collection of GPU metrics from Prometheus
	// These metrics include GPU utilization, memory usage, and temperature
	// Default is false, meaning metrics are collected by default
	// +optional
	DisableGPUMetrics bool `json:"disableGpuMetrics,omitempty"`

	// Frequency is how often to collect resource usage metrics
	Frequency string `json:"frequency,omitempty"`

	// BufferSize is the size of the sender buffer
	BufferSize int `json:"bufferSize,omitempty"`

	// NumResourceProcessors is the number of goroutines to process collected resources
	// +optional
	NumResourceProcessors *int `json:"numResourceProcessors,omitempty"`

	// MaskSecretData determines whether to redact secret values
	MaskSecretData bool `json:"maskSecretData,omitempty"`

	// NodeMetricsInterval is how often to collect node metrics (defaults to 6x regular frequency)
	NodeMetricsInterval string `json:"nodeMetricsInterval,omitempty"`

	// ClusterSnapshotInterval is how often to take cluster snapshots (defaults to 3h)
	ClusterSnapshotInterval string `json:"clusterSnapshotInterval,omitempty"`

	// WatchedCRDs is a list of custom resource definitions to explicitly watch
	WatchedCRDs []string `json:"watchedCRDs,omitempty"`

	// DisabledCollectors is a list of collector types to completely disable
	// Valid values include: "pod", "deployment", "statefulset", "daemonset", "service",
	// "configmap", "secret", "node", "event", etc.
	DisabledCollectors []string `json:"disabledCollectors,omitempty"`
}

// CollectionPolicyStatus defines the observed state of CollectionPolicy
type CollectionPolicyStatus struct {
	// Conditions represents the latest available observations of the policy's state
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// CollectedResources is the count of resources being collected
	CollectedResources int `json:"collectedResources,omitempty"`

	// LastUpdated is when the policy was last updated
	LastUpdated metav1.Time `json:"lastUpdated,omitempty"`

	// ActiveCollectors is a list of currently active collectors
	ActiveCollectors []string `json:"activeCollectors,omitempty"`

	// BufferSize is the current buffer size in use
	CurrentBufferSize int `json:"currentBufferSize,omitempty"`

	// BufferUsage is the percentage of buffer currently in use
	BufferUsage int `json:"bufferUsage,omitempty"`

	// LastSuccessfulDakrConnection is when the last successful connection to Dakr occurred
	LastSuccessfulDakrConnection metav1.Time `json:"lastSuccessfulDakrConnection,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Active Collectors",type="integer",JSONPath=".status.activeCollectors"
//+kubebuilder:printcolumn:name="Buffer Usage",type="integer",JSONPath=".status.bufferUsage"
//+kubebuilder:printcolumn:name="Last Updated",type="date",JSONPath=".status.lastUpdated"

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
