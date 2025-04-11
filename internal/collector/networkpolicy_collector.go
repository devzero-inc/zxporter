// internal/collector/networkpolicy_collector.go
package collector

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// NetworkPolicyCollector watches for networkpolicy events and collects networkpolicy data
type NetworkPolicyCollector struct {
	client                  kubernetes.Interface
	informerFactory         informers.SharedInformerFactory
	networkPolicyInformer   cache.SharedIndexInformer
	resourceChan            chan CollectedResource
	stopCh                  chan struct{}
	namespaces              []string
	excludedNetworkPolicies map[types.NamespacedName]bool
	logger                  logr.Logger
	mu                      sync.RWMutex
}

// ExcludedNetworkPolicy defines a networkpolicy to exclude from collection
type ExcludedNetworkPolicy struct {
	Namespace string
	Name      string
}

// NewNetworkPolicyCollector creates a new collector for networkpolicy resources
func NewNetworkPolicyCollector(
	client kubernetes.Interface,
	namespaces []string,
	excludedNetworkPolicies []ExcludedNetworkPolicy,
	logger logr.Logger,
) *NetworkPolicyCollector {
	// Convert excluded networkpolicies to a map for quicker lookups
	excludedNetworkPoliciesMap := make(map[types.NamespacedName]bool)
	for _, networkPolicy := range excludedNetworkPolicies {
		excludedNetworkPoliciesMap[types.NamespacedName{
			Namespace: networkPolicy.Namespace,
			Name:      networkPolicy.Name,
		}] = true
	}

	return &NetworkPolicyCollector{
		client:                  client,
		resourceChan:            make(chan CollectedResource, 50), // Network policies change infrequently
		stopCh:                  make(chan struct{}),
		namespaces:              namespaces,
		excludedNetworkPolicies: excludedNetworkPoliciesMap,
		logger:                  logger.WithName("networkpolicy-collector"),
	}
}

