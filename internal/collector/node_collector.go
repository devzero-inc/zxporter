// internal/collector/node_collector.go
package collector

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"sync"
	"time"

	gpuconst "github.com/NVIDIA/KAI-scheduler/pkg/common/constants"
	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	telemetry_logger "github.com/devzero-inc/zxporter/internal/logger"
	"github.com/devzero-inc/zxporter/internal/version"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	metricsapisv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsv1 "k8s.io/metrics/pkg/client/clientset/versioned"
)

// NodeCollectorConfig holds configuration for the node collector
type NodeCollectorConfig struct {
	// UpdateInterval specifies how often to collect metrics
	UpdateInterval time.Duration

	// DisableGPUMetrics determines whether to disable GPU metrics collection
	// Default is false, so metrics are collected by default
	DisableGPUMetrics bool
}

// NodeCollector collects node events and resource metrics
type NodeCollector struct {
	k8sClient       kubernetes.Interface
	metricsClient   *metricsv1.Clientset
	nodemonClient   *NodemonClient
	informerFactory informers.SharedInformerFactory
	nodeInformer    cache.SharedIndexInformer
	batchChan       chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan    chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher         *ResourcesBatcher
	stopCh          chan struct{}
	ticker          *time.Ticker
	config          NodeCollectorConfig
	excludedNodes   map[string]bool
	logger          logr.Logger
	metrics         *TelemetryMetrics
	telemetryLogger telemetry_logger.Logger
	mu              sync.RWMutex
	nodeToPodsMap   map[string]map[string]*corev1.Pod // Maps node name -> pod key -> pod object
	podInformer     cache.SharedIndexInformer
	podMapMutex     sync.RWMutex
}

// NewNodeCollector creates a new collector for node resources
func NewNodeCollector(
	k8sClient kubernetes.Interface,
	metricsClient *metricsv1.Clientset,
	config NodeCollectorConfig,
	excludedNodes []string,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	metrics *TelemetryMetrics,
	telemetryLogger telemetry_logger.Logger,
) *NodeCollector {
	// Convert excluded nodes to a map for quicker lookups
	excludedNodesMap := make(map[string]bool)
	for _, node := range excludedNodes {
		excludedNodesMap[node] = true
	}

	// Default update interval if not specified
	if config.UpdateInterval <= 0 {
		config.UpdateInterval = 10 * time.Second
	}

	// Create channels
	batchChan := make(chan CollectedResource, 100)      // For metrics
	resourceChan := make(chan []CollectedResource, 100) // For events and batched metrics

	// Create the batcher for metrics
	batcher := NewResourcesBatcher(
		maxBatchSize,
		maxBatchTime,
		batchChan,
		resourceChan, // Batcher output goes to the same channel as direct events
		logger,
	)

	ns := os.Getenv("POD_NAMESPACE")
	if ns == "" {
		ns = defaultNamespace
	}
	nodemonClient := NewNodemonClient(k8sClient, ns, logger)

	return &NodeCollector{
		k8sClient:       k8sClient,
		metricsClient:   metricsClient,
		nodemonClient:   nodemonClient,
		batchChan:       batchChan,
		resourceChan:    resourceChan,
		batcher:         batcher,
		stopCh:          make(chan struct{}),
		config:          config,
		excludedNodes:   excludedNodesMap,
		logger:          logger.WithName("node-collector"),
		metrics:         metrics,
		telemetryLogger: telemetryLogger,
		nodeToPodsMap:   make(map[string]map[string]*corev1.Pod),
	}
}

