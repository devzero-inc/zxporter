// internal/collector/karpenter_bootstrap_collector.go
package collector

import (
	"context"
	"fmt"
	"time"

	telemetry_logger "github.com/devzero-inc/zxporter/internal/logger"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// KarpenterBootstrapLabelSelector is the label selector used by the
// Karpenter bootstrap collector to watch only ConfigMaps produced by
// Karpenter's startup bootstrap routine.
const KarpenterBootstrapLabelSelector = "devzero.io/karpenter-bootstrap=true"

// KarpenterBootstrapCollector watches ConfigMaps labeled with
// devzero.io/karpenter-bootstrap=true across all namespaces and ships
// them to the DevZero control plane unmodified via the existing
// ConfigMap resource transport.
type KarpenterBootstrapCollector struct {
	client          kubernetes.Interface
	informerFactory informers.SharedInformerFactory
	informer        cache.SharedIndexInformer
	batchChan       chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan    chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher         *ResourcesBatcher
	stopCh          chan struct{}
	logger          logr.Logger
	telemetryLogger telemetry_logger.Logger
}

// NewKarpenterBootstrapCollector creates a new collector for the
// Karpenter bootstrap ConfigMap.
func NewKarpenterBootstrapCollector(
	client kubernetes.Interface,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
) *KarpenterBootstrapCollector {
	// Create channels. Bootstrap ConfigMaps are infrequent so a small buffer suffices.
	batchChan := make(chan CollectedResource, 16)
	resourceChan := make(chan []CollectedResource, 16)

	// Create the batcher
	batcher := NewResourcesBatcher(
		maxBatchSize,
		maxBatchTime,
		batchChan,
		resourceChan,
		logger,
	)

	return &KarpenterBootstrapCollector{
		client:          client,
		batchChan:       batchChan,
		resourceChan:    resourceChan,
		batcher:         batcher,
		stopCh:          make(chan struct{}),
		logger:          logger.WithName("karpenter-bootstrap-collector"),
		telemetryLogger: telemetryLogger,
	}
}

// Start begins watching for Karpenter bootstrap ConfigMaps.
func (c *KarpenterBootstrapCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting Karpenter bootstrap collector")

	// Create informer factory filtered by the bootstrap label selector across all namespaces.
	c.informerFactory = informers.NewSharedInformerFactoryWithOptions(
		c.client,
		0, // No resync period
		informers.WithNamespace(metav1.NamespaceAll),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.LabelSelector = KarpenterBootstrapLabelSelector
		}),
	)

	c.informer = c.informerFactory.Core().V1().ConfigMaps().Informer()

	_, err := c.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			cm, ok := obj.(*corev1.ConfigMap)
			if !ok {
				c.logger.Error(nil, "Failed to convert object to ConfigMap")
				return
			}
			c.handleConfigMapEvent(cm, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			cm, ok := newObj.(*corev1.ConfigMap)
			if !ok {
				c.logger.Error(nil, "Failed to convert updated object to ConfigMap")
				return
			}
			c.handleConfigMapEvent(cm, EventTypeUpdate)
		},
		DeleteFunc: func(obj interface{}) {
			cm, ok := obj.(*corev1.ConfigMap)
			if !ok {
				// Try to handle DeletedFinalStateUnknown
				if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
					if cm, ok = tombstone.Obj.(*corev1.ConfigMap); ok {
						c.handleConfigMapEvent(cm, EventTypeDelete)
						return
					}
				}
				c.logger.Error(nil, "Failed to convert deleted object to ConfigMap")
				return
			}
			c.handleConfigMapEvent(cm, EventTypeDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factory
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for Karpenter bootstrap informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.informer.HasSynced) {
		return fmt.Errorf("timed out waiting for Karpenter bootstrap caches to sync")
	}
	c.logger.Info("Karpenter bootstrap informer caches synced successfully")

	// Start the batcher after the cache is synced
	c.logger.Info("Starting resources batcher for Karpenter bootstrap ConfigMaps")
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

// handleConfigMapEvent emits a CollectedResource carrying the raw ConfigMap.
func (c *KarpenterBootstrapCollector) handleConfigMapEvent(
	cm *corev1.ConfigMap,
	eventType EventType,
) {
	key := fmt.Sprintf("%s/%s", cm.Namespace, cm.Name)

	c.batchChan <- CollectedResource{
		ResourceType: ConfigMap,
		Object:       cm,
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          key,
	}
}

// Stop gracefully shuts down the collector.
func (c *KarpenterBootstrapCollector) Stop() error {
	c.logger.Info("Stopping Karpenter bootstrap collector")

	// 1. Signal the informer factory to stop by closing stopCh.
	select {
	case <-c.stopCh:
		c.logger.Info("Karpenter bootstrap collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed Karpenter bootstrap collector stop channel")
	}

	// 2. Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed Karpenter bootstrap collector batch input channel")
	}

	// 3. Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("Karpenter bootstrap collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches.
func (c *KarpenterBootstrapCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles.
func (c *KarpenterBootstrapCollector) GetType() string {
	return "karpenter_bootstrap"
}

// IsAvailable returns true if labeled Karpenter bootstrap ConfigMaps can be listed
// in the cluster. A successful list (even with zero items) is taken to mean the
// core/v1 ConfigMap API is reachable.
func (c *KarpenterBootstrapCollector) IsAvailable(ctx context.Context) bool {
	_, err := c.client.CoreV1().ConfigMaps(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		LabelSelector: KarpenterBootstrapLabelSelector,
		Limit:         1,
	})
	if err != nil {
		c.logger.Error(err, "Failed to list Karpenter bootstrap ConfigMaps")
		return false
	}
	return true
}

// AddResource manually adds a ConfigMap resource to be processed by the collector.
// The informer-driven flow is the primary path; this exists to satisfy the
// ResourceCollector interface.
func (c *KarpenterBootstrapCollector) AddResource(resource interface{}) error {
	cm, ok := resource.(*corev1.ConfigMap)
	if !ok {
		return fmt.Errorf("expected *corev1.ConfigMap, got %T", resource)
	}
	c.handleConfigMapEvent(cm, EventTypeAdd)
	return nil
}
