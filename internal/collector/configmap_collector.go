// internal/collector/configmap_collector.go
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

// ConfigMapCollector watches for configmap events and collects configmap data
type ConfigMapCollector struct {
	client             kubernetes.Interface
	informerFactory    informers.SharedInformerFactory
	configMapInformer  cache.SharedIndexInformer
	resourceChan       chan CollectedResource
	stopCh             chan struct{}
	namespaces         []string
	excludedConfigMaps map[types.NamespacedName]bool
	logger             logr.Logger
	mu                 sync.RWMutex
}

// ExcludedConfigMap defines a configmap to exclude from collection
type ExcludedConfigMap struct {
	Namespace string
	Name      string
}

// NewConfigMapCollector creates a new collector for configmap resources
func NewConfigMapCollector(
	client kubernetes.Interface,
	namespaces []string,
	excludedConfigMaps []ExcludedConfigMap,
	logger logr.Logger,
) *ConfigMapCollector {
	// Convert excluded configmaps to a map for quicker lookups
	excludedConfigMapsMap := make(map[types.NamespacedName]bool)
	for _, configmap := range excludedConfigMaps {
		excludedConfigMapsMap[types.NamespacedName{
			Namespace: configmap.Namespace,
			Name:      configmap.Name,
		}] = true
	}

	return &ConfigMapCollector{
		client:             client,
		resourceChan:       make(chan CollectedResource, 100),
		stopCh:             make(chan struct{}),
		namespaces:         namespaces,
		excludedConfigMaps: excludedConfigMapsMap,
		logger:             logger.WithName("configmap-collector"),
	}
}

// Start begins the configmap collection process
func (c *ConfigMapCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting configmap collector", "namespaces", c.namespaces)

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

	// Create configmap informer
	c.configMapInformer = c.informerFactory.Core().V1().ConfigMaps().Informer()

	// Add event handlers
	_, err := c.configMapInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			configmap := obj.(*corev1.ConfigMap)
			c.handleConfigMapEvent(configmap, "add")
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldConfigMap := oldObj.(*corev1.ConfigMap)
			newConfigMap := newObj.(*corev1.ConfigMap)

			// Only handle meaningful updates
			if c.configMapChanged(oldConfigMap, newConfigMap) {
				c.handleConfigMapEvent(newConfigMap, "update")
			}
		},
		DeleteFunc: func(obj interface{}) {
			configmap := obj.(*corev1.ConfigMap)
			c.handleConfigMapEvent(configmap, "delete")
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.configMapInformer.HasSynced) {
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

// handleConfigMapEvent processes configmap events
func (c *ConfigMapCollector) handleConfigMapEvent(configmap *corev1.ConfigMap, eventType string) {
	if c.isExcluded(configmap) {
		return
	}

	c.logger.V(4).Info("Processing configmap event",
		"namespace", configmap.Namespace,
		"name", configmap.Name,
		"eventType", eventType)

	// Send the raw configmap object directly to the resource channel
	c.resourceChan <- CollectedResource{
		ResourceType: ConfigMap,
		Object:       configmap, // Send the entire configmap object as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", configmap.Namespace, configmap.Name),
	}
}

// configMapChanged detects meaningful changes in a configmap
func (c *ConfigMapCollector) configMapChanged(oldConfigMap, newConfigMap *corev1.ConfigMap) bool {
	// Ignore changes to ResourceVersion, which always changes even for irrelevant updates
	if oldConfigMap.ResourceVersion == newConfigMap.ResourceVersion {
		return false
	}

	// Check for data changes
	if !mapsEqual(oldConfigMap.Data, newConfigMap.Data) {
		return true
	}

	// Check for binary data changes
	if !binaryDataMapsEqual(oldConfigMap.BinaryData, newConfigMap.BinaryData) {
		return true
	}

	// Check for label changes
	if !mapsEqual(oldConfigMap.Labels, newConfigMap.Labels) {
		return true
	}

	// Check for annotation changes
	if !mapsEqual(oldConfigMap.Annotations, newConfigMap.Annotations) {
		return true
	}

	// No significant changes detected
	return false
}

// binaryDataMapsEqual compares two binary data maps for equality
func binaryDataMapsEqual(m1, m2 map[string][]byte) bool {
	if len(m1) != len(m2) {
		return false
	}

	for k, v1 := range m1 {
		v2, ok := m2[k]
		if !ok {
			return false
		}

		// Compare byte slices
		if len(v1) != len(v2) {
			return false
		}

		for i, b := range v1 {
			if b != v2[i] {
				return false
			}
		}
	}

	return true
}

// isExcluded checks if a configmap should be excluded from collection
func (c *ConfigMapCollector) isExcluded(configmap *corev1.ConfigMap) bool {
	// Check if monitoring specific namespaces and this configmap isn't in them
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		found := false
		for _, ns := range c.namespaces {
			if ns == configmap.Namespace {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}

	// Check if configmap is specifically excluded
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := types.NamespacedName{
		Namespace: configmap.Namespace,
		Name:      configmap.Name,
	}
	return c.excludedConfigMaps[key]
}

// Stop gracefully shuts down the configmap collector
func (c *ConfigMapCollector) Stop() error {
	c.logger.Info("Stopping configmap collector")
	close(c.stopCh)
	return nil
}

// GetResourceChannel returns the channel for collected resources
func (c *ConfigMapCollector) GetResourceChannel() <-chan CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *ConfigMapCollector) GetType() string {
	return "configmap"
}
