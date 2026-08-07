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
	PersistentVolumeClaimMetrics
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
	CSIDriver
	CSIStorageCapacity
	VolumeAttachment
	KubeflowNotebook
	VolcanoJob
	SparkApplication
	ScheduledSparkApplication
	WorkloadRecommendation
	WorkloadRule
	CNPGCluster
	ContainerOOMEvent
	ContainerCrashLoopEvent
	ContainerStartupLifecycle
	ContainerCPUThrottleEvent
	KarpenterSettings
	KyvernoPolicy
	KyvernoPolicyReport
	GatekeeperConstraintTemplate
	GatekeeperConstraint
	MigPartedConfig
	NodeLifecycleTransition
	PodUnschedulableEvent
	// ClusterAutoscalerStatus identifies the collector that watches the
	// kube-system/cluster-autoscaler-status ConfigMap. It is a COLLECTOR identity only:
	// that collector's output rides the ordinary Event resource type (see
	// cluster_autoscaler_status.go), so this value never reaches the wire and
	// deliberately has no ProtoType case.
	ClusterAutoscalerStatus
)

// String returns the string representation of the ResourceType
func (r ResourceType) String() string {
	names := map[ResourceType]string{
		Unknown:                      "unknown",
		Cluster:                      "cluster",
		Node:                         "node",
		Pod:                          "pod",
		Namespace:                    "namespace",
		Event:                        "event",
		Endpoints:                    "endpoints",
		ServiceAccount:               "service_account",
		LimitRange:                   "limit_range",
		ResourceQuota:                "resource_quota",
		Deployment:                   "deployment",
		StatefulSet:                  "stateful_set",
		DaemonSet:                    "daemon_set",
		ReplicaSet:                   "replica_set",
		ReplicationController:        "replication_controller",
		Job:                          "job",
		CronJob:                      "cron_job",
		PersistentVolumeClaim:        "persistent_volume_claim",
		PersistentVolume:             "persistent_volume",
		PersistentVolumeClaimMetrics: "pvc_metrics",
		StorageClass:                 "storage_class",
		Service:                      "service",
		Ingress:                      "ingress",
		IngressClass:                 "ingress_class",
		NetworkPolicy:                "network_policy",
		Role:                         "role",
		RoleBinding:                  "role_binding",
		ClusterRole:                  "cluster_role",
		ClusterRoleBinding:           "cluster_role_binding",
		HorizontalPodAutoscaler:      "horizontal_pod_autoscaler",
		VerticalPodAutoscaler:        "vertical_pod_autoscaler",
		PodDisruptionBudget:          "pod_disruption_budget",
		PodSecurityPolicy:            "pod_security_policy",
		CustomResourceDefinition:     "custom_resource_definition",
		NodeResource:                 "node_resource",
		Container:                    "container",
		ContainerResource:            "container_resource",
		CSINode:                      "csi_node",
		Karpenter:                    "karpenter",
		Datadog:                      "datadog",
		ArgoRollouts:                 "argo_rollouts",
		Keda:                         "keda",
		KedaScaledJob:                "keda_scaled_job",
		KedaScaledObject:             "keda_scaled_object",
		ClusterSnapshot:              "cluster_snapshot",
		CSIDriver:                    "csi_driver",
		CSIStorageCapacity:           "csi_storage_capacity",
		VolumeAttachment:             "volume_attachment",
		KubeflowNotebook:             "kubeflow_notebook",
		VolcanoJob:                   "volcano_job",
		SparkApplication:             "spark_application",
		ScheduledSparkApplication:    "scheduled_spark_application",
		WorkloadRecommendation:       "workload_recommendation",
		WorkloadRule:                 "workload_rule",
		CNPGCluster:                  "cnpg_cluster",
		ContainerOOMEvent:            "container_oom_event",
		ContainerCrashLoopEvent:      "container_crashloop_event",
		ContainerStartupLifecycle:    "container_startup_lifecycle",
		ContainerCPUThrottleEvent:    "container_cpu_throttle_event",
		KarpenterSettings:            "karpenter-settings",
		KyvernoPolicy:                "kyverno_policy",
		KyvernoPolicyReport:          "kyverno_policy_report",
		GatekeeperConstraintTemplate: "gatekeeper_constraint_template",
		GatekeeperConstraint:         "gatekeeper_constraint",
		MigPartedConfig:              "mig_parted_config",
		NodeLifecycleTransition:      "node_lifecycle_transition",
		PodUnschedulableEvent:        "pod_unschedulable_event",
		ClusterAutoscalerStatus:      "cluster_autoscaler_status",
	}

	if name, ok := names[r]; ok {
		return name
	}
	return "unknown"
}

