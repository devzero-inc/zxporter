// internal/collector/service_collector.go
package collector

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// ServiceCollector watches for service events and collects service data
type ServiceCollector struct {
	client           kubernetes.Interface
	informerFactory  informers.SharedInformerFactory
	serviceInformer  cache.SharedIndexInformer
	batchChan        chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan     chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher          *ResourcesBatcher
	stopCh           chan struct{}
	namespaces       []string
	excludedServices map[types.NamespacedName]bool
	logger           logr.Logger
	mu               sync.RWMutex
	cDHelper         ChangeDetectionHelper
}

// NewServiceCollector creates a new collector for service resources
func NewServiceCollector(
	client kubernetes.Interface,
	namespaces []string,
	excludedServices []ExcludedService,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
) *ServiceCollector {
	// Convert excluded services to a map for quicker lookups
	excludedServicesMap := make(map[types.NamespacedName]bool)
	for _, service := range excludedServices {
		excludedServicesMap[types.NamespacedName{
			Namespace: service.Namespace,
			Name:      service.Name,
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

	newLogger := logger.WithName("service-collector")
	return &ServiceCollector{
		client:           client,
		batchChan:        batchChan,
		resourceChan:     resourceChan,
		batcher:          batcher,
		stopCh:           make(chan struct{}),
		namespaces:       namespaces,
		excludedServices: excludedServicesMap,
		logger:           newLogger,
		cDHelper:         ChangeDetectionHelper{logger: newLogger}}
}

// Start begins the service collection process
func (c *ServiceCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting service collector", "namespaces", c.namespaces)

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

	// Create service informer
	c.serviceInformer = c.informerFactory.Core().V1().Services().Informer()

	// Add event handlers
	_, err := c.serviceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			service := obj.(*corev1.Service)
			c.handleServiceEvent(service, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldService := oldObj.(*corev1.Service)
			newService := newObj.(*corev1.Service)

			// Only handle meaningful updates
			if c.serviceChanged(oldService, newService) {
				c.handleServiceEvent(newService, EventTypeUpdate)
			}
		},
		DeleteFunc: func(obj interface{}) {
			service := obj.(*corev1.Service)
			c.handleServiceEvent(service, EventTypeDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.serviceInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Start the batcher after the cache is synced
	c.logger.Info("Starting resources batcher for services")
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

// handleServiceEvent processes service events
func (c *ServiceCollector) handleServiceEvent(service *corev1.Service, eventType EventType) {
	if c.isExcluded(service) {
		return
	}

	c.logger.Info("Processing service event",
		"namespace", service.Namespace,
		"name", service.Name,
		"eventType", eventType.String())

	// Send the raw service object to the batch channel
	c.batchChan <- CollectedResource{
		ResourceType: Service,
		Object:       service, // Send the entire service object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", service.Namespace, service.Name),
	}
}

// serviceChanged detects meaningful changes in a service
func (c *ServiceCollector) serviceChanged(oldService, newService *corev1.Service) bool {
	changed := c.cDHelper.objectMetaChanged(
		c.GetType(),
		oldService.Name,
		oldService.ObjectMeta,
		newService.ObjectMeta,
	)
	if changed != IgnoreChanges {
		return changed == PushChanges
	}

	// Check for spec changes
	// Type change
	if oldService.Spec.Type != newService.Spec.Type {
		return true
	}

	// ClusterIP change
	if oldService.Spec.ClusterIP != newService.Spec.ClusterIP {
		return true
	}

	// Ports changes
	if len(oldService.Spec.Ports) != len(newService.Spec.Ports) {
		return true
	}

	// Check each port
	for i, oldPort := range oldService.Spec.Ports {
		newPort := newService.Spec.Ports[i]
		if oldPort.Port != newPort.Port ||
			oldPort.TargetPort != newPort.TargetPort ||
			oldPort.NodePort != newPort.NodePort ||
			oldPort.Protocol != newPort.Protocol {
			return true
		}
	}

	// Check selector changes
	if !mapsEqual(oldService.Spec.Selector, newService.Spec.Selector) {
		return true
	}

	// External IPs
	if !stringSlicesEqual(oldService.Spec.ExternalIPs, newService.Spec.ExternalIPs) {
		return true
	}

	// LoadBalancer status changes
	if len(oldService.Status.LoadBalancer.Ingress) != len(newService.Status.LoadBalancer.Ingress) {
		return true
	}

	for i, oldIngress := range oldService.Status.LoadBalancer.Ingress {
		newIngress := newService.Status.LoadBalancer.Ingress[i]
		if oldIngress.IP != newIngress.IP || oldIngress.Hostname != newIngress.Hostname {
			return true
		}
	}

	// No significant changes detected
	return false
}

// isExcluded checks if a service should be excluded from collection
func (c *ServiceCollector) isExcluded(service *corev1.Service) bool {
	// Check if monitoring specific namespaces and this service isn't in them
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		found := false
		for _, ns := range c.namespaces {
			if ns == service.Namespace {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}

	// Check if service is specifically excluded
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := types.NamespacedName{
		Namespace: service.Namespace,
		Name:      service.Name,
	}
	return c.excludedServices[key]
}

// Stop gracefully shuts down the service collector
func (c *ServiceCollector) Stop() error {
	c.logger.Info("Stopping service collector")

	// 1. Signal the informer factory to stop by closing stopCh.
	select {
	case <-c.stopCh:
		c.logger.Info("Service collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed service collector stop channel")
	}

	// 2. Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed service collector batch input channel")
	}

	// 3. Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("Service collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *ServiceCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *ServiceCollector) GetType() string {
	return "service"
}

// IsAvailable checks if Service resources can be accessed in the cluster
func (c *ServiceCollector) IsAvailable(ctx context.Context) bool {
	return true
}

// AddResource manually adds a service resource to be processed by the collector
func (c *ServiceCollector) AddResource(resource interface{}) error {
	service, ok := resource.(*corev1.Service)
	if !ok {
		return fmt.Errorf("expected *corev1.Service, got %T", resource)
	}

	c.handleServiceEvent(service, EventTypeAdd)
	return nil
}