// Start begins the networkpolicy collection process
func (c *NetworkPolicyCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting networkpolicy collector", "namespaces", c.namespaces)

	// Create informer factory based on namespace configuration
	if len(c.namespaces) == 1 && c.namespaces[0] != "" {
		// Watch a specific namespace
		c.informerFactory = informers.NewSharedInformerFactoryWithOptions(
			c.client,
			0, // No resync period, rely on events
			informers.WithNamespace(c.namespaces[0]),
		)
	} else {
		// Watch all namespaces
		c.informerFactory = informers.NewSharedInformerFactory(c.client, 0)
	}

	// Create networkpolicy informer
	c.networkPolicyInformer = c.informerFactory.Networking().V1().NetworkPolicies().Informer()

	// Add event handlers
	_, err := c.networkPolicyInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			networkPolicy := obj.(*networkingv1.NetworkPolicy)
			c.handleNetworkPolicyEvent(networkPolicy, "add")
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldNetworkPolicy := oldObj.(*networkingv1.NetworkPolicy)
			newNetworkPolicy := newObj.(*networkingv1.NetworkPolicy)

			// Only handle meaningful updates
			if c.networkPolicyChanged(oldNetworkPolicy, newNetworkPolicy) {
				c.handleNetworkPolicyEvent(newNetworkPolicy, "update")
			}
		},
		DeleteFunc: func(obj interface{}) {
			networkPolicy := obj.(*networkingv1.NetworkPolicy)
			c.handleNetworkPolicyEvent(networkPolicy, "delete")
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.networkPolicyInformer.HasSynced) {
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

// handleNetworkPolicyEvent processes networkpolicy events
func (c *NetworkPolicyCollector) handleNetworkPolicyEvent(networkPolicy *networkingv1.NetworkPolicy, eventType string) {
	if c.isExcluded(networkPolicy) {
		return
	}

	c.logger.V(4).Info("Processing networkpolicy event",
		"namespace", networkPolicy.Namespace,
		"name", networkPolicy.Name,
		"eventType", eventType)

	// Send the raw networkpolicy object directly to the resource channel
	c.resourceChan <- CollectedResource{
		ResourceType: NetworkPolicy,
		Object:       networkPolicy, // Send the entire networkpolicy object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", networkPolicy.Namespace, networkPolicy.Name),
	}
}

// networkPolicyChanged detects meaningful changes in a networkpolicy
func (c *NetworkPolicyCollector) networkPolicyChanged(oldNetworkPolicy, newNetworkPolicy *networkingv1.NetworkPolicy) bool {
	// Ignore changes to ResourceVersion, which always changes even for irrelevant updates
	if oldNetworkPolicy.ResourceVersion == newNetworkPolicy.ResourceVersion {
		return false
	}

	// Check for pod selector changes
	if !reflect.DeepEqual(oldNetworkPolicy.Spec.PodSelector, newNetworkPolicy.Spec.PodSelector) {
		return true
	}

	// Check for policy types changes
	if !policyTypesEqual(oldNetworkPolicy.Spec.PolicyTypes, newNetworkPolicy.Spec.PolicyTypes) {
		return true
	}

	// Check for ingress rules changes
	if !reflect.DeepEqual(oldNetworkPolicy.Spec.Ingress, newNetworkPolicy.Spec.Ingress) {
		return true
	}

	// Check for egress rules changes
	if !reflect.DeepEqual(oldNetworkPolicy.Spec.Egress, newNetworkPolicy.Spec.Egress) {
		return true
	}

	// Check for label changes
	if !mapsEqual(oldNetworkPolicy.Labels, newNetworkPolicy.Labels) {
		return true
	}

	// Check for annotation changes
	if !mapsEqual(oldNetworkPolicy.Annotations, newNetworkPolicy.Annotations) {
		return true
	}

	// No significant changes detected
	return false
}

// policyTypesEqual compares two slices of policy types for equality
func policyTypesEqual(types1, types2 []networkingv1.PolicyType) bool {
	if len(types1) != len(types2) {
		return false
	}

	// Create maps for efficient comparison
	typesMap1 := make(map[networkingv1.PolicyType]bool)
	typesMap2 := make(map[networkingv1.PolicyType]bool)

	for _, t := range types1 {
		typesMap1[t] = true
	}

	for _, t := range types2 {
		typesMap2[t] = true
	}

	// Compare maps
	for t := range typesMap1 {
		if !typesMap2[t] {
			return false
		}
	}

	for t := range typesMap2 {
		if !typesMap1[t] {
			return false
		}
	}

	return true
}

// isExcluded checks if a networkpolicy should be excluded from collection
func (c *NetworkPolicyCollector) isExcluded(networkPolicy *networkingv1.NetworkPolicy) bool {
	// Check if monitoring specific namespaces and this networkpolicy isn't in them
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		found := false
		for _, ns := range c.namespaces {
			if ns == networkPolicy.Namespace {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}

	// Check if networkpolicy is specifically excluded
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := types.NamespacedName{
		Namespace: networkPolicy.Namespace,
		Name:      networkPolicy.Name,
	}
	return c.excludedNetworkPolicies[key]
}

// Stop gracefully shuts down the networkpolicy collector
func (c *NetworkPolicyCollector) Stop() error {
	c.logger.Info("Stopping networkpolicy collector")
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
func (c *NetworkPolicyCollector) GetResourceChannel() <-chan CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *NetworkPolicyCollector) GetType() string {
	return "networkpolicy"
}

// IsAvailable checks if NetworkPolicy resources can be accessed in the cluster
func (c *NetworkPolicyCollector) IsAvailable(ctx context.Context) bool {
	// Try to list NetworkPolicies with a limit of 1 to check availability with minimal overhead
	_, err := c.client.NetworkingV1().NetworkPolicies("").List(ctx, metav1.ListOptions{
		Limit: 1,
	})

	if err != nil {
		// Check if this is a "resource not found" type error
		if strings.Contains(err.Error(),
			"the server could not find the requested resource") {
			c.logger.Info("NetworkPolicy API not available in cluster")
			return false
		}

		// For other errors (permissions, etc), log the error
		c.logger.Error(err, "Error checking NetworkPolicy availability")
		return false
	}

	return true
}
