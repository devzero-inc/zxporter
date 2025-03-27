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
	resourceChan     chan CollectedResource
	stopCh           chan struct{}
	namespaces       []string
	excludedServices map[types.NamespacedName]bool
	logger           logr.Logger
	mu               sync.RWMutex
}

// ExcludedService defines a service to exclude from collection
type ExcludedService struct {
	Namespace string
	Name      string
}

// NewServiceCollector creates a new collector for service resources
func NewServiceCollector(
	client kubernetes.Interface,
	namespaces []string,
	excludedServices []ExcludedService,
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

	return &ServiceCollector{
		client:           client,
		resourceChan:     make(chan CollectedResource, 100),
		stopCh:           make(chan struct{}),
		namespaces:       namespaces,
		excludedServices: excludedServicesMap,
		logger:           logger.WithName("service-collector"),
	}
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
			c.handleServiceEvent(service, "add")
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldService := oldObj.(*corev1.Service)
			newService := newObj.(*corev1.Service)

			// Only handle meaningful updates
			if c.serviceChanged(oldService, newService) {
				c.handleServiceEvent(newService, "update")
			}
		},
		DeleteFunc: func(obj interface{}) {
			service := obj.(*corev1.Service)
			c.handleServiceEvent(service, "delete")
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

	// Keep this goroutine alive until context cancellation or stop
	go func() {
		<-ctx.Done()
		close(c.stopCh)
	}()

	return nil
}

// handleServiceEvent processes service events
func (c *ServiceCollector) handleServiceEvent(service *corev1.Service, eventType string) {
	if c.isExcluded(service) {
		return
	}

	c.logger.Info("Processing service event",
		"namespace", service.Namespace,
		"name", service.Name,
		"eventType", eventType)

	// Send the raw service object directly to the resource channel
	c.resourceChan <- CollectedResource{
		ResourceType: Service,
		Object:       service, // Send the entire service object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", service.Namespace, service.Name),
	}
}

// serviceChanged detects meaningful changes in a service
func (c *ServiceCollector) serviceChanged(oldService, newService *corev1.Service) bool {
	// Ignore changes to ResourceVersion, which always changes even for irrelevant updates
	if oldService.ResourceVersion == newService.ResourceVersion {
		return false
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

// mapsEqual compares two maps for equality
func mapsEqual(m1, m2 map[string]string) bool {
	if len(m1) != len(m2) {
		return false
	}

	for k, v1 := range m1 {
		if v2, ok := m2[k]; !ok || v1 != v2 {
			return false
		}
	}

	return true
}

// stringSlicesEqual compares two string slices for equality
func stringSlicesEqual(s1, s2 []string) bool {
	if len(s1) != len(s2) {
		return false
	}

	for i := range s1 {
		if s1[i] != s2[i] {
			return false
		}
	}

	return true
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
	close(c.stopCh)
	return nil
}

// GetResourceChannel returns the channel for collected resources
func (c *ServiceCollector) GetResourceChannel() <-chan CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *ServiceCollector) GetType() string {
	return "service"
}
