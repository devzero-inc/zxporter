package snap

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/devzero-inc/zxporter/internal/collector"
	"github.com/devzero-inc/zxporter/internal/transport"
	"google.golang.org/protobuf/types/known/timestamppb"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	"github.com/go-logr/logr"
	kedaclient "github.com/kedacore/keda/v2/pkg/generated/clientset/versioned"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
)

// ResourceListFunc defines a function type for listing resources by UID
type ResourceListFunc func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error)

// NamespacedResourceListFunc defines a function type for listing namespaced resources by UID
type NamespacedResourceListFunc func(ctx context.Context, namespace string, options metav1.ListOptions) (runtime.Object, error)

// ResourceExtractorFunc defines a function type for extracting first item from a list
type ResourceExtractorFunc func(runtime.Object) (interface{}, error)

// ClusterResourceExtractorFunc extracts a resource map from cluster-scoped snapshot
type ClusterResourceExtractorFunc func(*gen.ClusterScopedSnapshot) map[string]*gen.ResourceIdentifier

// NamespaceResourceExtractorFunc extracts a resource map from namespace snapshot
type NamespaceResourceExtractorFunc func(*gen.Namespace) map[string]*gen.ResourceIdentifier

// ResourceHandler combines a lister and extractor for a resource type
type ResourceHandler struct {
	Lister            ResourceListFunc
	Extractor         ResourceExtractorFunc
	SnapshotExtractor ClusterResourceExtractorFunc
}

// NamespacedResourceHandler combines a namespaced lister and extractor for a resource type
type NamespacedResourceHandler struct {
	Lister            NamespacedResourceListFunc
	Extractor         ResourceExtractorFunc
	SnapshotExtractor NamespaceResourceExtractorFunc
}

// ClusterSnapshotter takes periodic snapshots and computes deltas
type ClusterSnapshotter struct {
	client             kubernetes.Interface
	kedaClient         kedaclient.Interface
	logger             logr.Logger
	sender             transport.DirectSender
	collectorManager   *collector.CollectionManager
	stopCh             chan struct{}
	ticker             *time.Ticker
	interval           time.Duration
	mu                 sync.RWMutex
	namespaces         []string
	excludedPods       map[string]bool
	excludedNodes      map[string]bool
	clusterHandlers    map[string]ResourceHandler
	namespacedHandlers map[string]NamespacedResourceHandler
}

func NewClusterSnapshotter(
	client kubernetes.Interface,
	kedaClient kedaclient.Interface,
	interval time.Duration,
	sender transport.DirectSender,
	collectorManager *collector.CollectionManager,
	namespaces []string,
	excludedPods []collector.ExcludedPod,
	excludedNodes []string,
	logger logr.Logger,
) *ClusterSnapshotter {
	if interval <= 0 {
		interval = 3 * time.Hour // Default to 3 hours
	}

	excludedPodsMap := make(map[string]bool)
	for _, pod := range excludedPods {
		key := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
		excludedPodsMap[key] = true
	}

	excludedNodesMap := make(map[string]bool)
	for _, node := range excludedNodes {
		excludedNodesMap[node] = true
	}

	cs := &ClusterSnapshotter{
		client:           client,
		kedaClient:       kedaClient,
		logger:           logger.WithName("cluster-snapshotter"),
		sender:           sender,
		collectorManager: collectorManager,
		stopCh:           make(chan struct{}),
		interval:         interval,
		namespaces:       namespaces,
		excludedPods:     excludedPodsMap,
		excludedNodes:    excludedNodesMap,
	}

	// Initialize resource handlers
	cs.initializeResourceHandlers()
	cs.initializeNamespacedResourceHandlers()

	return cs
}

// Generic extractor function using Go generics
func extractFirstItem[T any, L interface{ ~[]T }](getItems func(runtime.Object) (L, bool)) ResourceExtractorFunc {
	return func(listResult runtime.Object) (interface{}, error) {
		items, ok := getItems(listResult)
		if !ok {
			return nil, fmt.Errorf("unexpected list type: %T", listResult)
		}
		if len(items) == 0 {
			return nil, nil
		}
		return &items[0], nil
	}
}

