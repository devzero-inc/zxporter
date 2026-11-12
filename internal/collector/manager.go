// internal/collector/manager.go
package collector

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	"github.com/devzero-inc/zxporter/internal/health"
	telemetry_logger "github.com/devzero-inc/zxporter/internal/logger"
	"github.com/devzero-inc/zxporter/internal/version"
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
	collectors map[string]ResourceCollector
	// collectorView is a copy-on-write snapshot of collectors, republished under mu by
	// every mutation of that map. GetCollector reads it without taking mu, because its
	// callers include informer event handlers (the node lifecycle fallback and the
	// DisruptionBlocked classifier resolve other collectors' caches through it), and a
	// handler that waits on mu stalls that informer's whole listener for as long as mu is
	// held. mu is held for seconds at a time by stopCollectorInternal, and on 2026-09-25 a
	// leaked mu stopped every Node event from reaching the control plane.
	collectorView    atomic.Pointer[map[string]ResourceCollector]
	collectorCtxs    map[string]*collectorRun   // The live run of each running (or starting) collector
	processorWg      map[string]*sync.WaitGroup // Track processor goroutines for each collector
	combinedChannel  chan []CollectedResource
	telemetryMetrics *TelemetryMetrics // Metrics for telemetry
	wg               sync.WaitGroup
	mu               sync.RWMutex
	bufferSize       int
	started          bool
	client           kubernetes.Interface
	config           *CollectionConfig
	logger           logr.Logger
	telemetryLogger  telemetry_logger.Logger
	healthManager    *health.HealthManager
}

// NewCollectionManager creates a new collection manager
func NewCollectionManager(config *CollectionConfig,
	client kubernetes.Interface,
	telemetryMetrics *TelemetryMetrics,
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
	healthManager *health.HealthManager,
) *CollectionManager {
	if config != nil && config.BufferSize > 0 {
		bufferSize = config.BufferSize
	}

	m := &CollectionManager{
		collectors:       make(map[string]ResourceCollector),
		collectorCtxs:    make(map[string]*collectorRun),
		processorWg:      make(map[string]*sync.WaitGroup),
		combinedChannel:  make(chan []CollectedResource, bufferSize),
		telemetryMetrics: telemetryMetrics,
		bufferSize:       bufferSize,
		client:           client,
		config:           config,
		logger:           logger,
		telemetryLogger:  telemetryLogger,
		healthManager:    healthManager,
	}
	m.publishCollectorViewLocked()
	return m
}

// collectorRun is one start of a collector, from reserveStart until it is stopped. It is
// compared by pointer: launchCollector re-checks, once Start returns, that its run is
// still the current entry in collectorCtxs, which is how a stop that landed during Start
// is detected even if a new run for the same type has been reserved since.
type collectorRun struct {
	cancel context.CancelFunc
	// starting is true from reserveStart until launchCollector's handshake. While it is
	// set, the goroutine inside Start owns the collector: a stop cancels the run and
	// unregisters it but leaves the Stop call to that goroutine, because stopping a
	// collector concurrently with its own Start is not safe for the collectors here, and
	// calling Stop from both sides would double-close their channels.
	starting bool
}

