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

// ExcludedArgoRollout identifies an Argo Rollout to exclude
type ExcludedArgoRollout struct {
	// Namespace is the Argo Rollout's namespace
	Namespace string `json:"namespace"`

	// Name is the Argo Rollout's name
	Name string `json:"name"`
}

// ArgoRolloutsCollector watches for Argo Rollouts resources
type ArgoRolloutsCollector struct {
	dynamicClient    dynamic.Interface
	resourceChan     chan CollectedResource
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

	return &ArgoRolloutsCollector{
		dynamicClient:    dynamicClient,
		resourceChan:     make(chan CollectedResource, 100),
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
			c.handleRolloutEvent(u, "add")
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

			c.handleRolloutEvent(newU, "update")
			// c.analyzeRolloutChanges(oldU, newU)
		},
		DeleteFunc: func(obj interface{}) {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				// Try to handle DeletedFinalStateUnknown
				if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
					if u, ok = tombstone.Obj.(*unstructured.Unstructured); ok {
						c.handleRolloutEvent(u, "delete")
						return
					}
				}
				c.logger.Error(nil, "Failed to convert deleted object")
				return
			}
			c.handleRolloutEvent(u, "delete")
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
func (c *ArgoRolloutsCollector) handleRolloutEvent(obj *unstructured.Unstructured, eventType string) {
	name := obj.GetName()
	namespace := obj.GetNamespace()

	// Check if this resource should be excluded
	if c.isExcluded(namespace, name) {
		return
	}

	c.logger.Info("Processing Argo Rollout event",
		"name", name,
		"namespace", namespace,
		"eventType", eventType)

	// Process the rollout
	processedObj := c.processRollout(obj)

	// Create a resource key
	key := fmt.Sprintf("%s/%s", namespace, name)

	// Send the processed resource to the channel
	c.resourceChan <- CollectedResource{
		ResourceType: ArgoRollouts,
		Object:       processedObj,
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          key,
	}
}

// // analyzeRolloutChanges detects important changes between resource versions
// func (c *ArgoRolloutsCollector) analyzeRolloutChanges(oldObj, newObj *unstructured.Unstructured) {
// 	name := newObj.GetName()
// 	namespace := newObj.GetNamespace()

// 	// Extract phase information
// 	oldPhase, _, _ := unstructured.NestedString(oldObj.Object, "status", "phase")
// 	newPhase, _, _ := unstructured.NestedString(newObj.Object, "status", "phase")

// 	// Check for phase changes
// 	if oldPhase != newPhase {
// 		c.logger.Info("Rollout phase changed",
// 			"namespace", namespace,
// 			"name", name,
// 			"oldPhase", oldPhase,
// 			"newPhase", newPhase)

// 		// Send a phase change event
// 		c.resourceChan <- CollectedResource{
// 			ResourceType: ResourceType("argo_rollout_phase"),
// 			Object: map[string]interface{}{
// 				"namespace": namespace,
// 				"name":      name,
// 				"oldPhase":  oldPhase,
// 				"newPhase":  newPhase,
// 				"timestamp": time.Now().Unix(),
// 			},
// 			Timestamp: time.Now(),
// 			EventType: "phase_changed",
// 			Key:       fmt.Sprintf("%s/%s", namespace, name),
// 		}
// 	}

// 	// Check for canary changes if this is a canary deployment
// 	oldCanaryWeight, oldCanaryFound, _ := unstructured.NestedInt64(oldObj.Object, "status", "canary", "weight")
// 	newCanaryWeight, newCanaryFound, _ := unstructured.NestedInt64(newObj.Object, "status", "canary", "weight")

// 	if (oldCanaryFound && newCanaryFound && oldCanaryWeight != newCanaryWeight) ||
// 		(oldCanaryFound != newCanaryFound) {
// 		c.logger.Info("Rollout canary weight changed",
// 			"namespace", namespace,
// 			"name", name,
// 			"oldWeight", oldCanaryWeight,
// 			"newWeight", newCanaryWeight)

// 		// Send a canary weight change event
// 		c.resourceChan <- CollectedResource{
// 			ResourceType: ResourceType("argo_rollout_canary"),
// 			Object: map[string]interface{}{
// 				"namespace": namespace,
// 				"name":      name,
// 				"weight":    newCanaryWeight,
// 				"hasCanary": newCanaryFound,
// 				"timestamp": time.Now().Unix(),
// 			},
// 			Timestamp: time.Now(),
// 			EventType: "canary_weight_changed",
// 			Key:       fmt.Sprintf("%s/%s", namespace, name),
// 		}
// 	}

// 	// Check for blue-green changes if this is a blue-green deployment
// 	oldActiveService, oldBGFound, _ := unstructured.NestedString(oldObj.Object, "status", "blueGreen", "activeService")
// 	newActiveService, newBGFound, _ := unstructured.NestedString(newObj.Object, "status", "blueGreen", "activeService")

// 	if (oldBGFound && newBGFound && oldActiveService != newActiveService) ||
// 		(oldBGFound != newBGFound) {
// 		c.logger.Info("Rollout blue-green active service changed",
// 			"namespace", namespace,
// 			"name", name,
// 			"oldService", oldActiveService,
// 			"newService", newActiveService)

// 		// Send a blue-green service change event
// 		c.resourceChan <- CollectedResource{
// 			ResourceType: ResourceType("argo_rollout_bluegreen"),
// 			Object: map[string]interface{}{
// 				"namespace":     namespace,
// 				"name":          name,
// 				"activeService": newActiveService,
// 				"hasBlueGreen":  newBGFound,
// 				"timestamp":     time.Now().Unix(),
// 			},
// 			Timestamp: time.Now(),
// 			EventType: "active_service_changed",
// 			Key:       fmt.Sprintf("%s/%s", namespace, name),
// 		}
// 	}
// }

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

	// Close the main stop channel
	if c.stopCh != nil {
		close(c.stopCh)
		c.stopCh = nil
	}

	return nil
}

// GetResourceChannel returns the channel for collected resources
func (c *ArgoRolloutsCollector) GetResourceChannel() <-chan CollectedResource {
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
