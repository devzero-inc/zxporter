// Package collector provides functionality for collecting metrics from Kubernetes resources
package collector

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Resettable is an interface for metrics that can be reset
// We have to reset any counters or gauges after sending them to dakr
// to avoid sending the same values again
type ResettableCollector interface {
	prometheus.Collector
	Reset()
}

// TelemetryMetrics holds Prometheus metrics for the collector
type TelemetryMetrics struct {
	// RequestDuration captures the duration of Prometheus API calls
	RequestDuration *prometheus.HistogramVec
	AllMetrics      []ResettableCollector
}

// NewTelemetryMetrics creates and registers Prometheus metrics
func NewTelemetryMetrics() *TelemetryMetrics {

	requestDuration := promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "prometheus_request_duration_seconds",
			Help:    "Duration of Prometheus API requests in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"status"},
	)
	return &TelemetryMetrics{
		RequestDuration: requestDuration,
		AllMetrics:      []ResettableCollector{requestDuration},
	}
}

// PrometheusRoundTripper is an http.RoundTripper that captures metrics about requests
type PrometheusRoundTripper struct {
	next    http.RoundTripper
	metrics *TelemetryMetrics
}

// NewPrometheusRoundTripper creates a new round tripper that captures metrics
func NewPrometheusRoundTripper(next http.RoundTripper, metrics *TelemetryMetrics) *PrometheusRoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	return &PrometheusRoundTripper{
		next:    next,
		metrics: metrics,
	}
}

// RoundTrip implements the http.RoundTripper interface
func (rt *PrometheusRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()

	// Execute the request
	resp, err := rt.next.RoundTrip(req)

	// Calculate duration
	duration := time.Since(start).Seconds()

	// Determine status code
	statusCode := 500 // Default to 500 for errors
	if err == nil && resp != nil {
		statusCode = resp.StatusCode
	} else if err != nil && resp == nil {
		// Context cancellation or other client-side errors
		statusCode = 500
	}

	// Record the metric
	rt.metrics.RequestDuration.WithLabelValues(strconv.Itoa(statusCode)).Observe(duration)

	return resp, err
}

// NewPrometheusClient creates a new Prometheus HTTP client with metrics
func NewPrometheusClient(metrics *TelemetryMetrics) *http.Client {
	return &http.Client{
		Transport: NewPrometheusRoundTripper(http.DefaultTransport, metrics),
	}
}
