package gpuexporter_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devzero-inc/zxporter/internal/gpuexporter"
)

// fakeQuerier is a test double for MetricsQuerier.
type fakeQuerier struct {
	metrics []gpuexporter.GPUMetric
	err     error
}

func (f *fakeQuerier) QueryMetrics(_ context.Context) ([]gpuexporter.GPUMetric, error) {
	return f.metrics, f.err
}

func sampleMetrics() []gpuexporter.GPUMetric {
	now := time.Date(2026, 2, 23, 12, 0, 0, 0, time.UTC)
	return []gpuexporter.GPUMetric{
		{
			NodeName:       "node-1",
			ModelName:      "A100",
			Device:         "nvidia0",
			Pod:            "train-pod",
			Container:      "trainer",
			Namespace:      "ml",
			GPUUtilization: 85.0,
			Temperature:    72.0,
			Timestamp:      now,
		},
		{
			NodeName:       "node-1",
			ModelName:      "A100",
			Device:         "nvidia1",
			Pod:            "inference-pod",
			Container:      "serve",
			Namespace:      "ml",
			GPUUtilization: 30.0,
			Temperature:    55.0,
			Timestamp:      now,
		},
		{
			NodeName:       "node-2",
			ModelName:      "V100",
			Device:         "nvidia0",
			Pod:            "batch-pod",
			Container:      "worker",
			Namespace:      "batch",
			GPUUtilization: 95.0,
			Temperature:    80.0,
			Timestamp:      now,
		},
	}
}

func TestContainerMetricsHandler(t *testing.T) {
	zapLog, _ := zap.NewDevelopment()
	log := zapr.NewLogger(zapLog)

	tests := []struct {
		name           string
		method         string
		query          string
		querier        *fakeQuerier
		wantStatus     int
		wantCount      int
		wantFirstPod   string
		wantEmptyArray bool
	}{
		{
			name:       "returns all metrics when no filters",
			method:     http.MethodGet,
			query:      "",
			querier:    &fakeQuerier{metrics: sampleMetrics()},
			wantStatus: http.StatusOK,
			wantCount:  3,
		},
		{
			name:         "filter by pod",
			method:       http.MethodGet,
			query:        "?pod=train-pod",
			querier:      &fakeQuerier{metrics: sampleMetrics()},
			wantStatus:   http.StatusOK,
			wantCount:    1,
			wantFirstPod: "train-pod",
		},
		{
			name:       "filter by namespace",
			method:     http.MethodGet,
			query:      "?namespace=ml",
			querier:    &fakeQuerier{metrics: sampleMetrics()},
			wantStatus: http.StatusOK,
			wantCount:  2,
		},
		{
			name:         "filter by container",
			method:       http.MethodGet,
			query:        "?container=worker",
			querier:      &fakeQuerier{metrics: sampleMetrics()},
			wantStatus:   http.StatusOK,
			wantCount:    1,
			wantFirstPod: "batch-pod",
		},
		{
			name:       "filter by node",
			method:     http.MethodGet,
			query:      "?node=node-2",
			querier:    &fakeQuerier{metrics: sampleMetrics()},
			wantStatus: http.StatusOK,
			wantCount:  1,
		},
		{
			name:         "filter by multiple params (pod + namespace)",
			method:       http.MethodGet,
			query:        "?pod=train-pod&namespace=ml",
			querier:      &fakeQuerier{metrics: sampleMetrics()},
			wantStatus:   http.StatusOK,
			wantCount:    1,
			wantFirstPod: "train-pod",
		},
		{
			name:           "no matching data returns empty array",
			method:         http.MethodGet,
			query:          "?pod=nonexistent",
			querier:        &fakeQuerier{metrics: sampleMetrics()},
			wantStatus:     http.StatusOK,
			wantCount:      0,
			wantEmptyArray: true,
		},
		{
			name:       "empty metrics from querier returns empty array",
			method:     http.MethodGet,
			query:      "",
			querier:    &fakeQuerier{metrics: nil},
			wantStatus: http.StatusOK,
			wantCount:  0,
		},
		{
			name:       "scrape error returns 500",
			method:     http.MethodGet,
			query:      "",
			querier:    &fakeQuerier{err: errors.New("scrape timeout")},
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:       "non-GET method returns 405",
			method:     http.MethodPost,
			query:      "",
			querier:    &fakeQuerier{metrics: sampleMetrics()},
			wantStatus: http.StatusMethodNotAllowed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := require.New(t)

			handler := gpuexporter.NewContainerMetricsHandler(tt.querier, log)
			req := httptest.NewRequest(tt.method, "/container/metrics"+tt.query, nil)
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			r.Equal(tt.wantStatus, rec.Code)

			if tt.wantStatus != http.StatusOK {
				return
			}

			var result []gpuexporter.GPUMetricResponse
			err := json.Unmarshal(rec.Body.Bytes(), &result)
			r.NoError(err, "response should be valid JSON array")
			r.Len(result, tt.wantCount)

			if tt.wantFirstPod != "" && len(result) > 0 {
				r.Equal(tt.wantFirstPod, result[0].Pod)
			}

			if tt.wantEmptyArray {
				r.Empty(result)
			}
		})
	}
}
