package collector

import (
	"context"
	"fmt"
	"reflect"
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

// WorkloadRuleCollector watches WorkloadRule CRD status changes and sends OOM events
// back to the control plane via SendResourceBatch.
type WorkloadRuleCollector struct {
	client          dynamic.Interface
	informerFactory dynamicinformer.DynamicSharedInformerFactory
	wrInformer      cache.SharedIndexInformer
	batchChan       chan CollectedResource
	resourceChan    chan []CollectedResource
	batcher         *ResourcesBatcher
	stopCh          chan struct{}
	stopOnce        sync.Once
	mu              sync.RWMutex
	stopped         bool
	namespaces      []string
	logger          logr.Logger
	telemetryLogger telemetry_logger.Logger
}

// WorkloadRule CRD resource identifier
var workloadRuleGVR = schema.GroupVersionResource{
	Group:    "dakr.devzero.io",
	Version:  "v1alpha1",
	Resource: "workloadrules",
}

// NewWorkloadRuleCollector creates a new collector for WorkloadRule CRDs
func NewWorkloadRuleCollector(
	client dynamic.Interface,
	namespaces []string,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
) *WorkloadRuleCollector {
	batchChan := make(chan CollectedResource, 100)
	resourceChan := make(chan []CollectedResource, 100)

	batcher := NewResourcesBatcher(
		maxBatchSize,
		maxBatchTime,
		batchChan,
		resourceChan,
		logger,
	)

	return &WorkloadRuleCollector{
		client:          client,
		batchChan:       batchChan,
		resourceChan:    resourceChan,
		batcher:         batcher,
		stopCh:          make(chan struct{}),
		namespaces:      namespaces,
		logger:          logger.WithName("workload-rule-collector"),
		telemetryLogger: telemetryLogger,
	}
}

// Start begins the WorkloadRule collection process
func (c *WorkloadRuleCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting WorkloadRule collector", "namespaces", c.namespaces)

	// 60s resync to catch missed status updates (OOM events appended to status)
	resyncPeriod := 60 * time.Second
	if len(c.namespaces) == 1 && c.namespaces[0] != "" {
		c.informerFactory = dynamicinformer.NewFilteredDynamicSharedInformerFactory(
			c.client,
			resyncPeriod,
			c.namespaces[0],
			nil,
		)
	} else {
		c.informerFactory = dynamicinformer.NewDynamicSharedInformerFactory(c.client, resyncPeriod)
	}

	c.wrInformer = c.informerFactory.ForResource(workloadRuleGVR).Informer()

	_, err := c.wrInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldWR := oldObj.(*unstructured.Unstructured)
			newWR := newObj.(*unstructured.Unstructured)

			// On resync (same ResourceVersion), still send if OOM events exist
			if oldWR.GetResourceVersion() == newWR.GetResourceVersion() {
				if c.hasOOMEvents(newWR) {
					c.handleWorkloadRuleEvent(newWR, EventTypeUpdate)
				}
				return
			}

			// Only send when OOM events have changed
			if c.oomEventsChanged(oldWR, newWR) {
				c.handleWorkloadRuleEvent(newWR, EventTypeUpdate)
			}
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	c.informerFactory.Start(c.stopCh)

	c.logger.Info("Waiting for WorkloadRule informer caches to sync")
	if !cache.WaitForCacheSync(ctx.Done(), c.wrInformer.HasSynced) {
		return fmt.Errorf("failed to sync WorkloadRule caches")
	}
	c.logger.Info("WorkloadRule informer caches synced successfully")

	c.logger.Info("Starting resources batcher for WorkloadRules")
	c.batcher.start()

	stopCh := c.stopCh
	go func() {
		select {
		case <-ctx.Done():
			_ = c.Stop()
		case <-stopCh:
			// Channel was closed by Stop() method
		}
	}()

	return nil
}

// handleWorkloadRuleEvent processes a WorkloadRule status update containing OOM events
func (c *WorkloadRuleCollector) handleWorkloadRuleEvent(wr *unstructured.Unstructured, eventType EventType) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.stopped {
		return
	}

	namespace := wr.GetNamespace()
	name := wr.GetName()

	c.logger.Info("Sending WorkloadRule status with OOM events to control plane",
		"namespace", namespace,
		"name", name,
	)
	c.batchChan <- CollectedResource{
		ResourceType: WorkloadRule,
		Object:       wr,
		Timestamp:    time.Now().UTC(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", namespace, name),
	}
}

// hasOOMEvents checks if a WorkloadRule has any OOM events in its status
func (c *WorkloadRuleCollector) hasOOMEvents(wr *unstructured.Unstructured) bool {
	events, found, err := unstructured.NestedSlice(wr.Object, "status", "oomEvents")
	if err != nil || !found {
		return false
	}
	return len(events) > 0
}

// oomEventsChanged detects if OOM events have changed between old and new versions
func (c *WorkloadRuleCollector) oomEventsChanged(oldWR, newWR *unstructured.Unstructured) bool {
	oldEvents, _, _ := unstructured.NestedSlice(oldWR.Object, "status", "oomEvents")
	newEvents, _, _ := unstructured.NestedSlice(newWR.Object, "status", "oomEvents")
	return !reflect.DeepEqual(oldEvents, newEvents)
}

// Stop gracefully shuts down the WorkloadRule collector
func (c *WorkloadRuleCollector) Stop() error {
	c.logger.Info("Stopping WorkloadRule collector")

	c.stopOnce.Do(func() {
		close(c.stopCh)
		c.logger.Info("Closed WorkloadRule collector stop channel")

		// Lock to ensure no in-flight event handlers are sending on batchChan
		c.mu.Lock()
		c.stopped = true
		close(c.batchChan)
		c.batchChan = nil
		c.mu.Unlock()
		c.logger.Info("Closed WorkloadRule collector batch input channel")
	})

	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("WorkloadRule collector batcher stopped")
	}

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *WorkloadRuleCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *WorkloadRuleCollector) GetType() string {
	return "workload_rule"
}

// IsAvailable checks if the WorkloadRule CRD is available in the cluster
func (c *WorkloadRuleCollector) IsAvailable(ctx context.Context) bool {
	_, err := c.client.Resource(workloadRuleGVR).List(ctx, metav1.ListOptions{Limit: 1})
	if err == nil {
		return true
	}

	if isResourceTypeUnavailableError(err) {
		c.logger.Info("WorkloadRule CRD not installed in cluster", "error", err.Error())
		return false
	}

	c.logger.Error(err, "Error checking WorkloadRule resource availability")
	return false
}

// AddResource manually adds a WorkloadRule resource to be processed
func (c *WorkloadRuleCollector) AddResource(resource interface{}) error {
	wr, ok := resource.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("expected *unstructured.Unstructured, got %T", resource)
	}
	c.handleWorkloadRuleEvent(wr, EventTypeAdd)
	return nil
}
