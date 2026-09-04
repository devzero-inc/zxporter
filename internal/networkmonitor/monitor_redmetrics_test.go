package networkmonitor

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	"github.com/devzero-inc/zxporter/internal/networkmonitor/redmetrics"
	"github.com/devzero-inc/zxporter/internal/transport"
)

// fakeDakrClient captures what the monitor actually put on the wire.
//
// It embeds the interface rather than implementing it: transport.DakrClient carries
// a dozen methods this test has no opinion about, and stubbing all of them would
// bury the one call under test. An unexpected call panics on the nil embedded value,
// which is the right outcome -- a flush reaching for anything else is a finding.
type fakeDakrClient struct {
	transport.DakrClient

	mu       sync.Mutex
	requests []*gen.SendContainerRedMetricsRequest
}

func (c *fakeDakrClient) SendContainerRedMetrics(
	ctx context.Context,
	req *gen.SendContainerRedMetricsRequest,
) (*gen.SendContainerRedMetricsResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, req)
	return &gen.SendContainerRedMetricsResponse{ProcessedCount: int32(len(req.Items))}, nil
}

func (c *fakeDakrClient) SendNetworkTrafficMetrics(
	ctx context.Context,
	req *gen.SendNetworkTrafficMetricsRequest,
) (*gen.SendNetworkTrafficMetricsResponse, error) {
	return &gen.SendNetworkTrafficMetricsResponse{}, nil
}

func (c *fakeDakrClient) sent() []*gen.SendContainerRedMetricsRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*gen.SendContainerRedMetricsRequest, len(c.requests))
	copy(out, c.requests)
	return out
}

// redTestClock drives the aggregator's notion of when a minute closed.
type redTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *redTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *redTestClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

const (
	redTestPodIP = "10.42.0.5"
	redTestPort  = 8080
)

// redTestPodCache seeds a standalone pod cache with one served pod, exactly as the
// informer would have populated it.
func redTestPodCache() *PodCache {
	pc := NewPodCache(nil)
	pc.updatePod(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "checkout-0",
			Namespace: "shop",
			UID:       "uid-checkout-0",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Ports: []corev1.ContainerPort{{ContainerPort: redTestPort}}},
				{Name: "istio-proxy", Ports: []corev1.ContainerPort{{ContainerPort: 15001}}},
			},
		},
		Status: corev1.PodStatus{PodIP: redTestPodIP},
	})
	return pc
}

func redTestEvent(cookie uint64, at time.Time) redmetrics.SocketEvent {
	return redmetrics.SocketEvent{
		SocketCookie: cookie,
		Path:         redmetrics.SyscallRecv,
		LocalAddr:    netip.MustParseAddr(redTestPodIP),
		LocalPort:    redTestPort,
		RemoteAddr:   netip.MustParseAddr("192.168.9.9"),
		RemotePort:   40000,
		TrueLen:      256,
		Timestamp:    at,
	}
}

func redTestMonitor(t *testing.T, client transport.DakrClient) *Monitor {
	t.Helper()
	m, err := NewMonitor(
		Config{NodeName: "node-a", OperatorVersion: "v-test", OperatorCommit: "sha-test"},
		logr.Discard(),
		redTestPodCache(),
		nil, nil, nil,
		client,
		nil,
	)
	if err != nil {
		t.Fatalf("NewMonitor: %v", err)
	}
	return m
}

func redTestTime(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad timestamp %q: %v", s, err)
	}
	return parsed
}

// TestMonitorShipsContainerRedMetrics drives the whole agent path a socket event
// travels: probe observation, port attribution against the real pod cache, minute
// bucketing, and the flush that puts a proto request on the wire. Everything below
// the probe itself is the real implementation -- the fake stops at the transport.
func TestMonitorShipsContainerRedMetrics(t *testing.T) {
	client := &fakeDakrClient{}
	monitor := redTestMonitor(t, client)

	clock := &redTestClock{now: redTestTime(t, "2026-08-25T09:00:20Z")}
	agg := redmetrics.NewAggregator(redmetrics.Config{}, clock, monitor.podCache, logr.Discard())
	monitor.EnableContainerRedMetrics(agg)

	// Two connections in the 09:00 minute, one of them chatty.
	agg.Observe(redTestEvent(1, clock.Now()))
	agg.Observe(redTestEvent(1, clock.Now()))
	agg.Observe(redTestEvent(1, clock.Now()))
	agg.Observe(redTestEvent(2, clock.Now()))

	clock.set(redTestTime(t, "2026-08-25T09:01:10Z"))
	if err := monitor.flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	sent := client.sent()
	if len(sent) != 1 {
		t.Fatalf("got %d requests, want 1", len(sent))
	}

	req := sent[0]
	if req.NodeName != "node-a" {
		t.Fatalf("node_name = %q, want node-a", req.NodeName)
	}
	if req.GetOperatorVersion() != "v-test" || req.GetOperatorCommit() != "sha-test" {
		t.Fatalf("operator identity = %q/%q", req.GetOperatorVersion(), req.GetOperatorCommit())
	}
	if len(req.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(req.Items))
	}

	item := req.Items[0]
	if item.PodName != "checkout-0" || item.PodNamespace != "shop" ||
		item.PodUid != "uid-checkout-0" || item.ContainerName != "app" {
		t.Fatalf("attribution = %+v", item)
	}
	if item.ConnCount != 2 {
		t.Fatalf("conn_count = %d, want 2 connections (not 4 observations)", item.ConnCount)
	}
	if got := item.Timestamp.AsTime(); !got.Equal(redTestTime(t, "2026-08-25T09:00:00Z")) {
		t.Fatalf("bucket minute = %v, want 09:00:00", got)
	}
	// The server rejects the entire request on any other length, so this is a
	// contract test rather than a tidiness check.
	if len(item.DurationBuckets) != redmetrics.DurationBucketCount {
		t.Fatalf("duration_buckets length = %d, want %d",
			len(item.DurationBuckets), redmetrics.DurationBucketCount)
	}
}

