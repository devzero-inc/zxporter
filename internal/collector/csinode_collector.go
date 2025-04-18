// internal/collector/csinode_collector.go
package collector

import (
	"context"
	"fmt"
	"sync"
	"time"

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
	resourceChan    chan CollectedResource
	stopCh          chan struct{}
	excludedNodes   map[string]bool
	logger          logr.Logger
	mu              sync.RWMutex
}

// NewCSINodeCollector creates a new collector for CSINode resources
func NewCSINodeCollector(
	client kubernetes.Interface,
	excludedNodes []string,
	logger logr.Logger,
) *CSINodeCollector {
	// Convert excluded nodes to a map for quicker lookups
	excludedNodesMap := make(map[string]bool)
	for _, node := range excludedNodes {
		excludedNodesMap[node] = true
	}

	return &CSINodeCollector{
		client:        client,
		resourceChan:  make(chan CollectedResource, 100),
		stopCh:        make(chan struct{}),
		excludedNodes: excludedNodesMap,
		logger:        logger.WithName("csinode-collector"),
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
			c.handleCSINodeEvent(csiNode, "add")
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			// oldCSINode := oldObj.(*storagev1.CSINode)
			newCSINode := newObj.(*storagev1.CSINode)
			c.handleCSINodeEvent(newCSINode, "update")
		},
		DeleteFunc: func(obj interface{}) {
			csiNode := obj.(*storagev1.CSINode)
			c.handleCSINodeEvent(csiNode, "delete")
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
func (c *CSINodeCollector) handleCSINodeEvent(csiNode *storagev1.CSINode, eventType string) {
	if c.isExcluded(csiNode) {
		return
	}

	c.logger.Info("Processing CSINode event",
		"name", csiNode.Name,
		"eventType", eventType)

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

	// Send the CSINode data to the resource channel
	c.resourceChan <- CollectedResource{
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
	eventType string,
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
	c.resourceChan <- CollectedResource{
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
	c.resourceChan <- CollectedResource{
		ResourceType: CSINode,
		Object:       driverInfo,
		Timestamp:    time.Now(),
		EventType:    "driver_removed",
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
	if c.stopCh != nil {
		close(c.stopCh)
		c.stopCh = nil
	}
	return nil
}

// GetResourceChannel returns the channel for collected resources
func (c *CSINodeCollector) GetResourceChannel() <-chan CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *CSINodeCollector) GetType() string {
	return "csinode"
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
