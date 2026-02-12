// internal/transport/telemetry_sender.go
package transport

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/devzero-inc/zxporter/internal/collector"
	"github.com/devzero-inc/zxporter/internal/health"
	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

const (
	// Circuit breaker configuration
	maxConsecutiveFailures = 5
	initialBackoff         = 15 * time.Second
	maxBackoff             = 5 * time.Minute
	// Timeout for collecting metrics from a single collector
	metricCollectionTimeout = 5 * time.Second
)

// TelemetrySender is responsible for periodically sending metrics to the DAKR server
type TelemetrySender struct {
	logger     logr.Logger
	dakrClient DakrClient
	metrics    *collector.TelemetryMetrics
	interval   time.Duration
	stopCh     chan struct{}
	isRunning  bool

	// Circuit breaker fields
	mu                  sync.RWMutex
	consecutiveFailures int
	lastFailureTime     time.Time
	circuitOpenUntil    time.Time
	healthManager       *health.HealthManager
}

// NewTelemetrySender creates a new TelemetrySender
func NewTelemetrySender(
	logger logr.Logger,
	dakrClient DakrClient,
	metrics *collector.TelemetryMetrics,
	interval time.Duration,
	healthManager *health.HealthManager,
) *TelemetrySender {
	if interval <= 0 {
		interval = 15 * time.Second // Default interval
	}

	return &TelemetrySender{
		logger:        logger.WithName("telemetry-sender"),
		dakrClient:    dakrClient,
		metrics:       metrics,
		interval:      interval,
		stopCh:        make(chan struct{}),
		healthManager: healthManager,
	}
}

// Start begins the periodic sending of metrics
func (s *TelemetrySender) Start(ctx context.Context) error {
	if s.isRunning {
		s.logger.Info("Telemetry sender is already running")
		return nil
	}

	s.logger.Info("Starting telemetry sender", "interval", s.interval)
	s.isRunning = true

	go s.run(ctx)
	return nil
}

// Stop halts the periodic sending of metrics
func (s *TelemetrySender) Stop() error {
	if !s.isRunning {
		return nil
	}

	s.logger.Info("Stopping telemetry sender")
	close(s.stopCh)
	s.isRunning = false
	return nil
}

// isCircuitOpen checks if the circuit breaker is open
func (s *TelemetrySender) isCircuitOpen() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return time.Now().Before(s.circuitOpenUntil)
}

// recordSuccess resets the failure counter on successful send
func (s *TelemetrySender) recordSuccess() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.consecutiveFailures = 0
	s.updateHealthStatus(health.HealthStatusHealthy, "Transport operational", map[string]string{"consecutive_failures": "0", "circuit_breaker": "closed"})
	s.circuitOpenUntil = time.Time{}
}

// recordFailure increments failure counter and opens circuit if needed
func (s *TelemetrySender) recordFailure() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.consecutiveFailures++
	s.lastFailureTime = time.Now()

	if s.consecutiveFailures >= maxConsecutiveFailures {
		// Calculate backoff duration with exponential increase
		backoffMultiplier := s.consecutiveFailures - maxConsecutiveFailures + 1
		backoffDuration := min(time.Duration(backoffMultiplier)*initialBackoff, maxBackoff)

		s.circuitOpenUntil = time.Now().Add(backoffDuration)
		s.logger.Info("Circuit breaker opened",
			"failures", s.consecutiveFailures,
			"reopenAt", s.circuitOpenUntil.Format(time.RFC3339),
			"backoffDuration", backoffDuration.String())
		s.updateHealthStatus(health.HealthStatusUnhealthy, fmt.Sprintf("Circuit breaker open, %d consecutive failures", s.consecutiveFailures), map[string]string{"circuit_breaker": "open", "reopen_at": s.circuitOpenUntil.Format(time.RFC3339)})
	} else {
		s.logger.Info("Recorded telemetry send failure",
			"consecutiveFailures", s.consecutiveFailures)
		s.updateHealthStatus(health.HealthStatusDegraded, fmt.Sprintf("%d consecutive failures", s.consecutiveFailures), map[string]string{"consecutive_failures": fmt.Sprintf("%d", s.consecutiveFailures), "circuit_breaker": "closed"})
	}
}

