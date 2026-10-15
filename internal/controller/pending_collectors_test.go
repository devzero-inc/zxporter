package controller

import (
	"context"
	"sync"
	"testing"
	"time"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	"github.com/devzero-inc/zxporter/internal/collector"
	"github.com/go-logr/logr"
)

type noopTelemetryLogger struct{}

func (noopTelemetryLogger) Report(gen.LogLevel, string, string, error, map[string]string) {}
func (noopTelemetryLogger) Stop()                                                         {}

// keyedCollector is a ResourceCollector whose GetType() is whatever the test says, so a
// test can make it disagree with the ResourceType it is tracked under.
type keyedCollector struct {
	collectorType string
	resourceCh    chan []collector.CollectedResource
}

func (c *keyedCollector) Start(context.Context) error { return nil }
func (c *keyedCollector) Stop() error                 { close(c.resourceCh); return nil }
func (c *keyedCollector) GetType() string             { return c.collectorType }
func (c *keyedCollector) GetResourceChannel() <-chan []collector.CollectedResource {
	return c.resourceCh
}
func (c *keyedCollector) IsAvailable(context.Context) bool       { return true }
func (c *keyedCollector) AddResource(resource interface{}) error { return nil }

var (
	testTelemetryMetrics     *collector.TelemetryMetrics
	testTelemetryMetricsOnce sync.Once
)

// NewTelemetryMetrics registers with the default Prometheus registry, so build it once.
func getTestTelemetryMetrics() *collector.TelemetryMetrics {
	testTelemetryMetricsOnce.Do(func() { testTelemetryMetrics = collector.NewTelemetryMetrics() })
	return testTelemetryMetrics
}

func newTestCollectionManager() *collector.CollectionManager {
	return collector.NewCollectionManager(
		&collector.CollectionConfig{BufferSize: 10},
		nil,
		getTestTelemetryMetrics(),
		logr.Discard(),
		noopTelemetryLogger{},
		nil,
	)
}

// TestRetryPendingCollectors_StartsUnderRegisteredKey covers the deferred start that runs
// once a CRD appears after zxporter started (the documented KEDA install order). The
// manager keys a collector by its GetType(); the retry used to start it by the pending
// entry's ResourceType name instead, so a collector whose two names differed was
// registered but never started — and the failed Start leaked the manager mutex.
func TestRetryPendingCollectors_StartsUnderRegisteredKey(t *testing.T) {
	mgr := newTestCollectionManager()
	r := &CollectionPolicyReconciler{Log: logr.Discard(), CollectionManager: mgr}
	r.pendingCollectors = []pendingCollector{{
		name: collector.KedaScaledJob, // wire name "keda_scaled_job"
		factory: func() collector.ResourceCollector {
			return &keyedCollector{collectorType: "scaledjob", resourceCh: make(chan []collector.CollectedResource, 1)}
		},
	}}

	done := make(chan struct{})
	go func() {
		r.retryPendingCollectors(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("retryPendingCollectors did not return")
	}

	if !mgr.IsCollectorRunning("scaledjob") {
		t.Fatalf("the collector registered under %q was not started", "scaledjob")
	}
	if len(r.pendingCollectors) != 0 {
		t.Errorf("a collector that registered and started should leave the pending list, got %d entries",
			len(r.pendingCollectors))
	}
	_ = mgr.StopCollector("scaledjob")
}