// Start begins the node collection process
func (c *NodeCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting node collector",
		"updateInterval", c.config.UpdateInterval,
		"disableGPUMetrics", c.config.DisableGPUMetrics)

	// Create informer factory
	c.informerFactory = informers.NewSharedInformerFactory(c.k8sClient, 0)

	// Create node informer
	c.nodeInformer = c.informerFactory.Core().V1().Nodes().Informer()

	c.podInformer = c.informerFactory.Core().V1().Pods().Informer()

	// Add pod event handlers
	_, err := c.podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod := obj.(*corev1.Pod)
			c.handlePodEvent(pod, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldPod := oldObj.(*corev1.Pod)
			newPod := newObj.(*corev1.Pod)
			c.handlePodEvent(newPod, EventTypeUpdate)

			// If node assignment changed, handle as delete for old node
			if oldPod.Spec.NodeName != newPod.Spec.NodeName {
				c.removePodFromNode(oldPod)
			}
		},
		DeleteFunc: func(obj interface{}) {
			pod := obj.(*corev1.Pod)
			c.handlePodEvent(pod, EventTypeDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add pod event handler: %w", err)
	}

	// Add event handlers
	_, err = c.nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			node := obj.(*corev1.Node)
			c.handleNodeEvent(node, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldNode := oldObj.(*corev1.Node)
			newNode := newObj.(*corev1.Node)

			// Only send updates if there's a meaningful change
			if c.nodeStatusChanged(oldNode, newNode) {
				c.handleNodeEvent(newNode, EventTypeUpdate)
			}
		},
		DeleteFunc: func(obj interface{}) {
			node := obj.(*corev1.Node)
			c.handleNodeEvent(node, EventTypeDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factory
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.nodeInformer.HasSynced, c.podInformer.HasSynced) {
		if c.telemetryLogger != nil {
			c.telemetryLogger.Report(
				gen.LogLevel_LOG_LEVEL_ERROR,
				"NodeCollector",
				"Timed out waiting for caches to sync",
				fmt.Errorf("cache sync timeout"),
				map[string]string{
					"excluded_nodes":   fmt.Sprintf("%v", c.excludedNodes),
					"zxporter_version": version.Get().String(),
				},
			)
		}
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Start the batcher (for metrics) after the cache is synced
	c.logger.Info("Starting resources batcher for node metrics")
	c.batcher.start()

	// Start a ticker to collect resource metrics at regular intervals
	c.ticker = time.NewTicker(c.config.UpdateInterval)

	// Start the resource collection loop
	go c.collectNodeResourcesLoop(ctx)

	// Monitor for context cancellation
	stopCh := c.stopCh
	go func() {
		select {
		case <-ctx.Done():
			c.Stop()
		case <-stopCh:
			// Channel was closed by Stop() method
		}
	}()

	return nil
}

// Add these new methods for pod event handling
func (c *NodeCollector) handlePodEvent(pod *corev1.Pod, eventType EventType) {
	// Skip pods not assigned to nodes yet
	if pod.Spec.NodeName == "" {
		return
	}

	// Skip excluded nodes
	if c.isExcluded(pod.Spec.NodeName) {
		return
	}

	switch eventType {
	case EventTypeAdd, EventTypeUpdate:
		c.addPodToNode(pod)
	case EventTypeDelete:
		c.removePodFromNode(pod)
	}
}

// addPodToNode add pod to node
func (c *NodeCollector) addPodToNode(pod *corev1.Pod) {
	c.podMapMutex.Lock()
	defer c.podMapMutex.Unlock()

	nodeName := pod.Spec.NodeName
	podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)

	// Initialize the pod map for this node if it doesn't exist
	if _, exists := c.nodeToPodsMap[nodeName]; !exists {
		c.nodeToPodsMap[nodeName] = make(map[string]*corev1.Pod)
	}

	c.nodeToPodsMap[nodeName][podKey] = pod
}

// removePodFromNode removes pod from existing node
func (c *NodeCollector) removePodFromNode(pod *corev1.Pod) {
	c.podMapMutex.Lock()
	defer c.podMapMutex.Unlock()

	nodeName := pod.Spec.NodeName
	podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)

	// Remove the pod from the node map
	if podMap, exists := c.nodeToPodsMap[nodeName]; exists {
		delete(podMap, podKey)
	}
}

