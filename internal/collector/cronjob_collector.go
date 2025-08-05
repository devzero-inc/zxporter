// internal/collector/cronjob_collector.go
package collector

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// CronJobCollector watches for cronjob events and collects cronjob data
type CronJobCollector struct {
	client           kubernetes.Interface
	informerFactory  informers.SharedInformerFactory
	cronJobInformer  cache.SharedIndexInformer
	batchChan        chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan     chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher          *ResourcesBatcher
	stopCh           chan struct{}
	namespaces       []string
	excludedCronJobs map[types.NamespacedName]bool
	logger           logr.Logger
	mu               sync.RWMutex
	cDHelper         ChangeDetectionHelper
}

// NewCronJobCollector creates a new collector for cronjob resources
func NewCronJobCollector(
	client kubernetes.Interface,
	namespaces []string,
	excludedCronJobs []ExcludedCronJob,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
) *CronJobCollector {
	// Convert excluded cronjobs to a map for quicker lookups
	excludedCronJobsMap := make(map[types.NamespacedName]bool)
	for _, cronJob := range excludedCronJobs {
		excludedCronJobsMap[types.NamespacedName{
			Namespace: cronJob.Namespace,
			Name:      cronJob.Name,
		}] = true
	}

	// Create channels
	batchChan := make(chan CollectedResource, 50) // Keep lower buffer for less frequent CronJobs
	resourceChan := make(chan []CollectedResource, 50)

	// Create the batcher
	batcher := NewResourcesBatcher(
		maxBatchSize,
		maxBatchTime,
		batchChan,
		resourceChan,
		logger,
	)

	newLogger := logger.WithName("cronjob-collector")
	return &CronJobCollector{
		client:           client,
		batchChan:        batchChan,
		resourceChan:     resourceChan,
		batcher:          batcher,
		stopCh:           make(chan struct{}),
		namespaces:       namespaces,
		excludedCronJobs: excludedCronJobsMap,
		logger:           newLogger,
		cDHelper:         ChangeDetectionHelper{logger: newLogger},
	}
}

