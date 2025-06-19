// internal/collector/scaledjob_collector.go
package collector

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/go-logr/logr"
	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	kedaclient "github.com/kedacore/keda/v2/pkg/generated/clientset/versioned"
	kedainformers "github.com/kedacore/keda/v2/pkg/generated/informers/externalversions"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
)

// ScaledJobCollector watches for ScaledJob events and collects ScaledJob data
type ScaledJobCollector struct {
	client             kedaclient.Interface
	informerFactory    kedainformers.SharedInformerFactory
	scaledJobInformer  cache.SharedIndexInformer
	batchChan          chan CollectedResource
	resourceChan       chan []CollectedResource
	batcher            *ResourcesBatcher
	stopCh             chan struct{}
	namespaces         []string
	excludedScaledJobs map[types.NamespacedName]bool
	logger             logr.Logger
	mu                 sync.RWMutex
	cDHelper           ChangeDetectionHelper
}

// ExcludedScaledJob represents a ScaledJob to exclude from collection
type ExcludedScaledJob struct {
	Namespace string `yaml:"namespace"`
	Name      string `yaml:"name"`
}

// NewScaledJobCollector creates a new collector for ScaledJob resources
func NewScaledJobCollector(
	client kedaclient.Interface,
	namespaces []string,
	excludedScaledJobs []ExcludedScaledJob,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
) *ScaledJobCollector {
	// Convert excluded ScaledJobs to a map for quicker lookups
	excludedScaledJobsMap := make(map[types.NamespacedName]bool)
	for _, scaledJob := range excludedScaledJobs {
		excludedScaledJobsMap[types.NamespacedName{
			Namespace: scaledJob.Namespace,
			Name:      scaledJob.Name,
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

	newLogger := logger.WithName("keda-scaledjob-collector")
	return &ScaledJobCollector{
		client:             client,
		batchChan:          batchChan,
		resourceChan:       resourceChan,
		batcher:            batcher,
		stopCh:             make(chan struct{}),
		namespaces:         namespaces,
		excludedScaledJobs: excludedScaledJobsMap,
		logger:             newLogger,
		cDHelper:           ChangeDetectionHelper{logger: newLogger},
	}
}

// Start begins the ScaledJob collection process
func (c *ScaledJobCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting ScaledJob collector", "namespaces", c.namespaces)

	// Create informer factory based on namespace configuration
	if len(c.namespaces) == 1 && c.namespaces[0] != "" {
		// Watch a specific namespace
		c.informerFactory = kedainformers.NewSharedInformerFactoryWithOptions(
			c.client,
			0, // No resync period, rely on events
			kedainformers.WithNamespace(c.namespaces[0]),
		)
	} else {
		// Watch all namespaces
		c.informerFactory = kedainformers.NewSharedInformerFactory(c.client, 0)
	}

	// Create ScaledJob informer
	c.scaledJobInformer = c.informerFactory.Keda().V1alpha1().ScaledJobs().Informer()

	// Add event handlers
	_, err := c.scaledJobInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			scaledJob := obj.(*kedav1alpha1.ScaledJob)
			c.handleScaledJobEvent(scaledJob, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldScaledJob := oldObj.(*kedav1alpha1.ScaledJob)
			newScaledJob := newObj.(*kedav1alpha1.ScaledJob)

			// Only handle meaningful updates
			if c.scaledJobChanged(oldScaledJob, newScaledJob) {
				c.handleScaledJobEvent(newScaledJob, EventTypeUpdate)
			}
		},
		DeleteFunc: func(obj interface{}) {
			scaledJob := obj.(*kedav1alpha1.ScaledJob)
			c.handleScaledJobEvent(scaledJob, EventTypeDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.scaledJobInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Start the batcher after the cache is synced
	c.logger.Info("Starting resources batcher for ScaledJobs")
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

// handleScaledJobEvent processes ScaledJob events
func (c *ScaledJobCollector) handleScaledJobEvent(scaledJob *kedav1alpha1.ScaledJob, eventType EventType) {
	if c.isExcluded(scaledJob) {
		return
	}

	c.logger.Info("Processing ScaledJob event",
		"namespace", scaledJob.Namespace,
		"name", scaledJob.Name,
		"eventType", eventType.String(),
		"jobTargetRef", scaledJob.Spec.JobTargetRef)

	// Send the raw ScaledJob object to the batch channel
	c.batchChan <- CollectedResource{
		ResourceType: KedaScaledJob,
		Object:       scaledJob,
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", scaledJob.Namespace, scaledJob.Name),
	}
}

// scaledJobChanged detects meaningful changes in a ScaledJob
func (c *ScaledJobCollector) scaledJobChanged(oldScaledJob, newScaledJob *kedav1alpha1.ScaledJob) bool {
	changed := c.cDHelper.objectMetaChanged(
		c.GetType(),
		oldScaledJob.Name,
		oldScaledJob.ObjectMeta,
		newScaledJob.ObjectMeta,
	)
	if changed != IgnoreChanges {
		return changed == PushChanges
	}

	// Check for spec changes (most important for KEDA jobs)
	if !reflect.DeepEqual(oldScaledJob.Spec, newScaledJob.Spec) {
		c.logger.V(1).Info("ScaledJob spec changed",
			"namespace", newScaledJob.Namespace,
			"name", newScaledJob.Name)
		return true
	}

	// Check for status changes that matter for job execution
	if !c.scaledJobStatusEqual(&oldScaledJob.Status, &newScaledJob.Status) {
		c.logger.V(1).Info("ScaledJob status changed",
			"namespace", newScaledJob.Namespace,
			"name", newScaledJob.Name)
		return true
	}

	// Check for generation changes
	if oldScaledJob.Generation != newScaledJob.Generation {
		return true
	}

	// No significant changes detected
	return false
}

// scaledJobStatusEqual compares ScaledJob status for meaningful changes
func (c *ScaledJobCollector) scaledJobStatusEqual(oldStatus, newStatus *kedav1alpha1.ScaledJobStatus) bool {
	// Compare last active time changes (important for job scheduling)
	if (oldStatus.LastActiveTime == nil) != (newStatus.LastActiveTime == nil) {
		return false
	}
	if oldStatus.LastActiveTime != nil && newStatus.LastActiveTime != nil &&
		!oldStatus.LastActiveTime.Equal(newStatus.LastActiveTime) {
		return false
	}

	// Compare conditions length first for quick check
	if len(oldStatus.Conditions) != len(newStatus.Conditions) {
		return false
	}

	// Deep check on conditions - important for KEDA job health monitoring
	oldConditions := make(map[string]kedav1alpha1.Condition)
	for _, condition := range oldStatus.Conditions {
		oldConditions[string(condition.Type)] = condition
	}

	for _, newCondition := range newStatus.Conditions {
		oldCondition, exists := oldConditions[string(newCondition.Type)]
		if !exists ||
			oldCondition.Status != newCondition.Status ||
			oldCondition.Reason != newCondition.Reason ||
			oldCondition.Message != newCondition.Message {
			return false
		}
	}

	return true
}

// isExcluded checks if a ScaledJob should be excluded from collection
func (c *ScaledJobCollector) isExcluded(scaledJob *kedav1alpha1.ScaledJob) bool {
	// Check if monitoring specific namespaces and this ScaledJob isn't in them
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		found := false
		for _, ns := range c.namespaces {
			if ns == scaledJob.Namespace {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}

	// Check if ScaledJob is specifically excluded
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := types.NamespacedName{
		Namespace: scaledJob.Namespace,
		Name:      scaledJob.Name,
	}
	return c.excludedScaledJobs[key]
}

// Stop gracefully shuts down the ScaledJob collector
func (c *ScaledJobCollector) Stop() error {
	c.logger.Info("Stopping ScaledJob collector")

	// 1. Signal the informer factory to stop by closing stopCh.
	select {
	case <-c.stopCh:
		c.logger.Info("ScaledJob collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed ScaledJob collector stop channel")
	}

	// 2. Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed ScaledJob collector batch input channel")
	}

	// 3. Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("ScaledJob collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *ScaledJobCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *ScaledJobCollector) GetType() string {
	return "scaledjob"
}

// IsAvailable checks if ScaledJob resources can be accessed in the cluster
func (c *ScaledJobCollector) IsAvailable(ctx context.Context) bool {
	// Check if KEDA CRDs are installed by attempting to list ScaledJobs
	_, err := c.client.KedaV1alpha1().ScaledJobs("").List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil {
		c.logger.V(1).Info("ScaledJobs not available in cluster", "error", err.Error())
		return false
	}
	return true
}