// Calculate resource requests and limits for a node
func (c *NodeCollector) calculateNodeWorkloadResources(nodeName string) map[string]interface{} {
	c.podMapMutex.RLock()
	defer c.podMapMutex.RUnlock()

	result := map[string]interface{}{
		"cpuRequestsMillis":   int64(0),
		"cpuLimitsMillis":     int64(0),
		"memoryRequestsBytes": int64(0),
		"memoryLimitsBytes":   int64(0),
		"gpuRequestCount":     int64(0),
		"gpuLimitCount":       int64(0),
	}

	// Check if we have pods for this node
	podMap, exists := c.nodeToPodsMap[nodeName]
	if !exists {
		return result
	}

	// Calculate total requests and limits
	for _, pod := range podMap {
		// Skip pods not in Running or Pending phase
		if pod.Status.Phase != corev1.PodRunning && pod.Status.Phase != corev1.PodPending {
			continue
		}

		// Calculate resources for containers
		for _, container := range pod.Spec.Containers {
			// CPU requests
			if val, ok := container.Resources.Requests[corev1.ResourceCPU]; ok {
				result["cpuRequestsMillis"] = result["cpuRequestsMillis"].(int64) + val.MilliValue()
			}

			// CPU limits
			if val, ok := container.Resources.Limits[corev1.ResourceCPU]; ok {
				result["cpuLimitsMillis"] = result["cpuLimitsMillis"].(int64) + val.MilliValue()
			}

			// Memory requests
			if val, ok := container.Resources.Requests[corev1.ResourceMemory]; ok {
				result["memoryRequestsBytes"] = result["memoryRequestsBytes"].(int64) + val.Value()
			}

			// Memory limits
			if val, ok := container.Resources.Limits[corev1.ResourceMemory]; ok {
				result["memoryLimitsBytes"] = result["memoryLimitsBytes"].(int64) + val.Value()
			}

			// GPU requests
			if val, ok := container.Resources.Requests[gpuconst.GpuResource]; ok {
				result["gpuRequestCount"] = result["gpuRequestCount"].(int64) + val.Value()
			}

			// GPU limits
			if val, ok := container.Resources.Limits[gpuconst.GpuResource]; ok {
				result["gpuLimitCount"] = result["gpuLimitCount"].(int64) + val.Value()
			}
		}
	}

	return result
}

// handleNodeEvent processes node add, update, and delete events
func (c *NodeCollector) handleNodeEvent(node *corev1.Node, eventType EventType) {
	if c.isExcluded(node.Name) {
		return
	}

	// Send node events directly to resourceChan as a single-item batch
	c.resourceChan <- []CollectedResource{
		{
			ResourceType: Node,
			Object:       node,
			Timestamp:    time.Now(),
			EventType:    eventType,
			Key:          node.Name,
		},
	}
}

// nodeStatusChanged checks if there have been meaningful changes to node status
func (c *NodeCollector) nodeStatusChanged(oldNode, newNode *corev1.Node) bool {
	// Check if conditions changed
	if len(oldNode.Status.Conditions) != len(newNode.Status.Conditions) {
		return true
	}

	// Create map of old conditions for quick lookup
	oldConditions := make(map[corev1.NodeConditionType]corev1.NodeCondition)
	for _, condition := range oldNode.Status.Conditions {
		oldConditions[condition.Type] = condition
	}

	// Check if any condition changed
	for _, newCondition := range newNode.Status.Conditions {
		oldCondition, exists := oldConditions[newCondition.Type]
		if !exists || oldCondition.Status != newCondition.Status ||
			oldCondition.Reason != newCondition.Reason ||
			oldCondition.Message != newCondition.Message {
			return true
		}
	}

	// Check if allocatable resources changed (CPU, memory, GPU)
	if !oldNode.Status.Allocatable.Cpu().Equal(*newNode.Status.Allocatable.Cpu()) ||
		!oldNode.Status.Allocatable.Memory().Equal(*newNode.Status.Allocatable.Memory()) {
		return true
	}
	oldAllocGPU := oldNode.Status.Allocatable[corev1.ResourceName(gpuconst.GpuResource)]
	newAllocGPU := newNode.Status.Allocatable[corev1.ResourceName(gpuconst.GpuResource)]
	if !oldAllocGPU.Equal(newAllocGPU) {
		return true
	}

	// Check if capacity changed (CPU, memory, GPU)
	if !oldNode.Status.Capacity.Cpu().Equal(*newNode.Status.Capacity.Cpu()) ||
		!oldNode.Status.Capacity.Memory().Equal(*newNode.Status.Capacity.Memory()) {
		return true
	}
	oldCapGPU := oldNode.Status.Capacity[corev1.ResourceName(gpuconst.GpuResource)]
	newCapGPU := newNode.Status.Capacity[corev1.ResourceName(gpuconst.GpuResource)]
	if !oldCapGPU.Equal(newCapGPU) {
		return true
	}

	if !reflect.DeepEqual(oldNode.Labels, newNode.Labels) {
		return true
	}

	if !reflect.DeepEqual(oldNode.Annotations, newNode.Annotations) {
		return true
	}

	if !reflect.DeepEqual(oldNode.UID, newNode.UID) {
		return true
	}

	// No significant changes detected
	return false
}