// resourceProtoTypes maps ResourceType to its wire enum. Package-level so the
// map is built once, not per call: ProtoType runs once per resource item in
// the transport send loop, which is hot on large clusters.
var resourceProtoTypes = map[ResourceType]gen.ResourceType{
	Node:                         gen.ResourceType_RESOURCE_TYPE_NODE,
	Pod:                          gen.ResourceType_RESOURCE_TYPE_POD,
	Namespace:                    gen.ResourceType_RESOURCE_TYPE_NAMESPACE,
	Event:                        gen.ResourceType_RESOURCE_TYPE_EVENT,
	Endpoints:                    gen.ResourceType_RESOURCE_TYPE_ENDPOINTS,
	ServiceAccount:               gen.ResourceType_RESOURCE_TYPE_SERVICE_ACCOUNT,
	LimitRange:                   gen.ResourceType_RESOURCE_TYPE_LIMIT_RANGE,
	ResourceQuota:                gen.ResourceType_RESOURCE_TYPE_RESOURCE_QUOTA,
	Deployment:                   gen.ResourceType_RESOURCE_TYPE_DEPLOYMENT,
	StatefulSet:                  gen.ResourceType_RESOURCE_TYPE_STATEFUL_SET,
	DaemonSet:                    gen.ResourceType_RESOURCE_TYPE_DAEMON_SET,
	ReplicaSet:                   gen.ResourceType_RESOURCE_TYPE_REPLICA_SET,
	ReplicationController:        gen.ResourceType_RESOURCE_TYPE_REPLICATION_CONTROLLER,
	Job:                          gen.ResourceType_RESOURCE_TYPE_JOB,
	CronJob:                      gen.ResourceType_RESOURCE_TYPE_CRON_JOB,
	PersistentVolumeClaim:        gen.ResourceType_RESOURCE_TYPE_PERSISTENT_VOLUME_CLAIM,
	PersistentVolume:             gen.ResourceType_RESOURCE_TYPE_PERSISTENT_VOLUME,
	PersistentVolumeClaimMetrics: gen.ResourceType_RESOURCE_TYPE_PVC_METRICS,
	StorageClass:                 gen.ResourceType_RESOURCE_TYPE_STORAGE_CLASS,
	Service:                      gen.ResourceType_RESOURCE_TYPE_SERVICE,
	Ingress:                      gen.ResourceType_RESOURCE_TYPE_INGRESS,
	IngressClass:                 gen.ResourceType_RESOURCE_TYPE_INGRESS_CLASS,
	NetworkPolicy:                gen.ResourceType_RESOURCE_TYPE_NETWORK_POLICY,
	Role:                         gen.ResourceType_RESOURCE_TYPE_ROLE,
	RoleBinding:                  gen.ResourceType_RESOURCE_TYPE_ROLE_BINDING,
	ClusterRole:                  gen.ResourceType_RESOURCE_TYPE_CLUSTER_ROLE,
	ClusterRoleBinding:           gen.ResourceType_RESOURCE_TYPE_CLUSTER_ROLE_BINDING,
	HorizontalPodAutoscaler:      gen.ResourceType_RESOURCE_TYPE_HORIZONTAL_POD_AUTOSCALER,
	VerticalPodAutoscaler:        gen.ResourceType_RESOURCE_TYPE_VERTICAL_POD_AUTOSCALER,
	PodDisruptionBudget:          gen.ResourceType_RESOURCE_TYPE_POD_DISRUPTION_BUDGET,
	PodSecurityPolicy:            gen.ResourceType_RESOURCE_TYPE_POD_SECURITY_POLICY,
	CustomResourceDefinition:     gen.ResourceType_RESOURCE_TYPE_CUSTOM_RESOURCE_DEFINITION,
	NodeResource:                 gen.ResourceType_RESOURCE_TYPE_NODE_RESOURCE,
	Container:                    gen.ResourceType_RESOURCE_TYPE_CONTAINER,
	ContainerResource:            gen.ResourceType_RESOURCE_TYPE_CONTAINER_RESOURCE,
	Cluster:                      gen.ResourceType_RESOURCE_TYPE_CLUSTER,
	CSINode:                      gen.ResourceType_RESOURCE_TYPE_CSI_NODE,
	Karpenter:                    gen.ResourceType_RESOURCE_TYPE_KARPENTER,
	Datadog:                      gen.ResourceType_RESOURCE_TYPE_DATADOG,
	ArgoRollouts:                 gen.ResourceType_RESOURCE_TYPE_ARGO_ROLLOUTS,
	Keda:                         gen.ResourceType_RESOURCE_TYPE_KEDA,
	KedaScaledJob:                gen.ResourceType_RESOURCE_TYPE_KEDA_SCALED_JOB,
	KedaScaledObject:             gen.ResourceType_RESOURCE_TYPE_KEDA_SCALED_OBJECT,
	ClusterSnapshot:              gen.ResourceType_RESOURCE_TYPE_CLUSTER_SNAPSHOT,
	CSIDriver:                    gen.ResourceType_RESOURCE_TYPE_CSI_DRIVER,
	CSIStorageCapacity:           gen.ResourceType_RESOURCE_TYPE_CSI_STORAGE_CAPACITY,
	VolumeAttachment:             gen.ResourceType_RESOURCE_TYPE_VOLUME_ATTACHMENT,
	KubeflowNotebook:             gen.ResourceType_RESOURCE_TYPE_KUBEFLOW_NOTEBOOK,
	VolcanoJob:                   gen.ResourceType_RESOURCE_TYPE_VOLCANO_JOB,
	SparkApplication:             gen.ResourceType_RESOURCE_TYPE_SPARK_APPLICATION,
	ScheduledSparkApplication:    gen.ResourceType_RESOURCE_TYPE_SCHEDULED_SPARK_APPLICATION,
	WorkloadRecommendation:       gen.ResourceType_RESOURCE_TYPE_WORKLOAD_RECOMMENDATION,
	WorkloadRule:                 gen.ResourceType_RESOURCE_TYPE_WORKLOAD_RULE,
	CNPGCluster:                  gen.ResourceType_RESOURCE_TYPE_CNPG_CLUSTER,
	ContainerOOMEvent:            gen.ResourceType_RESOURCE_TYPE_CONTAINER_OOM_EVENT,
	ContainerCrashLoopEvent:      gen.ResourceType_RESOURCE_TYPE_CONTAINER_CRASHLOOP_EVENT,
	ContainerStartupLifecycle:    gen.ResourceType_RESOURCE_TYPE_CONTAINER_STARTUP_LIFECYCLE,
	ContainerCPUThrottleEvent:    gen.ResourceType_RESOURCE_TYPE_CONTAINER_CPU_THROTTLE_EVENT,
	KarpenterSettings:            gen.ResourceType_RESOURCE_TYPE_KARPENTER_SETTINGS,
	KyvernoPolicy:                gen.ResourceType_RESOURCE_TYPE_KYVERNO_POLICY,
	KyvernoPolicyReport:          gen.ResourceType_RESOURCE_TYPE_KYVERNO_POLICY_REPORT,
	GatekeeperConstraintTemplate: gen.ResourceType_RESOURCE_TYPE_GATEKEEPER_CONSTRAINT_TEMPLATE,
	GatekeeperConstraint:         gen.ResourceType_RESOURCE_TYPE_GATEKEEPER_CONSTRAINT,
	MigPartedConfig:              gen.ResourceType_RESOURCE_TYPE_MIG_PARTED_CONFIG,
	NodeLifecycleTransition:      gen.ResourceType_RESOURCE_TYPE_NODE_LIFECYCLE_TRANSITION,
	PodUnschedulableEvent:        gen.ResourceType_RESOURCE_TYPE_POD_UNSCHEDULABLE_EVENT,
}