// publishCollectorViewLocked republishes the lock-free snapshot GetCollector reads. Must
// be called with mu held, after every write to m.collectors.
func (m *CollectionManager) publishCollectorViewLocked() {
	view := make(map[string]ResourceCollector, len(m.collectors))
	for collectorType, collector := range m.collectors {
		view[collectorType] = collector
	}
	m.collectorView.Store(&view)
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
	m.publishCollectorViewLocked()

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

	run, running := m.collectorCtxs[collectorType]

	// Unpublish before Stop and the drain wait below, so lock-free readers stop being
	// handed a collector that is shutting down.
	delete(m.collectors, collectorType)
	m.publishCollectorViewLocked()

	if !running {
		// Registered but not running: its Start failed, or it was never started. It is
		// still removed, or the replacement a restart registers next is refused as
		// "already registered" and the type stays dark until the pod restarts.
		m.logger.Info("Removed collector that was not running", "type", collectorType)
		return nil
	}

	m.logger.Info("Stopping collector", "type", collectorType)

	// Cancel the context for this collector
	run.cancel()
	delete(m.collectorCtxs, collectorType)

	if run.starting {
		// Its Start is still in flight. Waiting for it here would deadlock (the starter
		// needs mu to finish), so hand over instead: with the run gone, launchCollector's
		// handshake stops the collector itself and never attaches a processor.
		m.logger.Info("Collector stopped while starting; its starter will stop it", "type", collectorType)
		return nil
	}

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
			m.logger.Info(
				"Timeout waiting for processor goroutine to finish",
				"type",
				collectorType,
			)
		}
	}

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

	var collectorTypes []string
	for _, collector := range m.collectors {
		collectorTypes = append(collectorTypes, collector.GetType())
	}

	m.logger.Info("Starting all collectors",
		"count", len(m.collectors),
		"collector_types", strings.Join(collectorTypes, ", "))

	// Start each collector in its own goroutine
	for collectorType, collector := range m.collectors {
		go func(collectorType string, collector ResourceCollector) {
			errCh := make(chan error, 1)

			go func() {
				errCh <- m.startCollectorInternal(collectorType, collector)
			}()

			select {
			case err := <-errCh:
				if err != nil {
					m.telemetryLogger.Report(
						gen.LogLevel_LOG_LEVEL_ERROR,
						"CollectionManager_StartAll",
						"Failed to start collector",
						err,
						map[string]string{
							"error_type":       "resource_collectors_start_failed",
							"collector_type":   collectorType,
							"zxporter_version": version.Get().String(),
						},
					)
					m.logger.Info(
						"Failed to start collector",
						"type",
						collectorType,
						"error",
						err.Error(),
					)
				} else {
					m.telemetryLogger.Report(
						gen.LogLevel_LOG_LEVEL_INFO,
						"CollectionManager_StartAll",
						"Successfully started collector",
						err,
						map[string]string{
							"error_type":       "resource_collectors_start_succeed",
							"collector_type":   collectorType,
							"zxporter_version": version.Get().String(),
						},
					)
					m.logger.Info("Successfully started collector", "type", collector.GetType())
				}
			case <-time.After(10 * time.Second):
				m.telemetryLogger.Report(
					gen.LogLevel_LOG_LEVEL_WARN,
					"CollectionManager_StartAll",
					"Timed out starting collector",
					nil,
					map[string]string{
						"event_type":       "resource_collectors_start_timed_out",
						"collector_type":   collectorType,
						"zxporter_version": version.Get().String(),
					},
				)
				m.logger.Error(
					errors.New("timeout"),
					"Timed out starting collector",
					"type",
					collectorType,
				)
			}
		}(collectorType, collector)
	}

	m.started = true
	if m.healthManager != nil {
		m.healthManager.ClearLivenessSuppression()
		m.healthManager.ClearReadinessSuppression()
	}
	m.updateHealthStatus(
		health.ComponentCollectorManager,
		health.HealthStatusHealthy,
		fmt.Sprintf("%d collectors started", len(m.collectors)),
		map[string]string{"collector_count": fmt.Sprintf("%d", len(m.collectors))},
	)
	m.updateHealthStatus(
		health.ComponentBufferQueue,
		health.HealthStatusHealthy,
		"Buffer is operational",
		map[string]string{"capacity": fmt.Sprintf("%d", m.bufferSize)},
	)

	return nil
}

// StartCollector starts a specific collector
func (m *CollectionManager) StartCollector(ctx context.Context, collectorType string) error {
	collector, collectorCtx, run, wg, err := m.reserveStart(collectorType, nil)
	if err != nil {
		return err
	}
	return m.launchCollector(collectorType, collector, collectorCtx, run, wg)
}

// startCollectorInternal is a helper function to start a collector with appropriate context management
func (m *CollectionManager) startCollectorInternal(
	collectorType string,
	collector ResourceCollector,
) error {
	_, collectorCtx, run, wg, err := m.reserveStart(collectorType, collector)
	if err != nil {
		return err
	}
	return m.launchCollector(collectorType, collector, collectorCtx, run, wg)
}

