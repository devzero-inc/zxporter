// internal/collector/karpenter_settings_collector.go
package collector

import (
	"context"
	"fmt"
	"sync"
	"time"

	telemetry_logger "github.com/devzero-inc/zxporter/internal/logger"
	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
)

var karpenterSettingsGVR = schema.GroupVersionResource{
	Group:    "devzero.karpenter.sh",
	Version:  "v1alpha1",
	Resource: "karpentersettings",
}

// KarpenterSettingsCollector watches the DevZero KarpenterSettings cluster-scoped
// singleton (name "default") and emits it as RESOURCE_TYPE_KARPENTER_SETTINGS.
// dakr extracts status.bootstrap from the payload and writes it to
// karpenter_bootstrap_snapshots to populate helm_defaults in the UI.
type KarpenterSettingsCollector struct {
	dynamicClient   dynamic.Interface
	batchChan       chan CollectedResource
	resourceChan    chan []CollectedResource
	batcher         *ResourcesBatcher
	stopCh          chan struct{}
	logger          logr.Logger
	telemetryLogger telemetry_logger.Logger
	mu              sync.Mutex
}

// NewKarpenterSettingsCollector constructs the collector.
func NewKarpenterSettingsCollector(
	dynamicClient dynamic.Interface,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
) *KarpenterSettingsCollector {
	batchChan := make(chan CollectedResource, 10)
	resourceChan := make(chan []CollectedResource, 10)
	batcher := NewResourcesBatcher(maxBatchSize, maxBatchTime, batchChan, resourceChan, logger)

	return &KarpenterSettingsCollector{
		dynamicClient:   dynamicClient,
		batchChan:       batchChan,
		resourceChan:    resourceChan,
		batcher:         batcher,
		stopCh:          make(chan struct{}),
		logger:          logger.WithName("karpenter-settings-collector"),
		telemetryLogger: telemetryLogger,
	}
}

// Start begins watching the KarpenterSettings CRD.
func (c *KarpenterSettingsCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting KarpenterSettings collector")

	// If the CRD is not installed, skip silently — clusters without the DevZero
	// Karpenter fork simply don't have this resource.
	if _, err := c.dynamicClient.Resource(karpenterSettingsGVR).List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
		c.logger.V(4).Info("KarpenterSettings CRD not available, skipping", "error", err)
		return nil
	}

	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		c.dynamicClient, 0, "", nil,
	)
	informer := factory.ForResource(karpenterSettingsGVR).Informer()

	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if u, ok := obj.(*unstructured.Unstructured); ok {
				c.emit(u, EventTypeAdd)
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if u, ok := newObj.(*unstructured.Unstructured); ok {
				c.emit(u, EventTypeUpdate)
			}
		},
		DeleteFunc: func(obj interface{}) {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				if tombstone, ok2 := obj.(cache.DeletedFinalStateUnknown); ok2 {
					u, ok = tombstone.Obj.(*unstructured.Unstructured)
				}
			}
			if ok {
				c.emit(u, EventTypeDelete)
			}
		},
	})
	if err != nil {
		return fmt.Errorf("add event handler: %w", err)
	}

	stopCh := c.stopCh
	go informer.Run(stopCh)

	syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if !cache.WaitForCacheSync(syncCtx.Done(), informer.HasSynced) {
		return fmt.Errorf("timeout waiting for KarpenterSettings cache to sync")
	}

	c.logger.Info("KarpenterSettings informer started and synced")
	c.batcher.start()

	go func() {
		select {
		case <-ctx.Done():
			c.Stop() //nolint:errcheck
		case <-stopCh:
		}
	}()

	return nil
}

// emit sends a KarpenterSettings object to the batch channel.
// Only the "default" singleton is relevant; other names are silently ignored.
func (c *KarpenterSettingsCollector) emit(obj *unstructured.Unstructured, eventType EventType) {
	if obj.GetName() != "default" {
		return
	}
	key := fmt.Sprintf("karpentersettings/%s", obj.GetName())
	c.batchChan <- CollectedResource{
		ResourceType: KarpenterSettings,
		Object:       obj.Object,
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          key,
	}
}

// Stop shuts down the collector.
func (c *KarpenterSettingsCollector) Stop() error {
	c.logger.Info("Stopping KarpenterSettings collector")
	c.mu.Lock()
	defer c.mu.Unlock()

	select {
	case <-c.stopCh:
	default:
		close(c.stopCh)
	}
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
	}
	if c.batcher != nil {
		c.batcher.stop()
	}
	return nil
}

// GetResourceChannel returns the output channel for batched resources.
func (c *KarpenterSettingsCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the string key used to identify this collector.
func (c *KarpenterSettingsCollector) GetType() string {
	return KarpenterSettings.String()
}

// IsAvailable returns true when the KarpenterSettings CRD exists in the cluster.
func (c *KarpenterSettingsCollector) IsAvailable(ctx context.Context) bool {
	_, err := c.dynamicClient.Resource(karpenterSettingsGVR).List(ctx, metav1.ListOptions{Limit: 1})
	return err == nil
}

// AddResource implements ResourceCollector for manual injection (used in tests).
func (c *KarpenterSettingsCollector) AddResource(resource interface{}) error {
	obj, ok := resource.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("expected *unstructured.Unstructured, got %T", resource)
	}
	c.emit(obj, EventTypeAdd)
	return nil
}
