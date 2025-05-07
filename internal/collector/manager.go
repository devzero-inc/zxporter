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

// Default buffer size if not specified
var bufferSize int = 1000

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
	collectorCtxs   map[string]context.CancelFunc // Track context cancellation functions for each collector
	processorWg     map[string]*sync.WaitGroup    // Track processor goroutines for each collector
	combinedChannel chan []CollectedResource
	wg              sync.WaitGroup
	mu              sync.RWMutex
	bufferSize      int
	started         bool
	client          kubernetes.Interface
	config          *CollectionConfig
	logger          logr.Logger
}

// NewCollectionManager creates a new collection manager
func NewCollectionManager(config *CollectionConfig, client kubernetes.Interface, logger logr.Logger) *CollectionManager {
	if config != nil && config.BufferSize > 0 {
		bufferSize = config.BufferSize
	}

	return &CollectionManager{
		collectors:      make(map[string]ResourceCollector),
		collectorCtxs:   make(map[string]context.CancelFunc),
		processorWg:     make(map[string]*sync.WaitGroup),
		combinedChannel: make(chan []CollectedResource, bufferSize),
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

	// Initialize wait group for this collector type if it doesn't exist
	if _, exists := m.processorWg[collectorType]; !exists {
		m.processorWg[collectorType] = &sync.WaitGroup{}
	}

	return nil
}

// DeregisterCollector stops and removes a specific collector
func (m *CollectionManager) DeregisterCollector(collectorType string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	err := m.stopCollectorInternal(collectorType)
	if err != nil {
		m.logger.Error(err, "Error stopping collector during deregistration", "type", collectorType)
	}

	delete(m.processorWg, collectorType)

	m.logger.Info("Successfully deregistered collector", "type", collectorType)
	return nil
}

// stopCollectorInternal stops a specific collector and cleans up resources. Should be called with the mutex held.
func (m *CollectionManager) stopCollectorInternal(collectorType string) error {
	collector, exists := m.collectors[collectorType]
	if !exists {
		return fmt.Errorf("collector for type %s not registered", collectorType)
	}

	cancel, ctxExists := m.collectorCtxs[collectorType]
	if !ctxExists {
		return fmt.Errorf("collector %s is not running", collectorType)
	}

	m.logger.Info("Stopping collector", "type", collectorType)

	// Cancel the context for this collector
	cancel()
	delete(m.collectorCtxs, collectorType)

	// Stop the collector
	if err := collector.Stop(); err != nil {
		m.logger.Error(err, "Error stopping collector", "type", collectorType)
	}

	// Wait for the processor goroutine to finish
	wg, wgExists := m.processorWg[collectorType]
	if wgExists && wg != nil {
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()

		select {
		case <-done:
			m.logger.Info("Processor goroutine finished cleanly", "type", collectorType)
		case <-time.After(5 * time.Second):
			m.logger.Info("Timeout waiting for processor goroutine to finish", "type", collectorType)
		}
	}

	// Remove the collector from the map
	delete(m.collectors, collectorType)

	return nil
}

// StopCollector stops a specific collector
func (m *CollectionManager) StopCollector(collectorType string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopCollectorInternal(collectorType)

}

// StartAll starts all registered collectors
func (m *CollectionManager) StartAll(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.started {
		return fmt.Errorf("collection manager already started")
	}

	m.logger.Info("Starting all collectors", "count", len(m.collectors))

	// Start each collector in its own goroutine
	for collectorType, collector := range m.collectors {
		if err := m.startCollectorInternal(ctx, collectorType, collector); err != nil {
			m.logger.Error(err, "Failed to start collector", "type", collectorType)
			return fmt.Errorf("failed to start collector %s: %w", collectorType, err)
		}
	}

	m.started = true
	return nil
}

// StartCollector starts a specific collector
func (m *CollectionManager) StartCollector(ctx context.Context, collectorType string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	collector, exists := m.collectors[collectorType]
	if !exists {
		return fmt.Errorf("collector for type %s not registered", collectorType)
	}

	if _, ctxExists := m.collectorCtxs[collectorType]; ctxExists {
		return fmt.Errorf("collector %s is already running", collectorType)
	}

	return m.startCollectorInternal(ctx, collectorType, collector)
}

// startCollectorInternal is a helper function to start a collector with appropriate context management
func (m *CollectionManager) startCollectorInternal(ctx context.Context, collectorType string, collector ResourceCollector) error {
	m.logger.Info("Starting collector", "type", collectorType)

	// Create a new context for this collector that can be cancelled individually
	collectorCtx, cancel := context.WithCancel(ctx)
	m.collectorCtxs[collectorType] = cancel

	// Make sure the processor wait group exists for this collector
	if _, exists := m.processorWg[collectorType]; !exists {
		m.processorWg[collectorType] = &sync.WaitGroup{}
	}

	// Start this collector
	if err := collector.Start(collectorCtx); err != nil {
		cancel() // Clean up the context
		delete(m.collectorCtxs, collectorType)
		return fmt.Errorf("failed to start collector %s: %w", collectorType, err)
	}

	// Start a goroutine to read from this collector's channel
	m.wg.Add(1)
	wg := m.processorWg[collectorType]
	wg.Add(1)
	go m.processCollectorChannel(collectorType, collector, wg)

	return nil
}

// StopAll stops all registered collectors
func (m *CollectionManager) StopAll() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.started {
		return nil // Nothing to stop
	}

	m.logger.Info("Stopping all collectors", "count", len(m.collectors))

	// Cancel the contexts for all collectors
	for collectorType, cancel := range m.collectorCtxs {
		m.logger.Info("Cancelling context for collector", "type", collectorType)
		cancel()
	}

	// Stop each collector
	for collectorType, collector := range m.collectors {
		m.logger.Info("Stopping collector", "type", collectorType)
		if err := collector.Stop(); err != nil {
			m.logger.Error(err, "Error stopping collector", "type", collectorType)
			// Continue stopping others even if one fails
		}
	}

	// Wait for all processor goroutines to finish for each collector
	for collectorType, wg := range m.processorWg {
		m.logger.Info("Waiting for processor to finish", "type", collectorType)
		done := make(chan struct{})
		go func(wg *sync.WaitGroup) {
			wg.Wait()
			close(done)
		}(wg)

		select {
		case <-done:
			m.logger.Info("Processor goroutine finished cleanly", "type", collectorType)
		case <-time.After(5 * time.Second):
			m.logger.Info("Timeout waiting for processor goroutine to finish", "type", collectorType)
		}
	}

	// Close the combined channel
	originalBufferSize := cap(m.combinedChannel)
	close(m.combinedChannel)
	m.combinedChannel = make(chan []CollectedResource, originalBufferSize)

	// Clear all tracking maps
	m.collectorCtxs = make(map[string]context.CancelFunc)
	m.processorWg = make(map[string]*sync.WaitGroup)

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

	m.started = false
	return nil
}

