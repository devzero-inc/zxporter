// internal/collector/interface.go
package collector

import (
	"context"
	"time"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
)

// ResourceType is a type for the type of resource being collected
type ResourceType int

// String returns the string representation of the ResourceType
func (r ResourceType) String() string {
	names := map[ResourceType]string{
		Unknown:                  "unknown",
		Node:                     "node",
		Pod:                      "pod",
		Namespace:                "namespace",
		Event:                    "event",
		Endpoints:                "endpoints",
		ServiceAccount:           "service_account",
		LimitRange:               "limit_range",
		ResourceQuota:            "resource_quota",
		Deployment:               "deployment",
		StatefulSet:              "stateful_set",
		DaemonSet:                "daemon_set",
		ReplicaSet:               "replica_set",
		ReplicationController:    "replication_controller",
		Job:                      "job",
		CronJob:                  "cron_job",
		PersistentVolumeClaim:    "persistent_volume_claim",
		PersistentVolume:         "persistent_volume",
		StorageClass:             "storage_class",
		Service:                  "service",
		Ingress:                  "ingress",
		IngressClass:             "ingress_class",
		NetworkPolicy:            "network_policy",
		Role:                     "role",
		RoleBinding:              "role_binding",
		ClusterRole:              "cluster_role",
		ClusterRoleBinding:       "cluster_role_binding",
		HorizontalPodAutoscaler:  "horizontal_pod_autoscaler",
		VerticalPodAutoscaler:    "vertical_pod_autoscaler",
		PodDisruptionBudget:      "pod_disruption_budget",
		PodSecurityPolicy:        "pod_security_policy",
		CustomResourceDefinition: "custom_resource_definition",
		CustomResource:           "custom_resource",
		ConfigMap:                "config_map",
		Secret:                   "secret",
		Container:                "container",
		NodeResource:             "node_resource",
		ContainerResource:        "container_resource",
	}

	if name, ok := names[r]; ok {
		return name
	}
	return "unknown"
}

// ProtoType returns the string representation of the ResourceType for the protobuf
func (r ResourceType) ProtoType() gen.ResourceType {
	switch r {
	case Unknown:
		return gen.ResourceType_RESOURCE_TYPE_UNSPECIFIED
	case Node:
		return gen.ResourceType_RESOURCE_TYPE_NODE
	case Pod:
		return gen.ResourceType_RESOURCE_TYPE_POD
	case Namespace:
		return gen.ResourceType_RESOURCE_TYPE_NAMESPACE
	case Event:
		return gen.ResourceType_RESOURCE_TYPE_EVENT
	case Endpoints:
		return gen.ResourceType_RESOURCE_TYPE_ENDPOINTS
	case ServiceAccount:
		return gen.ResourceType_RESOURCE_TYPE_SERVICE_ACCOUNT
	case LimitRange:
		return gen.ResourceType_RESOURCE_TYPE_LIMIT_RANGE
	case ResourceQuota:
		return gen.ResourceType_RESOURCE_TYPE_RESOURCE_QUOTA
	case Deployment:
		return gen.ResourceType_RESOURCE_TYPE_DEPLOYMENT
	case StatefulSet:
		return gen.ResourceType_RESOURCE_TYPE_STATEFUL_SET
	case DaemonSet:
		return gen.ResourceType_RESOURCE_TYPE_DAEMON_SET
	case ReplicaSet:
		return gen.ResourceType_RESOURCE_TYPE_REPLICA_SET
	case ReplicationController:
		return gen.ResourceType_RESOURCE_TYPE_REPLICATION_CONTROLLER
	case Job:
		return gen.ResourceType_RESOURCE_TYPE_JOB
	case CronJob:
		return gen.ResourceType_RESOURCE_TYPE_CRON_JOB
	case PersistentVolumeClaim:
		return gen.ResourceType_RESOURCE_TYPE_PERSISTENT_VOLUME_CLAIM
	case PersistentVolume:
		return gen.ResourceType_RESOURCE_TYPE_PERSISTENT_VOLUME
	case StorageClass:
		return gen.ResourceType_RESOURCE_TYPE_STORAGE_CLASS
	case Service:
		return gen.ResourceType_RESOURCE_TYPE_SERVICE
	case Ingress:
		return gen.ResourceType_RESOURCE_TYPE_INGRESS
	case IngressClass:
		return gen.ResourceType_RESOURCE_TYPE_INGRESS_CLASS
	case NetworkPolicy:
		return gen.ResourceType_RESOURCE_TYPE_NETWORK_POLICY
	case Role:
		return gen.ResourceType_RESOURCE_TYPE_ROLE
	case RoleBinding:
		return gen.ResourceType_RESOURCE_TYPE_ROLE_BINDING
	case ClusterRole:
		return gen.ResourceType_RESOURCE_TYPE_CLUSTER_ROLE
	case ClusterRoleBinding:
		return gen.ResourceType_RESOURCE_TYPE_CLUSTER_ROLE_BINDING
	case HorizontalPodAutoscaler:
		return gen.ResourceType_RESOURCE_TYPE_HORIZONTAL_POD_AUTOSCALER
	case VerticalPodAutoscaler:
		return gen.ResourceType_RESOURCE_TYPE_VERTICAL_POD_AUTOSCALER
	case PodDisruptionBudget:
		return gen.ResourceType_RESOURCE_TYPE_POD_DISRUPTION_BUDGET
	case PodSecurityPolicy:
		return gen.ResourceType_RESOURCE_TYPE_POD_SECURITY_POLICY
	case CustomResourceDefinition:
		return gen.ResourceType_RESOURCE_TYPE_CUSTOM_RESOURCE_DEFINITION
	case CustomResource:
		return gen.ResourceType_RESOURCE_TYPE_CUSTOM_RESOURCE
	case ConfigMap:
		return gen.ResourceType_RESOURCE_TYPE_CONFIG_MAP
	case Secret:
		return gen.ResourceType_RESOURCE_TYPE_SECRET
	case Container:
		return gen.ResourceType_RESOURCE_TYPE_CONTAINER
	case NodeResource:
		return gen.ResourceType_RESOURCE_TYPE_NODE_RESOURCE
	case ContainerResource:
		return gen.ResourceType_RESOURCE_TYPE_CONTAINER_RESOURCE
	default:
		return gen.ResourceType_RESOURCE_TYPE_UNSPECIFIED
	}
}