// initializeResourceHandlers sets up the resource handler maps
func (c *ClusterSnapshotter) initializeResourceHandlers() {
	c.clusterHandlers = map[string]ResourceHandler{
		"persistent_volume": {
			Lister: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
				return c.client.CoreV1().PersistentVolumes().List(ctx, options)
			},
			Extractor: extractFirstItem(func(obj runtime.Object) ([]corev1.PersistentVolume, bool) {
				if list, ok := obj.(*corev1.PersistentVolumeList); ok {
					return list.Items, true
				}
				return nil, false
			}),
			SnapshotExtractor: func(snapshot *gen.ClusterScopedSnapshot) map[string]*gen.ResourceIdentifier {
				return snapshot.PersistentVolumes
			},
		},
		"storage_class": {
			Lister: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
				return c.client.StorageV1().StorageClasses().List(ctx, options)
			},
			Extractor: extractFirstItem(func(obj runtime.Object) ([]storagev1.StorageClass, bool) {
				if list, ok := obj.(*storagev1.StorageClassList); ok {
					return list.Items, true
				}
				return nil, false
			}),
			SnapshotExtractor: func(snapshot *gen.ClusterScopedSnapshot) map[string]*gen.ResourceIdentifier {
				return snapshot.StorageClasses
			},
		},
		"cluster_role": {
			Lister: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
				return c.client.RbacV1().ClusterRoles().List(ctx, options)
			},
			Extractor: extractFirstItem(func(obj runtime.Object) ([]rbacv1.ClusterRole, bool) {
				if list, ok := obj.(*rbacv1.ClusterRoleList); ok {
					return list.Items, true
				}
				return nil, false
			}),
			SnapshotExtractor: func(snapshot *gen.ClusterScopedSnapshot) map[string]*gen.ResourceIdentifier {
				return snapshot.ClusterRoles
			},
		},
		"cluster_role_binding": {
			Lister: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
				return c.client.RbacV1().ClusterRoleBindings().List(ctx, options)
			},
			Extractor: extractFirstItem(func(obj runtime.Object) ([]rbacv1.ClusterRoleBinding, bool) {
				if list, ok := obj.(*rbacv1.ClusterRoleBindingList); ok {
					return list.Items, true
				}
				return nil, false
			}),
			SnapshotExtractor: func(snapshot *gen.ClusterScopedSnapshot) map[string]*gen.ResourceIdentifier {
				return snapshot.ClusterRoleBindings
			},
		},
		"csi_node": {
			Lister: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
				return c.client.StorageV1().CSINodes().List(ctx, options)
			},
			Extractor: extractFirstItem(func(obj runtime.Object) ([]storagev1.CSINode, bool) {
				if list, ok := obj.(*storagev1.CSINodeList); ok {
					return list.Items, true
				}
				return nil, false
			}),
			SnapshotExtractor: func(snapshot *gen.ClusterScopedSnapshot) map[string]*gen.ResourceIdentifier {
				return snapshot.CsiNodes
			},
		},
		"csi_driver": {
			Lister: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
				return c.client.StorageV1().CSIDrivers().List(ctx, options)
			},
			Extractor: extractFirstItem(func(obj runtime.Object) ([]storagev1.CSIDriver, bool) {
				if list, ok := obj.(*storagev1.CSIDriverList); ok {
					return list.Items, true
				}
				return nil, false
			}),
			SnapshotExtractor: func(snapshot *gen.ClusterScopedSnapshot) map[string]*gen.ResourceIdentifier {
				return snapshot.CsiDrivers
			},
		},
		"volume_attachment": {
			Lister: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
				return c.client.StorageV1().VolumeAttachments().List(ctx, options)
			},
			Extractor: extractFirstItem(func(obj runtime.Object) ([]storagev1.VolumeAttachment, bool) {
				if list, ok := obj.(*storagev1.VolumeAttachmentList); ok {
					return list.Items, true
				}
				return nil, false
			}),
			SnapshotExtractor: func(snapshot *gen.ClusterScopedSnapshot) map[string]*gen.ResourceIdentifier {
				return snapshot.VolumeAttachments
			},
		},
		"ingress_class": {
			Lister: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
				return c.client.NetworkingV1().IngressClasses().List(ctx, options)
			},
			Extractor: extractFirstItem(func(obj runtime.Object) ([]networkingv1.IngressClass, bool) {
				if list, ok := obj.(*networkingv1.IngressClassList); ok {
					return list.Items, true
				}
				return nil, false
			}),
			SnapshotExtractor: func(snapshot *gen.ClusterScopedSnapshot) map[string]*gen.ResourceIdentifier {
				return snapshot.IngressClasses
			},
		},
	}
}