// collectNodeResourcesLoop collects node resource metrics at regular intervals
func (c *NodeCollector) collectNodeResourcesLoop(ctx context.Context) {
	// Collect immediately on start
	c.collectAllNodeResources(ctx)

	// Then collect based on ticker
	for {
		select {
		case <-c.stopCh:
			return
		case <-c.ticker.C:
			c.collectAllNodeResources(ctx)
		}
	}
}

// collectAllNodeResources collects resource metrics for all nodes
func (c *NodeCollector) collectAllNodeResources(ctx context.Context) {
	// Skip if metrics client is unavailable
	if c.metricsClient == nil {
		c.logger.Info("Metrics client not available, skipping node metrics collection")
		c.telemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_ERROR,
			"NodeCollector",
			"Metrics client not available, skipping node metrics collection",
			fmt.Errorf("metrics server client not available or properly set"),
			map[string]string{
				"collector_type":   c.GetType(),
				"error_type":       "nil_metrics_server_client",
				"zxporter_version": version.Get().String(),
			},
		)
		return
	}

	// Build node metrics from nodemon /node/metrics endpoint (uses kubelet stats/summary
	// node-level data which includes system processes, not just container aggregation)
	nodeMetricsList := &metricsapisv1beta1.NodeMetricsList{}

	nodes := c.nodeInformer.GetIndexer().List()
	for _, obj := range nodes {
		node, ok := obj.(*corev1.Node)
		if !ok {
			continue
		}
		nm := metricsapisv1beta1.NodeMetrics{
			ObjectMeta: metav1.ObjectMeta{Name: node.Name},
			Usage:      corev1.ResourceList{},
		}
		// Fetch node-level metrics from nodemon (includes system process CPU/memory)
		nodeMetric, err := c.nodemonClient.FetchNodeMetricsByNode(ctx, node.Name)
		if err != nil {
			c.logger.V(1).Info("Failed to fetch node metrics from nodemon, skipping CPU/memory", "node", node.Name, "error", err)
		} else if nodeMetric != nil {
			cpuMillis := int64(nodeMetric.CPUUsageNanoCores / 1_000_000)
			nm.Usage[corev1.ResourceCPU] = *resource.NewMilliQuantity(cpuMillis, resource.DecimalSI)
			nm.Usage[corev1.ResourceMemory] = *resource.NewQuantity(int64(nodeMetric.MemoryWorkingSet), resource.BinarySI)
		}
		nodeMetricsList.Items = append(nodeMetricsList.Items, nm)
	}
	c.logger.V(1).Info("Built node metrics from nodemon container data", "nodes", len(nodeMetricsList.Items))

	if c.telemetryLogger != nil {
		c.telemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_INFO,
			"NodeCollector",
			"Successfully fetched node metrics from metrics server",
			nil,
			map[string]string{
				"node_count":       fmt.Sprintf("%d", len(nodeMetricsList.Items)),
				"excluded_nodes":   fmt.Sprintf("%v", c.excludedNodes),
				"event_type":       "metrics_server_query_success",
				"zxporter_version": version.Get().String(),
			},
		)
	}

	// Process each node's metrics
	for _, nodeMetrics := range nodeMetricsList.Items {
		// Skip excluded nodes
		if c.isExcluded(nodeMetrics.Name) {
			continue
		}

		// Get the node object from the cache
		nodeObj, exists, err := c.nodeInformer.GetIndexer().GetByKey(nodeMetrics.Name)
		if err != nil {
			c.logger.Error(err, "Failed to get node from cache", "name", nodeMetrics.Name)
			c.telemetryLogger.Report(
				gen.LogLevel_LOG_LEVEL_ERROR,
				"NodeCollector",
				"Failed to get node from cache",
				err,
				map[string]string{
					"collector_type":    c.GetType(),
					"node_metrics_name": nodeMetrics.Name,
					"error_type":        "node_cache_fail",
					"zxporter_version":  version.Get().String(),
				},
			)
			continue
		}

		if !exists {
			c.logger.Info("Node not found in cache", "name", nodeMetrics.Name)
			c.telemetryLogger.Report(
				gen.LogLevel_LOG_LEVEL_ERROR,
				"NodeCollector",
				"Node not found in informer cache",
				fmt.Errorf("node not found in informer cache"),
				map[string]string{
					"collector_type":    c.GetType(),
					"node_metrics_name": nodeMetrics.Name,
					"error_type":        "node_cache_fail",
					"zxporter_version":  version.Get().String(),
				},
			)
			continue
		}

		node, ok := nodeObj.(*corev1.Node)
		if !ok {
			c.logger.Error(nil, "Failed to convert to Node type", "name", nodeMetrics.Name)
			c.telemetryLogger.Report(
				gen.LogLevel_LOG_LEVEL_ERROR,
				"NodeCollector",
				"Failed to convert to Node type",
				fmt.Errorf("failed to convert cache node to node type object"),
				map[string]string{
					"collector_type":    c.GetType(),
					"node_metrics_name": nodeMetrics.Name,
					"error_type":        "node_object",
					"zxporter_version":  version.Get().String(),
				},
			)
			continue
		}

		// Extract CPU usage in millicores
		cpuQuantity := nodeMetrics.Usage.Cpu()
		cpuUsage := cpuQuantity.MilliValue()

		// Extract memory usage in bytes
		memoryQuantity := nodeMetrics.Usage.Memory()
		memoryUsage := memoryQuantity.Value()

		// Get allocatable resources from the node
		cpuAllocatable := node.Status.Allocatable.Cpu().MilliValue()
		memoryAllocatable := node.Status.Allocatable.Memory().Value()

		// Get capacity from the node
		cpuCapacity := node.Status.Capacity.Cpu().MilliValue()
		memoryCapacity := node.Status.Capacity.Memory().Value()

		// Calculate utilization percentages
		cpuUtilizationPercent := float64(cpuUsage) / float64(cpuAllocatable) * 100
		memoryUtilizationPercent := float64(memoryUsage) / float64(memoryAllocatable) * 100

		// Fetch network and I/O metrics from nodemon
		var networkMetrics map[string]float64
		var gpuMetrics map[string]interface{}

		if netMetrics, netErr := c.collectNodeNetworkIOMetrics(ctx, node.Name); netErr != nil {
			c.logger.Error(netErr, "Failed to collect node network and io metrics from nodemon",
				"name", node.Name)
			networkMetrics = make(map[string]float64)
		} else {
			networkMetrics = netMetrics
		}

		// Fetch GPU metrics from nodemon if enabled
		if !c.config.DisableGPUMetrics && c.nodemonClient != nil {
			nodemonMetrics, fetchErr := c.nodemonClient.FetchMetricsByNode(ctx, node.Name)
			if fetchErr != nil {
				c.logger.Error(fetchErr, "Failed to fetch GPU metrics from nodemon", "node", node.Name)
				gpuMetrics = make(map[string]interface{})
			} else if len(nodemonMetrics) > 0 {
				gpuMetrics = NodeGPUMetricsFromNodemon(nodemonMetrics)
			} else {
				gpuMetrics = make(map[string]interface{})
			}
		}

		// Create resource data
		resourceData := map[string]interface{}{
			// Node identification
			"nodeName": node.Name,

			// Resource usage
			"cpuUsageMillis":         cpuUsage,
			"memoryUsageBytes":       memoryUsage,
			"cpuAllocatableMillis":   cpuAllocatable,
			"memoryAllocatableBytes": memoryAllocatable,
			"cpuCapacityMillis":      cpuCapacity,
			"memoryCapacityBytes":    memoryCapacity,

			// Utilization percentages
			"cpuUtilizationPercent":    cpuUtilizationPercent,
			"memoryUtilizationPercent": memoryUtilizationPercent,

			// Node properties
			"labels":                  node.Labels,
			"taints":                  node.Spec.Taints,
			"conditions":              node.Status.Conditions,
			"kubeletVersion":          node.Status.NodeInfo.KubeletVersion,
			"osImage":                 node.Status.NodeInfo.OSImage,
			"kernelVersion":           node.Status.NodeInfo.KernelVersion,
			"containerRuntimeVersion": node.Status.NodeInfo.ContainerRuntimeVersion,

			// Include the full node object for any other needed details
			"node": node,
		}

		// Add network metrics if available
		if len(networkMetrics) > 0 {
			resourceData["networkReceiveBytes"] = networkMetrics["NetworkReceiveBytes"]
			resourceData["networkTransmitBytes"] = networkMetrics["NetworkTransmitBytes"]
			resourceData["networkReceivePackets"] = networkMetrics["NetworkReceivePackets"]
			resourceData["networkTransmitPackets"] = networkMetrics["NetworkTransmitPackets"]
			resourceData["networkReceiveErrors"] = networkMetrics["NetworkReceiveErrors"]
			resourceData["networkTransmitErrors"] = networkMetrics["NetworkTransmitErrors"]
			resourceData["networkReceiveDropped"] = networkMetrics["NetworkReceiveDropped"]
			resourceData["networkTransmitDropped"] = networkMetrics["NetworkTransmitDropped"]
			resourceData["fsReadBytes"] = networkMetrics["FSReadBytes"]
			resourceData["fsWriteBytes"] = networkMetrics["FSWriteBytes"]
			resourceData["fsReads"] = networkMetrics["FSReads"]
			resourceData["fsWrites"] = networkMetrics["FSWrites"]
		}

		// Add GPU metrics if available
		if len(gpuMetrics) > 0 {
			// Basic GPU counts and utilization
			resourceData["gpuCount"] = gpuMetrics["GPUCount"]
			resourceData["gpuUtilizationAvg"] = gpuMetrics["GPUUtilizationAvg"]
			resourceData["gpuUtilizationMax"] = gpuMetrics["GPUUtilizationMax"]

			// GPU memory
			resourceData["gpuMemoryUsedTotal"] = gpuMetrics["GPUMemoryUsedTotal"]
			resourceData["gpuMemoryFreeTotal"] = gpuMetrics["GPUMemoryFreeTotal"]
			resourceData["gpuMemoryTotalMb"] = gpuMetrics["GPUMemoryTotalMb"]

			// GPU power and temperature
			resourceData["gpuPowerUsageTotal"] = gpuMetrics["GPUPowerUsageTotal"]
			resourceData["gpuTemperatureAvg"] = gpuMetrics["GPUTemperatureAvg"]
			resourceData["gpuTemperatureMax"] = gpuMetrics["GPUTemperatureMax"]
			resourceData["gpuMemoryTemperatureAvg"] = gpuMetrics["GPUMemoryTemperatureAvg"]
			resourceData["gpuMemoryTemperatureMax"] = gpuMetrics["GPUMemoryTemperatureMax"]

			// GPU utilization details
			resourceData["gpuTensorUtilizationAvg"] = gpuMetrics["GPUTensorUtilizationAvg"]
			resourceData["gpuDramUtilizationAvg"] = gpuMetrics["GPUDramUtilizationAvg"]
			resourceData["gpuPCIeTxBytesTotal"] = gpuMetrics["GPUPCIeTxBytesTotal"]
			resourceData["gpuPCIeRxBytesTotal"] = gpuMetrics["GPUPCIeRxBytesTotal"]

			// Graphic utilization
			resourceData["gpuGraphicsUtilizationAvg"] = gpuMetrics["GPUGraphicsUtilizationAvg"]

			// GPU models and identifiers
			resourceData["gpuModels"] = gpuMetrics["GPUModels"]
			resourceData["gpuUUIDs"] = gpuMetrics["GPUUUIDs"]
			resourceData["gpuUsage"] = gpuMetrics["GPUUsage"]
		}

		workloadResources := c.calculateNodeWorkloadResources(node.Name)

		for k, v := range workloadResources {
			resourceData[k] = v
		}

		// Send node resource metrics to the batch channel for batching
		c.batchChan <- CollectedResource{
			ResourceType: NodeResource,
			Object:       resourceData,
			Timestamp:    time.Now(),
			EventType:    EventTypeMetrics,
			Key:          node.Name,
		}
	}
}

