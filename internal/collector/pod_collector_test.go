package collector

import (
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// newTestPodCollector builds a minimal PodCollector sufficient to exercise
// trackStartupLifecycle in isolation, without the full NewPodCollector
// dependency graph (client, batcher, resourceChan).
func newTestPodCollector() (*PodCollector, chan CollectedResource) {
	batchChan := make(chan CollectedResource, 10)
	return &PodCollector{
		batchChan:      batchChan,
		logger:         logr.Discard(),
		startupTracker: make(map[startupLifecycleKey]*startupLifecycleEntry),
	}, batchChan
}

// alreadyReadyLongLivedPod builds a pod whose PodScheduled condition and
// container Running state both point far in the past (2 days), with
// Ready already true — simulating a stable, long-running pod being
// (re)discovered by trackStartupLifecycle with no live tracker entry, e.g.
// after the prior entry for this (pod, container, restartCount) key was
// already emitted-and-deleted, and a later, unrelated pod status update
// (kubelet heartbeat, condition timestamp refresh, etc.) triggers another
// UpdateFunc callback.
func alreadyReadyLongLivedPod(scheduledAt, startedAt time.Time) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:       types.UID("pod-uid-1"),
			Name:      "cnpg-controller-manager-abc123",
			Namespace: "cnpg-system",
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{
					Type:               corev1.PodScheduled,
					Status:             corev1.ConditionTrue,
					LastTransitionTime: metav1.NewTime(scheduledAt),
				},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:         "app",
					Ready:        true,
					RestartCount: 0,
					State: corev1.ContainerState{
						Running: &corev1.ContainerStateRunning{
							StartedAt: metav1.NewTime(startedAt),
						},
					},
				},
			},
		},
	}
}

// TestTrackStartupLifecycle_DoesNotFabricateReadyDurationForStaleRediscovery
// reproduces the bug behind services#8891: a container observed for the
// first time by trackStartupLifecycle (no live tracker entry) that is
// ALREADY Running and Ready has no reliable "time to ready" data available —
// the only timestamps on hand are the pod's original PodScheduled time
// (days old) and time.Now() (this callback's wall-clock time), and computing
// now.Sub(pendingAt) fabricates an ever-growing, wrong duration on every
// re-observation instead of reporting the real (short) startup time.
//
// No lifecycle event should be emitted in this case — the container's
// genuine startup was either already captured by snapshotStartupLifecycles
// during the initial informer sync, or was already correctly reported by an
// earlier, real transition observation and this is a stale re-discovery.
func TestTrackStartupLifecycle_DoesNotFabricateReadyDurationForStaleRediscovery(t *testing.T) {
	c, batchChan := newTestPodCollector()

	scheduledAt := time.Now().Add(-48 * time.Hour)
	startedAt := scheduledAt.Add(3 * time.Second)
	pod := alreadyReadyLongLivedPod(scheduledAt, startedAt)

	c.trackStartupLifecycle(nil, pod)

	select {
	case ev := <-batchChan:
		t.Fatalf("expected no lifecycle event to be emitted for a stale rediscovery, got: %+v", ev)
	default:
		// No event emitted — correct.
	}

	assert.Empty(t, c.startupTracker, "no tracker entry should be left behind for a stale rediscovery")
}

// TestTrackStartupLifecycle_EmitsCorrectDurationForGenuineFreshStartup is the
// control case: a container genuinely observed transitioning through
// Pending -> ContainerCreating -> Running -> Ready across separate calls
// (the normal, working path) must still emit a correct, small duration.
func TestTrackStartupLifecycle_EmitsCorrectDurationForGenuineFreshStartup(t *testing.T) {
	c, batchChan := newTestPodCollector()

	scheduledAt := time.Now().Add(-5 * time.Second)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:       types.UID("pod-uid-2"),
			Name:      "fresh-pod-abc123",
			Namespace: "default",
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{
					Type:               corev1.PodScheduled,
					Status:             corev1.ConditionTrue,
					LastTransitionTime: metav1.NewTime(scheduledAt),
				},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:         "app",
					Ready:        false,
					RestartCount: 0,
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"},
					},
				},
			},
		},
	}

	// Step 1: observe the container while it's still ContainerCreating.
	c.trackStartupLifecycle(nil, pod)
	require.Len(t, c.startupTracker, 1, "entry should be tracked after observing ContainerCreating")

	select {
	case ev := <-batchChan:
		t.Fatalf("expected no lifecycle event yet (container not Ready), got: %+v", ev)
	default:
	}

	// Step 2: container transitions to Running and Ready.
	runningAt := scheduledAt.Add(2 * time.Second)
	pod.Status.ContainerStatuses[0].Ready = true
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{
		Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(runningAt)},
	}
	c.trackStartupLifecycle(nil, pod)

	assert.Empty(t, c.startupTracker, "entry should be cleaned up after a successful emission")

	select {
	case ev := <-batchChan:
		payload, ok := ev.Object.(map[string]interface{})
		require.True(t, ok, "expected event payload to be a map[string]interface{}")
		timeToReadyMs, ok := payload["time_to_ready_ms"].(int64)
		require.True(t, ok, "expected time_to_ready_ms to be present as int64")
		// Ready ~2s after scheduling; must be small and sane, not fabricated.
		assert.Less(t, timeToReadyMs, int64(10_000), "genuine fresh startup must report a small, real duration")
		assert.GreaterOrEqual(t, timeToReadyMs, int64(0))
	default:
		t.Fatal("expected a lifecycle event to be emitted for a genuine fresh startup")
	}
}