// initializeNamespacedResourceHandlers sets up the namespaced resource handler maps
func (c *ClusterSnapshotter) initializeNamespacedResourceHandlers() {
	c.namespacedHandlers = map[string]NamespacedResourceHandler{
		"deployment": {
			Lister: func(ctx context.Context, namespace string, options metav1.ListOptions) (runtime.Object, error) {
				return c.client.AppsV1().Deployments(namespace).List(ctx, options)
			},
			Extractor: extractFirstItem(func(obj runtime.Object) ([]appsv1.Deployment, bool) {
				if list, ok := obj.(*appsv1.DeploymentList); ok {
					return list.Items, true
				}
				return nil, false
			}),
			SnapshotExtractor: func(namespace *gen.Namespace) map[string]*gen.ResourceIdentifier {
				return namespace.Deployments
			},
		},
		"stateful_set": {
			Lister: func(ctx context.Context, namespace string, options metav1.ListOptions) (runtime.Object, error) {
				return c.client.AppsV1().StatefulSets(namespace).List(ctx, options)
			},
			Extractor: extractFirstItem(func(obj runtime.Object) ([]appsv1.StatefulSet, bool) {
				if list, ok := obj.(*appsv1.StatefulSetList); ok {
					return list.Items, true
				}
				return nil, false
			}),
			SnapshotExtractor: func(namespace *gen.Namespace) map[string]*gen.ResourceIdentifier {
				return namespace.StatefulSets
			},
		},
		"daemon_set": {
			Lister: func(ctx context.Context, namespace string, options metav1.ListOptions) (runtime.Object, error) {
				return c.client.AppsV1().DaemonSets(namespace).List(ctx, options)
			},
			Extractor: extractFirstItem(func(obj runtime.Object) ([]appsv1.DaemonSet, bool) {
				if list, ok := obj.(*appsv1.DaemonSetList); ok {
					return list.Items, true
				}
				return nil, false
			}),
			SnapshotExtractor: func(namespace *gen.Namespace) map[string]*gen.ResourceIdentifier {
				return namespace.DaemonSets
			},
		},
		"replica_set": {
			Lister: func(ctx context.Context, namespace string, options metav1.ListOptions) (runtime.Object, error) {
				return c.client.AppsV1().ReplicaSets(namespace).List(ctx, options)
			},
			Extractor: extractFirstItem(func(obj runtime.Object) ([]appsv1.ReplicaSet, bool) {
				if list, ok := obj.(*appsv1.ReplicaSetList); ok {
					return list.Items, true
				}
				return nil, false
			}),
			SnapshotExtractor: func(namespace *gen.Namespace) map[string]*gen.ResourceIdentifier {
				return namespace.ReplicaSets
			},
		},
		"service": {
			Lister: func(ctx context.Context, namespace string, options metav1.ListOptions) (runtime.Object, error) {
				return c.client.CoreV1().Services(namespace).List(ctx, options)
			},
			Extractor: extractFirstItem(func(obj runtime.Object) ([]corev1.Service, bool) {
				if list, ok := obj.(*corev1.ServiceList); ok {
					return list.Items, true
				}
				return nil, false
			}),
			SnapshotExtractor: func(namespace *gen.Namespace) map[string]*gen.ResourceIdentifier {
				return namespace.Services
			},
		},
		"persistent_volume_claim": {
			Lister: func(ctx context.Context, namespace string, options metav1.ListOptions) (runtime.Object, error) {
				return c.client.CoreV1().PersistentVolumeClaims(namespace).List(ctx, options)
			},
			Extractor: extractFirstItem(func(obj runtime.Object) ([]corev1.PersistentVolumeClaim, bool) {
				if list, ok := obj.(*corev1.PersistentVolumeClaimList); ok {
					return list.Items, true
				}
				return nil, false
			}),
			SnapshotExtractor: func(namespace *gen.Namespace) map[string]*gen.ResourceIdentifier {
				return namespace.Pvcs
			},
		},
		"job": {
			Lister: func(ctx context.Context, namespace string, options metav1.ListOptions) (runtime.Object, error) {
				return c.client.BatchV1().Jobs(namespace).List(ctx, options)
			},
			Extractor: extractFirstItem(func(obj runtime.Object) ([]batchv1.Job, bool) {
				if list, ok := obj.(*batchv1.JobList); ok {
					return list.Items, true
				}
				return nil, false
			}),
			SnapshotExtractor: func(namespace *gen.Namespace) map[string]*gen.ResourceIdentifier {
				return namespace.Jobs
			},
		},
		"cron_job": {
			Lister: func(ctx context.Context, namespace string, options metav1.ListOptions) (runtime.Object, error) {
				return c.client.BatchV1().CronJobs(namespace).List(ctx, options)
			},
			Extractor: extractFirstItem(func(obj runtime.Object) ([]batchv1.CronJob, bool) {
				if list, ok := obj.(*batchv1.CronJobList); ok {
					return list.Items, true
				}
				return nil, false
			}),
			SnapshotExtractor: func(namespace *gen.Namespace) map[string]*gen.ResourceIdentifier {
				return namespace.CronJobs
			},
		},
		"ingress": {
			Lister: func(ctx context.Context, namespace string, options metav1.ListOptions) (runtime.Object, error) {
				return c.client.NetworkingV1().Ingresses(namespace).List(ctx, options)
			},
			Extractor: extractFirstItem(func(obj runtime.Object) ([]networkingv1.Ingress, bool) {
				if list, ok := obj.(*networkingv1.IngressList); ok {
					return list.Items, true
				}
				return nil, false
			}),
			SnapshotExtractor: func(namespace *gen.Namespace) map[string]*gen.ResourceIdentifier {
				return namespace.Ingresses
			},
		},
		"network_policy": {
			Lister: func(ctx context.Context, namespace string, options metav1.ListOptions) (runtime.Object, error) {
				return c.client.NetworkingV1().NetworkPolicies(namespace).List(ctx, options)
			},
			Extractor: extractFirstItem(func(obj runtime.Object) ([]networkingv1.NetworkPolicy, bool) {
				if list, ok := obj.(*networkingv1.NetworkPolicyList); ok {
					return list.Items, true
				}
				return nil, false
			}),
			SnapshotExtractor: func(namespace *gen.Namespace) map[string]*gen.ResourceIdentifier {
				return namespace.NetworkPolicies
			},
		},
		"service_account": {
			Lister: func(ctx context.Context, namespace string, options metav1.ListOptions) (runtime.Object, error) {
				return c.client.CoreV1().ServiceAccounts(namespace).List(ctx, options)
			},
			Extractor: extractFirstItem(func(obj runtime.Object) ([]corev1.ServiceAccount, bool) {
				if list, ok := obj.(*corev1.ServiceAccountList); ok {
					return list.Items, true
				}
				return nil, false
			}),
			SnapshotExtractor: func(namespace *gen.Namespace) map[string]*gen.ResourceIdentifier {
				return namespace.ServiceAccounts
			},
		},
		"role": {
			Lister: func(ctx context.Context, namespace string, options metav1.ListOptions) (runtime.Object, error) {
				return c.client.RbacV1().Roles(namespace).List(ctx, options)
			},
			Extractor: extractFirstItem(func(obj runtime.Object) ([]rbacv1.Role, bool) {
				if list, ok := obj.(*rbacv1.RoleList); ok {
					return list.Items, true
				}
				return nil, false
			}),
			SnapshotExtractor: func(namespace *gen.Namespace) map[string]*gen.ResourceIdentifier {
				return namespace.Roles
			},
		},
		"role_binding": {
			Lister: func(ctx context.Context, namespace string, options metav1.ListOptions) (runtime.Object, error) {
				return c.client.RbacV1().RoleBindings(namespace).List(ctx, options)
			},
			Extractor: extractFirstItem(func(obj runtime.Object) ([]rbacv1.RoleBinding, bool) {
				if list, ok := obj.(*rbacv1.RoleBindingList); ok {
					return list.Items, true
				}
				return nil, false
			}),
			SnapshotExtractor: func(namespace *gen.Namespace) map[string]*gen.ResourceIdentifier {
				return namespace.RoleBindings
			},
		},
		"endpoints": {
			Lister: func(ctx context.Context, namespace string, options metav1.ListOptions) (runtime.Object, error) {
				return c.client.CoreV1().Endpoints(namespace).List(ctx, options)
			},
			Extractor: extractFirstItem(func(obj runtime.Object) ([]corev1.Endpoints, bool) {
				if list, ok := obj.(*corev1.EndpointsList); ok {
					return list.Items, true
				}
				return nil, false
			}),
			SnapshotExtractor: func(namespace *gen.Namespace) map[string]*gen.ResourceIdentifier {
				return namespace.Endpoints
			},
		},
		"horizontal_pod_autoscaler": {
			Lister: func(ctx context.Context, namespace string, options metav1.ListOptions) (runtime.Object, error) {
				return c.client.AutoscalingV2().HorizontalPodAutoscalers(namespace).List(ctx, options)
			},
			Extractor: extractFirstItem(func(obj runtime.Object) ([]autoscalingv2.HorizontalPodAutoscaler, bool) {
				if list, ok := obj.(*autoscalingv2.HorizontalPodAutoscalerList); ok {
					return list.Items, true
				}
				return nil, false
			}),
			SnapshotExtractor: func(namespace *gen.Namespace) map[string]*gen.ResourceIdentifier {
				return namespace.HorizontalPodAutoscalers
			},
		},
	}
}

