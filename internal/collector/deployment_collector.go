// internal/collector/deployment_collector.go
package collector

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// DeploymentCollector watches for deployment events and collects deployment data
type DeploymentCollector struct {
	client              kubernetes.Interface
	informerFactory     informers.SharedInformerFactory
	deploymentInformer  cache.SharedIndexInformer
	batchChan           chan CollectedResource
	resourceChan        chan []CollectedResource
	batcher             *ResourcesBatcher
	stopCh              chan struct{}
	namespaces          []string
	excludedDeployments map[types.NamespacedName]bool
	logger              logr.Logger
	mu                  sync.RWMutex
}

// NewDeploymentCollector creates a new collector for deployment resources
func NewDeploymentCollector(
	client kubernetes.Interface,
	namespaces []string,
	excludedDeployments []ExcludedDeployment,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
) *DeploymentCollector {
	// Convert excluded deployments to a map for quicker lookups
	excludedDeploymentsMap := make(map[types.NamespacedName]bool)
	for _, deployment := range excludedDeployments {
		excludedDeploymentsMap[types.NamespacedName{
			Namespace: deployment.Namespace,
			Name:      deployment.Name,
		}] = true
	}

	// Create channels
	batchChan := make(chan CollectedResource, 100)
	resourceChan := make(chan []CollectedResource, 100) // Batch channel buffer size

	// Create the batcher
	batcher := NewResourcesBatcher(
		maxBatchSize,
		maxBatchTime,
		batchChan,
		resourceChan,
		logger,
	)

	return &DeploymentCollector{
		client:              client,
		batchChan:           batchChan,
		resourceChan:        resourceChan,
		batcher:             batcher,
		stopCh:              make(chan struct{}),
		namespaces:          namespaces,
		excludedDeployments: excludedDeploymentsMap,
		logger:              logger.WithName("deployment-collector"),
	}
}

// Start begins the deployment collection process
func (c *DeploymentCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting deployment collector", "namespaces", c.namespaces)

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

	// Create deployment informer
	c.deploymentInformer = c.informerFactory.Apps().V1().Deployments().Informer()

	// Add event handlers
	_, err := c.deploymentInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			deployment := obj.(*appsv1.Deployment)
			c.handleDeploymentEvent(deployment, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldDeployment := oldObj.(*appsv1.Deployment)
			newDeployment := newObj.(*appsv1.Deployment)

			// Only handle meaningful updates
			if c.deploymentChanged(oldDeployment, newDeployment) {
				c.handleDeploymentEvent(newDeployment, EventTypeUpdate)
			}
		},
		DeleteFunc: func(obj interface{}) {
			deployment := obj.(*appsv1.Deployment)
			c.handleDeploymentEvent(deployment, EventTypeDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.deploymentInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Start the batcher after the cache is synced
	c.logger.Info("Starting resources batcher for deployments")
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

// handleDeploymentEvent processes deployment events
func (c *DeploymentCollector) handleDeploymentEvent(deployment *appsv1.Deployment, eventType EventType) {
	if c.isExcluded(deployment) {
		return
	}

	c.logger.Info("Processing deployment event",
		"namespace", deployment.Namespace,
		"name", deployment.Name,
		"eventType", eventType.String())

	// Send the raw deployment object to the batch channel
	c.batchChan <- CollectedResource{
		ResourceType: Deployment,
		Object:       deployment, // Send the entire deployment object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", deployment.Namespace, deployment.Name),
	}
}

// deploymentChanged detects meaningful changes in a deployment
func (c *DeploymentCollector) deploymentChanged(oldDeployment, newDeployment *appsv1.Deployment) bool {
	// Ignore changes to ResourceVersion, which always changes even for irrelevant updates
	if oldDeployment.ResourceVersion == newDeployment.ResourceVersion {
		return false
	}

	// Check for spec changes
	if oldDeployment.Spec.Replicas == nil || newDeployment.Spec.Replicas == nil {
		return true
	}

	if *oldDeployment.Spec.Replicas != *newDeployment.Spec.Replicas {
		return true
	}

	// Check for status changes
	if oldDeployment.Status.Replicas != newDeployment.Status.Replicas ||
		oldDeployment.Status.AvailableReplicas != newDeployment.Status.AvailableReplicas ||
		oldDeployment.Status.ReadyReplicas != newDeployment.Status.ReadyReplicas ||
		oldDeployment.Status.UpdatedReplicas != newDeployment.Status.UpdatedReplicas ||
		oldDeployment.Status.UnavailableReplicas != newDeployment.Status.UnavailableReplicas {
		return true
	}

	// Check for generation changes
	if oldDeployment.Generation != newDeployment.Generation ||
		oldDeployment.Status.ObservedGeneration != newDeployment.Status.ObservedGeneration {
		return true
	}

	// Check for changes in conditions
	if len(oldDeployment.Status.Conditions) != len(newDeployment.Status.Conditions) {
		return true
	}

	// Deep check on conditions
	oldConditions := make(map[string]appsv1.DeploymentCondition)
	for _, condition := range oldDeployment.Status.Conditions {
		oldConditions[string(condition.Type)] = condition
	}

	for _, newCondition := range newDeployment.Status.Conditions {
		oldCondition, exists := oldConditions[string(newCondition.Type)]
		if !exists || oldCondition.Status != newCondition.Status ||
			oldCondition.Reason != newCondition.Reason ||
			oldCondition.Message != newCondition.Message {
			return true
		}
	}

	// No significant changes detected
	return false
}

// isExcluded checks if a deployment should be excluded from collection
func (c *DeploymentCollector) isExcluded(deployment *appsv1.Deployment) bool {
	// Check if monitoring specific namespaces and this deployment isn't in them
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		found := false
		for _, ns := range c.namespaces {
			if ns == deployment.Namespace {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}

	// Check if deployment is specifically excluded
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := types.NamespacedName{
		Namespace: deployment.Namespace,
		Name:      deployment.Name,
	}
	return c.excludedDeployments[key]
}

// Stop gracefully shuts down the deployment collector
func (c *DeploymentCollector) Stop() error {
	c.logger.Info("Stopping deployment collector")

	// 1. Signal the informer factory to stop by closing stopCh.
	select {
	case <-c.stopCh:
		c.logger.Info("Deployment collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed deployment collector stop channel")
	}

	// 2. Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed deployment collector batch input channel")
	}

	// 3. Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("Deployment collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *DeploymentCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *DeploymentCollector) GetType() string {
	return "deployment"
}

// IsAvailable checks if Deployment resources can be accessed in the cluster
func (c *DeploymentCollector) IsAvailable(ctx context.Context) bool {
	return true
}
