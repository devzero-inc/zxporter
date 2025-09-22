package controller

import (
	"testing"
	"time"

	monitoringv1 "github.com/devzero-inc/zxporter/api/v1"
	"github.com/go-logr/logr"
)

func TestClusterSnapshotIntervalConfiguration(t *testing.T) {
	tests := []struct {
		name           string
		envValue       string
		expectedResult time.Duration
		expectError    bool
	}{
		{
			name:           "Default interval when not specified",
			envValue:       "",
			expectedResult: 15 * time.Minute,
			expectError:    false,
		},
		{
			name:           "Custom 30 minute interval",
			envValue:       "30m",
			expectedResult: 30 * time.Minute,
			expectError:    false,
		},
		{
			name:           "Custom 1 hour interval",
			envValue:       "1h",
			expectedResult: 1 * time.Hour,
			expectError:    false,
		},
		{
			name:           "Custom 45 minute interval for reduced network usage",
			envValue:       "45m",
			expectedResult: 45 * time.Minute,
			expectError:    false,
		},
		{
			name:           "Custom 2 hour interval for large clusters",
			envValue:       "2h",
			expectedResult: 2 * time.Hour,
			expectError:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a mock environment spec
			mockSpec := &monitoringv1.CollectionPolicySpec{
				Policies: monitoringv1.Policies{
					ClusterSnapshotInterval: tt.envValue,
				},
			}

			// Create a mock reconciler
			r := &CollectionPolicyReconciler{}

			// Parse the configuration
			config, _ := r.createNewConfig(mockSpec, logr.Discard())

			if config.ClusterSnapshotInterval != tt.expectedResult {
				t.Errorf("Expected ClusterSnapshotInterval %v, got %v", tt.expectedResult, config.ClusterSnapshotInterval)
			}
		})
	}
}

func TestClusterSnapshotIntervalNetworkOptimization(t *testing.T) {
	// Test that longer intervals are properly configured for network optimization
	testCases := []struct {
		name         string
		interval     string
		expectedText string
	}{
		{
			name:         "Extended interval reduces network traffic",
			interval:     "60m",
			expectedText: "1h0m0s",
		},
		{
			name:         "Very long interval for cost optimization",
			interval:     "4h",
			expectedText: "4h0m0s",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			mockSpec := &monitoringv1.CollectionPolicySpec{
				Policies: monitoringv1.Policies{
					ClusterSnapshotInterval: tc.interval,
				},
			}

			// Create a mock reconciler
			r := &CollectionPolicyReconciler{}
			config, _ := r.createNewConfig(mockSpec, logr.Discard())

			if config.ClusterSnapshotInterval.String() != tc.expectedText {
				t.Errorf("Expected interval %s, got %s", tc.expectedText, config.ClusterSnapshotInterval.String())
			}

			// Verify the interval is longer than the default 15 minutes
			if config.ClusterSnapshotInterval <= 15*time.Minute {
				t.Errorf("Expected interval %v to be longer than default 15m", config.ClusterSnapshotInterval)
			}

			t.Logf("✅ Successfully configured cluster snapshot interval: %v", config.ClusterSnapshotInterval)
			t.Logf("   This provides %.1fx reduction in snapshot frequency compared to default",
				float64(config.ClusterSnapshotInterval)/float64(15*time.Minute))
		})
	}
}