func (c *ClusterSnapshotter) Start(ctx context.Context) error {
	c.logger.Info("Starting cluster snapshotter", "interval", c.interval)

	c.ticker = time.NewTicker(c.interval)

	go func() {
		c.takeSnapshot(ctx)
	}()

	go c.snapshotLoop(ctx)

	go func() {
		select {
		case <-ctx.Done():
			c.Stop()
		case <-c.stopCh:
		}
	}()

	return nil
}

func (c *ClusterSnapshotter) snapshotLoop(ctx context.Context) {
	for {
		select {
		case <-c.stopCh:
			return
		case <-c.ticker.C:
			c.takeSnapshot(ctx)
		}
	}
}

func (c *ClusterSnapshotter) takeSnapshot(ctx context.Context) {
	c.logger.Info("Taking cluster snapshot")
	startTime := time.Now()

	snapshot, err := c.captureClusterState(ctx)
	if err != nil {
		c.logger.Error(err, "Failed to capture cluster state")
		return
	}

	c.sendSnapshot(ctx, snapshot, true)

	c.logger.Info("Snapshot completed", "duration", time.Since(startTime))
}

func (c *ClusterSnapshotter) captureClusterState(ctx context.Context) (*ClusterSnapshot, error) {
	now := time.Now().UTC()
	clusterID := ""
	if ctx.Value("cluster_id") != nil {
		clusterID, _ = ctx.Value("cluster_id").(string)
	}

	var snapshotID string
	if clusterID != "" {
		snapshotID = fmt.Sprintf("snapshot-%s-%d", clusterID, now.UnixNano())
	} else {
		snapshotID = fmt.Sprintf("snapshot-unknown-%d", now.UnixNano())
	}

	snapshot := &ClusterSnapshot{
		ClusterInfo:   &ClusterInfo{},
		Nodes:         make(map[string]*NodeData),
		Namespaces:    make(map[string]*Namespace),
		ClusterScoped: &ClusterScopedSnapshot{},
		Timestamp:     timestamppb.New(now),
		SnapshotId:    snapshotID,
	}

	if err := c.captureClusterInfo(ctx, snapshot); err != nil {
		return nil, fmt.Errorf("failed to capture cluster info: %w", err)
	}

	if err := c.captureNodes(ctx, snapshot); err != nil {
		return nil, fmt.Errorf("failed to capture nodes: %w", err)
	}

	if err := c.captureNamespaces(ctx, snapshot); err != nil {
		return nil, fmt.Errorf("failed to capture namespaces: %w", err)
	}

	if err := c.captureClusterScopedResources(ctx, snapshot); err != nil {
		return nil, fmt.Errorf("failed to capture cluster-scoped resources: %w", err)
	}

	return snapshot, nil
}

