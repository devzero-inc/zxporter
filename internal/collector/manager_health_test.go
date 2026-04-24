package collector

import (
	"context"
	"sync"
	"testing"
	"time"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	"github.com/devzero-inc/zxporter/internal/health"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
)

// noopTelemetryLogger is a no-op implementation of telemetry_logger.Logger
type noopTelemetryLogger struct{}

func (n *noopTelemetryLogger) Report(
	level gen.LogLevel,
	source string,
	msg string,
	err error,
	fields map[string]string,
) {
}
func (n *noopTelemetryLogger) Stop() {}

var (
	sharedTelemetryMetrics     *TelemetryMetrics
	sharedTelemetryMetricsOnce sync.Once
)

func getTestTelemetryMetrics() *TelemetryMetrics {
	sharedTelemetryMetricsOnce.Do(func() {
		sharedTelemetryMetrics = NewTelemetryMetrics()
	})
	return sharedTelemetryMetrics
}

func TestCollectionManager_HealthyAfterStartAll(t *testing.T) {
	hm := health.NewHealthManager()
	hm.Register(health.ComponentCollectorManager)
	hm.Register(health.ComponentBufferQueue)

	mgr := NewCollectionManager(
		&CollectionConfig{BufferSize: 10},
		nil,
		getTestTelemetryMetrics(),
		logr.Discard(),
		&noopTelemetryLogger{},
		hm,
	)

	mock := &mockCollector{
		collectorType: "test_resource",
		resourceCh:    make(chan []CollectedResource, 10),
	}
	err := mgr.RegisterCollector(mock)
	assert.NoError(t, err)

	err = mgr.StartAll(context.Background())
	assert.NoError(t, err)

	// Give goroutines time to start
	time.Sleep(100 * time.Millisecond)

	status, exists := hm.GetStatus(health.ComponentCollectorManager)
	assert.True(t, exists)
	assert.Equal(t, health.HealthStatusHealthy, status.Status)

	bufStatus, exists := hm.GetStatus(health.ComponentBufferQueue)
	assert.True(t, exists)
	assert.Equal(t, health.HealthStatusHealthy, bufStatus.Status)

	_ = mgr.StopAll()
}

func TestCollectionManager_UnhealthyAfterStopAll(t *testing.T) {
	hm := health.NewHealthManager()
	hm.Register(health.ComponentCollectorManager)
	hm.Register(health.ComponentBufferQueue)

	mgr := NewCollectionManager(
		&CollectionConfig{BufferSize: 10},
		nil,
		getTestTelemetryMetrics(),
		logr.Discard(),
		&noopTelemetryLogger{},
		hm,
	)

	mock := &mockCollector{
		collectorType: "test_resource",
		resourceCh:    make(chan []CollectedResource, 10),
	}
	_ = mgr.RegisterCollector(mock)
	_ = mgr.StartAll(context.Background())
	time.Sleep(100 * time.Millisecond)

	_ = mgr.StopAll()

	status, _ := hm.GetStatus(health.ComponentCollectorManager)
	assert.Equal(t, health.HealthStatusUnhealthy, status.Status)
}

func TestCollectionManager_NilHealthManager(t *testing.T) {
	mgr := NewCollectionManager(
		&CollectionConfig{BufferSize: 10},
		nil,
		getTestTelemetryMetrics(),
		logr.Discard(),
		&noopTelemetryLogger{},
		nil,
	)
	assert.NotNil(t, mgr)
}

// mockCollector implements ResourceCollector for testing
type mockCollector struct {
	collectorType string
	resourceCh    chan []CollectedResource
	started       bool
}

func (m *mockCollector) Start(
	ctx context.Context,
) error {
	m.started = true
	return nil
}

func (m *mockCollector) Stop() error                                    { close(m.resourceCh); return nil }
func (m *mockCollector) GetType() string                                { return m.collectorType }
func (m *mockCollector) GetResourceChannel() <-chan []CollectedResource { return m.resourceCh }
func (m *mockCollector) IsAvailable(ctx context.Context) bool           { return true }
func (m *mockCollector) AddResource(resource interface{}) error         { return nil }