// collectNodeNetworkIOMetrics collects network and I/O metrics for a node
// using the nodemon DaemonSet.
func (c *NodeCollector) collectNodeNetworkIOMetrics(
	ctx context.Context,
	nodeName string,
) (map[string]float64, error) {
	m, err := c.nodemonClient.FetchNodeMetricsByNode(ctx, nodeName)
	if err != nil {
		return nil, fmt.Errorf("fetching node metrics from nodemon for %s: %w", nodeName, err)
	}
	if m == nil {
		return nil, fmt.Errorf("no nodemon pod found on node %s", nodeName)
	}

	return map[string]float64{
		"NetworkReceiveBytes":    m.NetworkRxBytesPerSec,
		"NetworkTransmitBytes":   m.NetworkTxBytesPerSec,
		"NetworkReceivePackets":  m.NetworkRxPacketsPerSec,
		"NetworkTransmitPackets": m.NetworkTxPacketsPerSec,
		"NetworkReceiveErrors":   m.NetworkRxErrorsPerSec,
		"NetworkTransmitErrors":  m.NetworkTxErrorsPerSec,
		"NetworkReceiveDropped":  m.NetworkRxDropsPerSec,
		"NetworkTransmitDropped": m.NetworkTxDropsPerSec,
		"FSReadBytes":            m.DiskReadBytesPerSec,
		"FSWriteBytes":           m.DiskWriteBytesPerSec,
		"FSReads":                m.DiskReadOpsPerSec,
		"FSWrites":               m.DiskWriteOpsPerSec,
	}, nil
}

