// internal/collector/ingress_collector.go
package collector

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// IngressCollector watches for ingress events and collects ingress data
type IngressCollector struct {
	client            kubernetes.Interface
	informerFactory   informers.SharedInformerFactory
	ingressInformer   cache.SharedIndexInformer
	resourceChan      chan CollectedResource
	stopCh            chan struct{}
	namespaces        []string
	excludedIngresses map[types.NamespacedName]bool
	logger            logr.Logger
	mu                sync.RWMutex
}

// ExcludedIngress defines an ingress to exclude from collection
type ExcludedIngress struct {
	Namespace string
	Name      string
}

// NewIngressCollector creates a new collector for ingress resources
func NewIngressCollector(
	client kubernetes.Interface,
	namespaces []string,
	excludedIngresses []ExcludedIngress,
	logger logr.Logger,
) *IngressCollector {
	// Convert excluded ingresses to a map for quicker lookups
	excludedIngressesMap := make(map[types.NamespacedName]bool)
	for _, ingress := range excludedIngresses {
		excludedIngressesMap[types.NamespacedName{
			Namespace: ingress.Namespace,
			Name:      ingress.Name,
		}] = true
	}

	return &IngressCollector{
		client:            client,
		resourceChan:      make(chan CollectedResource, 100),
		stopCh:            make(chan struct{}),
		namespaces:        namespaces,
		excludedIngresses: excludedIngressesMap,
		logger:            logger.WithName("ingress-collector"),
	}
}

