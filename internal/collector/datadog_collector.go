// internal/collector/datadog_collector.go
package collector

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
)

// ExcludedDatadogExtendedDaemonSetReplicaSet identifies an ExtendedDaemonSetReplicaSet to exclude
type ExcludedDatadogExtendedDaemonSetReplicaSet struct {
	// Namespace is the ExtendedDaemonSetReplicaSet's namespace
	Namespace string `json:"namespace"`

	// Name is the ExtendedDaemonSetReplicaSet's name
	Name string `json:"name"`
}

// DatadogCollector watches for DataDog custom resources
type DatadogCollector struct {
	dynamicClient       dynamic.Interface
	resourceChan        chan CollectedResource
	stopCh              chan struct{}
	informers           map[string]cache.SharedIndexInformer
	informerStopChs     map[string]chan struct{}
	namespaces          []string
	excludedReplicaSets map[types.NamespacedName]bool
	logger              logr.Logger
	mu                  sync.RWMutex
}

// NewDatadogCollector creates a new collector for DataDog resources
func NewDatadogCollector(
	dynamicClient dynamic.Interface,
	namespaces []string,
	excludedReplicaSets []ExcludedDatadogExtendedDaemonSetReplicaSet,
	logger logr.Logger,
) *DatadogCollector {
	// Convert excluded replica sets to a map for quicker lookups
	excludedReplicaSetsMap := make(map[types.NamespacedName]bool)
	for _, rs := range excludedReplicaSets {
		excludedReplicaSetsMap[types.NamespacedName{
			Namespace: rs.Namespace,
			Name:      rs.Name,
		}] = true
	}

	return &DatadogCollector{
		dynamicClient:       dynamicClient,
		resourceChan:        make(chan CollectedResource, 100),
		stopCh:              make(chan struct{}),
		informers:           make(map[string]cache.SharedIndexInformer),
		informerStopChs:     make(map[string]chan struct{}),
		namespaces:          namespaces,
		excludedReplicaSets: excludedReplicaSetsMap,
		logger:              logger.WithName("datadog-collector"),
	}
}

// Start begins the DataDog resources collection process
func (c *DatadogCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting DataDog collector", "namespaces", c.namespaces)

	// Define the ExtendedDaemonSetReplicaSet GVR
	gvr := schema.GroupVersionResource{
		Group:    "datadoghq.com",
		Version:  "v1alpha1",
		Resource: "extendeddaemonsetreplicasets",
	}

	// Set up informers based on namespace configuration
	var factory dynamicinformer.DynamicSharedInformerFactory
	if len(c.namespaces) == 1 && c.namespaces[0] != "" {
		// Watch a specific namespace
		factory = dynamicinformer.NewFilteredDynamicSharedInformerFactory(
			c.dynamicClient,
			0, // No resync period
			c.namespaces[0],
			nil,
		)
	} else {
		// Watch all namespaces
		factory = dynamicinformer.NewFilteredDynamicSharedInformerFactory(
			c.dynamicClient,
			0,  // No resync period
			"", // All namespaces
			nil,
		)
	}

	// Create informer for ExtendedDaemonSetReplicaSets
	informer := factory.ForResource(gvr).Informer()

	// Add event handlers
	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				c.logger.Error(nil, "Failed to convert object to unstructured")
				return
			}
			c.handleReplicaSetEvent(u, "add")
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			_, ok := oldObj.(*unstructured.Unstructured)
			if !ok {
				c.logger.Error(nil, "Failed to convert old object to unstructured")
				return
			}

			newU, ok := newObj.(*unstructured.Unstructured)
			if !ok {
				c.logger.Error(nil, "Failed to convert new object to unstructured")
				return
			}

			c.handleReplicaSetEvent(newU, "update")
		},
		DeleteFunc: func(obj interface{}) {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				// Try to handle DeletedFinalStateUnknown
				if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
					if u, ok = tombstone.Obj.(*unstructured.Unstructured); ok {
						c.handleReplicaSetEvent(u, "delete")
						return
					}
				}
				c.logger.Error(nil, "Failed to convert deleted object")
				return
			}
			c.handleReplicaSetEvent(u, "delete")
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler to informer for ExtendedDaemonSetReplicaSets: %w", err)
	}

	// Store informer and create stop channel
	replicaSetKey := "extendeddaemonsetreplicasets"
	c.informers[replicaSetKey] = informer
	c.informerStopChs[replicaSetKey] = make(chan struct{})

	// Start the informer
	go informer.Run(c.informerStopChs[replicaSetKey])

	// Wait for cache sync with timeout
	syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if !cache.WaitForCacheSync(syncCtx.Done(), informer.HasSynced) {
		return fmt.Errorf("timeout waiting for ExtendedDaemonSetReplicaSets cache to sync")
	}

	c.logger.Info("Successfully started informer for ExtendedDaemonSetReplicaSets")

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

// handleReplicaSetEvent processes DataDog ExtendedDaemonSetReplicaSet events
func (c *DatadogCollector) handleReplicaSetEvent(obj *unstructured.Unstructured, eventType string) {
	name := obj.GetName()
	namespace := obj.GetNamespace()

	// Check if this resource should be excluded
	if c.isExcluded(namespace, name) {
		return
	}

	c.logger.Info("Processing ExtendedDaemonSetReplicaSet event",
		"name", name,
		"namespace", namespace,
		"eventType", eventType)

	// Create a resource key
	key := fmt.Sprintf("%s/%s", namespace, name)

	// Send the processed resource to the channel
	c.resourceChan <- CollectedResource{
		ResourceType: Datadog,
		Object:       obj,
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          key,
	}
}

// isExcluded checks if a replica set should be excluded
func (c *DatadogCollector) isExcluded(namespace, name string) bool {
	// Check if monitoring specific namespaces and this resource isn't in them
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		found := false
		for _, ns := range c.namespaces {
			if ns == namespace {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}

	// Check if resource is specifically excluded
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := types.NamespacedName{
		Namespace: namespace,
		Name:      name,
	}
	return c.excludedReplicaSets[key]
}

// Stop gracefully shuts down the DataDog collector
func (c *DatadogCollector) Stop() error {
	c.logger.Info("Stopping DataDog collector")

	// Stop all informers
	for key, stopCh := range c.informerStopChs {
		c.logger.Info("Stopping informer", "resource", key)
		close(stopCh)
	}

	// Clear maps
	c.informers = make(map[string]cache.SharedIndexInformer)
	c.informerStopChs = make(map[string]chan struct{})

	// Close the main stop channel
	if c.stopCh != nil {
		close(c.stopCh)
		c.stopCh = nil
	}

	return nil
}

// GetResourceChannel returns the channel for collected resources
func (c *DatadogCollector) GetResourceChannel() <-chan CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *DatadogCollector) GetType() string {
	return "datadog"
}

// IsAvailable checks if DataDog resources can be accessed in the cluster
func (c *DatadogCollector) IsAvailable(ctx context.Context) bool {
	gvr := schema.GroupVersionResource{
		Group:    "datadoghq.com",
		Version:  "v1alpha1",
		Resource: "extendeddaemonsetreplicasets",
	}

	_, err := c.dynamicClient.Resource(gvr).List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil {
		c.logger.Info("DataDog ExtendedDaemonSetReplicaSet resources not available in the cluster", "error", err.Error())
		return false
	}
	return true
}