// Start begins the cronjob collection process
func (c *CronJobCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting cronjob collector", "namespaces", c.namespaces)

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

	// Create cronjob informer
	c.cronJobInformer = c.informerFactory.Batch().V1().CronJobs().Informer()

	// Add event handlers
	_, err := c.cronJobInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			cronJob := obj.(*batchv1.CronJob)
			c.handleCronJobEvent(cronJob, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldCronJob := oldObj.(*batchv1.CronJob)
			newCronJob := newObj.(*batchv1.CronJob)

			// Only handle meaningful updates
			if c.cronJobChanged(oldCronJob, newCronJob) {
				c.handleCronJobEvent(newCronJob, EventTypeUpdate)
			}
		},
		DeleteFunc: func(obj interface{}) {
			cronJob := obj.(*batchv1.CronJob)
			c.handleCronJobEvent(cronJob, EventTypeDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.cronJobInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Start the batcher after the cache is synced
	c.logger.Info("Starting resources batcher for CronJobs")
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

// handleCronJobEvent processes cronjob events
func (c *CronJobCollector) handleCronJobEvent(cronJob *batchv1.CronJob, eventType EventType) {
	if c.isExcluded(cronJob) {
		return
	}

	c.logger.Info("Processing cronjob event",
		"namespace", cronJob.Namespace,
		"name", cronJob.Name,
		"eventType", eventType.String(),
		"schedule", cronJob.Spec.Schedule)

	// Send the raw cronjob object to the batch channel
	c.batchChan <- CollectedResource{
		ResourceType: CronJob,
		Object:       cronJob, // Send the entire cronjob object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", cronJob.Namespace, cronJob.Name),
	}
}

// cronJobChanged detects meaningful changes in a cronjob
func (c *CronJobCollector) cronJobChanged(oldCronJob, newCronJob *batchv1.CronJob) bool {
	changed := c.cDHelper.objectMetaChanged(
		c.GetType(),
		oldCronJob.Name,
		oldCronJob.ObjectMeta,
		newCronJob.ObjectMeta,
	)
	if changed != IgnoreChanges {
		return changed == PushChanges
	}

	// Check for schedule changes
	if oldCronJob.Spec.Schedule != newCronJob.Spec.Schedule {
		return true
	}

	// Check for last successful execution time changes
	if !timePointerEqual(oldCronJob.Status.LastSuccessfulTime, newCronJob.Status.LastSuccessfulTime) {
		return true
	}

	// Check for last schedule time changes
	if !timePointerEqual(oldCronJob.Status.LastScheduleTime, newCronJob.Status.LastScheduleTime) {
		return true
	}

	// Check for suspend changes
	if getBoolValue(oldCronJob.Spec.Suspend) != getBoolValue(newCronJob.Spec.Suspend) {
		return true
	}

	// Check for concurrency policy changes
	if oldCronJob.Spec.ConcurrencyPolicy != newCronJob.Spec.ConcurrencyPolicy {
		return true
	}

	// Check for starting deadline seconds changes
	if !int64PointerEqual(oldCronJob.Spec.StartingDeadlineSeconds, newCronJob.Spec.StartingDeadlineSeconds) {
		return true
	}

	// Check for history limit changes
	if !int32PointerEqual(oldCronJob.Spec.SuccessfulJobsHistoryLimit, newCronJob.Spec.SuccessfulJobsHistoryLimit) ||
		!int32PointerEqual(oldCronJob.Spec.FailedJobsHistoryLimit, newCronJob.Spec.FailedJobsHistoryLimit) {
		return true
	}

	// Check for active jobs changes
	if len(oldCronJob.Status.Active) != len(newCronJob.Status.Active) {
		return true
	}

	// No significant changes detected
	return false
}

// isExcluded checks if a cronjob should be excluded from collection
func (c *CronJobCollector) isExcluded(cronJob *batchv1.CronJob) bool {
	// Check if monitoring specific namespaces and this cronjob isn't in them
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		found := false
		for _, ns := range c.namespaces {
			if ns == cronJob.Namespace {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}

	// Check if cronjob is specifically excluded
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := types.NamespacedName{
		Namespace: cronJob.Namespace,
		Name:      cronJob.Name,
	}
	return c.excludedCronJobs[key]
}

// Stop gracefully shuts down the cronjob collector
func (c *CronJobCollector) Stop() error {
	c.logger.Info("Stopping cronjob collector")

	// 1. Signal the informer factory to stop by closing stopCh.
	select {
	case <-c.stopCh:
		c.logger.Info("CronJob collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed cronjob collector stop channel")
	}

	// 2. Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed cronjob collector batch input channel")
	}

	// 3. Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("CronJob collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *CronJobCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *CronJobCollector) GetType() string {
	return "cron_job"
}

// IsAvailable checks if CronJob resources can be accessed in the cluster
func (c *CronJobCollector) IsAvailable(ctx context.Context) bool {
	// Try to list CronJobs with limit=1 to check availability with minimal overhead
	_, err := c.client.BatchV1().CronJobs("").List(ctx, metav1.ListOptions{
		Limit: 1,
	})

	if err != nil {
		// Check if this is a "resource not found" type error
		if strings.Contains(err.Error(), "the server could not find the requested resource") {
			c.logger.Info("CronJob v1 API not available in cluster")
			return false
		}

		// For other errors (permissions, etc), log the error
		c.logger.Error(err, "Error checking CronJob availability")
		return false
	}

	return true
}

// AddResource manually adds a cronjob resource to be processed by the collector
func (c *CronJobCollector) AddResource(resource interface{}) error {
	cronJob, ok := resource.(*batchv1.CronJob)
	if !ok {
		return fmt.Errorf("expected *batchv1.CronJob, got %T", resource)
	}

	c.handleCronJobEvent(cronJob, EventTypeAdd)
	return nil
}
