// internal/collector/node_collector.go
package collector

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/api"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	metricsv1 "k8s.io/metrics/pkg/client/clientset/versioned"
)

// NodeCollectorConfig holds configuration for the node collector
type NodeCollectorConfig struct {
	// UpdateInterval specifies how often to collect metrics
	UpdateInterval time.Duration

	// PrometheusURL specifies the URL of the Prometheus instance to query
	// If empty, defaults to in-cluster Prometheus at http://prometheus.monitoring:9090
	PrometheusURL string

	// QueryTimeout specifies the timeout for Prometheus queries
	QueryTimeout time.Duration

	// DisableNetworkIOMetrics determines whether to disable network and I/O metrics collection
	// Default is false, so metrics are collected by default
	DisableNetworkIOMetrics bool

	// DisableGPUMetrics determines whether to disable GPU metrics collection
	// Default is false, so metrics are collected by default
	DisableGPUMetrics bool
}

// NodeCollector collects node events and resource metrics
type NodeCollector struct {
	k8sClient       kubernetes.Interface
	metricsClient   *metricsv1.Clientset
	prometheusAPI   v1.API
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
	metrics         *PrometheusMetrics
	mu              sync.RWMutex
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
	metrics *PrometheusMetrics,
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

	// Default Prometheus URL if not specified
	if config.PrometheusURL == "" {
		config.PrometheusURL = "http://prometheus.monitoring:9090"
	}

	// Default query timeout if not specified
	if config.QueryTimeout <= 0 {
		config.QueryTimeout = 10 * time.Second
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

	return &NodeCollector{
		k8sClient:     k8sClient,
		metricsClient: metricsClient,
		batchChan:     batchChan,
		resourceChan:  resourceChan,
		batcher:       batcher,
		stopCh:        make(chan struct{}),
		config:        config,
		excludedNodes: excludedNodesMap,
		logger:        logger.WithName("node-collector"),
		metrics:       metrics,
	}
}

// initPrometheusClient initializes the Prometheus client with fallback mechanisms
func (c *NodeCollector) initPrometheusClient(ctx context.Context) error {
	c.logger.Info("Initializing Prometheus client",
		"prometheusURL", c.config.PrometheusURL)

	// Create a custom HTTP client with metrics
	httpClient := NewPrometheusClient(c.metrics)

	client, err := api.NewClient(api.Config{
		Address: c.config.PrometheusURL,
		Client:  httpClient,
	})
	if err != nil {
		c.logger.Error(err, "Failed to create Prometheus client, node network, I/O and GPU metrics will be disabled")
		return err
	}

	// Set the API client
	c.prometheusAPI = v1.NewAPI(client)

	// Verify access with a simple query
	queryCtx, cancel := context.WithTimeout(ctx, c.config.QueryTimeout)
	defer cancel()

	_, _, err = c.prometheusAPI.Query(queryCtx, "up", time.Now())
	if err != nil {
		// Check if this is a permission error
		if strings.Contains(err.Error(), "forbidden") || strings.Contains(err.Error(), "unauthorized") {
			c.logger.Error(err, "Permission denied when accessing Prometheus. Please ensure the service account has proper RBAC permissions")
			c.logger.Info("You may need to apply the 'zxporter-prometheus-reader' ClusterRole and ClusterRoleBinding")

			// Continue with basic metrics only
			c.prometheusAPI = nil
			return fmt.Errorf("permission error accessing Prometheus: %w", err)
		}

		// Handle other connection errors
		c.logger.Error(err, "Failed to connect to Prometheus, node network, I/O and GPU metrics will be disabled")
		c.prometheusAPI = nil
		return err
	}

	c.logger.Info("Successfully connected to Prometheus for node network, I/O and GPU metrics")
	return nil
}

// Start begins the node collection process
func (c *NodeCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting node collector",
		"updateInterval", c.config.UpdateInterval,
		"disableNetworkIOMetrics", c.config.DisableNetworkIOMetrics,
		"disableGPUMetrics", c.config.DisableGPUMetrics)

	// Initialize Prometheus client if network/IO metrics are not disabled
	if !c.config.DisableNetworkIOMetrics || !c.config.DisableGPUMetrics {
		// Use the more robust initialization method
		if err := c.initPrometheusClient(ctx); err != nil {
			// Log but continue - we can still collect CPU/memory metrics
			c.logger.Info("Continuing with basic node metrics collection only")
		}
	}

	// Create informer factory
	c.informerFactory = informers.NewSharedInformerFactory(c.k8sClient, 0)

	// Create node informer
	c.nodeInformer = c.informerFactory.Core().V1().Nodes().Informer()

	// Add event handlers
	_, err := c.nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
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
	if !cache.WaitForCacheSync(c.stopCh, c.nodeInformer.HasSynced) {
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

// handleNodeEvent processes node add, update, and delete events
func (c *NodeCollector) handleNodeEvent(node *corev1.Node, eventType EventType) {
	if c.isExcluded(node.Name) {
		return
	}

	c.logger.Info("Processing node event",
		"name", node.Name,
		"eventType", eventType.String())

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

	// Check if allocatable resources changed
	if !oldNode.Status.Allocatable.Cpu().Equal(*newNode.Status.Allocatable.Cpu()) ||
		!oldNode.Status.Allocatable.Memory().Equal(*newNode.Status.Allocatable.Memory()) {
		return true
	}

	// Check if capacity changed
	if !oldNode.Status.Capacity.Cpu().Equal(*newNode.Status.Capacity.Cpu()) ||
		!oldNode.Status.Capacity.Memory().Equal(*newNode.Status.Capacity.Memory()) {
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
	c.logger.Info("Collecting node resource metrics")

	// Skip if metrics client is unavailable
	if c.metricsClient == nil {
		c.logger.Info("Metrics client not available, skipping node metrics collection")
		return
	}

	// Fetch node metrics from the metrics server
	nodeMetricsList, err := c.metricsClient.MetricsV1beta1().NodeMetricses().List(ctx, metav1.ListOptions{})
	if err != nil {
		c.logger.Error(err, "Failed to get node metrics from metrics server")
		return
	}

	// Create a context with timeout for Prometheus queries if needed
	var queryCtx context.Context
	var cancel context.CancelFunc
	if !c.config.DisableNetworkIOMetrics && !c.config.DisableGPUMetrics && c.prometheusAPI != nil {
		queryCtx, cancel = context.WithTimeout(ctx, c.config.QueryTimeout)
		defer cancel()
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
			continue
		}

		if !exists {
			c.logger.Info("Node not found in cache", "name", nodeMetrics.Name)
			continue
		}

		node, ok := nodeObj.(*corev1.Node)
		if !ok {
			c.logger.Error(nil, "Failed to convert to Node type", "name", nodeMetrics.Name)
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

		// Fetch network metrics for the node if enabled
		var networkMetrics map[string]float64
		if !c.config.DisableNetworkIOMetrics && c.prometheusAPI != nil && queryCtx != nil {
			networkMetrics, err = c.collectNodeNetworkMetrics(queryCtx, node.Name)
			if err != nil {
				c.logger.Error(err, "Failed to collect node network metrics",
					"name", node.Name)
				// Continue with basic metrics
				networkMetrics = make(map[string]float64)
			}
		}

		// Fetch I/O metrics for the node if enabled
		var ioMetrics map[string]float64
		if !c.config.DisableNetworkIOMetrics && c.prometheusAPI != nil && queryCtx != nil {
			ioMetrics, err = c.collectNodeIOMetrics(queryCtx, node.Name)
			if err != nil {
				c.logger.Error(err, "Failed to collect node I/O metrics",
					"name", node.Name)
				// Continue with basic metrics
				ioMetrics = make(map[string]float64)
			}
		}

		// Fetch GPU metrics for the node if enabled
		var gpuMetrics map[string]interface{}
		if !c.config.DisableGPUMetrics && c.prometheusAPI != nil && queryCtx != nil {
			gpuMetrics, err = c.collectNodeGPUMetrics(queryCtx, node.Name)
			if err != nil {
				c.logger.Error(err, "Failed to collect node GPU metrics",
					"name", node.Name)
				// Continue with other metrics
				gpuMetrics = make(map[string]interface{})
			}
			c.logger.Info("GPU metrics",
				"node", node.Name,
				"resourceData", gpuMetrics)
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
		}

		// Add I/O metrics if available
		if len(ioMetrics) > 0 {
			resourceData["fsReadBytes"] = ioMetrics["FSReadBytes"]
			resourceData["fsWriteBytes"] = ioMetrics["FSWriteBytes"]
			resourceData["fsReads"] = ioMetrics["FSReads"]
			resourceData["fsWrites"] = ioMetrics["FSWrites"]
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
		}

		c.logger.Info("GPU metrics",
			"node", node.Name,
			"resourceData", resourceData)

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

// collectNodeNetworkMetrics collects network metrics for a node using Prometheus queries
func (c *NodeCollector) collectNodeNetworkMetrics(ctx context.Context, nodeName string) (map[string]float64, error) {
	metrics := make(map[string]float64)

	// Define queries for network metrics
	queries := map[string]string{
		"NetworkReceiveBytes":    fmt.Sprintf(`sum(rate(node_network_receive_bytes_total{instance=~"%s:.*"}[5m]))`, nodeName),
		"NetworkTransmitBytes":   fmt.Sprintf(`sum(rate(node_network_transmit_bytes_total{instance=~"%s:.*"}[5m]))`, nodeName),
		"NetworkReceivePackets":  fmt.Sprintf(`sum(rate(node_network_receive_packets_total{instance=~"%s:.*"}[5m]))`, nodeName),
		"NetworkTransmitPackets": fmt.Sprintf(`sum(rate(node_network_transmit_packets_total{instance=~"%s:.*"}[5m]))`, nodeName),
		"NetworkReceiveErrors":   fmt.Sprintf(`sum(rate(node_network_receive_errs_total{instance=~"%s:.*"}[5m]))`, nodeName),
		"NetworkTransmitErrors":  fmt.Sprintf(`sum(rate(node_network_transmit_errs_total{instance=~"%s:.*"}[5m]))`, nodeName),
		"NetworkReceiveDropped":  fmt.Sprintf(`sum(rate(node_network_receive_drop_total{instance=~"%s:.*"}[5m]))`, nodeName),
		"NetworkTransmitDropped": fmt.Sprintf(`sum(rate(node_network_transmit_drop_total{instance=~"%s:.*"}[5m]))`, nodeName),
	}

	// Execute each query and store the result
	for metricName, query := range queries {
		metrics[metricName] = 0 // Default to 0 for all metrics

		result, _, err := c.prometheusAPI.Query(ctx, query, time.Now())
		if err != nil {
			c.logger.Error(err, "Error querying Prometheus",
				"metric", metricName,
				"query", query)
			continue
		}

		// Extract value from result (if any)
		if result.Type() == model.ValVector {
			vector := result.(model.Vector)
			if len(vector) > 0 {
				metrics[metricName] = float64(vector[0].Value)
			}
		}
	}

	return metrics, nil
}

// collectNodeIOMetrics collects I/O metrics for a node using Prometheus queries
func (c *NodeCollector) collectNodeIOMetrics(ctx context.Context, nodeName string) (map[string]float64, error) {
	metrics := make(map[string]float64)

	// Define queries for I/O metrics
	queries := map[string]string{
		"FSReadBytes":  fmt.Sprintf(`sum(rate(node_disk_read_bytes_total{instance=~"%s:.*"}[5m]))`, nodeName),
		"FSWriteBytes": fmt.Sprintf(`sum(rate(node_disk_written_bytes_total{instance=~"%s:.*"}[5m]))`, nodeName),
		"FSReads":      fmt.Sprintf(`sum(rate(node_disk_reads_completed_total{instance=~"%s:.*"}[5m]))`, nodeName),
		"FSWrites":     fmt.Sprintf(`sum(rate(node_disk_writes_completed_total{instance=~"%s:.*"}[5m]))`, nodeName),
	}

	// Execute each query and store the result
	for metricName, query := range queries {
		metrics[metricName] = 0 // Default to 0 for all metrics

		result, _, err := c.prometheusAPI.Query(ctx, query, time.Now())
		if err != nil {
			c.logger.Error(err, "Error querying Prometheus",
				"metric", metricName,
				"query", query)
			continue
		}

		// Extract value from result (if any)
		if result.Type() == model.ValVector {
			vector := result.(model.Vector)
			if len(vector) > 0 {
				metrics[metricName] = float64(vector[0].Value)
			}
		}
	}

	return metrics, nil
}

// collectNodeGPUMetrics collects GPU metrics for a node using Prometheus queries
func (c *NodeCollector) collectNodeGPUMetrics(ctx context.Context, nodeName string) (map[string]interface{}, error) {
	metrics := make(map[string]interface{})

	// First query to check if this node has any GPUs
	nodeGPUQuery := fmt.Sprintf(`count(DCGM_FI_DEV_GPU_UTIL{node="%s"})`, nodeName)

	result, _, err := c.prometheusAPI.Query(ctx, nodeGPUQuery, time.Now())
	if err != nil {
		return nil, fmt.Errorf("error querying GPU availability: %w", err)
	}

	// Check if node has GPU metrics
	hasGPU := false
	if result.Type() == model.ValVector {
		vector := result.(model.Vector)
		if len(vector) > 0 && float64(vector[0].Value) > 0 {
			hasGPU = true
		}
	}

	if !hasGPU {
		// Return empty metrics if no GPU is on this node
		return metrics, nil
	}

	// Node has GPUs, collect metrics
	queries := map[string]string{
		"GPUCount":                  fmt.Sprintf(`count(DCGM_FI_DEV_GPU_UTIL{node="%s"})`, nodeName),
		"GPUUtilizationAvg":         fmt.Sprintf(`avg(DCGM_FI_DEV_GPU_UTIL{node="%s"})`, nodeName),
		"GPUUtilizationMax":         fmt.Sprintf(`max(DCGM_FI_DEV_GPU_UTIL{node="%s"})`, nodeName),
		"GPUMemoryUsedTotal":        fmt.Sprintf(`sum(DCGM_FI_DEV_FB_USED{node="%s"})`, nodeName),
		"GPUMemoryFreeTotal":        fmt.Sprintf(`sum(DCGM_FI_DEV_FB_FREE{node="%s"})`, nodeName),
		"GPUPowerUsageTotal":        fmt.Sprintf(`sum(DCGM_FI_DEV_POWER_USAGE{node="%s"})`, nodeName),
		"GPUTemperatureAvg":         fmt.Sprintf(`avg(DCGM_FI_DEV_GPU_TEMP{node="%s"})`, nodeName),
		"GPUTemperatureMax":         fmt.Sprintf(`max(DCGM_FI_DEV_GPU_TEMP{node="%s"})`, nodeName),
		"GPUMemoryTemperatureAvg":   fmt.Sprintf(`avg(DCGM_FI_DEV_MEMORY_TEMP{node="%s"})`, nodeName),
		"GPUMemoryTemperatureMax":   fmt.Sprintf(`max(DCGM_FI_DEV_MEMORY_TEMP{node="%s"})`, nodeName),
		"GPUTensorUtilizationAvg":   fmt.Sprintf(`avg(DCGM_FI_PROF_PIPE_TENSOR_ACTIVE{node="%s"})`, nodeName),
		"GPUDramUtilizationAvg":     fmt.Sprintf(`avg(DCGM_FI_PROF_DRAM_ACTIVE{node="%s"})`, nodeName),
		"GPUPCIeTxBytesTotal":       fmt.Sprintf(`sum(DCGM_FI_PROF_PCIE_TX_BYTES{node="%s"})`, nodeName),
		"GPUPCIeRxBytesTotal":       fmt.Sprintf(`sum(DCGM_FI_PROF_PCIE_RX_BYTES{node="%s"})`, nodeName),
		"GPUGraphicsUtilizationAvg": fmt.Sprintf(`avg(DCGM_FI_PROF_GR_ENGINE_ACTIVE{node="%s"})`, nodeName),
	}

	// Execute each query and store the result
	for metricName, query := range queries {
		result, _, err := c.prometheusAPI.Query(ctx, query, time.Now())
		if err != nil {
			c.logger.Error(err, "Error querying Prometheus for GPU metrics",
				"metric", metricName,
				"query", query,
				"node", nodeName)
			continue
		}

		// Extract value from result (if any)
		if result.Type() == model.ValVector {
			vector := result.(model.Vector)
			if len(vector) > 0 {
				metrics[metricName] = float64(vector[0].Value)
			}
		}
	}

	// If we found no GPU metrics, return an empty map
	if len(metrics) == 0 {
		return metrics, nil
	}

	// Get GPU models on this node - this requires a specific query and parsing (not sure if parsing is working or not :))
	modelQuery := fmt.Sprintf(`DCGM_FI_DEV_GPU_UTIL{node="%s"}`, nodeName)
	result, _, err = c.prometheusAPI.Query(ctx, modelQuery, time.Now())
	if err == nil && result.Type() == model.ValVector {
		vector := result.(model.Vector)

		// Store unique GPU models
		gpuModels := make(map[string]int)
		gpuUUIDs := make([]string, 0)

		for _, sample := range vector {
			model := string(sample.Metric["modelName"])
			if model != "" {
				gpuModels[model]++
			}

			// Collect UUID if available
			uuid := string(sample.Metric["UUID"])
			if uuid != "" {
				gpuUUIDs = append(gpuUUIDs, uuid)
			}
		}

		// Convert model map to a summarized string
		modelSummary := make([]string, 0)
		for model, count := range gpuModels {
			modelSummary = append(modelSummary, fmt.Sprintf("%dx %s", count, model))
		}

		metrics["GPUModels"] = modelSummary
		metrics["GPUUUIDs"] = gpuUUIDs
	}

	// Calculate total GPU memory
	if memUsed, ok := metrics["GPUMemoryUsedTotal"].(float64); ok {
		if memFree, ok := metrics["GPUMemoryFreeTotal"].(float64); ok {
			metrics["GPUMemoryTotalMb"] = memUsed + memFree
		}
	}

	return metrics, nil
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

// IsAvailable checks if Node resources can be accessed in the cluster
func (c *NodeCollector) IsAvailable(ctx context.Context) bool {
	// Check if the metrics client is available - this is required for basic metrics
	if c.metricsClient == nil {
		c.logger.Info("Metrics client is not available, cannot collect node metrics")
		return false
	}

	// Try a simple query to check if the metrics server is available
	_, err := c.metricsClient.MetricsV1beta1().NodeMetricses().List(ctx, metav1.ListOptions{
		Limit: 1, // Only request a single item to minimize load
	})
	if err != nil {
		c.logger.Info("Metrics server API not available for node metrics", "error", err.Error())
		return false
	}

	// Even if Prometheus is not available, we can still collect basic node metrics
	// so we return true here
	return true
}