func (c *ClusterSnapshotter) sendSnapshot(ctx context.Context, snapshot *ClusterSnapshot, isFullSnapshot bool) {
	// dont send multiple snapshots at once
	c.mu.Lock()
	defer c.mu.Unlock()

	snapshotType := "delta"
	if isFullSnapshot {
		snapshotType = "full"
	}

	c.logger.Info("Sending cluster snapshot",
		"type", snapshotType,
		"snapshotId", snapshot.SnapshotId,
		"nodes", len(snapshot.Nodes),
		"namespaces", len(snapshot.Namespaces))

	// Estimate snapshot size
	jsonBytes, err := json.Marshal(snapshot)
	if err != nil {
		c.logger.Error(err, "Failed to marshal snapshot for size estimation")
		return
	}

	snapshotSize := len(jsonBytes)
	c.logger.Info("Snapshot size calculated",
		"size_bytes", snapshotSize,
		"size_mb", float64(snapshotSize)/(1024*1024))

	var sendErr error
	var clusterID string

	// checking if sender supports streaming (we have to cleanup out transport layer, those are confusing)
	if streamingSender, ok := c.sender.(interface {
		SendClusterSnapshotStream(ctx context.Context, snapshot *gen.ClusterSnapshot, snapshotID string, timestamp time.Time) (string, *gen.ClusterSnapshot, error)
	}); ok {
		var missingResources *gen.ClusterSnapshot
		clusterID, missingResources, sendErr = streamingSender.SendClusterSnapshotStream(ctx, snapshot, snapshot.SnapshotId, snapshot.Timestamp.AsTime())

		// If we have missing resources, refresh them
		if sendErr == nil && missingResources != nil {
			c.logger.Info("Received missing resources, triggering refresh")
			if refreshErr := c.refreshMissingResources(ctx, missingResources); refreshErr != nil {
				c.logger.Error(refreshErr, "Failed to refresh missing resources")
			}
		}
	} else {
		c.logger.Info("Sender doesn't support streaming")
		sendErr = fmt.Errorf("snapshot streaming not supported")
	}

	if sendErr != nil {
		c.logger.Error(sendErr, "Failed to send cluster snapshot",
			"size", snapshotSize)
		return
	}

	c.logger.Info("Successfully sent cluster snapshot",
		"cluster_id", clusterID)
}

