package collector

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

// mockPrometheusAPI implements v1.API for testing
type mockPrometheusAPI struct {
	queryResults map[string]model.Value
}

func (m *mockPrometheusAPI) Query(ctx context.Context, query string, ts time.Time, opts ...v1.Option) (model.Value, v1.Warnings, error) {
	if result, ok := m.queryResults[query]; ok {
		return result, nil, nil
	}
	return &model.Scalar{Value: 0, Timestamp: model.TimeFromUnix(time.Now().Unix())}, nil, nil
}

func (m *mockPrometheusAPI) QueryRange(ctx context.Context, query string, r v1.Range, opts ...v1.Option) (model.Value, v1.Warnings, error) {
	return nil, nil, nil
}

func (m *mockPrometheusAPI) QueryExemplars(ctx context.Context, query string, startTime, endTime time.Time) ([]v1.ExemplarQueryResult, error) {
	return nil, nil
}

func (m *mockPrometheusAPI) Series(ctx context.Context, matches []string, startTime, endTime time.Time, opts ...v1.Option) ([]model.LabelSet, v1.Warnings, error) {
	return nil, nil, nil
}

func (m *mockPrometheusAPI) LabelNames(ctx context.Context, matches []string, startTime, endTime time.Time, opts ...v1.Option) ([]string, v1.Warnings, error) {
	return nil, nil, nil
}

func (m *mockPrometheusAPI) LabelValues(ctx context.Context, label string, matches []string, startTime, endTime time.Time, opts ...v1.Option) (model.LabelValues, v1.Warnings, error) {
	return nil, nil, nil
}

func (m *mockPrometheusAPI) Alerts(ctx context.Context) (v1.AlertsResult, error) {
	return v1.AlertsResult{}, nil
}

func (m *mockPrometheusAPI) AlertManagers(ctx context.Context) (v1.AlertManagersResult, error) {
	return v1.AlertManagersResult{}, nil
}

func (m *mockPrometheusAPI) Buildinfo(ctx context.Context) (v1.BuildinfoResult, error) {
	return v1.BuildinfoResult{}, nil
}
func (m *mockPrometheusAPI) CleanTombstones(ctx context.Context) error { return nil }
func (m *mockPrometheusAPI) Config(ctx context.Context) (v1.ConfigResult, error) {
	return v1.ConfigResult{}, nil
}

func (m *mockPrometheusAPI) DeleteSeries(ctx context.Context, matches []string, startTime, endTime time.Time) error {
	return nil
}

func (m *mockPrometheusAPI) Flags(ctx context.Context) (v1.FlagsResult, error) {
	return v1.FlagsResult{}, nil
}

func (m *mockPrometheusAPI) Metadata(ctx context.Context, metric, limit string) (map[string][]v1.Metadata, error) {
	return nil, nil
}

func (m *mockPrometheusAPI) Runtimeinfo(ctx context.Context) (v1.RuntimeinfoResult, error) {
	return v1.RuntimeinfoResult{}, nil
}

func (m *mockPrometheusAPI) Snapshot(ctx context.Context, skipHead bool) (v1.SnapshotResult, error) {
	return v1.SnapshotResult{}, nil
}

func (m *mockPrometheusAPI) Targets(ctx context.Context) (v1.TargetsResult, error) {
	return v1.TargetsResult{}, nil
}

func (m *mockPrometheusAPI) TargetsMetadata(ctx context.Context, matchTarget, metric, limit string) ([]v1.MetricMetadata, error) {
	return nil, nil
}

func (m *mockPrometheusAPI) TSDB(ctx context.Context, opts ...v1.Option) (v1.TSDBResult, error) {
	return v1.TSDBResult{}, nil
}

func (m *mockPrometheusAPI) WalReplay(ctx context.Context) (v1.WalReplayStatus, error) {
	return v1.WalReplayStatus{}, nil
}

func (m *mockPrometheusAPI) Rules(ctx context.Context) (v1.RulesResult, error) {
	return v1.RulesResult{}, nil
}

func TestHistoricalCollector_FetchPercentiles(t *testing.T) {
	mock := &mockPrometheusAPI{
		queryResults: make(map[string]model.Value),
	}

	hc := NewHistoricalMetricsCollector(logr.Discard(), mock)

	workload := HistoricalWorkloadQuery{
		Namespace:    "default",
		WorkloadName: "web-app",
		WorkloadKind: "Deployment",
		PodRegex:     "web-app-.*",
		Containers:   []string{"app"},
	}

	result, err := hc.FetchPercentiles(context.Background(), workload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Workload == nil {
		t.Fatal("expected workload identifier")
	}
	if len(result.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(result.Containers))
	}
	if result.Containers[0].ContainerName != "app" {
		t.Fatalf("expected container 'app', got '%s'", result.Containers[0].ContainerName)
	}
}
