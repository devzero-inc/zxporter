// internal/collector/node_collector.go
package collector

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	metricsv1 "k8s.io/metrics/pkg/client/clientset/versioned"
)

// NodeCollector collects node events and resource metrics
type NodeCollector struct {
	k8sClient       kubernetes.Interface
	metricsClient   *metricsv1.Clientset
	informerFactory informers.SharedInformerFactory
	nodeInformer    cache.SharedIndexInformer
	resourceChan    chan CollectedResource
	stopCh          chan struct{}
	ticker          *time.Ticker
	updateInterval  time.Duration
	excludedNodes   map[string]bool
	logger          logr.Logger
	mu              sync.RWMutex
}

// ExcludedDeployment defines a deployment to exclude from collection
type ExcludedDeployment struct {
	Namespace string
	Name      string
}

// NewNodeCollector creates a new collector for node resources
func NewNodeCollector(
	k8sClient kubernetes.Interface,
	metricsClient *metricsv1.Clientset,
	excludedNodes []string,
	updateInterval time.Duration,
	logger logr.Logger,
) *NodeCollector {
	// Convert excluded nodes to a map for quicker lookups
	excludedNodesMap := make(map[string]bool)
	for _, node := range excludedNodes {
		excludedNodesMap[node] = true
	}

	// Default update interval if not specified
	if updateInterval <= 0 {
		updateInterval = 10 * time.Second
	}

	return &NodeCollector{
		k8sClient:      k8sClient,
		metricsClient:  metricsClient,
		resourceChan:   make(chan CollectedResource, 100),
		stopCh:         make(chan struct{}),
		updateInterval: updateInterval,
		excludedNodes:  excludedNodesMap,
		logger:         logger.WithName("node-collector"),
	}
}

// Start begins the node collection process
func (c *NodeCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting node collector", "updateInterval", c.updateInterval)

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
	c.ticker = time.NewTicker(c.updateInterval)

	// Start the resource collection loop
	go c.collectNodeResourcesLoop(ctx)

	// Monitor for context cancellation
	go func() {
		<-ctx.Done()
		c.logger.Info("Context cancelled, stopping node collector")
		c.Stop()
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

	close(c.stopCh)
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
	return true
}
