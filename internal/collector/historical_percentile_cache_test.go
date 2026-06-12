package collector

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/protobuf/types/known/timestamppb"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	"github.com/devzero-inc/zxporter/internal/health"
)

// mockPercentileFetcher is a test double for PercentileFetcher.
type mockPercentileFetcher struct {
	results map[string]*gen.HistoricalMetricsSummary
	err     error
}

func (m *mockPercentileFetcher) FetchWorkloadPercentiles(
	_ context.Context,
	_ string,
	_ []HistoricalWorkloadQuery,
) (map[string]*gen.HistoricalMetricsSummary, error) {
	if m.err != nil {
		return nil, m.err
	}
	// Return a copy so tests cannot mutate the mock's state.
	out := make(map[string]*gen.HistoricalMetricsSummary, len(m.results))
	for k, v := range m.results {
		out[k] = v
	}
	return out, nil
}

// helpers -------------------------------------------------------------------

func makeTestSummary(namespace, name, kind string, containers ...string) *gen.HistoricalMetricsSummary {
	now := time.Now()
	cs := make([]*gen.ContainerHistoricalMetrics, 0, len(containers))
	for _, c := range containers {
		cs = append(cs, &gen.ContainerHistoricalMetrics{
			ContainerName: c,
			CpuP50:        100,
			MemP50:        1024 * 1024,
		})
	}
	return &gen.HistoricalMetricsSummary{
		Workload: &gen.MpaWorkloadIdentifier{
			Namespace: namespace,
			Name:      name,
			Kind:      kind,
		},
		Containers:  cs,
		WindowStart: timestamppb.New(now.Add(-24 * time.Hour)),
		WindowEnd:   timestamppb.New(now),
		SampleCount: 42,
	}
}

func cacheKey(namespace, name, kind string) string {
	return namespace + "/" + name + "/" + kind
}

// ---------------------------------------------------------------------------
// Test 1: cache is populated on Refresh and FetchPercentiles returns data.
// ---------------------------------------------------------------------------

func TestHistoricalPercentileCache_ServesFromCache(t *testing.T) {
	key := cacheKey("default", "web-app", "Deployment")
	summary := makeTestSummary("default", "web-app", "Deployment", "app", "sidecar")

	fetcher := &mockPercentileFetcher{
		results: map[string]*gen.HistoricalMetricsSummary{key: summary},
	}

	hm := health.NewHealthManager()
	hm.Register(health.ComponentPrometheus)

	cache := NewHistoricalPercentileCache(logr.Discard(), fetcher, "cluster-1", hm)
	cache.Refresh(context.Background())

	workload := HistoricalWorkloadQuery{
		Namespace:    "default",
		WorkloadName: "web-app",
		WorkloadKind: "Deployment",
	}
	got, err := cache.FetchPercentiles(context.Background(), workload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil summary")
	}
	if got.Workload == nil {
		t.Fatal("expected workload identifier in summary")
	}
	if got.Workload.Name != "web-app" {
		t.Fatalf("expected workload name 'web-app', got %q", got.Workload.Name)
	}
	if len(got.Containers) != 2 {
		t.Fatalf("expected 2 containers, got %d", len(got.Containers))
	}
	if got.SampleCount != 42 {
		t.Fatalf("expected SampleCount 42, got %d", got.SampleCount)
	}
}

// ---------------------------------------------------------------------------
// Test 2: FetchPercentilesForAll returns only the requested workloads.
// ---------------------------------------------------------------------------

func TestHistoricalPercentileCache_FetchPercentilesForAll(t *testing.T) {
	k1 := cacheKey("ns1", "svc-a", "Deployment")
	k2 := cacheKey("ns1", "svc-b", "Deployment")
	k3 := cacheKey("ns2", "svc-c", "StatefulSet")

	fetcher := &mockPercentileFetcher{
		results: map[string]*gen.HistoricalMetricsSummary{
			k1: makeTestSummary("ns1", "svc-a", "Deployment", "app"),
			k2: makeTestSummary("ns1", "svc-b", "Deployment", "proxy"),
			k3: makeTestSummary("ns2", "svc-c", "StatefulSet", "db"),
		},
	}

	cache := NewHistoricalPercentileCache(logr.Discard(), fetcher, "cluster-1", nil)
	cache.Refresh(context.Background())

	workloads := []HistoricalWorkloadQuery{
		{Namespace: "ns1", WorkloadName: "svc-a", WorkloadKind: "Deployment"},
		{Namespace: "ns2", WorkloadName: "svc-c", WorkloadKind: "StatefulSet"},
	}
	results := cache.FetchPercentilesForAll(context.Background(), workloads)

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if _, ok := results[k1]; !ok {
		t.Errorf("expected result for key %q", k1)
	}
	if _, ok := results[k3]; !ok {
		t.Errorf("expected result for key %q", k3)
	}
	// svc-b was NOT requested — it must not appear.
	if _, ok := results[k2]; ok {
		t.Errorf("unexpected result for key %q", k2)
	}
}

