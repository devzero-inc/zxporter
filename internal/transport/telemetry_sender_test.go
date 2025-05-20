package transport

import (
	"testing"

	"github.com/devzero-inc/zxporter/internal/collector"
	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap/zaptest"
)

func TestCollectAndResetTelemetryMetrics(t *testing.T) {
	// Create a zap test logger that will log to the test's output
	zapLogger := zaptest.NewLogger(t)

	// Convert the zap logger to a logr.Logger
	logger := zapr.NewLogger(zapLogger)

	// Create a counter metric
	counter := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "test_counter",
		Help: "Test counter for testing",
	}, []string{"label1", "label2"})
	counter.WithLabelValues("value1", "value2").Inc() // Increment the counter

	// Create a TelemetrySender with the counter
	sender := &TelemetrySender{
		logger: logger,
		metrics: &collector.TelemetryMetrics{
			AllMetrics: []collector.ResettableCollector{counter},
		},
	}

	// Call the function being tested
	metricFamilies, err := sender.collectAndResetTelemetryMetrics([]collector.ResettableCollector{counter})

	// Assertions
	assert.NoError(t, err)
	assert.NotEmpty(t, metricFamilies)
	assert.Equal(t, 1, len(metricFamilies))
	assert.Equal(t, "test_counter", *metricFamilies[0].Name)
	assert.Equal(t, 1, len(metricFamilies[0].Metric))
}

// Test with empty metrics
func TestCollectAndResetTelemetryMetricsEmpty(t *testing.T) {
	// Create a logger
	logger := logr.Discard()

	// Create a TelemetrySender with no metrics
	sender := &TelemetrySender{
		logger: logger,
		metrics: &collector.TelemetryMetrics{
			AllMetrics: []collector.ResettableCollector{},
		},
	}

	// Call the function being tested with empty metrics
	metricFamilies, err := sender.collectAndResetTelemetryMetrics([]collector.ResettableCollector{})

	// Assertions
	assert.NoError(t, err)
	assert.Empty(t, metricFamilies)
}

// Test with multiple metrics
func TestCollectAndResetTelemetryMetricsMultiple(t *testing.T) {
	// Create a logger
	logger := logr.Discard()

	// Create metrics
	// Create a counter metric
	counter1 := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "test_counter_1",
		Help: "Test counter 1 for testing",
	}, []string{"label"})
	counter1.WithLabelValues("value").Inc() // Increment the counter

	counter2 := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "test_counter_2",
		Help: "Test counter 2 for testing",
	}, []string{"label"})
	counter2.WithLabelValues("value").Inc()
	counter2.WithLabelValues("value").Inc() // Increment twice

	// Create a TelemetrySender with the counters
	sender := &TelemetrySender{
		logger: logger,
		metrics: &collector.TelemetryMetrics{
			AllMetrics: []collector.ResettableCollector{counter1, counter2},
		},
	}

	// Call the function being tested
	metricFamilies, err := sender.collectAndResetTelemetryMetrics([]collector.ResettableCollector{counter1, counter2})

	// Assertions
	assert.NoError(t, err)
	assert.NotEmpty(t, metricFamilies)
	assert.Equal(t, 2, len(metricFamilies))

	// Check that we have the right metrics
	metricNames := make(map[string]bool)
	for _, family := range metricFamilies {
		metricNames[*family.Name] = true
	}
	assert.True(t, metricNames["test_counter_1"], "test_counter_1 should be in the metrics")
	assert.True(t, metricNames["test_counter_2"], "test_counter_2 should be in the metrics")
}