// enum for resource type
const (
	Unknown                  ResourceType = 0
	Node                     ResourceType = 1
	Pod                      ResourceType = 2
	Namespace                ResourceType = 3
	Event                    ResourceType = 4
	Endpoints                ResourceType = 5
	ServiceAccount           ResourceType = 6
	LimitRange               ResourceType = 7
	ResourceQuota            ResourceType = 8
	Deployment               ResourceType = 9
	StatefulSet              ResourceType = 10
	DaemonSet                ResourceType = 11
	ReplicaSet               ResourceType = 12
	ReplicationController    ResourceType = 13
	Job                      ResourceType = 14
	CronJob                  ResourceType = 15
	PersistentVolumeClaim    ResourceType = 16
	PersistentVolume         ResourceType = 17
	StorageClass             ResourceType = 18
	Service                  ResourceType = 19
	Ingress                  ResourceType = 20
	IngressClass             ResourceType = 21
	NetworkPolicy            ResourceType = 22
	Role                     ResourceType = 23
	RoleBinding              ResourceType = 24
	ClusterRole              ResourceType = 25
	ClusterRoleBinding       ResourceType = 26
	HorizontalPodAutoscaler  ResourceType = 27
	VerticalPodAutoscaler    ResourceType = 28
	PodDisruptionBudget      ResourceType = 29
	PodSecurityPolicy        ResourceType = 30
	CustomResourceDefinition ResourceType = 31
	CustomResource           ResourceType = 32
	ConfigMap                ResourceType = 33
	Secret                   ResourceType = 34
	Container                ResourceType = 35
	NodeResource             ResourceType = 36
	ContainerResource        ResourceType = 37
)

// CollectedResource represents a resource collected from the Kubernetes API
type CollectedResource struct {
	// ResourceType is the type of resource (pod, container, node, etc.)

	ResourceType ResourceType

	// Object is the actual Kubernetes resource object
	Object interface{}

	// Timestamp is when the resource was collected
	Timestamp time.Time

	// EventType indicates whether this is an add, update, or delete event
	EventType string

	// Key is a unique identifier for this resource
	Key string
}

// ResourceCollector defines methods for collecting specific resource types
type ResourceCollector interface {
	// Start begins watching for resources
	Start(ctx context.Context) error

	// Stop halts watching for resources
	Stop() error

	// GetResourceChannel returns a channel for receiving collected resources
	GetResourceChannel() <-chan CollectedResource

	// GetType returns the type of resource this collector handles
	GetType() string
}
