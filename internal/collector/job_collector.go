// internal/collector/job_collector.go
package collector

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// JobCollector watches for job events and collects job data
type JobCollector struct {
	client          kubernetes.Interface
	informerFactory informers.SharedInformerFactory
	jobInformer     cache.SharedIndexInformer
	batchChan       chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan    chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher         *ResourcesBatcher
	stopCh          chan struct{}
	namespaces      []string
	excludedJobs    map[types.NamespacedName]bool
	logger          logr.Logger
	mu              sync.RWMutex
	cDHelper        ChangeDetectionHelper
}

// NewJobCollector creates a new collector for job resources
func NewJobCollector(
	client kubernetes.Interface,
	namespaces []string,
	excludedJobs []ExcludedJob,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
) *JobCollector {
	// Convert excluded jobs to a map for quicker lookups
	excludedJobsMap := make(map[types.NamespacedName]bool)
	for _, job := range excludedJobs {
		excludedJobsMap[types.NamespacedName{
			Namespace: job.Namespace,
			Name:      job.Name,
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

	newLogger := logger.WithName("job-collector")
	return &JobCollector{
		client:       client,
		batchChan:    batchChan,
		resourceChan: resourceChan,
		batcher:      batcher,
		stopCh:       make(chan struct{}),
		namespaces:   namespaces,
		excludedJobs: excludedJobsMap,
		logger:       newLogger,
		cDHelper:     ChangeDetectionHelper{logger: newLogger},
	}
}

// Start begins the job collection process
func (c *JobCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting job collector", "namespaces", c.namespaces)

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

	// Create job informer
	c.jobInformer = c.informerFactory.Batch().V1().Jobs().Informer()

	// Add event handlers
	_, err := c.jobInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			job := obj.(*batchv1.Job)
			c.handleJobEvent(job, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldJob := oldObj.(*batchv1.Job)
			newJob := newObj.(*batchv1.Job)

			// Only handle meaningful updates
			if c.jobChanged(oldJob, newJob) {
				c.handleJobEvent(newJob, EventTypeUpdate)
			}
		},
		DeleteFunc: func(obj interface{}) {
			job := obj.(*batchv1.Job)
			c.handleJobEvent(job, EventTypeDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.jobInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Start the batcher after the cache is synced
	c.logger.Info("Starting resources batcher for Jobs")
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

// handleJobEvent processes job events
func (c *JobCollector) handleJobEvent(job *batchv1.Job, eventType EventType) {
	if c.isExcluded(job) {
		return
	}

	c.logger.Info("Processing job event",
		"namespace", job.Namespace,
		"name", job.Name,
		"eventType", eventType.String())

	// Send the raw job object to the batch channel
	c.batchChan <- CollectedResource{
		ResourceType: Job,
		Object:       job, // Send the entire job object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", job.Namespace, job.Name),
	}
}

// jobChanged detects meaningful changes in a job
func (c *JobCollector) jobChanged(oldJob, newJob *batchv1.Job) bool {
	changed := c.cDHelper.objectMetaChanged(
		c.GetType(),
		oldJob.Name,
		oldJob.ObjectMeta,
		newJob.ObjectMeta,
	)
	if changed != IgnoreChanges {
		return changed == PushChanges
	}

	// Check for status changes
	if oldJob.Status.Active != newJob.Status.Active ||
		oldJob.Status.Succeeded != newJob.Status.Succeeded ||
		oldJob.Status.Failed != newJob.Status.Failed {
		return true
	}

	// Check for completion time changes
	if (oldJob.Status.CompletionTime == nil && newJob.Status.CompletionTime != nil) ||
		(oldJob.Status.CompletionTime != nil && newJob.Status.CompletionTime == nil) ||
		(oldJob.Status.CompletionTime != nil && newJob.Status.CompletionTime != nil &&
			!oldJob.Status.CompletionTime.Equal(newJob.Status.CompletionTime)) {
		return true
	}

	// Check for start time changes
	if (oldJob.Status.StartTime == nil && newJob.Status.StartTime != nil) ||
		(oldJob.Status.StartTime != nil && newJob.Status.StartTime == nil) ||
		(oldJob.Status.StartTime != nil && newJob.Status.StartTime != nil &&
			!oldJob.Status.StartTime.Equal(newJob.Status.StartTime)) {
		return true
	}

	// Check for condition changes
	if len(oldJob.Status.Conditions) != len(newJob.Status.Conditions) {
		return true
	}

	// Deep check on conditions
	oldConditions := make(map[string]batchv1.JobCondition)
	for _, condition := range oldJob.Status.Conditions {
		oldConditions[string(condition.Type)] = condition
	}

	for _, newCondition := range newJob.Status.Conditions {
		oldCondition, exists := oldConditions[string(newCondition.Type)]
		if !exists || oldCondition.Status != newCondition.Status ||
			oldCondition.Reason != newCondition.Reason ||
			oldCondition.Message != newCondition.Message {
			return true
		}
	}

	// Check for parallelism or completions changes
	if (oldJob.Spec.Parallelism == nil && newJob.Spec.Parallelism != nil) ||
		(oldJob.Spec.Parallelism != nil && newJob.Spec.Parallelism == nil) ||
		(oldJob.Spec.Parallelism != nil && newJob.Spec.Parallelism != nil &&
			*oldJob.Spec.Parallelism != *newJob.Spec.Parallelism) {
		return true
	}

	if (oldJob.Spec.Completions == nil && newJob.Spec.Completions != nil) ||
		(oldJob.Spec.Completions != nil && newJob.Spec.Completions == nil) ||
		(oldJob.Spec.Completions != nil && newJob.Spec.Completions != nil &&
			*oldJob.Spec.Completions != *newJob.Spec.Completions) {
		return true
	}

	// Check for owner reference changes (could indicate adoption by a CronJob)
	if len(oldJob.OwnerReferences) != len(newJob.OwnerReferences) {
		return true
	}

	// No significant changes detected
	return false
}

// isExcluded checks if a job should be excluded from collection
func (c *JobCollector) isExcluded(job *batchv1.Job) bool {
	// Check if monitoring specific namespaces and this job isn't in them
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		found := false
		for _, ns := range c.namespaces {
			if ns == job.Namespace {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}

	// Check if job is specifically excluded
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := types.NamespacedName{
		Namespace: job.Namespace,
		Name:      job.Name,
	}
	return c.excludedJobs[key]
}

// Stop gracefully shuts down the job collector
func (c *JobCollector) Stop() error {
	c.logger.Info("Stopping job collector")

	// 1. Signal the informer factory to stop by closing stopCh.
	select {
	case <-c.stopCh:
		c.logger.Info("Job collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed job collector stop channel")
	}

	// 2. Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed job collector batch input channel")
	}

	// 3. Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("Job collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *JobCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *JobCollector) GetType() string {
	return "job"
}

// IsAvailable checks if Job resources can be accessed in the cluster
func (c *JobCollector) IsAvailable(ctx context.Context) bool {
	return true
}