// reserveStart claims collectorType for starting, in one critical section: it resolves
// the collector (when known is nil, or confirms known is still the registered instance),
// refuses one that is already running, and records the run that marks it running. The
// check and the claim are atomic so two concurrent starts cannot both pass the "already
// running" check.
//
// The mutex is released by defer on every path. The early returns here used to leave it
// held, which on 2026-09-25 wedged the manager, and every informer handler that looks a
// collector up through it, until the pod restarted.
func (m *CollectionManager) reserveStart(
	collectorType string,
	known ResourceCollector,
) (ResourceCollector, context.Context, *collectorRun, *sync.WaitGroup, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	registered, exists := m.collectors[collectorType]
	if !exists {
		return nil, nil, nil, nil, fmt.Errorf("collector for type %s not registered", collectorType)
	}
	// StartAll hands its goroutines the instance it saw; if that was stopped or replaced
	// before the goroutine got here, starting it would run an unregistered collector.
	if known != nil && known != registered {
		return nil, nil, nil, nil, fmt.Errorf("collector for type %s was replaced before it started", collectorType)
	}
	collector := registered

	if _, running := m.collectorCtxs[collectorType]; running {
		return nil, nil, nil, nil, fmt.Errorf("collector %s is already running", collectorType)
	}

	m.logger.Info("Starting collector", "type", collectorType)

	// Create a new context for this collector that can be cancelled individually
	collectorCtx, cancel := context.WithCancel(context.Background())
	run := &collectorRun{cancel: cancel, starting: true}
	m.collectorCtxs[collectorType] = run

	// Make sure the processor wait group exists for this collector
	wg, exists := m.processorWg[collectorType]
	if !exists || wg == nil {
		wg = &sync.WaitGroup{}
		m.processorWg[collectorType] = wg
	}

	return collector, collectorCtx, run, wg, nil
}

// launchCollector runs the collector's Start outside the manager mutex (Start can block,
// and must not hold up other collectors), then attaches its channel processor.
//
// After Start returns it re-checks, under mu, that run is still the current entry. A stop
// that landed during Start has removed it and left the collector to this goroutine (see
// collectorRun.starting), so this goroutine stops the collector and does not attach a
// processor: its channel may never close, and the processor would leak.
func (m *CollectionManager) launchCollector(
	collectorType string,
	collector ResourceCollector,
	collectorCtx context.Context,
	run *collectorRun,
	wg *sync.WaitGroup,
) error {
	if err := collector.Start(collectorCtx); err != nil { // this is the blocking call some time.
		run.cancel() // Clean up the context
		m.mu.Lock()
		if m.collectorCtxs[collectorType] == run {
			delete(m.collectorCtxs, collectorType)
		}
		m.mu.Unlock()
		m.updateHealthStatus(
			health.ComponentCollectorManager,
			health.HealthStatusDegraded,
			fmt.Sprintf("collector %s failed to start", collectorType),
			map[string]string{"failed_collector": collectorType},
		)
		return fmt.Errorf("failed to start collector %s: %w", collectorType, err)
	}

	m.mu.Lock()
	if m.collectorCtxs[collectorType] != run {
		m.mu.Unlock()
		m.logger.Info("Collector was stopped while starting; stopping it", "type", collectorType)
		if err := collector.Stop(); err != nil {
			m.logger.Error(err, "Error stopping collector that was stopped while starting", "type", collectorType)
		}
		return fmt.Errorf("collector %s was stopped while it was starting", collectorType)
	}
	run.starting = false
	// The Adds are taken under mu because StopAll and stopCollectorInternal Wait on
	// these groups while holding it; without that edge an Add is unordered with a
	// concurrent Wait (a WaitGroup misuse the race detector reports).
	m.wg.Add(1)
	wg.Add(1)
	m.mu.Unlock()

	// Start a goroutine to read from this collector's channel
	go m.processCollectorChannel(collectorType, collector, wg)

	return nil
}

