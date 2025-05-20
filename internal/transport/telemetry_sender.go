// internal/transport/telemetry_sender.go
package transport

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/devzero-inc/zxporter/internal/collector"
	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// TelemetrySender is responsible for periodically sending metrics to the DAKR server
type TelemetrySender struct {
	logger     logr.Logger
	dakrClient DakrClient
	metrics    *collector.TelemetryMetrics
	interval   time.Duration
	stopCh     chan struct{}
	isRunning  bool
}

// NewTelemetrySender creates a new TelemetrySender
func NewTelemetrySender(
	logger logr.Logger,
	dakrClient DakrClient,
	metrics *collector.TelemetryMetrics,
	interval time.Duration,
) *TelemetrySender {
	if interval <= 0 {
		interval = 15 * time.Second // Default interval
	}

	return &TelemetrySender{
		logger:     logger.WithName("telemetry-sender"),
		dakrClient: dakrClient,
		metrics:    metrics,
		interval:   interval,
		stopCh:     make(chan struct{}),
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

// run is the main loop that periodically sends metrics
func (s *TelemetrySender) run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := s.sendMetrics(ctx); err != nil {
				s.logger.Error(err, "Failed to send metrics")
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
	processedCount, err := s.dakrClient.SendTelemetryMetrics(ctx, telemetryMetrics)
	if err != nil {
		return fmt.Errorf("failed to send metrics to DAKR: %w", err)
	}

	s.logger.Info("Successfully sent metrics to DAKR",
		"metricCount", len(telemetryMetrics),
		"processedCount", processedCount)
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

		// Collect metrics into the channel
		go func(m prometheus.Collector) {
			m.Collect(metricCh)
			close(metricCh)
		}(metric)

		// Process each collected metric
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

		// Reset the metric after collection
		metric.Reset()
	}
	s.logger.Info("Collected telemetry metrics", "count", len(telemetryMetrics))
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