// processCollectorChannel reads from a collector's channel and forwards to the combined channel
func (m *CollectionManager) processCollectorChannel(collectorType string, collector ResourceCollector, wg *sync.WaitGroup) {
	defer m.wg.Done()
	defer wg.Done()

	m.logger.Info("Starting to process collector channel", "type", collectorType)
	resourceChan := collector.GetResourceChannel()

	for resources := range resourceChan {
		// Skip if channel is closed
		if len(resources) == 0 {
			continue
		}

		// Forward to the combined channel
		select {
		case m.combinedChannel <- resources:
			// Successfully sent
		default:
			// Channel full, log warning
			m.logger.Info("Combined channel buffer full, dropping resource",
				"type", resources[0].ResourceType)
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
func (m *CollectionManager) GetCombinedChannel() <-chan []CollectedResource {
	return m.combinedChannel
}

// GetCollectorTypes returns a list of all registered collector types
func (m *CollectionManager) GetCollectorTypes() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	types := make([]string, 0, len(m.collectors))
	for collectorType := range m.collectors {
		types = append(types, collectorType)
	}
	return types
}

// IsCollectorRunning checks if a specific collector is currently running
func (m *CollectionManager) IsCollectorRunning(collectorType string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	_, exists := m.collectorCtxs[collectorType]
	return exists
}