// StopAll stops all registered collectors
// TODO: FIX THIS, FIX THIS, stop all currently acts like reset button, preparing
// the same CollectionManager instance to be used again, which doesnt feels good.
func (m *CollectionManager) StopAll() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.started {
		return nil // Nothing to stop
	}

	m.logger.Info("Stopping all collectors", "count", len(m.collectors))

	// Cancel the contexts for all collectors
	for collectorType, run := range m.collectorCtxs {
		m.logger.Info("Cancelling context for collector", "type", collectorType)
		run.cancel()
	}

	// Stop each collector
	for collectorType, collector := range m.collectors {
		if run, ok := m.collectorCtxs[collectorType]; ok && run.starting {
			// Its Start is in flight; the reset of collectorCtxs below hands it to
			// launchCollector's handshake, which stops it. See collectorRun.starting.
			m.logger.Info("Collector is still starting; its starter will stop it", "type", collectorType)
			continue
		}
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
			m.logger.Info(
				"Timeout waiting for processor goroutine to finish",
				"type",
				collectorType,
			)
		}
	}

	// Close the combined channel
	originalBufferSize := cap(m.combinedChannel)
	close(m.combinedChannel)
	m.combinedChannel = make(chan []CollectedResource, originalBufferSize)

	// Clear all tracking maps
	m.collectorCtxs = make(map[string]*collectorRun)
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
	m.updateHealthStatus(
		health.ComponentCollectorManager,
		health.HealthStatusUnhealthy,
		"All collectors stopped",
		nil,
	)
	m.updateHealthStatus(
		health.ComponentBufferQueue,
		health.HealthStatusUnhealthy,
		"Buffer stopped",
		nil,
	)
	return nil
}

// processCollectorChannel reads from a collector's channel and forwards to the combined channel
func (m *CollectionManager) processCollectorChannel(
	collectorType string,
	collector ResourceCollector,
	wg *sync.WaitGroup,
) {
	defer m.wg.Done()
	defer wg.Done()

	m.logger.Info("Starting to process collector channel", "type", collectorType)
	resourceChan := collector.GetResourceChannel()

	if resourceChan == nil {
		m.logger.Info("Collector channel is nil, skipping", "type", collectorType)
		return
	}

	for resources := range resourceChan {
		// Skip if channel is closed
		if len(resources) == 0 {
			continue
		}

		// Update metrics for the ingested resources
		m.telemetryMetrics.MessagesIngested.WithLabelValues(resources[0].ResourceType.String()).
			Add(float64(len(resources)))

		// Forward to the combined channel with a timeout to reduce silent drops
		select {
		case m.combinedChannel <- resources:
			// Successfully sent
		case <-time.After(5 * time.Second):
			m.logger.Error(nil, "Combined channel buffer full, dropping resources after timeout",
				"count", len(resources),
				"type", resources[0].ResourceType.String())
			m.updateHealthStatus(
				health.ComponentBufferQueue,
				health.HealthStatusDegraded,
				"Buffer full, dropping resources",
				map[string]string{
					"capacity":      fmt.Sprintf("%d", m.bufferSize),
					"resource_type": resources[0].ResourceType.String(),
				},
			)
			m.telemetryMetrics.MessagesDropped.WithLabelValues(resources[0].ResourceType.String()).
				Add(float64(len(resources)))
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

// GetCollector returns a specific collector by type, or nil if not found.
//
// Lock-free by design: it reads the snapshot published by publishCollectorViewLocked and
// never waits on mu, so it is safe to call from an informer event handler. See the
// collectorView field.
func (m *CollectionManager) GetCollector(collectorType string) ResourceCollector {
	view := m.collectorView.Load()
	if view == nil {
		return nil
	}
	collector, exists := (*view)[collectorType]
	if !exists {
		return nil
	}
	return collector
}

// updateHealthStatus updates the health status of the collection manager
func (m *CollectionManager) updateHealthStatus(
	component string,
	status health.HealthStatus,
	message string,
	metadata map[string]string,
) {
	if m.healthManager != nil {
		m.healthManager.UpdateStatus(component, status, message, metadata)
	}
}