func (c *ClusterSnapshotter) Stop() error {
	c.logger.Info("Stopping cluster snapshotter")

	if c.ticker != nil {
		c.ticker.Stop()
		c.logger.Info("Stopped cluster snapshotter ticker")
	}

	select {
	case <-c.stopCh:
		c.logger.Info("Cluster snapshotter stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed cluster snapshotter stop channel")
	}

	return nil
}

func (c *ClusterSnapshotter) GetResourceChannel() <-chan []collector.CollectedResource {
	return nil
}

func (c *ClusterSnapshotter) GetType() string {
	return "cluster_snapshot"
}

func (c *ClusterSnapshotter) IsAvailable(ctx context.Context) bool {
	_, err := c.client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{Limit: 1})
	return err == nil
}

// AddResource manually adds a resource - not supported for cluster snapshotter
func (c *ClusterSnapshotter) AddResource(resource interface{}) error {
	return fmt.Errorf("AddResource not supported for cluster snapshotter - snapshots are collected automatically")
}

// refreshMissingResources processes missing resources by fetching them from kubernetes and sending to collectors
func (c *ClusterSnapshotter) refreshMissingResources(ctx context.Context, missingResources *gen.ClusterSnapshot) error {
	if c.collectorManager == nil {
		return fmt.Errorf("collector manager not available")
	}

	c.logger.Info("Starting refresh of missing resources")

	// Refresh cluster-scoped resources
	if err := c.refreshClusterScopedResources(ctx, missingResources.ClusterScoped); err != nil {
		c.logger.Error(err, "Failed to refresh cluster-scoped resources")
		// Continue with namespaced resources even if cluster-scoped fails
	}

	// Refresh namespaced resources
	for nsUID, namespace := range missingResources.Namespaces {
		if err := c.refreshNamespaceResources(ctx, nsUID, namespace); err != nil {
			c.logger.Error(err, "Failed to refresh namespace resources", "namespace", namespace.Namespace.Name)
			// Continue with other namespaces even if one fails
		}
	}

	c.logger.Info("Completed refresh of missing resources")
	return nil
}

