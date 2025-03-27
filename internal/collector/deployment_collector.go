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
	resourceChan        chan CollectedResource
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

	return &DeploymentCollector{
		client:              client,
		resourceChan:        make(chan CollectedResource, 100),
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
			c.handleDeploymentEvent(deployment, "add")
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldDeployment := oldObj.(*appsv1.Deployment)
			newDeployment := newObj.(*appsv1.Deployment)

			// Only handle meaningful updates
			if c.deploymentChanged(oldDeployment, newDeployment) {
				c.handleDeploymentEvent(newDeployment, "update")
			}
		},
		DeleteFunc: func(obj interface{}) {
			deployment := obj.(*appsv1.Deployment)
			c.handleDeploymentEvent(deployment, "delete")
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

	// Keep this goroutine alive until context cancellation or stop
	go func() {
		<-ctx.Done()
		close(c.stopCh)
	}()

	return nil
}

// handleDeploymentEvent processes deployment events
func (c *DeploymentCollector) handleDeploymentEvent(deployment *appsv1.Deployment, eventType string) {
	if c.isExcluded(deployment) {
		return
	}

	c.logger.Info("Processing deployment event",
		"namespace", deployment.Namespace,
		"name", deployment.Name,
		"eventType", eventType)

	// Send the raw deployment object directly to the resource channel
	c.resourceChan <- CollectedResource{
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
	close(c.stopCh)
	return nil
}

// GetResourceChannel returns the channel for collected resources
func (c *DeploymentCollector) GetResourceChannel() <-chan CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *DeploymentCollector) GetType() string {
	return "deployment"
}