// ProtoType returns the protobuf wire enum for the ResourceType.
func (r ResourceType) ProtoType() gen.ResourceType {
	if protoType, ok := resourceProtoTypes[r]; ok {
		return protoType
	}
	return gen.ResourceType_RESOURCE_TYPE_UNSPECIFIED
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

	// AddResource manually adds a resource to be processed by the collector
	AddResource(resource interface{}) error
}

// Kubernetes container termination reason constants. Using constants instead of
// raw strings prevents typo-induced silent failures across the OOM detection paths.
const (
	// ReasonOOMKilled is the termination reason kubelet sets when the OOM killer
	// terminates a container that exceeded its memory limit.
	ReasonOOMKilled = "OOMKilled"

	// ReasonStartError is the termination reason for containers that fail during
	// init. When the message contains "oom", it indicates an OOM during startup.
	ReasonStartError = "StartError"
)

// MpaMetricsPublisher is the interface the collector package uses to publish
// metrics directly to the MPA gRPC stream (bypassing the combinedChannel pipeline).
// Implemented by server.MpaServer.
type MpaMetricsPublisher interface {
	PublishMetrics(metrics *ContainerMetricsSnapshot, timestamp time.Time)
}

// HistoricalWorkloadQuery defines what to query for a workload.
type HistoricalWorkloadQuery struct {
	Namespace    string
	WorkloadName string
	WorkloadKind string
	PodRegex     string   // e.g., "web-app-.*"
	Containers   []string // container names to query
}

// HistoricalPercentileProvider abstracts historical percentile data retrieval.
// Implemented by HistoricalPercentileCache (DAKR-backed).
type HistoricalPercentileProvider interface {
	FetchPercentiles(ctx context.Context, workload HistoricalWorkloadQuery) (*gen.HistoricalMetricsSummary, error)
	FetchPercentilesForAll(ctx context.Context, workloads []HistoricalWorkloadQuery) map[string]*gen.HistoricalMetricsSummary
	DiscoverContainers(ctx context.Context, namespace, podRegex string) ([]string, error)
}
