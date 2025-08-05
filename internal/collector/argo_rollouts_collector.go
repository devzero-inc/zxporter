// internal/collector/argo_rollouts_collector.go
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

// ArgoRolloutsCollector watches for Argo Rollouts resources
type ArgoRolloutsCollector struct {
	dynamicClient    dynamic.Interface
	batchChan        chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan     chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher          *ResourcesBatcher
	stopCh           chan struct{}
	informers        map[string]cache.SharedIndexInformer
	informerStopChs  map[string]chan struct{}
	namespaces       []string
	excludedRollouts map[types.NamespacedName]bool
	logger           logr.Logger
	mu               sync.RWMutex
}

// NewArgoRolloutsCollector creates a new collector for Argo Rollouts resources
func NewArgoRolloutsCollector(
	dynamicClient dynamic.Interface,
	namespaces []string,
	excludedRollouts []ExcludedArgoRollout,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
) *ArgoRolloutsCollector {
	// Convert excluded rollouts to a map for quicker lookups
	excludedRolloutsMap := make(map[types.NamespacedName]bool)
	for _, r := range excludedRollouts {
		excludedRolloutsMap[types.NamespacedName{
			Namespace: r.Namespace,
			Name:      r.Name,
		}] = true
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

	return &ArgoRolloutsCollector{
		dynamicClient:    dynamicClient,
		batchChan:        batchChan,
		resourceChan:     resourceChan,
		batcher:          batcher,
		stopCh:           make(chan struct{}),
		informers:        make(map[string]cache.SharedIndexInformer),
		informerStopChs:  make(map[string]chan struct{}),
		namespaces:       namespaces,
		excludedRollouts: excludedRolloutsMap,
		logger:           logger.WithName("argo-rollouts-collector"),
	}
}

// Start begins the Argo Rollouts resources collection process
func (c *ArgoRolloutsCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting Argo Rollouts collector", "namespaces", c.namespaces)

	// Define the Rollout GVR
	gvr := schema.GroupVersionResource{
		Group:    "argoproj.io",
		Version:  "v1alpha1",
		Resource: "rollouts",
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

	// Create informer for Rollouts
	informer := factory.ForResource(gvr).Informer()

	// Add event handlers
	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				c.logger.Error(nil, "Failed to convert object to unstructured")
				return
			}
			c.handleRolloutEvent(u, EventTypeAdd)
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

			c.handleRolloutEvent(newU, EventTypeUpdate)
		},
		DeleteFunc: func(obj interface{}) {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				// Try to handle DeletedFinalStateUnknown
				if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
					if u, ok = tombstone.Obj.(*unstructured.Unstructured); ok {
						c.handleRolloutEvent(u, EventTypeDelete)
						return
					}
				}
				c.logger.Error(nil, "Failed to convert deleted object")
				return
			}
			c.handleRolloutEvent(u, EventTypeDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler to informer for Argo Rollouts: %w", err)
	}

	// Store informer and create stop channel
	rolloutKey := "rollouts"
	c.informers[rolloutKey] = informer
	c.informerStopChs[rolloutKey] = make(chan struct{})

	// Start the informer
	go informer.Run(c.informerStopChs[rolloutKey])

	// Wait for cache sync with timeout
	syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if !cache.WaitForCacheSync(syncCtx.Done(), informer.HasSynced) {
		return fmt.Errorf("timeout waiting for Argo Rollouts cache to sync")
	}

	c.logger.Info("Successfully started informer for Argo Rollouts")

	// Start the batcher after the cache is synced
	c.logger.Info("Starting resources batcher for Argo Rollouts")
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

// handleRolloutEvent processes Argo Rollout events
func (c *ArgoRolloutsCollector) handleRolloutEvent(obj *unstructured.Unstructured, eventType EventType) {
	name := obj.GetName()
	namespace := obj.GetNamespace()

	// Check if this resource should be excluded
	if c.isExcluded(namespace, name) {
		return
	}

	c.logger.Info("Processing Argo Rollout event",
		"name", name,
		"namespace", namespace,
		"eventType", eventType.String())

	// Process the rollout
	processedObj := c.processRollout(obj)

	// Create a resource key
	key := fmt.Sprintf("%s/%s", namespace, name)

	// Send the processed resource to the batch channel
	c.batchChan <- CollectedResource{
		ResourceType: ArgoRollouts,
		Object:       processedObj,
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          key,
	}
}

