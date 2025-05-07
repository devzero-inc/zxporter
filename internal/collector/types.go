package collector

// AllResourceTypes returns all defined resource types
func AllResourceTypes() []ResourceType {
	return []ResourceType{
		Pod, Deployment, StatefulSet, DaemonSet, Service,
		PersistentVolumeClaim, Event, Job, CronJob, ReplicationController,
		Ingress, NetworkPolicy, Endpoints, ServiceAccount, LimitRange,
		ResourceQuota, HorizontalPodAutoscaler, VerticalPodAutoscaler,
		Role, RoleBinding, ClusterRole, ClusterRoleBinding, PodDisruptionBudget,
		StorageClass, PersistentVolume, IngressClass, Node, NodeResource,
		Cluster, ContainerResource,
		Namespace, CSINode, Karpenter, Datadog,
	}
}

// GetResourceTypeFromString converts a string to the corresponding ResourceType
func GetResourceTypeFromString(typeStr string) ResourceType {
	for _, rt := range AllResourceTypes() {
		if rt.String() == typeStr {
			return rt
		}
	}
	return Unknown
}

// ExcludedPod identifies a pod to exclude from collection
type ExcludedPod struct {
	Namespace string
	Name      string
}

// ExcludedDeployment defines a deployment to exclude from collection
type ExcludedDeployment struct {
	Namespace string
	Name      string
}

// ExcludedStatefulSet defines a statefulset to exclude from collection
type ExcludedStatefulSet struct {
	Namespace string
	Name      string
}

// ExcludedDaemonSet defines a daemonset to exclude from collection
type ExcludedDaemonSet struct {
	Namespace string
	Name      string
}

// ExcludedService defines a service to exclude from collection
type ExcludedService struct {
	Namespace string
	Name      string
}

// ExcludedPVC defines a PVC to exclude from collection
type ExcludedPVC struct {
	Namespace string
	Name      string
}

// ExcludedEvent defines an event to exclude from collection
type ExcludedEvent struct {
	Namespace string
	Name      string
}

// ExcludedJob defines a job to exclude from collection
type ExcludedJob struct {
	Namespace string
	Name      string
}

// ExcludedCronJob defines a cronjob to exclude from collection
type ExcludedCronJob struct {
	Namespace string
	Name      string
}

// ExcludedReplicationController defines a replicationcontroller to exclude from collection
type ExcludedReplicationController struct {
	Namespace string
	Name      string
}

// ExcludedIngress defines an ingress to exclude from collection
type ExcludedIngress struct {
	Namespace string
	Name      string
}

// ExcludedNetworkPolicy defines a networkpolicy to exclude from collection
type ExcludedNetworkPolicy struct {
	Namespace string
	Name      string
}

// ExcludedEndpoint defines an endpoints resource to exclude from collection
type ExcludedEndpoint struct {
	Namespace string
	Name      string
}

// ExcludedServiceAccount defines a serviceaccount to exclude from collection
type ExcludedServiceAccount struct {
	Namespace string
	Name      string
}

// ExcludedLimitRange defines a limitrange to exclude from collection
type ExcludedLimitRange struct {
	Namespace string
	Name      string
}

// ExcludedResourceQuota defines a resourcequota to exclude from collection
type ExcludedResourceQuota struct {
	Namespace string
	Name      string
}

// ExcludedHPA defines a HPA to exclude from collection
type ExcludedHPA struct {
	Namespace string
	Name      string
}

// ExcludedVPA defines a VPA to exclude from collection
type ExcludedVPA struct {
	Namespace string
	Name      string
}

// ExcludedRole defines a Role to exclude from collection
type ExcludedRole struct {
	Namespace string
	Name      string
}

// ExcludedRoleBinding defines a RoleBinding to exclude from collection
type ExcludedRoleBinding struct {
	Namespace string
	Name      string
}

// ExcludedPDB defines a PodDisruptionBudget to exclude from collection
type ExcludedPDB struct {
	Namespace string
	Name      string
}

// ExcludedReplicaSet defines a replicaset to exclude from collection
type ExcludedReplicaSet struct {
	Namespace string
	Name      string
}

// ExcludedDatadogExtendedDaemonSetReplicaSet identifies an ExtendedDaemonSetReplicaSet to exclude
type ExcludedDatadogExtendedDaemonSetReplicaSet struct {
	// Namespace is the ExtendedDaemonSetReplicaSet's namespace
	Namespace string `json:"namespace"`

	// Name is the ExtendedDaemonSetReplicaSet's name
	Name string `json:"name"`
}

// ExcludedArgoRollout identifies an Argo Rollout to exclude
type ExcludedArgoRollout struct {
	// Namespace is the Argo Rollout's namespace
	Namespace string `json:"namespace"`

	// Name is the Argo Rollout's name
	Name string `json:"name"`
}
