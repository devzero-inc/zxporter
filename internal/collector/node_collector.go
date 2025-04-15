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
}

// NodeCollector collects node events and resource metrics
type NodeCollector struct {
	k8sClient       kubernetes.Interface
	metricsClient   *metricsv1.Clientset
	prometheusAPI   v1.API
	informerFactory informers.SharedInformerFactory
	nodeInformer    cache.SharedIndexInformer
	resourceChan    chan CollectedResource
	stopCh          chan struct{}
	ticker          *time.Ticker
	config          NodeCollectorConfig
	excludedNodes   map[string]bool
	logger          logr.Logger
	mu              sync.RWMutex
}

// NewNodeCollector creates a new collector for node resources
func NewNodeCollector(
	k8sClient kubernetes.Interface,
	metricsClient *metricsv1.Clientset,
	config NodeCollectorConfig,
	excludedNodes []string,
	logger logr.Logger,
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

	return &NodeCollector{
		k8sClient:     k8sClient,
		metricsClient: metricsClient,
		resourceChan:  make(chan CollectedResource, 100),
		stopCh:        make(chan struct{}),
		config:        config,
		excludedNodes: excludedNodesMap,
		logger:        logger.WithName("node-collector"),
	}
}

// initPrometheusClient initializes the Prometheus client with fallback mechanisms
func (c *NodeCollector) initPrometheusClient(ctx context.Context) error {
	if c.config.DisableNetworkIOMetrics {
		c.logger.Info("Network and I/O metrics collection is disabled")
		return nil
	}

	c.logger.Info("Initializing Prometheus client for node network and I/O metrics",
		"prometheusURL", c.config.PrometheusURL)

	client, err := api.NewClient(api.Config{
		Address: c.config.PrometheusURL,
	})
	if err != nil {
		c.logger.Error(err, "Failed to create Prometheus client, node network and I/O metrics will be disabled")
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
		c.logger.Error(err, "Failed to connect to Prometheus, node network and I/O metrics will be disabled")
		c.prometheusAPI = nil
		return err
	}

	c.logger.Info("Successfully connected to Prometheus for node network and I/O metrics")
	return nil
}

// Start begins the node collection process
func (c *NodeCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting node collector",
		"updateInterval", c.config.UpdateInterval,
		"disableNetworkIOMetrics", c.config.DisableNetworkIOMetrics)

	// Initialize Prometheus client if network/IO metrics are not disabled
	if !c.config.DisableNetworkIOMetrics {
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
			c.handleNodeEvent(node, "add")
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldNode := oldObj.(*corev1.Node)
			newNode := newObj.(*corev1.Node)

			// Only send updates if there's a meaningful change
			if c.nodeStatusChanged(oldNode, newNode) {
				c.handleNodeEvent(newNode, "update")
			}
		},
		DeleteFunc: func(obj interface{}) {
			node := obj.(*corev1.Node)
			c.handleNodeEvent(node, "delete")
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
func (c *NodeCollector) handleNodeEvent(node *corev1.Node, eventType string) {
	if c.isExcluded(node.Name) {
		return
	}

	c.logger.Info("Processing node event",
		"name", node.Name,
		"eventType", eventType)

	// Send the raw node object to the resource channel
	c.resourceChan <- CollectedResource{
		ResourceType: Node,
		Object:       node,
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          node.Name,
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
	if !c.config.DisableNetworkIOMetrics && c.prometheusAPI != nil {
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
		if networkMetrics != nil {
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
		if ioMetrics != nil {
			resourceData["fsReadBytes"] = ioMetrics["FSReadBytes"]
			resourceData["fsWriteBytes"] = ioMetrics["FSWriteBytes"]
			resourceData["fsReads"] = ioMetrics["FSReads"]
			resourceData["fsWrites"] = ioMetrics["FSWrites"]
		}

		// Send the resource usage data to the channel
		c.resourceChan <- CollectedResource{
			ResourceType: NodeResource,
			Object:       resourceData,
			Timestamp:    time.Now(),
			EventType:    "metrics",
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

// isExcluded checks if a node should be excluded from collection
func (c *NodeCollector) isExcluded(nodeName string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.excludedNodes[nodeName]
}

// Stop gracefully shuts down the node collector
func (c *NodeCollector) Stop() error {
	c.logger.Info("Stopping node collector")

	if c.ticker != nil {
		c.ticker.Stop()
	}

	if c.stopCh != nil {
		if c.stopCh != nil {
			close(c.stopCh)
			c.stopCh = nil
		}
		c.stopCh = nil
	}
	return nil
}

// GetResourceChannel returns the channel for collected resources
func (c *NodeCollector) GetResourceChannel() <-chan CollectedResource {
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