// ---------------------------------------------------------------------------
// Test 3: DiscoverContainers extracts container names from cached summaries.
// ---------------------------------------------------------------------------

func TestHistoricalPercentileCache_DiscoverContainers(t *testing.T) {
	k1 := cacheKey("prod", "api", "Deployment")
	k2 := cacheKey("prod", "worker", "Deployment")
	k3 := cacheKey("staging", "api", "Deployment") // different namespace

	fetcher := &mockPercentileFetcher{
		results: map[string]*gen.HistoricalMetricsSummary{
			k1: makeTestSummary("prod", "api", "Deployment", "web", "sidecar"),
			k2: makeTestSummary("prod", "worker", "Deployment", "worker", "sidecar"),
			k3: makeTestSummary("staging", "api", "Deployment", "web"),
		},
	}

	cache := NewHistoricalPercentileCache(logr.Discard(), fetcher, "cluster-1", nil)
	cache.Refresh(context.Background())

	containers, err := cache.DiscoverContainers(context.Background(), "prod", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Expect "web", "sidecar", "worker" — all unique names from "prod" namespace.
	// Order is not guaranteed.
	if len(containers) != 3 {
		t.Fatalf("expected 3 unique container names, got %d: %v", len(containers), containers)
	}
	seen := make(map[string]bool, len(containers))
	for _, c := range containers {
		seen[c] = true
	}
	for _, want := range []string{"web", "sidecar", "worker"} {
		if !seen[want] {
			t.Errorf("expected container %q in results", want)
		}
	}
	// staging namespace must not leak through.
	if seen[""] {
		t.Error("empty container name should not appear")
	}
}

// ---------------------------------------------------------------------------
// Test 4: Querying a non-cached workload returns an empty summary, not error.
// ---------------------------------------------------------------------------

func TestHistoricalPercentileCache_MissingWorkloadReturnsEmptySummary(t *testing.T) {
	fetcher := &mockPercentileFetcher{
		results: map[string]*gen.HistoricalMetricsSummary{},
	}

	cache := NewHistoricalPercentileCache(logr.Discard(), fetcher, "cluster-1", nil)
	cache.Refresh(context.Background())

	workload := HistoricalWorkloadQuery{
		Namespace:    "missing",
		WorkloadName: "ghost",
		WorkloadKind: "Deployment",
	}
	got, err := cache.FetchPercentiles(context.Background(), workload)
	if err != nil {
		t.Fatalf("expected no error for missing workload, got: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil (empty) summary for missing workload")
	}
	if len(got.Containers) != 0 {
		t.Fatalf("expected 0 containers in empty summary, got %d", len(got.Containers))
	}
}

// ---------------------------------------------------------------------------
// Test 5: A fetcher error on second Refresh keeps stale data in the cache.
// ---------------------------------------------------------------------------

func TestHistoricalPercentileCache_FetcherErrorKeepsStaleCache(t *testing.T) {
	key := cacheKey("default", "stable-app", "Deployment")
	summary := makeTestSummary("default", "stable-app", "Deployment", "app")

	fetcher := &mockPercentileFetcher{
		results: map[string]*gen.HistoricalMetricsSummary{key: summary},
	}

	hm := health.NewHealthManager()
	hm.Register(health.ComponentPrometheus)

	cache := NewHistoricalPercentileCache(logr.Discard(), fetcher, "cluster-1", hm)

	// First refresh: success — cache is warm.
	cache.Refresh(context.Background())

	workload := HistoricalWorkloadQuery{
		Namespace:    "default",
		WorkloadName: "stable-app",
		WorkloadKind: "Deployment",
	}
	got, err := cache.FetchPercentiles(context.Background(), workload)
	if err != nil || got == nil {
		t.Fatalf("expected stale data after first refresh, got err=%v, summary=%v", err, got)
	}

	// Inject a fetcher error for the second refresh.
	fetcher.err = errors.New("DAKR unavailable")
	cache.Refresh(context.Background())

	// Stale data must still be served.
	got, err = cache.FetchPercentiles(context.Background(), workload)
	if err != nil {
		t.Fatalf("unexpected error after failed refresh: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil stale summary after fetcher error")
	}
	if got.Workload == nil || got.Workload.Name != "stable-app" {
		t.Fatalf("stale summary has wrong workload: %+v", got.Workload)
	}

	// Health status should reflect degraded state.
	status, ok := hm.GetStatus(health.ComponentPrometheus)
	if !ok {
		t.Fatal("expected prometheus component to be registered")
	}
	if status.Status != health.HealthStatusDegraded {
		t.Fatalf("expected Degraded health after fetch error, got %v", status.Status)
	}
}
