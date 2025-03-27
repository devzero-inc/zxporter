// internal/collector/manager.go
package collector

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/client-go/kubernetes"
)

// CollectionConfig contains configuration for collection
type CollectionConfig struct {
	// Namespaces to include (empty means all)
	Namespaces []string

	// ExcludedNamespaces are namespaces to exclude from collection
	ExcludedNamespaces []string

	// ExcludedPods are pods to exclude from collection
	ExcludedPods []ExcludedPod

	// ExcludedDaemonSets are daemonsets to exclude from Collection
	ExcludedDaemonSets []ExcludedDaemonSet

	// ExcludedStatefulSets are statefulsets to exclude from Collection
	ExcludedStatefulSets []ExcludedStatefulSet

	// BufferSize is the size of the combined channel buffer
	BufferSize int
}

// CollectionManager orchestrates multiple collectors
type CollectionManager struct {
	collectors      map[string]ResourceCollector
	combinedChannel chan CollectedResource
	wg              sync.WaitGroup
	mu              sync.RWMutex
	bufferSize      int
	started         bool
	stopped         bool
	client          kubernetes.Interface
	config          *CollectionConfig
	logger          logr.Logger
}

// NewCollectionManager creates a new collection manager
func NewCollectionManager(config *CollectionConfig, client kubernetes.Interface, logger logr.Logger) *CollectionManager {
	// Default buffer size if not specified
	bufferSize := 1000
	if config != nil && config.BufferSize > 0 {
		bufferSize = config.BufferSize
	}

	return &CollectionManager{
		collectors:      make(map[string]ResourceCollector),
		combinedChannel: make(chan CollectedResource, bufferSize),
		bufferSize:      bufferSize,
		client:          client,
		config:          config,
		logger:          logger,
	}
}

// RegisterCollector adds a new collector
func (m *CollectionManager) RegisterCollector(collector ResourceCollector) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	collectorType := collector.GetType()
	if _, exists := m.collectors[collectorType]; exists {
		return fmt.Errorf("collector for type %s already registered", collectorType)
	}

	m.logger.Info("Registering collector", "type", collectorType)
	m.collectors[collectorType] = collector
	return nil
}

// StartAll starts all registered collectors
func (m *CollectionManager) StartAll(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.started {
		return fmt.Errorf("collection manager already started")
	}

	if m.stopped {
		return fmt.Errorf("collection manager was stopped and cannot be restarted")
	}

	m.logger.Info("Starting all collectors", "count", len(m.collectors))

	// Start each collector in its own goroutine
	for collectorType, collector := range m.collectors {
		m.logger.Info("Starting collector", "type", collectorType)

		// Start this collector
		if err := collector.Start(ctx); err != nil {
			m.logger.Error(err, "Failed to start collector", "type", collectorType)
			return fmt.Errorf("failed to start collector %s: %w", collectorType, err)
		}

		// Start a goroutine to read from this collector's channel
		m.wg.Add(1)
		go m.processCollectorChannel(collectorType, collector)
	}

	m.started = true
	return nil
}

// StopAll stops all registered collectors
func (m *CollectionManager) StopAll() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.started {
		return nil // Nothing to stop
	}

	if m.stopped {
		return nil // Already stopped
	}

	m.logger.Info("Stopping all collectors", "count", len(m.collectors))

	// Stop each collector
	for collectorType, collector := range m.collectors {
		m.logger.Info("Stopping collector", "type", collectorType)
		if err := collector.Stop(); err != nil {
			m.logger.Error(err, "Error stopping collector", "type", collectorType)
			// Continue stopping others even if one fails
		}
	}

	// Close the combined channel
	close(m.combinedChannel)

	// Wait for all goroutines to finish (with timeout)
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		m.logger.Info("All collector goroutines finished cleanly")
	case <-time.After(10 * time.Second):
		m.logger.Info("Timeout waiting for collector goroutines to finish")
	}

	m.stopped = true
	return nil
}

// processCollectorChannel reads from a collector's channel and forwards to the combined channel
func (m *CollectionManager) processCollectorChannel(collectorType string, collector ResourceCollector) {
	defer m.wg.Done()

	m.logger.Info("Starting to process collector channel", "type", collectorType)
	resourceChan := collector.GetResourceChannel()

	for resource := range resourceChan {
		// Skip if channel is closed
		if resource.ResourceType == Unknown {
			continue
		}

		// Apply any filtering here if needed
		if m.shouldCollectResource(resource) {
			// Forward to the combined channel
			select {
			case m.combinedChannel <- resource:
				// Successfully sent
			default:
				// Channel full, log warning
				m.logger.Info("Combined channel buffer full, dropping resource",
					"type", resource.ResourceType,
					"key", resource.Key)
			}
		}
	}

	m.logger.Info("Collector channel closed, stopping processor", "type", collectorType)
}

// shouldCollectResource checks if a resource should be collected based on configuration
func (m *CollectionManager) shouldCollectResource(resource CollectedResource) bool {
	// Implement filtering logic based on configuration
	return true
}

// GetCombinedChannel returns the combined channel for all collectors
func (m *CollectionManager) GetCombinedChannel() <-chan CollectedResource {
	return m.combinedChannel
}