// Start begins the ingress collection process
func (c *IngressCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting ingress collector", "namespaces", c.namespaces)

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

	// Create ingress informer
	c.ingressInformer = c.informerFactory.Networking().V1().Ingresses().Informer()

	// Add event handlers
	_, err := c.ingressInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ingress := obj.(*networkingv1.Ingress)
			c.handleIngressEvent(ingress, "add")
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldIngress := oldObj.(*networkingv1.Ingress)
			newIngress := newObj.(*networkingv1.Ingress)

			// Only handle meaningful updates
			if c.ingressChanged(oldIngress, newIngress) {
				c.handleIngressEvent(newIngress, "update")
			}
		},
		DeleteFunc: func(obj interface{}) {
			ingress := obj.(*networkingv1.Ingress)
			c.handleIngressEvent(ingress, "delete")
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.ingressInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

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

// handleIngressEvent processes ingress events
func (c *IngressCollector) handleIngressEvent(ingress *networkingv1.Ingress, eventType string) {
	if c.isExcluded(ingress) {
		return
	}

	c.logger.V(4).Info("Processing ingress event",
		"namespace", ingress.Namespace,
		"name", ingress.Name,
		"eventType", eventType)

	// Send the raw ingress object directly to the resource channel
	c.resourceChan <- CollectedResource{
		ResourceType: Ingress,
		Object:       ingress, // Send the entire ingress object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", ingress.Namespace, ingress.Name),
	}
}

// ingressChanged detects meaningful changes in an ingress
func (c *IngressCollector) ingressChanged(oldIngress, newIngress *networkingv1.Ingress) bool {
	// Ignore changes to ResourceVersion, which always changes even for irrelevant updates
	if oldIngress.ResourceVersion == newIngress.ResourceVersion {
		return false
	}

	// Check for ingress class changes
	if oldIngress.Spec.IngressClassName != newIngress.Spec.IngressClassName {
		return true
	}

	// Check for rule changes (hosts, paths, backends)
	if !ingressRulesEqual(oldIngress.Spec.Rules, newIngress.Spec.Rules) {
		return true
	}

	// Check for TLS config changes
	if !ingressTLSEqual(oldIngress.Spec.TLS, newIngress.Spec.TLS) {
		return true
	}

	// Check for default backend changes
	if (oldIngress.Spec.DefaultBackend == nil) != (newIngress.Spec.DefaultBackend == nil) {
		return true
	}

	if oldIngress.Spec.DefaultBackend != nil && newIngress.Spec.DefaultBackend != nil {
		if !ingressBackendEqual(oldIngress.Spec.DefaultBackend, newIngress.Spec.DefaultBackend) {
			return true
		}
	}

	// Check for annotation changes
	if !mapsEqual(oldIngress.Annotations, newIngress.Annotations) {
		return true
	}

	// Check for load balancer status changes
	if len(oldIngress.Status.LoadBalancer.Ingress) != len(newIngress.Status.LoadBalancer.Ingress) {
		return true
	}

	for i, oldLB := range oldIngress.Status.LoadBalancer.Ingress {
		newLB := newIngress.Status.LoadBalancer.Ingress[i]
		if oldLB.IP != newLB.IP || oldLB.Hostname != newLB.Hostname {
			return true
		}
	}

	// No significant changes detected
	return false
}

// ingressRulesEqual compares two slices of ingress rules for equality
func ingressRulesEqual(rules1, rules2 []networkingv1.IngressRule) bool {
	if len(rules1) != len(rules2) {
		return false
	}

	// Create a string representation of each rule for comparison
	rules1Map := make(map[string]bool)
	rules2Map := make(map[string]bool)

	for _, rule := range rules1 {
		key := rule.Host
		if rule.HTTP != nil {
			for _, path := range rule.HTTP.Paths {
				pathKey := fmt.Sprintf("%s:%s:%s", key, path.Path, string(*path.PathType))
				if path.Backend.Service != nil {
					pathKey += fmt.Sprintf(":%s:%s", path.Backend.Service.Name, path.Backend.Service.Port.String())
				}
				rules1Map[pathKey] = true
			}
		} else {
			rules1Map[key] = true
		}
	}

	for _, rule := range rules2 {
		key := rule.Host
		if rule.HTTP != nil {
			for _, path := range rule.HTTP.Paths {
				pathKey := fmt.Sprintf("%s:%s:%s", key, path.Path, string(*path.PathType))
				if path.Backend.Service != nil {
					pathKey += fmt.Sprintf(":%s:%s", path.Backend.Service.Name, path.Backend.Service.Port.String())
				}
				rules2Map[pathKey] = true
			}
		} else {
			rules2Map[key] = true
		}
	}

	// Compare maps
	if len(rules1Map) != len(rules2Map) {
		return false
	}

	for key := range rules1Map {
		if !rules2Map[key] {
			return false
		}
	}

	return true
}

// ingressTLSEqual compares two slices of ingress TLS configurations for equality
func ingressTLSEqual(tls1, tls2 []networkingv1.IngressTLS) bool {
	if len(tls1) != len(tls2) {
		return false
	}

	// Create a map of TLS entries by secretName
	tls1Map := make(map[string][]string)
	tls2Map := make(map[string][]string)

	for _, tls := range tls1 {
		tls1Map[tls.SecretName] = tls.Hosts
	}

	for _, tls := range tls2 {
		tls2Map[tls.SecretName] = tls.Hosts
	}

	// Compare maps
	if len(tls1Map) != len(tls2Map) {
		return false
	}

	for secretName, hosts1 := range tls1Map {
		hosts2, exists := tls2Map[secretName]
		if !exists {
			return false
		}

		// Compare hosts
		if len(hosts1) != len(hosts2) {
			return false
		}

		hostsMap1 := make(map[string]bool)
		for _, host := range hosts1 {
			hostsMap1[host] = true
		}

		for _, host := range hosts2 {
			if !hostsMap1[host] {
				return false
			}
		}
	}

	return true
}

// ingressBackendEqual compares two ingress backends for equality
func ingressBackendEqual(backend1, backend2 *networkingv1.IngressBackend) bool {
	// Check if one is nil but not the other
	if (backend1 == nil) != (backend2 == nil) {
		return false
	}

	// Both are nil
	if backend1 == nil && backend2 == nil {
		return true
	}

	// Check service backends
	if (backend1.Service == nil) != (backend2.Service == nil) {
		return false
	}

	if backend1.Service != nil && backend2.Service != nil {
		if backend1.Service.Name != backend2.Service.Name {
			return false
		}

		// Compare ports
		if !ingressServiceBackendPortEqual(&backend1.Service.Port, &backend2.Service.Port) {
			return false
		}
	}

	// Check resource backends
	if (backend1.Resource == nil) != (backend2.Resource == nil) {
		return false
	}

	if backend1.Resource != nil && backend2.Resource != nil {
		if backend1.Resource.APIGroup != backend2.Resource.APIGroup ||
			backend1.Resource.Kind != backend2.Resource.Kind ||
			backend1.Resource.Name != backend2.Resource.Name {
			return false
		}
	}

	return true
}

// ingressServiceBackendPortEqual compares two service backend ports for equality
func ingressServiceBackendPortEqual(port1, port2 *networkingv1.ServiceBackendPort) bool {
	if port1 == nil && port2 == nil {
		return true
	}

	if port1 == nil || port2 == nil {
		return false
	}

	// If Name is set
	if port1.Name != "" || port2.Name != "" {
		return port1.Name == port2.Name
	}

	// If Number is set
	return port1.Number == port2.Number
}

// isExcluded checks if an ingress should be excluded from collection
func (c *IngressCollector) isExcluded(ingress *networkingv1.Ingress) bool {
	// Check if monitoring specific namespaces and this ingress isn't in them
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		found := false
		for _, ns := range c.namespaces {
			if ns == ingress.Namespace {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}

	// Check if ingress is specifically excluded
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := types.NamespacedName{
		Namespace: ingress.Namespace,
		Name:      ingress.Name,
	}
	return c.excludedIngresses[key]
}

// Stop gracefully shuts down the ingress collector
func (c *IngressCollector) Stop() error {
	c.logger.Info("Stopping ingress collector")
	if c.stopCh != nil {
		if c.stopCh != nil {
			close(c.stopCh)
			c.stopCh = nil
		}
		c.stopCh = nil
	}
	return nil
}

// GetResourceChannel returns the channel for collected resources
func (c *IngressCollector) GetResourceChannel() <-chan CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *IngressCollector) GetType() string {
	return "ingress"
}

// IsAvailable checks if Ingress resources can be accessed in the cluster
func (c *IngressCollector) IsAvailable(ctx context.Context) bool {
	// Try to list Ingresses with a limit of 1 to check availability with minimal overhead
	_, err := c.client.NetworkingV1().Ingresses("").List(ctx, metav1.ListOptions{
		Limit: 1,
	})

	if err != nil {
		// Check if this is a "resource not found" type error
		if strings.Contains(err.Error(),
			"the server could not find the requested resource") {
			c.logger.Info("Ingress API not available in cluster")
			return false
		}

		// For other errors (permissions, etc), log the error
		c.logger.Error(err, "Error checking Ingress availability")
		return false
	}

	return true
}
