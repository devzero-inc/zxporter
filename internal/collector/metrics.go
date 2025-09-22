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
	RequestDuration  *prometheus.HistogramVec
	MessagesIngested *prometheus.CounterVec
	MessagesSent     *prometheus.CounterVec
	AllMetrics       []ResettableCollector
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
	messagesIngested := promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "collector_messages_ingested_total",
			Help: "Total number of messages ingested into the buffers",
		},
		[]string{"collector"},
	)
	messagesSent := promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "collector_messages_sent_total",
			Help: "Total number of messages sent by the collector",
		},
		[]string{"collector"},
	)

	return &TelemetryMetrics{
		RequestDuration:  requestDuration,
		MessagesIngested: messagesIngested,
		MessagesSent:     messagesSent,
		AllMetrics: []ResettableCollector{
			requestDuration,
			messagesIngested,
			messagesSent,
		},
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

// NewPrometheusClient creates a new Prometheus HTTP client with metrics and compression
func NewPrometheusClient(metrics *TelemetryMetrics) *http.Client {
	// Create a transport with compression enabled for Prometheus API calls
	// This is especially beneficial for large query results
	transport := &http.Transport{
		DisableCompression: false,  // Enable gzip compression for responses
		// Use reasonable defaults for connection pooling
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     30 * time.Second,
	}

	return &http.Client{
		Transport: NewPrometheusRoundTripper(transport, metrics),
		Timeout:   30 * time.Second,  // Add reasonable timeout
	}
}
