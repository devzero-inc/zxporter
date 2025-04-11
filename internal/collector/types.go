package collector

// AllResourceTypes returns all defined resource types
func AllResourceTypes() []ResourceType {
	return []ResourceType{
		Pod, Deployment, StatefulSet, DaemonSet, Service, ConfigMap,
		PersistentVolumeClaim, Event, Job, CronJob, ReplicationController,
		Ingress, NetworkPolicy, Endpoints, ServiceAccount, LimitRange,
		ResourceQuota, HorizontalPodAutoscaler, VerticalPodAutoscaler,
		Role, RoleBinding, ClusterRole, ClusterRoleBinding, PodDisruptionBudget,
		Secret, StorageClass, PersistentVolume, IngressClass, Node, NodeResource,
		CustomResourceDefinition, CustomResource, Cluster, ContainerResource,
		Namespace,
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