// TestMonitorSendsNothingWhenFeatureIsOff pins the off-by-default guarantee at the
// level that matters. The aggregator is never attached, and the monitor must behave
// exactly as it did before this feature existed -- no request, no error.
func TestMonitorSendsNothingWhenFeatureIsOff(t *testing.T) {
	client := &fakeDakrClient{}
	monitor := redTestMonitor(t, client)

	if err := monitor.flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if sent := client.sent(); len(sent) != 0 {
		t.Fatalf("got %d requests, want none while the feature is off", len(sent))
	}
}

// TestMonitorSendsNothingForAnEmptyMinute pins that a quiet minute costs nothing. A
// DaemonSet flushing on a ticker across a large fleet must not send an empty request
// per node per interval merely to say so.
func TestMonitorSendsNothingForAnEmptyMinute(t *testing.T) {
	client := &fakeDakrClient{}
	monitor := redTestMonitor(t, client)

	clock := &redTestClock{now: redTestTime(t, "2026-08-25T09:00:20Z")}
	monitor.EnableContainerRedMetrics(
		redmetrics.NewAggregator(redmetrics.Config{}, clock, monitor.podCache, logr.Discard()),
	)

	clock.set(redTestTime(t, "2026-08-25T09:02:00Z"))
	if err := monitor.flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if sent := client.sent(); len(sent) != 0 {
		t.Fatalf("got %d requests, want none for a minute with no traffic", len(sent))
	}
}

// TestMonitorDrainsWithoutADakrClient pins the drain-before-the-client-check
// ordering. A minute closes whether or not anyone is listening; if the aggregator
// were only drained once a client existed, a node running without one would
// accumulate closed buckets for as long as the process lived.
func TestMonitorDrainsWithoutADakrClient(t *testing.T) {
	monitor := redTestMonitor(t, nil)

	clock := &redTestClock{now: redTestTime(t, "2026-08-25T09:00:20Z")}
	agg := redmetrics.NewAggregator(redmetrics.Config{}, clock, monitor.podCache, logr.Discard())
	monitor.EnableContainerRedMetrics(agg)
	agg.Observe(redTestEvent(1, clock.Now()))

	clock.set(redTestTime(t, "2026-08-25T09:01:10Z"))
	if err := monitor.flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// The minute was drained and discarded, not held. Re-flushing with a client
	// attached would resurface it if it had merely been parked.
	if items := agg.Flush(); len(items) != 0 {
		t.Fatalf("got %+v, want the closed minute already drained", items)
	}
}

// TestMonitorEmitsEachMinuteOnce pins the property the absence of retry rests on:
// consecutive flushes never re-send a minute, so nothing downstream can double count
// one by being called twice.
func TestMonitorEmitsEachMinuteOnce(t *testing.T) {
	client := &fakeDakrClient{}
	monitor := redTestMonitor(t, client)

	clock := &redTestClock{now: redTestTime(t, "2026-08-25T09:00:20Z")}
	agg := redmetrics.NewAggregator(redmetrics.Config{}, clock, monitor.podCache, logr.Discard())
	monitor.EnableContainerRedMetrics(agg)
	agg.Observe(redTestEvent(1, clock.Now()))

	clock.set(redTestTime(t, "2026-08-25T09:01:10Z"))
	for i := 0; i < 3; i++ {
		if err := monitor.flush(context.Background()); err != nil {
			t.Fatalf("flush %d: %v", i, err)
		}
	}

	sent := client.sent()
	if len(sent) != 1 {
		t.Fatalf("got %d requests, want the minute sent exactly once", len(sent))
	}
	if sent[0].Items[0].ConnCount != 1 {
		t.Fatalf("conn_count = %d, want 1", sent[0].Items[0].ConnCount)
	}
}
