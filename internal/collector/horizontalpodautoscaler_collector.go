// internal/collector/horizontalpodautoscaler_collector.go
package collector

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// HorizontalPodAutoscalerCollector watches for HPA events and collects HPA data
type HorizontalPodAutoscalerCollector struct {
	client                          kubernetes.Interface
	informerFactory                 informers.SharedInformerFactory
	horizontalPodAutoscalerInformer cache.SharedIndexInformer
	resourceChan                    chan CollectedResource
	stopCh                          chan struct{}
	namespaces                      []string
	excludedHPAs                    map[types.NamespacedName]bool
	logger                          logr.Logger
	mu                              sync.RWMutex
}

// ExcludedHPA defines a HPA to exclude from collection
type ExcludedHPA struct {
	Namespace string
	Name      string
}

// NewHorizontalPodAutoscalerCollector creates a new collector for HPA resources
func NewHorizontalPodAutoscalerCollector(
	client kubernetes.Interface,
	namespaces []string,
	excludedHPAs []ExcludedHPA,
	logger logr.Logger,
) *HorizontalPodAutoscalerCollector {
	// Convert excluded HPAs to a map for quicker lookups
	excludedHPAsMap := make(map[types.NamespacedName]bool)
	for _, hpa := range excludedHPAs {
		excludedHPAsMap[types.NamespacedName{
			Namespace: hpa.Namespace,
			Name:      hpa.Name,
		}] = true
	}

	return &HorizontalPodAutoscalerCollector{
		client:       client,
		resourceChan: make(chan CollectedResource, 100),
		stopCh:       make(chan struct{}),
		namespaces:   namespaces,
		excludedHPAs: excludedHPAsMap,
		logger:       logger.WithName("hpa-collector"),
	}
}

// Start begins the HPA collection process
func (c *HorizontalPodAutoscalerCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting HorizontalPodAutoscaler collector", "namespaces", c.namespaces)

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

	// Create HPA informer - use v2 version which is more feature-rich
	c.horizontalPodAutoscalerInformer = c.informerFactory.Autoscaling().V2().HorizontalPodAutoscalers().Informer()

	// Add event handlers
	_, err := c.horizontalPodAutoscalerInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			hpa := obj.(*autoscalingv2.HorizontalPodAutoscaler)
			c.handleHPAEvent(hpa, "add")
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldHPA := oldObj.(*autoscalingv2.HorizontalPodAutoscaler)
			newHPA := newObj.(*autoscalingv2.HorizontalPodAutoscaler)

			// Only handle meaningful updates
			if c.hpaChanged(oldHPA, newHPA) {
				c.handleHPAEvent(newHPA, "update")
			}
		},
		DeleteFunc: func(obj interface{}) {
			hpa := obj.(*autoscalingv2.HorizontalPodAutoscaler)
			c.handleHPAEvent(hpa, "delete")
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.horizontalPodAutoscalerInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Keep this goroutine alive until context cancellation or stop
	go func() {
		<-ctx.Done()
		close(c.stopCh)
	}()

	return nil
}

// handleHPAEvent processes HPA events
func (c *HorizontalPodAutoscalerCollector) handleHPAEvent(hpa *autoscalingv2.HorizontalPodAutoscaler, eventType string) {
	if c.isExcluded(hpa) {
		return
	}

	c.logger.V(4).Info("Processing HorizontalPodAutoscaler event",
		"namespace", hpa.Namespace,
		"name", hpa.Name,
		"eventType", eventType)

	// Send the raw HPA object directly to the resource channel
	c.resourceChan <- CollectedResource{
		ResourceType: HorizontalPodAutoscaler,
		Object:       hpa, // Send the entire HPA object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", hpa.Namespace, hpa.Name),
	}
}

