// internal/collector/limitrange_collector.go
package collector

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// LimitRangeCollector watches for limitrange events and collects limitrange data
type LimitRangeCollector struct {
	client              kubernetes.Interface
	informerFactory     informers.SharedInformerFactory
	limitRangeInformer  cache.SharedIndexInformer
	resourceChan        chan CollectedResource
	stopCh              chan struct{}
	namespaces          []string
	excludedLimitRanges map[types.NamespacedName]bool
	logger              logr.Logger
	mu                  sync.RWMutex
}

// ExcludedLimitRange defines a limitrange to exclude from collection
type ExcludedLimitRange struct {
	Namespace string
	Name      string
}

// NewLimitRangeCollector creates a new collector for limitrange resources
func NewLimitRangeCollector(
	client kubernetes.Interface,
	namespaces []string,
	excludedLimitRanges []ExcludedLimitRange,
	logger logr.Logger,
) *LimitRangeCollector {
	// Convert excluded limitranges to a map for quicker lookups
	excludedLimitRangesMap := make(map[types.NamespacedName]bool)
	for _, lr := range excludedLimitRanges {
		excludedLimitRangesMap[types.NamespacedName{
			Namespace: lr.Namespace,
			Name:      lr.Name,
		}] = true
	}

	return &LimitRangeCollector{
		client:              client,
		resourceChan:        make(chan CollectedResource, 50), // LimitRanges change infrequently
		stopCh:              make(chan struct{}),
		namespaces:          namespaces,
		excludedLimitRanges: excludedLimitRangesMap,
		logger:              logger.WithName("limitrange-collector"),
	}
}

// Start begins the limitrange collection process
func (c *LimitRangeCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting limitrange collector", "namespaces", c.namespaces)

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

	// Create limitrange informer
	c.limitRangeInformer = c.informerFactory.Core().V1().LimitRanges().Informer()

	// Add event handlers
	_, err := c.limitRangeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			lr := obj.(*corev1.LimitRange)
			c.handleLimitRangeEvent(lr, "add")
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldLR := oldObj.(*corev1.LimitRange)
			newLR := newObj.(*corev1.LimitRange)

			// Only handle meaningful updates
			if c.limitRangeChanged(oldLR, newLR) {
				c.handleLimitRangeEvent(newLR, "update")
			}
		},
		DeleteFunc: func(obj interface{}) {
			lr := obj.(*corev1.LimitRange)
			c.handleLimitRangeEvent(lr, "delete")
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.limitRangeInformer.HasSynced) {
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

// handleLimitRangeEvent processes limitrange events
func (c *LimitRangeCollector) handleLimitRangeEvent(lr *corev1.LimitRange, eventType string) {
	if c.isExcluded(lr) {
		return
	}

	c.logger.V(4).Info("Processing limitrange event",
		"namespace", lr.Namespace,
		"name", lr.Name,
		"eventType", eventType)

	// Send the raw limitrange object directly to the resource channel
	c.resourceChan <- CollectedResource{
		ResourceType: LimitRange,
		Object:       lr, // Send the entire limitrange object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", lr.Namespace, lr.Name),
	}
}

// limitRangeChanged detects meaningful changes in a limitrange
func (c *LimitRangeCollector) limitRangeChanged(oldLR, newLR *corev1.LimitRange) bool {
	// Ignore changes to ResourceVersion, which always changes even for irrelevant updates
	if oldLR.ResourceVersion == newLR.ResourceVersion {
		return false
	}

	// Check for limit changes
	if !reflect.DeepEqual(oldLR.Spec.Limits, newLR.Spec.Limits) {
		return true
	}

	// Check for label changes
	if !mapsEqual(oldLR.Labels, newLR.Labels) {
		return true
	}

	// Check for annotation changes
	if !mapsEqual(oldLR.Annotations, newLR.Annotations) {
		return true
	}

	// No significant changes detected
	return false
}

// isExcluded checks if a limitrange should be excluded from collection
func (c *LimitRangeCollector) isExcluded(lr *corev1.LimitRange) bool {
	// Check if monitoring specific namespaces and this limitrange isn't in them
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		found := false
		for _, ns := range c.namespaces {
			if ns == lr.Namespace {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}

	// Check if limitrange is specifically excluded
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := types.NamespacedName{
		Namespace: lr.Namespace,
		Name:      lr.Name,
	}
	return c.excludedLimitRanges[key]
}

// Stop gracefully shuts down the limitrange collector
func (c *LimitRangeCollector) Stop() error {
	c.logger.Info("Stopping limitrange collector")
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
func (c *LimitRangeCollector) GetResourceChannel() <-chan CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *LimitRangeCollector) GetType() string {
	return "limitrange"
}

// IsAvailable checks if LimitRange resources can be accessed in the cluster
func (c *LimitRangeCollector) IsAvailable(ctx context.Context) bool {
	return true
}