// refreshClusterScopedResources refreshes cluster-scoped resources that are missing
func (c *ClusterSnapshotter) refreshClusterScopedResources(ctx context.Context, clusterScoped *gen.ClusterScopedSnapshot) error {
	if clusterScoped == nil {
		return nil
	}

	// Iterate through all cluster resource handlers
	for resourceType, handler := range c.clusterHandlers {
		// Get the resource map for this type from the snapshot
		resourceMap := handler.SnapshotExtractor(clusterScoped)

		// Refresh each resource in the map
		for resourceUID, resource := range resourceMap {
			if err := c.refreshResource(ctx, resourceType, resourceUID, resource.Name); err != nil {
				c.logger.Error(err, "Failed to refresh cluster resource", "type", resourceType, "name", resource.Name, "uid", resourceUID)
			}
		}
	}

	return nil
}

// refreshNamespaceResources refreshes namespaced resources that are missing
func (c *ClusterSnapshotter) refreshNamespaceResources(ctx context.Context, nsUID string, namespace *gen.Namespace) error {
	if namespace == nil {
		return nil
	}

	nsName := namespace.Namespace.Name
	c.logger.Info("Refreshing namespace resources", "namespace", nsName, "uid", nsUID)

	// Iterate through all namespaced resource handlers
	for resourceType, handler := range c.namespacedHandlers {
		// Get the resource map for this type from the namespace snapshot
		resourceMap := handler.SnapshotExtractor(namespace)

		// Refresh each resource in the map
		for resourceUID, resource := range resourceMap {
			if err := c.refreshNamespacedResource(ctx, resourceType, nsName, resourceUID, resource.Name); err != nil {
				c.logger.Error(err, "Failed to refresh namespaced resource", "type", resourceType, "name", resource.Name, "namespace", nsName, "uid", resourceUID)
			}
		}
	}

	return nil
}

