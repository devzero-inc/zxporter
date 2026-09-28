package controller

import (
	"testing"
	"time"

	monitoringv1 "github.com/devzero-inc/zxporter/api/v1"
	"github.com/devzero-inc/zxporter/internal/collector"
	"github.com/go-logr/logr"
)

// TestSchedulerMetricsScrapeIntervalConfiguration exercises createNewConfig's
// parsing of Policies.SchedulerMetricsScrapeInterval. Guards against a
// regression that silently drops the interval>0 guard or the default
// fallback branch: the collector's own constructor also falls back to
// DefaultSchedulerMetricsScrapeInterval on a non-positive value (see
// NewSchedulerMetricsCollector's doc comment), so a bug here would produce
// no visible symptom other than an unexpectedly short/long scrape cadence.
func TestSchedulerMetricsScrapeIntervalConfiguration(t *testing.T) {
	tests := []struct {
		name           string
		envValue       string
		expectedResult time.Duration
	}{
		{
			name:           "Default interval when not specified",
			envValue:       "",
			expectedResult: collector.DefaultSchedulerMetricsScrapeInterval,
		},
		{
			name:           "Custom 30 second interval",
			envValue:       "30s",
			expectedResult: 30 * time.Second,
		},
		{
			name:           "Zero duration falls back to default",
			envValue:       "0s",
			expectedResult: collector.DefaultSchedulerMetricsScrapeInterval,
		},
		{
			name:           "Malformed value falls back to default",
			envValue:       "bogus",
			expectedResult: collector.DefaultSchedulerMetricsScrapeInterval,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockSpec := &monitoringv1.CollectionPolicySpec{
				Policies: monitoringv1.Policies{
					SchedulerMetricsScrapeInterval: tt.envValue,
				},
			}

			r := &CollectionPolicyReconciler{}
			config, _ := r.createNewConfig(mockSpec, logr.Discard())

			if config.SchedulerMetricsScrapeInterval != tt.expectedResult {
				t.Errorf(
					"Expected SchedulerMetricsScrapeInterval %v, got %v",
					tt.expectedResult,
					config.SchedulerMetricsScrapeInterval,
				)
			}
		})
	}
}