// processRollout extracts relevant fields from Argo Rollout objects
func (c *ArgoRolloutsCollector) processRollout(obj *unstructured.Unstructured) map[string]interface{} {
	// Extract common metadata
	result := map[string]interface{}{
		"name":              obj.GetName(),
		"namespace":         obj.GetNamespace(),
		"uid":               string(obj.GetUID()),
		"resourceVersion":   obj.GetResourceVersion(),
		"creationTimestamp": obj.GetCreationTimestamp().Unix(),
		"labels":            obj.GetLabels(),
		"annotations":       obj.GetAnnotations(),
		"raw":               obj,
	}

	// Get owner references if any
	if owners := obj.GetOwnerReferences(); len(owners) > 0 {
		ownerRefs := make([]map[string]interface{}, 0, len(owners))
		for _, owner := range owners {
			ownerRefs = append(ownerRefs, map[string]interface{}{
				"apiVersion": owner.APIVersion,
				"kind":       owner.Kind,
				"name":       owner.Name,
				"uid":        string(owner.UID),
			})
		}
		result["ownerReferences"] = ownerRefs
	}

	// Extract spec fields
	_, found, _ := unstructured.NestedMap(obj.Object, "spec")
	if found {
		// Extract important spec fields explicitly for easier access

		// Get strategy type
		strategy, foundStrategy, _ := unstructured.NestedMap(obj.Object, "spec", "strategy")
		if foundStrategy {
			result["strategy"] = strategy

			// Check for strategy type
			if _, canaryExists, _ := unstructured.NestedMap(obj.Object, "spec", "strategy", "canary"); canaryExists {
				result["strategyType"] = "canary"
			} else if _, blueGreenExists, _ := unstructured.NestedMap(obj.Object, "spec", "strategy", "blueGreen"); blueGreenExists {
				result["strategyType"] = "blueGreen"
			}
		}

		// Get replicas
		replicas, foundReplicas, _ := unstructured.NestedInt64(obj.Object, "spec", "replicas")
		if foundReplicas {
			result["replicas"] = replicas
		}

		// Get selector
		selector, foundSelector, _ := unstructured.NestedMap(obj.Object, "spec", "selector")
		if foundSelector {
			result["selector"] = selector
		}

		// Get template
		template, foundTemplate, _ := unstructured.NestedMap(obj.Object, "spec", "template")
		if foundTemplate {
			result["template"] = template
		}
	}

	// Extract status fields
	_, found, _ = unstructured.NestedMap(obj.Object, "status")
	if found {
		// Get rollout status phase
		phase, foundPhase, _ := unstructured.NestedString(obj.Object, "status", "phase")
		if foundPhase {
			result["phase"] = phase
		}

		// Get current step
		currentStep, foundCurrentStep, _ := unstructured.NestedInt64(obj.Object, "status", "currentStep")
		if foundCurrentStep {
			result["currentStep"] = currentStep
		}

		// Get message
		message, foundMessage, _ := unstructured.NestedString(obj.Object, "status", "message")
		if foundMessage {
			result["message"] = message
		}

		// Get canary details
		canary, foundCanary, _ := unstructured.NestedMap(obj.Object, "status", "canary")
		if foundCanary {
			result["canary"] = canary
		}

		// Get blue-green details
		blueGreen, foundBlueGreen, _ := unstructured.NestedMap(obj.Object, "status", "blueGreen")
		if foundBlueGreen {
			result["blueGreen"] = blueGreen
		}

		// Get available replicas
		availableReplicas, foundAvailable, _ := unstructured.NestedInt64(obj.Object, "status", "availableReplicas")
		if foundAvailable {
			result["availableReplicas"] = availableReplicas
		}

		// Get updated replicas
		updatedReplicas, foundUpdated, _ := unstructured.NestedInt64(obj.Object, "status", "updatedReplicas")
		if foundUpdated {
			result["updatedReplicas"] = updatedReplicas
		}

		// Get conditions
		conditions, foundConditions, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
		if foundConditions {
			result["conditions"] = conditions
		}
	}

	return result
}

// isExcluded checks if a rollout should be excluded
func (c *ArgoRolloutsCollector) isExcluded(namespace, name string) bool {
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
	return c.excludedRollouts[key]
}

// Stop gracefully shuts down the Argo Rollouts collector
func (c *ArgoRolloutsCollector) Stop() error {
	c.logger.Info("Stopping Argo Rollouts collector")

	// Stop all informers
	for key, stopCh := range c.informerStopChs {
		c.logger.Info("Stopping informer", "resource", key)
		close(stopCh)
	}

	// Clear maps
	c.informers = make(map[string]cache.SharedIndexInformer)
	c.informerStopChs = make(map[string]chan struct{})

	// Close the main stop channel (signals informers to stop)
	select {
	case <-c.stopCh:
		c.logger.Info("Argo Rollouts collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed Argo Rollouts collector stop channel")
	}

	// Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed Argo Rollouts collector batch input channel")
	}

	// Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("Argo Rollouts collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *ArgoRolloutsCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *ArgoRolloutsCollector) GetType() string {
	return "argo_rollouts"
}

// IsAvailable checks if Argo Rollouts resources can be accessed in the cluster
func (c *ArgoRolloutsCollector) IsAvailable(ctx context.Context) bool {
	gvr := schema.GroupVersionResource{
		Group:    "argoproj.io",
		Version:  "v1alpha1",
		Resource: "rollouts",
	}

	_, err := c.dynamicClient.Resource(gvr).List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil {
		c.logger.Info("Argo Rollouts resources not available in the cluster", "error", err.Error())
		return false
	}
	return true
}

// AddResource manually adds an Argo Rollout resource to be processed by the collector
func (c *ArgoRolloutsCollector) AddResource(resource interface{}) error {
	rollout, ok := resource.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("expected *unstructured.Unstructured, got %T", resource)
	}

	c.handleRolloutEvent(rollout, EventTypeAdd)
	return nil
}