// refreshResource fetches and refreshes a cluster-scoped resource
func (c *ClusterSnapshotter) refreshResource(ctx context.Context, resourceType, resourceUID, resourceName string) error {
	collector := c.collectorManager.GetCollector(resourceType)
	if collector == nil {
		c.logger.Info("No collector found for resource type", "type", resourceType, "name", resourceName)
		return nil
	}

	c.logger.Info("Refreshing cluster resource", "type", resourceType, "name", resourceName, "uid", resourceUID)

	// Fetch the resource from Kubernetes API using name
	resource, err := c.fetchClusterResourceByName(ctx, resourceType, resourceName)
	if err != nil {
		return fmt.Errorf("failed to fetch %s with name %s: %w", resourceType, resourceName, err)
	}

	if resource == nil {
		c.logger.Info("Resource not found in cluster", "type", resourceType, "name", resourceName, "uid", resourceUID)
		return nil
	}

	// Add the resource to the collector
	if err := collector.AddResource(resource); err != nil {
		return fmt.Errorf("failed to add %s with name %s to collector: %w", resourceType, resourceName, err)
	}

	c.logger.Info("Successfully refreshed resource", "type", resourceType, "uid", resourceUID)
	return nil
}

// refreshNamespacedResource fetches and refreshes a namespaced resource
func (c *ClusterSnapshotter) refreshNamespacedResource(ctx context.Context, resourceType, namespace, resourceUID, resourceName string) error {
	collector := c.collectorManager.GetCollector(resourceType)
	if collector == nil {
		c.logger.Info("No collector found for resource type", "type", resourceType, "name", resourceName, "namespace", namespace)
		return nil
	}

	c.logger.Info("Refreshing namespaced resource", "type", resourceType, "name", resourceName, "uid", resourceUID, "namespace", namespace)

	// Fetch the resource from Kubernetes API using name
	resource, err := c.fetchNamespacedResourceByName(ctx, resourceType, namespace, resourceName)
	if err != nil {
		return fmt.Errorf("failed to fetch %s with name %s in namespace %s: %w", resourceType, resourceName, namespace, err)
	}

	if resource == nil {
		c.logger.Info("Resource not found in cluster", "type", resourceType, "name", resourceName, "uid", resourceUID, "namespace", namespace)
		return nil
	}

	// Add the resource to the collector
	if err := collector.AddResource(resource); err != nil {
		return fmt.Errorf("failed to add %s with name %s in namespace %s to collector: %w", resourceType, resourceName, namespace, err)
	}

	c.logger.Info("Successfully refreshed resource", "type", resourceType, "uid", resourceUID, "namespace", namespace)
	return nil
}

// fetchClusterResourceByName fetches a cluster-scoped resource from Kubernetes API by name
func (c *ClusterSnapshotter) fetchClusterResourceByName(ctx context.Context, resourceType, resourceName string) (interface{}, error) {
	handler, exists := c.clusterHandlers[resourceType]
	if !exists {
		c.logger.Info("Unknown cluster-scoped resource type", "type", resourceType)
		return nil, nil
	}

	fieldSelector := fmt.Sprintf("metadata.name=%s", resourceName)
	listOptions := metav1.ListOptions{
		FieldSelector: fieldSelector,
		Limit:         1, // We only expect one result
	}

	// Use the handler's lister to get the resource list
	listResult, err := handler.Lister(ctx, listOptions)
	if err != nil {
		return nil, err
	}

	// Use the handler's generic extractor to get the first item
	return handler.Extractor(listResult)
}

// fetchNamespacedResourceByName fetches a namespaced resource from Kubernetes API by name
func (c *ClusterSnapshotter) fetchNamespacedResourceByName(ctx context.Context, resourceType, namespace, resourceName string) (interface{}, error) {
	handler, exists := c.namespacedHandlers[resourceType]
	if !exists {
		c.logger.Info("Unknown namespaced resource type", "type", resourceType, "namespace", namespace)
		return nil, nil
	}

	fieldSelector := fmt.Sprintf("metadata.name=%s", resourceName)
	listOptions := metav1.ListOptions{
		FieldSelector: fieldSelector,
		Limit:         1, // We only expect one result
	}

	// Use the handler's lister to get the resource list
	listResult, err := handler.Lister(ctx, namespace, listOptions)
	if err != nil {
		return nil, err
	}

	// Use the handler's generic extractor to get the first item
	return handler.Extractor(listResult)
}
