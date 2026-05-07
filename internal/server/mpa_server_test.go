package server

import (
	"testing"
	"time"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	"github.com/devzero-inc/zxporter/internal/collector"
	"github.com/go-logr/logr"
)

func mustRecv(t *testing.T, ch <-chan *gen.MpaStreamResponse) *gen.MpaStreamResponse {
	t.Helper()
	select {
	case msg := <-ch:
		return msg
	case <-time.After(1 * time.Second):
		t.Fatalf("timed out waiting for stream message")
		return nil
	}
}

func getFirstItem(t *testing.T, msg *gen.MpaStreamResponse) *gen.ContainerMetricItem {
	t.Helper()
	rt := msg.GetRealtimeMetrics()
	if rt == nil || len(rt.Items) != 1 {
		t.Fatalf("expected 1 realtime item, got %#v", msg)
	}
	return rt.Items[0]
}

func TestSubscriptionManager_OomKillCountIsCumulativeAndSticky(t *testing.T) {
	sm := NewSubscriptionManager(logr.Discard())
	updates := make(chan *gen.MpaStreamResponse, 10)
	clientID := sm.Register(updates)
	defer sm.Unregister(clientID)

	// Subscribe to Deployment/foo in ns.
	sm.UpdateSubscription(clientID, &gen.MpaWorkloadSubscription{Workloads: []*gen.MpaWorkloadIdentifier{{
		Namespace: "ns",
		Name:      "foo",
		Kind:      "Deployment",
	}}})

	base := &collector.ContainerMetricsSnapshot{
		Namespace:     "ns",
		PodName:       "foo-abc",
		ContainerName: "c1",
		WorkloadKind:  "Deployment",
		WorkloadName:  "foo",
	}

	// Initial sample.
	{
		s := *base
		s.RestartCount = 0
		s.LastTerminationReason = ""
		sm.Broadcast(&s, time.Now())
		item := getFirstItem(t, mustRecv(t, updates))
		if item.OomKillCount != 0 {
			t.Fatalf("expected oom_kill_count=0, got %d", item.OomKillCount)
		}
	}

	// First OOM edge.
	{
		s := *base
		s.RestartCount = 1
		s.LastTerminationReason = collector.ReasonOOMKilled
		sm.Broadcast(&s, time.Now())
		item := getFirstItem(t, mustRecv(t, updates))
		if item.OomKillCount != 1 {
			t.Fatalf("expected oom_kill_count=1, got %d", item.OomKillCount)
		}
	}

	// Subsequent non-OOM sample should retain cumulative count.
	{
		s := *base
		s.RestartCount = 1
		s.LastTerminationReason = ""
		sm.Broadcast(&s, time.Now())
		item := getFirstItem(t, mustRecv(t, updates))
		if item.OomKillCount != 1 {
			t.Fatalf("expected oom_kill_count=1 (sticky), got %d", item.OomKillCount)
		}
	}

	// Second OOM edge increments.
	{
		s := *base
		s.RestartCount = 2
		s.LastTerminationReason = collector.ReasonOOMKilled
		sm.Broadcast(&s, time.Now())
		item := getFirstItem(t, mustRecv(t, updates))
		if item.OomKillCount != 2 {
			t.Fatalf("expected oom_kill_count=2, got %d", item.OomKillCount)
		}
	}

	// Duplicate OOM with same restartCount must not double-count.
	{
		s := *base
		s.RestartCount = 2
		s.LastTerminationReason = collector.ReasonOOMKilled
		sm.Broadcast(&s, time.Now())
		item := getFirstItem(t, mustRecv(t, updates))
		if item.OomKillCount != 2 {
			t.Fatalf("expected oom_kill_count=2 (no double count), got %d", item.OomKillCount)
		}
	}
}

func TestSubscriptionManager_OomCountSurvivesDroppedSample(t *testing.T) {
	sm := NewSubscriptionManager(logr.Discard())
	updates := make(chan *gen.MpaStreamResponse, 1) // small buffer
	clientID := sm.Register(updates)
	defer sm.Unregister(clientID)

	sm.UpdateSubscription(clientID, &gen.MpaWorkloadSubscription{Workloads: []*gen.MpaWorkloadIdentifier{{
		Namespace: "ns",
		Name:      "foo",
		Kind:      "Deployment",
	}}})

	base := &collector.ContainerMetricsSnapshot{
		Namespace:     "ns",
		PodName:       "foo-abc",
		ContainerName: "c1",
		WorkloadKind:  "Deployment",
		WorkloadName:  "foo",
	}

	// Fill channel so next send is dropped.
	updates <- &gen.MpaStreamResponse{Payload: &gen.MpaStreamResponse_RealtimeMetrics{RealtimeMetrics: &gen.ContainerMetricsBatch{Items: []*gen.ContainerMetricItem{{
		Workload: &gen.MpaWorkloadIdentifier{Namespace: "ns", Name: "foo", Kind: "Deployment"},
	}}}}}

	// OOM sample is processed (state updated) but dropped due to full channel.
	{
		s := *base
		s.RestartCount = 1
		s.LastTerminationReason = collector.ReasonOOMKilled
		sm.Broadcast(&s, time.Now())
	}

	// Drain the pre-filled message.
	_ = mustRecv(t, updates)

	// Next normal sample should reflect the cumulative OOM count=1.
	{
		s := *base
		s.RestartCount = 1
		s.LastTerminationReason = ""
		sm.Broadcast(&s, time.Now())
		item := getFirstItem(t, mustRecv(t, updates))
		if item.OomKillCount != 1 {
			t.Fatalf("expected oom_kill_count=1 after dropped OOM sample, got %d", item.OomKillCount)
		}
	}
}