// hpaChanged detects meaningful changes in a HPA
func (c *HorizontalPodAutoscalerCollector) hpaChanged(oldHPA, newHPA *autoscalingv2.HorizontalPodAutoscaler) bool {
	// Ignore changes to ResourceVersion, which always changes even for irrelevant updates
	if oldHPA.ResourceVersion == newHPA.ResourceVersion {
		return false
	}

	// Check for min/max replicas changes
	if oldHPA.Spec.MinReplicas != newHPA.Spec.MinReplicas ||
		oldHPA.Spec.MaxReplicas != newHPA.Spec.MaxReplicas {
		return true
	}

	// Check for target reference changes
	if oldHPA.Spec.ScaleTargetRef.Kind != newHPA.Spec.ScaleTargetRef.Kind ||
		oldHPA.Spec.ScaleTargetRef.Name != newHPA.Spec.ScaleTargetRef.Name ||
		oldHPA.Spec.ScaleTargetRef.APIVersion != newHPA.Spec.ScaleTargetRef.APIVersion {
		return true
	}

	// Check for metric specification changes
	if !reflect.DeepEqual(oldHPA.Spec.Metrics, newHPA.Spec.Metrics) {
		return true
	}

	// Check for behavior changes
	if !reflect.DeepEqual(oldHPA.Spec.Behavior, newHPA.Spec.Behavior) {
		return true
	}

	// Check for status changes
	if oldHPA.Status.CurrentReplicas != newHPA.Status.CurrentReplicas ||
		oldHPA.Status.DesiredReplicas != newHPA.Status.DesiredReplicas {
		return true
	}

	// Check for current metric changes
	if !reflect.DeepEqual(oldHPA.Status.CurrentMetrics, newHPA.Status.CurrentMetrics) {
		return true
	}

	// Check for condition changes
	if !reflect.DeepEqual(oldHPA.Status.Conditions, newHPA.Status.Conditions) {
		return true
	}

	// Check for label changes
	if !mapsEqual(oldHPA.Labels, newHPA.Labels) {
		return true
	}

	// Check for annotation changes
	if !mapsEqual(oldHPA.Annotations, newHPA.Annotations) {
		return true
	}

	// No significant changes detected
	return false
}

// isExcluded checks if a HPA should be excluded from collection
func (c *HorizontalPodAutoscalerCollector) isExcluded(hpa *autoscalingv2.HorizontalPodAutoscaler) bool {
	// Check if monitoring specific namespaces and this HPA isn't in them
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		found := false
		for _, ns := range c.namespaces {
			if ns == hpa.Namespace {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}

	// Check if HPA is specifically excluded
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := types.NamespacedName{
		Namespace: hpa.Namespace,
		Name:      hpa.Name,
	}
	return c.excludedHPAs[key]
}

// Stop gracefully shuts down the HPA collector
func (c *HorizontalPodAutoscalerCollector) Stop() error {
	c.logger.Info("Stopping HorizontalPodAutoscaler collector")
	close(c.stopCh)
	return nil
}

// GetResourceChannel returns the channel for collected resources
func (c *HorizontalPodAutoscalerCollector) GetResourceChannel() <-chan CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *HorizontalPodAutoscalerCollector) GetType() string {
	return "horizontalpodautoscaler"
}

// IsAvailable checks if HPA v2 resources can be accessed in the cluster
func (c *HorizontalPodAutoscalerCollector) IsAvailable(ctx context.Context) bool {
	// Try to list HPAs with a limit of 1 to check availability with minimal overhead
	_, err := c.client.AutoscalingV2().HorizontalPodAutoscalers("").List(ctx, metav1.ListOptions{
		Limit: 1,
	})

	if err != nil {
		// Check if this is a "resource not found" type error
		if strings.Contains(err.Error(),
			"the server could not find the requested resource") {
			c.logger.Info("HorizontalPodAutoscaler v2 API not available in cluster")
			return false
		}

		// For other errors (permissions, etc), log the error
		c.logger.Error(err, "Error checking HorizontalPodAutoscaler availability")
		return false
	}

	return true
}
