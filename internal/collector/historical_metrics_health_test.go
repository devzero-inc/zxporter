package collector

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/devzero-inc/zxporter/internal/health"
	"github.com/go-logr/logr"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
)

type errorPrometheusAPI struct {
	mockPrometheusAPI
}

func (m *errorPrometheusAPI) Query(ctx context.Context, query string, ts time.Time, opts ...v1.Option) (model.Value, v1.Warnings, error) {
	return nil, nil, fmt.Errorf("prometheus unavailable")
}

func TestHistoricalCollector_HealthyOnSuccess(t *testing.T) {
	hm := health.NewHealthManager()
	hm.Register(health.ComponentPrometheus)

	mock := &mockPrometheusAPI{queryResults: make(map[string]model.Value)}
	hc := NewHistoricalMetricsCollector(logr.Discard(), mock, hm)

	workload := HistoricalWorkloadQuery{
		Namespace:    "default",
		WorkloadName: "web-app",
		WorkloadKind: "Deployment",
		PodRegex:     "web-app-.*",
		Containers:   []string{"app"},
	}

	_, err := hc.FetchPercentiles(context.Background(), workload)
	assert.NoError(t, err)

	status, _ := hm.GetStatus(health.ComponentPrometheus)
	assert.Equal(t, health.HealthStatusHealthy, status.Status)
}

func TestHistoricalCollector_DegradedOnQueryError(t *testing.T) {
	hm := health.NewHealthManager()
	hm.Register(health.ComponentPrometheus)

	mock := &errorPrometheusAPI{}
	hc := NewHistoricalMetricsCollector(logr.Discard(), mock, hm)

	_, err := hc.DiscoverContainers(context.Background(), "default", "web-app-.*")
	assert.Error(t, err)

	status, _ := hm.GetStatus(health.ComponentPrometheus)
	assert.Equal(t, health.HealthStatusDegraded, status.Status)
}

func TestHistoricalCollector_NilHealthManager(t *testing.T) {
	mock := &mockPrometheusAPI{queryResults: make(map[string]model.Value)}
	hc := NewHistoricalMetricsCollector(logr.Discard(), mock, nil)
	assert.NotNil(t, hc)
}