// isExcluded checks if a node should be excluded from collection
func (c *NodeCollector) isExcluded(nodeName string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.excludedNodes[nodeName]
}

// Stop gracefully shuts down the node collector
func (c *NodeCollector) Stop() error {
	c.logger.Info("Stopping node collector")

	// 1. Stop the ticker
	if c.ticker != nil {
		c.ticker.Stop()
		c.logger.Info("Stopped node collector ticker")
	}

	// 2. Signal the informer factory and collection loop to stop by closing stopCh.
	select {
	case <-c.stopCh:
		c.logger.Info("Node collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed node collector stop channel")
	}

	// 3. Close the batchChan (input to the batcher for metrics).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed node collector batch input channel")
	}

	// 4. Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop() // This will close resourceChan when done
		c.logger.Info("Node collector batcher stopped")
	}

	// 5. Clear nodeToPodsMap
	c.podMapMutex.Lock()
	c.nodeToPodsMap = make(map[string]map[string]*corev1.Pod)
	c.podMapMutex.Unlock()

	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *NodeCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *NodeCollector) GetType() string {
	return "node"
}

// IsAvailable checks if Node resources can be accessed in the cluster.
// Always returns true — nodemon pods are discovered dynamically.
func (c *NodeCollector) IsAvailable(ctx context.Context) bool {
	return c.nodemonClient != nil
}

// AddResource manually adds a node resource to be processed by the collector
func (c *NodeCollector) AddResource(resource interface{}) error {
	node, ok := resource.(*corev1.Node)
	if !ok {
		return fmt.Errorf("expected *corev1.Node, got %T", resource)
	}

	c.handleNodeEvent(node, EventTypeAdd)
	return nil
}
