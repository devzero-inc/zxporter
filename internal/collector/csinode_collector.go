// internal/collector/csinode_collector.go
package collector

import (
	"context"
	"fmt"
	"sync"
	"time"

	telemetry_logger "github.com/devzero-inc/zxporter/internal/logger"
	"github.com/go-logr/logr"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// CSINodeCollector watches for CSINode events and collects CSINode data
type CSINodeCollector struct {
	client          kubernetes.Interface
	informerFactory informers.SharedInformerFactory
	csiNodeInformer cache.SharedIndexInformer
	batchChan       chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan    chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher         *ResourcesBatcher
	stopCh          chan struct{}
	excludedNodes   map[string]bool
	logger          logr.Logger
	telemetryLogger telemetry_logger.Logger
	mu              sync.RWMutex
}

// NewCSINodeCollector creates a new collector for CSINode resources
func NewCSINodeCollector(
	client kubernetes.Interface,
	excludedNodes []string,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
) *CSINodeCollector {
	// Convert excluded nodes to a map for quicker lookups
	excludedNodesMap := make(map[string]bool)
	for _, node := range excludedNodes {
		excludedNodesMap[node] = true
	}

	// Create channels
	batchChan := make(chan CollectedResource, 100)
	resourceChan := make(chan []CollectedResource, 100)

	// Create the batcher
	batcher := NewResourcesBatcher(
		maxBatchSize,
		maxBatchTime,
		batchChan,
		resourceChan,
		logger,
	)

	return &CSINodeCollector{
		client:          client,
		batchChan:       batchChan,
		resourceChan:    resourceChan,
		batcher:         batcher,
		stopCh:          make(chan struct{}),
		excludedNodes:   excludedNodesMap,
		logger:          logger.WithName("csinode-collector"),
		telemetryLogger: telemetryLogger,
	}
}

// Start begins the CSINode collection process
func (c *CSINodeCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting CSINode collector")

	// Create informer factory for all namespaces
	c.informerFactory = informers.NewSharedInformerFactory(c.client, 0)

	// Create CSINode informer
	c.csiNodeInformer = c.informerFactory.Storage().V1().CSINodes().Informer()

	// Add event handlers
	_, err := c.csiNodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			csiNode := obj.(*storagev1.CSINode)
			c.handleCSINodeEvent(csiNode, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			// oldCSINode := oldObj.(*storagev1.CSINode)
			newCSINode := newObj.(*storagev1.CSINode)
			c.handleCSINodeEvent(newCSINode, EventTypeUpdate)
		},
		DeleteFunc: func(obj interface{}) {
			csiNode := obj.(*storagev1.CSINode)
			c.handleCSINodeEvent(csiNode, EventTypeDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler to CSINode informer: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.csiNodeInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Start the batcher after the cache is synced
	c.logger.Info("Starting resources batcher for CSINodes")
	c.batcher.start()

	// Keep this goroutine alive until context cancellation or stop
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

// handleCSINodeEvent processes CSINode add, update, and delete events
func (c *CSINodeCollector) handleCSINodeEvent(csiNode *storagev1.CSINode, eventType EventType) {
	if c.isExcluded(csiNode) {
		return
	}

	c.logger.Info("Processing CSINode event",
		"name", csiNode.Name,
		"eventType", eventType.String())

	// Convert the drivers to a more digestible format
	drivers := make([]map[string]interface{}, 0, len(csiNode.Spec.Drivers))
	for _, driver := range csiNode.Spec.Drivers {
		driverInfo := map[string]interface{}{
			"name":              driver.Name,
			"node_id":           driver.NodeID,
			"available":         driver.Allocatable != nil,
			"topology_keys":     []string{},
			"volume_plugin_dir": "",
		}

		if driver.Allocatable != nil && driver.Allocatable.Count != nil {
			driverInfo["max_volumes"] = *driver.Allocatable.Count
		}

		if driver.TopologyKeys != nil {
			driverInfo["topology_keys"] = driver.TopologyKeys
		}

		// if driver.VolumeLifecycleModes != nil {
		// 	driverInfo["volume_lifecycle_modes"] = driver.VolumeLifecycleModes
		// }

		drivers = append(drivers, driverInfo)
	}

	// Create an enriched CSINode resource
	enrichedCSINode := map[string]interface{}{
		"name":              csiNode.Name,
		"uid":               string(csiNode.UID),
		"resourceVersion":   csiNode.ResourceVersion,
		"creationTimestamp": csiNode.CreationTimestamp.Unix(),
		"labels":            csiNode.Labels,
		"annotations":       csiNode.Annotations,
		"drivers":           drivers,
		"driver_count":      len(csiNode.Spec.Drivers),
		"raw":               csiNode, // Include the raw object for reference
	}

	// Send the CSINode data to the batch channel
	c.batchChan <- CollectedResource{
		ResourceType: CSINode, // Use string-based ResourceType
		Object:       enrichedCSINode,
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          csiNode.Name,
	}
}

// sendCSIDriverEvent sends a specific event related to a CSI driver change
func (c *CSINodeCollector) sendCSIDriverEvent(
	csiNode *storagev1.CSINode,
	driver storagev1.CSINodeDriver,
	eventType EventType,
) {
	driverInfo := map[string]interface{}{
		"node":          csiNode.Name,
		"driver_name":   driver.Name,
		"node_id":       driver.NodeID,
		"topology_keys": driver.TopologyKeys,
		"timestamp":     time.Now().Unix(),
	}

	if driver.Allocatable != nil && driver.Allocatable.Count != nil {
		driverInfo["max_volumes"] = *driver.Allocatable.Count
	}

	// Send the driver-specific event
	c.batchChan <- CollectedResource{
		ResourceType: CSINode,
		Object:       driverInfo,
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", csiNode.Name, driver.Name),
	}
}

// sendCSIDriverRemovedEvent sends an event when a driver is removed
func (c *CSINodeCollector) sendCSIDriverRemovedEvent(csiNode *storagev1.CSINode, driverName string) {
	driverInfo := map[string]interface{}{
		"node":        csiNode.Name,
		"driver_name": driverName,
		"timestamp":   time.Now().Unix(),
	}

	// Send the driver removal event
	c.batchChan <- CollectedResource{
		ResourceType: CSINode,
		Object:       driverInfo,
		Timestamp:    time.Now(),
		EventType:    EventTypeDelete,
		Key:          fmt.Sprintf("%s/%s", csiNode.Name, driverName),
	}
}

// isExcluded checks if a CSINode should be excluded from collection
func (c *CSINodeCollector) isExcluded(csiNode *storagev1.CSINode) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.excludedNodes[csiNode.Name]
}

// Stop gracefully shuts down the CSINode collector
func (c *CSINodeCollector) Stop() error {
	c.logger.Info("Stopping CSINode collector")

	// 1. Signal the informer factory to stop by closing stopCh.
	select {
	case <-c.stopCh:
		c.logger.Info("CSINode collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed CSINode collector stop channel")
	}

	// 2. Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed CSINode collector batch input channel")
	}

	// 3. Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("CSINode collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *CSINodeCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *CSINodeCollector) GetType() string {
	return "csi_node"
}

// IsAvailable checks if CSINode resources can be accessed in the cluster
func (c *CSINodeCollector) IsAvailable(ctx context.Context) bool {
	_, err := c.client.StorageV1().CSINodes().List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil {
		c.logger.Info("CSINode resources not available in the cluster", "error", err.Error())
		return false
	}
	return true
}

// AddResource manually adds a CSINode resource to be processed by the collector
func (c *CSINodeCollector) AddResource(resource interface{}) error {
	csiNode, ok := resource.(*storagev1.CSINode)
	if !ok {
		return fmt.Errorf("expected *storagev1.CSINode, got %T", resource)
	}

	c.handleCSINodeEvent(csiNode, EventTypeAdd)
	return nil
}