// run is the main loop that periodically sends metrics
func (s *TelemetrySender) run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Check circuit breaker
			if s.isCircuitOpen() {
				s.logger.V(1).Info("Circuit breaker is open, skipping metric send")
				continue
			}

			if err := s.sendMetrics(ctx); err != nil {
				s.logger.Error(err, "Failed to send metrics")
				s.recordFailure()
			} else {
				s.recordSuccess()
			}
		case <-s.stopCh:
			s.logger.Info("Telemetry sender stopped")
			return
		case <-ctx.Done():
			s.logger.Info("Context cancelled, stopping telemetry sender")
			return
		}
	}
}

// sendMetrics collects and sends metrics to the DAKR server
func (s *TelemetrySender) sendMetrics(ctx context.Context) error {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error(fmt.Errorf("panic occurred: %v", r), "Recovered from panic")
		}
	}()

	if s.metrics == nil {
		s.logger.Info("No metrics to send")
		return nil
	}

	// Collect telemetry metrics from the Prometheus registry
	telemetryMetrics, err := s.collectAndResetTelemetryMetrics(s.metrics.AllMetrics)
	if err != nil {
		return fmt.Errorf("failed to collect metrics: %w", err)
	}

	if len(telemetryMetrics) == 0 {
		s.logger.Info("No metrics to send")
		return nil
	}

	// Send the metrics to the DAKR server using the dedicated method
	_, err = s.dakrClient.SendTelemetryMetrics(ctx, telemetryMetrics)
	if err != nil {
		return fmt.Errorf("failed to send metrics to DAKR: %w", err)
	}

	return nil
}

// collectAndResetTelemetryMetrics gathers metrics from the Prometheus registry and converts them to TelemetryMetric objects
func (s *TelemetrySender) collectAndResetTelemetryMetrics(metrics []collector.ResettableCollector) ([]*dto.MetricFamily, error) {
	var telemetryMetrics []*dto.MetricFamily

	// Check if metrics are available
	if len(metrics) == 0 {
		s.logger.Info("No metrics available to collect")
		return telemetryMetrics, nil
	}

	// Process each metric in AllMetrics
	for _, metric := range metrics {
		// Create a channel to receive metrics
		metricCh := make(chan prometheus.Metric, 1024)

		// Create done channel for timeout
		done := make(chan struct{})

		// Collect metrics with timeout protection
		go func(m prometheus.Collector) {
			defer func() {
				if r := recover(); r != nil {
					s.logger.Error(fmt.Errorf("panic in metric collection: %v", r),
						"Recovered from panic during metric collection")
				}
				close(done)
				close(metricCh)
			}()

			// Collect with timeout
			collectDone := make(chan struct{})
			go func() {
				defer close(collectDone)
				m.Collect(metricCh)
			}()

			// Wait for collection or timeout
			select {
			case <-collectDone:
				// Collection completed successfully
			case <-time.After(metricCollectionTimeout):
				s.logger.Error(errors.New("metric collection timeout"),
					"Metric collection timed out",
					"timeout", metricCollectionTimeout)
			}
		}(metric)

		// Wait for goroutine to complete or timeout
		select {
		case <-done:
			// Process collected metrics
			for m := range metricCh {
				dtoMetric := &dto.Metric{}
				m.Write(dtoMetric)
				metricName := getMetricName(m.Desc().String())
				telemetryMetrics = append(telemetryMetrics,
					&dto.MetricFamily{
						Name:   &metricName,
						Metric: []*dto.Metric{dtoMetric},
					},
				)
			}
		case <-time.After(metricCollectionTimeout + time.Second):
			s.logger.Error(errors.New("metric processing timeout"),
				"Failed to process metrics within timeout")
		}

		// Reset the metric after collection
		metric.Reset()
	}
	return telemetryMetrics, nil
}

func getMetricName(descStr string) string {
	var metricName string
	// Simple parsing to extract fqName (metric name)
	// Not robust, but works for standard Desc.String() output
	if i := strings.Index(descStr, `fqName: "`); i != -1 {
		rest := descStr[i+len(`fqName: "`):]
		if j := strings.Index(rest, `"`); j != -1 {
			metricName = rest[:j]
		}
	}
	return metricName
}

func (s *TelemetrySender) updateHealthStatus(status health.HealthStatus, message string, metadata map[string]string) {
	if s.healthManager != nil {
		s.healthManager.UpdateStatus(health.ComponentDakrTransport, status, message, metadata)
	}
}
