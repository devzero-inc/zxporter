// internal/collector/interface.go
package collector

import (
	"context"
	"time"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
)

// ResourceType represents the type of a resource
type EventType int

const (
	// EventTypeUnknown represents an unknown event type
	EventTypeUnknown EventType = iota
	// EventTypeAdd represents an added resource
	EventTypeAdd
	// EventTypeUpdate represents an updated resource
	EventTypeUpdate
	// EventTypeDelete represents a deleted resource
	EventTypeDelete
	// EventTypeMetadata represents a metadata resource
	EventTypeMetadata
	// EventTypeMetrics represents a metrics resource
	EventTypeMetrics
	// EventTypeContainerStarted represents a container started event
	EventTypeContainerStarted
	// EventTypeContainerStopped represents a container stopped event
	EventTypeContainerStopped
	// EventTypeContainerRestarted represents a container restarted event
	EventTypeContainerRestarted
	// EventTypeSnapshot represents a cluster snapshot
	EventTypeSnapshot
)

// String returns the string representation of the EventType
func (e EventType) String() string {
	names := map[EventType]string{
		EventTypeUnknown:            "unknown",
		EventTypeAdd:                "add",
		EventTypeUpdate:             "update",
		EventTypeDelete:             "delete",
		EventTypeMetadata:           "metadata",
		EventTypeMetrics:            "metrics",
		EventTypeContainerStarted:   "container_started",
		EventTypeContainerStopped:   "container_stopped",
		EventTypeContainerRestarted: "container_restarted",
		EventTypeSnapshot:           "cluster_snapshot",
	}

	if name, ok := names[e]; ok {
		return name
	}
	return "unknown"
}

// ProtoType returns the string representation of the EventType for the protobuf
func (e EventType) ProtoType() gen.EventType {
	switch e {
	case EventTypeUnknown:
		return gen.EventType_EVENT_TYPE_UNSPECIFIED
	case EventTypeAdd:
		return gen.EventType_EVENT_TYPE_ADD
	case EventTypeUpdate:
		return gen.EventType_EVENT_TYPE_UPDATE
	case EventTypeDelete:
		return gen.EventType_EVENT_TYPE_DELETE
	case EventTypeMetadata:
		return gen.EventType_EVENT_TYPE_METADATA
	case EventTypeMetrics:
		return gen.EventType_EVENT_TYPE_METRICS
	case EventTypeContainerStarted:
		return gen.EventType_EVENT_TYPE_CONTAINER_STARTED
	case EventTypeContainerStopped:
		return gen.EventType_EVENT_TYPE_CONTAINER_STOPPED
	case EventTypeContainerRestarted:
		return gen.EventType_EVENT_TYPE_CONTAINER_RESTARTED
	case EventTypeSnapshot:
		return gen.EventType_EVENT_TYPE_CLUSTER_SNAPSHOT
	default:
		return gen.EventType_EVENT_TYPE_UNSPECIFIED
	}
}

// ResourceType is a type for the type of resource being collected
type ResourceType int

// enum for resource type
const (
	Unknown ResourceType = iota
	Cluster
	Node
	Pod
	Namespace
	Event
	Endpoints
	ServiceAccount
	LimitRange
	ResourceQuota
	Deployment
	StatefulSet
	DaemonSet
	ReplicaSet
	ReplicationController
	Job
	CronJob
	PersistentVolumeClaim
	PersistentVolume
	StorageClass
	Service
	Ingress
	IngressClass
	NetworkPolicy
	Role
	RoleBinding
	ClusterRole
	ClusterRoleBinding
	HorizontalPodAutoscaler
	VerticalPodAutoscaler
	PodDisruptionBudget
	PodSecurityPolicy
	CustomResourceDefinition // leaving here to not screw up enum numbering
	CustomResource           // leaving here to not screw up enum numbering
	ConfigMap                // leaving here to not screw up enum numbering
	Secret                   // leaving here to not screw up enum numbering
	Container                // leaving here to not screw up enum numbering
	NodeResource
	ContainerResource
	CSINode
	Karpenter
	Datadog
	ArgoRollouts
	Keda
	KedaScaledJob
	KedaScaledObject
	ClusterSnapshot
)

// String returns the string representation of the ResourceType
func (r ResourceType) String() string {
	names := map[ResourceType]string{
		Unknown:                  "unknown",
		Cluster:                  "cluster",
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
		NodeResource:             "node_resource",
		ContainerResource:        "container_resource",
		CSINode:                  "csi_node",
		Karpenter:                "karpenter",
		Datadog:                  "datadog",
		ArgoRollouts:             "argo_rollouts",
		Keda:                     "keda",
		KedaScaledJob:            "keda_scaled_job",
		KedaScaledObject:         "keda_scaled_object",
		ClusterSnapshot:          "cluster_snapshot",
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
	case NodeResource:
		return gen.ResourceType_RESOURCE_TYPE_NODE_RESOURCE
	case ContainerResource:
		return gen.ResourceType_RESOURCE_TYPE_CONTAINER_RESOURCE
	case Cluster:
		return gen.ResourceType_RESOURCE_TYPE_CLUSTER
	case CSINode:
		return gen.ResourceType_RESOURCE_TYPE_CSI_NODE
	case Karpenter:
		return gen.ResourceType_RESOURCE_TYPE_KARPENTER
	case Datadog:
		return gen.ResourceType_RESOURCE_TYPE_DATADOG
	case ArgoRollouts:
		return gen.ResourceType_RESOURCE_TYPE_ARGO_ROLLOUTS
	case Keda:
		return gen.ResourceType_RESOURCE_TYPE_KEDA
	case KedaScaledJob:
		return gen.ResourceType_RESOURCE_TYPE_KEDA_SCALED_JOB
	case KedaScaledObject:
		return gen.ResourceType_RESOURCE_TYPE_KEDA_SCALED_OBJECT
	case ClusterSnapshot:
		return gen.ResourceType_RESOURCE_CLUSTER_SNAPSHOT
	default:
		return gen.ResourceType_RESOURCE_TYPE_UNSPECIFIED
	}
}

// CollectedResource represents a resource collected from the Kubernetes API
type CollectedResource struct {
	// ResourceType is the type of resource (pod, container, node, etc.)

	ResourceType ResourceType

	// Object is the actual Kubernetes resource object
	Object interface{}

	// Timestamp is when the resource was collected
	Timestamp time.Time

	// EventType indicates whether this is an add, update, or delete event
	EventType EventType

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
	GetResourceChannel() <-chan []CollectedResource

	// GetType returns the type of resource this collector handles
	GetType() string

	// Returns true if the resource is available
	IsAvailable(ctx context.Context) bool
}
