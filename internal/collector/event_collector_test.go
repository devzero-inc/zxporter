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

// The FailedScheduling messages in this file are assembled from upstream kube-scheduler
// source rather than invented:
//
//   - The envelope is FitError.Error() in pkg/scheduler/framework/types.go:
//     "0/%v nodes are available: <sorted reason histogram>. preemption: <postfilter msg>".
//     The histogram entries are "<node count> <reason>", sorted lexicographically — which
//     is why clause ORDER carries no meaning and this classifier ranks by node count.
//   - Each reason is a plugin's exported ErrReason constant, from
//     pkg/scheduler/framework/plugins/*:
//     noderesources/fit.go        fmt.Sprintf("Insufficient %v", resourceName)
//     tainttoleration             "node(s) had untolerated taint {%s: %s}"
//     nodeaffinity                "node(s) didn't match Pod's node affinity/selector"
//     nodename                    "node(s) didn't match the requested node name"
//     interpodaffinity            "node(s) didn't match pod affinity rules" /
//     "node(s) didn't match pod anti-affinity rules"
//     podtopologyspread           "node(s) didn't match pod topology spread constraints"
//     (+ " (missing required label)")
//     volumebinding               "node(s) didn't find available persistent volumes to
//     bind", "node(s) had volume node affinity conflict",
//     "pod has unbound immediate PersistentVolumeClaims",
//     `persistentvolumeclaim "%s" not found`
//     nodevolumelimits            "node(s) exceed max volume count"
//     volumezone                  "node(s) had no available volume zone"
//
// The pre-v1.24 phrasings ("node(s) had taints that the pod didn't tolerate",
// "node(s) didn't match node selector") are the same constants at their older values;
// customer clusters span a wide version range, so both are matched.
//
// Cases marked SYNTHETIC below are deliberately-constructed inputs for behaviour no real
// scheduler emits (garbage text, a phrase planted in the preemption section) and are
// labelled as such so they are never mistaken for observed samples.

func TestClassifyFailedSchedulingMessage(t *testing.T) {
	cases := []struct {
		name    string
		message string
		want    string
	}{
		{
			name:    "insufficient cpu",
			message: "0/3 nodes are available: 3 Insufficient cpu. preemption: 0/3 nodes are available: 3 No preemption victims found for incoming pod.",
			want:    podUnschedulableReasonInsufficientCPU,
		},
		{
			name:    "insufficient memory",
			message: "0/1 nodes are available: 1 Insufficient memory. preemption: 0/1 nodes are available: 1 No preemption victims found for incoming pod.",
			want:    podUnschedulableReasonInsufficientMemory,
		},
		{
			name:    "insufficient nvidia gpu",
			message: "0/4 nodes are available: 4 Insufficient nvidia.com/gpu. preemption: 0/4 nodes are available: 4 No preemption victims found for incoming pod.",
			want:    podUnschedulableReasonInsufficientGPU,
		},
		{
			// The extended-resource name is vendor-chosen, so the bucket cannot key off
			// a fixed list of names.
			name:    "insufficient amd gpu",
			message: "0/2 nodes are available: 2 Insufficient amd.com/gpu.",
			want:    podUnschedulableReasonInsufficientGPU,
		},
		{
			// MIG slices and Gaudi carry no "gpu" substring at all.
			name:    "insufficient mig slice",
			message: "0/2 nodes are available: 2 Insufficient nvidia.com/mig-1g.5gb.",
			want:    podUnschedulableReasonInsufficientGPU,
		},
		{
			name:    "insufficient habana gaudi",
			message: "0/2 nodes are available: 2 Insufficient habana.ai/gaudi.",
			want:    podUnschedulableReasonInsufficientGPU,
		},
		{
			name:    "untolerated taint",
			message: "0/1 nodes are available: 1 node(s) had untolerated taint {node-role.kubernetes.io/control-plane: }. preemption: 0/1 nodes are available: 1 Preemption is not helpful for scheduling.",
			want:    podUnschedulableReasonTaints,
		},
		{
			name:    "untolerated taint pre-1.24 phrasing",
			message: "0/3 nodes are available: 3 node(s) had taints that the pod didn't tolerate.",
			want:    podUnschedulableReasonTaints,
		},
		{
			name:    "node affinity or selector",
			message: "0/2 nodes are available: 2 node(s) didn't match Pod's node affinity/selector. preemption: 0/2 nodes are available: 2 Preemption is not helpful for scheduling.",
			want:    podUnschedulableReasonNodeAffinity,
		},
		{
			name:    "node selector pre-1.24 phrasing",
			message: "0/5 nodes are available: 5 node(s) didn't match node selector.",
			want:    podUnschedulableReasonNodeAffinity,
		},
		{
			name:    "pod anti-affinity rules",
			message: "0/3 nodes are available: 3 node(s) didn't match pod anti-affinity rules.",
			want:    podUnschedulableReasonNodeAffinity,
		},
		{
			name:    "combined affinity phrasing",
			message: "0/3 nodes are available: 3 node(s) didn't match pod affinity/anti-affinity rules.",
			want:    podUnschedulableReasonNodeAffinity,
		},
		{
			name:    "topology spread constraints",
			message: "0/6 nodes are available: 6 node(s) didn't match pod topology spread constraints. preemption: 0/6 nodes are available: 6 Preemption is not helpful for scheduling.",
			want:    podUnschedulableReasonTopologySpread,
		},
		{
			name:    "topology spread missing required label",
			message: "0/4 nodes are available: 4 node(s) didn't match pod topology spread constraints (missing required label).",
			want:    podUnschedulableReasonTopologySpread,
		},
		{
			// A PreFilter rejection: no node count, because the pod is rejected before
			// any node is scored.
			name:    "unbound immediate persistent volume claims",
			message: "0/3 nodes are available: pod has unbound immediate PersistentVolumeClaims. preemption: 0/3 nodes are available: 3 Preemption is not helpful for scheduling.",
			want:    podUnschedulableReasonVolumeBinding,
		},
		{
			name:    "persistentvolumeclaim not found",
			message: `0/3 nodes are available: persistentvolumeclaim "data-postgres-0" not found.`,
			want:    podUnschedulableReasonVolumeBinding,
		},
		{
			name:    "no available persistent volumes to bind",
			message: "0/3 nodes are available: 3 node(s) didn't find available persistent volumes to bind.",
			want:    podUnschedulableReasonVolumeBinding,
		},
		{
			name:    "volume node affinity conflict",
			message: "0/6 nodes are available: 6 node(s) had volume node affinity conflict.",
			want:    podUnschedulableReasonVolumeBinding,
		},
		{
			name:    "exceed max volume count",
			message: "0/4 nodes are available: 4 node(s) exceed max volume count.",
			want:    podUnschedulableReasonVolumeBinding,
		},
		{
			// The dominant blocker wins: 3 nodes short on CPU outweighs 2 tainted ones.
			name:    "mixed reasons resolve to the one blocking the most nodes",
			message: "0/5 nodes are available: 2 node(s) had untolerated taint {dedicated: gpu}, 3 Insufficient cpu. preemption: 0/5 nodes are available: 5 No preemption victims found for incoming pod.",
			want:    podUnschedulableReasonInsufficientCPU,
		},
		{
			// Same message, reversed dominance — proves the bucket tracks the counts and
			// not the clause order the scheduler happened to sort them into.
			name:    "mixed reasons with taints dominant",
			message: "0/5 nodes are available: 1 Insufficient cpu, 4 node(s) had untolerated taint {dedicated: gpu}.",
			want:    podUnschedulableReasonTaints,
		},
		{
			// Two taint clauses for two different taints are one blocker over 4 nodes,
			// which beats the 3 CPU-short nodes.
			name:    "repeated clauses for one bucket accumulate",
			message: "0/7 nodes are available: 2 node(s) had untolerated taint {dedicated: gpu}, 2 node(s) had untolerated taint {spot: true}, 3 Insufficient cpu.",
			want:    podUnschedulableReasonTaints,
		},
		{
			// An exact tie breaks on failedSchedulingBucketPriority, which puts resource
			// exhaustion first — the actionable-by-scaling case.
			name:    "tie breaks toward resource exhaustion",
			message: "0/4 nodes are available: 2 Insufficient memory, 2 node(s) didn't match pod topology spread constraints.",
			want:    podUnschedulableReasonInsufficientMemory,
		},
		{
			// CPU before memory in the same priority order.
			name:    "cpu and memory tie breaks toward cpu",
			message: "0/4 nodes are available: 2 Insufficient cpu, 2 Insufficient memory.",
			want:    podUnschedulableReasonInsufficientCPU,
		},
		{
			// SYNTHETIC: no real scheduler puts a filter reason in the preemption
			// section. Planted here because if one ever did, its node counts describe why
			// preemption could not help — not why the pod is unschedulable — and folding
			// them in would flip the bucket.
			name:    "preemption section is not classified",
			message: "0/9 nodes are available: 1 Insufficient memory. preemption: 0/9 nodes are available: 8 Insufficient cpu.",
			want:    podUnschedulableReasonInsufficientMemory,
		},
		{
			// Real, and deliberately Other: these are genuine capacity failures with no
			// bucket in the taxonomy. Guessing them into InsufficientCPU/Memory would be
			// worse than reporting them honestly as unclassified.
			name:    "insufficient ephemeral storage has no bucket",
			message: "0/3 nodes are available: 3 Insufficient ephemeral-storage.",
			want:    podUnschedulableReasonOther,
		},
		{
			name:    "insufficient pods has no bucket",
			message: "0/3 nodes are available: 3 Insufficient pods.",
			want:    podUnschedulableReasonOther,
		},
		{
			// Real message, unmatched phrase — the scheduler's text is unversioned and
			// drifts, so this is the expected steady-state for anything new.
			name:    "unrecognised scheduler reason",
			message: "0/3 nodes are available: 3 node(s) were unschedulable. preemption: 0/3 nodes are available: 3 Preemption is not helpful for scheduling.",
			want:    podUnschedulableReasonOther,
		},
		{
			// SYNTHETIC: a malformed message, standing in for a truncated or
			// non-scheduler FailedScheduling event.
			name:    "malformed message",
			message: "0/ nodes are avail",
			want:    podUnschedulableReasonOther,
		},
		{
			name:    "empty message",
			message: "",
			want:    podUnschedulableReasonOther,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, classifyFailedSchedulingMessage(tc.message))
		})
	}
}

// TestFailedSchedulingFilterScope pins where the classifier stops reading, since that cut
// is what keeps the preemption attempt's node counts out of the histogram.
func TestFailedSchedulingFilterScope(t *testing.T) {
	message := "0/3 nodes are available: 3 Insufficient cpu. preemption: 0/3 nodes are available: 3 No preemption victims found for incoming pod."
	assert.Equal(t, "0/3 nodes are available: 3 Insufficient cpu. ", failedSchedulingFilterScope(message))

	// Pre-v1.24 messages have no preemption section at all and must survive whole.
	withoutPreemption := "0/3 nodes are available: 3 Insufficient cpu."
	assert.Equal(t, withoutPreemption, failedSchedulingFilterScope(withoutPreemption))
}

// newTestEventCollector builds an EventCollector sufficient to exercise handleEvent's
// emission in isolation, without the informer / client / batcher dependency graph
// NewEventCollector needs.
func newTestEventCollector() (*EventCollector, chan CollectedResource) {
	batchChan := make(chan CollectedResource, 32)
	return &EventCollector{
		batchChan:        batchChan,
		logger:           logr.Discard(),
		eventCounts:      make(map[string]int),
		excludedEvents:   make(map[types.NamespacedName]bool),
		maxEventsPerType: 1000,
	}, batchChan
}

// failedSchedulingEvent builds a core/v1 Event as kube-scheduler writes one: named after
// the pod with a suffix, with the pod itself in involvedObject.
func failedSchedulingEvent(namespace, podName, message string, count int32, lastSeen time.Time) *corev1.Event {
	return &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      podName + ".17c9f4a1b2c3d4e5",
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:      "Pod",
			Namespace: namespace,
			Name:      podName,
		},
		Reason:         "FailedScheduling",
		Type:           corev1.EventTypeWarning,
		Message:        message,
		Count:          count,
		FirstTimestamp: metav1.NewTime(lastSeen.Add(-time.Minute)),
		LastTimestamp:  metav1.NewTime(lastSeen),
	}
}

// drainResources returns every resource currently buffered on the batch channel, grouped
// by type.
func drainResources(t *testing.T, batchChan chan CollectedResource) map[ResourceType][]CollectedResource {
	t.Helper()

	resources := make(map[ResourceType][]CollectedResource)
	for {
		select {
		case resource := <-batchChan:
			resources[resource.ResourceType] = append(resources[resource.ResourceType], resource)
		default:
			return resources
		}
	}
}

// TestHandleEvent_EmitsClassifiedEventAlongsideRawEvent is the additive contract: the
// classified row is an addition to k8s_events ingestion, never a replacement for it.
func TestHandleEvent_EmitsClassifiedEventAlongsideRawEvent(t *testing.T) {
	c, batchChan := newTestEventCollector()

	lastSeen := time.Date(2026, 8, 3, 10, 0, 42, 531_000_000, time.UTC)
	message := "0/5 nodes are available: 2 node(s) had untolerated taint {dedicated: gpu}, 3 Insufficient cpu. preemption: 0/5 nodes are available: 5 No preemption victims found for incoming pod."
	c.handleEvent(failedSchedulingEvent("payments", "checkout-7d9f8b6c4d-x2k9l", message, 7, lastSeen), EventTypeAdd)

	resources := drainResources(t, batchChan)
	require.Len(t, resources[Event], 1, "the raw event must still reach k8s_events")
	require.Len(t, resources[PodUnschedulableEvent], 1)

	object, ok := resources[PodUnschedulableEvent][0].Object.(map[string]interface{})
	require.True(t, ok, "payload should be a map, got %T", resources[PodUnschedulableEvent][0].Object)

	assert.Equal(t, "payments", object["namespace"])
	// The pod, from involvedObject — not the Event's own suffixed metadata name.
	assert.Equal(t, "checkout-7d9f8b6c4d-x2k9l", object["pod_name"])
	assert.Equal(t, "2026-08-03T10:00:42.531Z", object["timestamp"])
	assert.Equal(t, podUnschedulableReasonInsufficientCPU, object["reason_bucket"])
	// Verbatim, including the taint clause the bucket discarded and the counts it
	// collapsed — this is what makes a misclassification recoverable.
	assert.Equal(t, message, object["raw_message"])
	assert.Equal(t, uint32(7), object["retry_count"])
}

// TestHandleEvent_EmitsOneResourcePerObservedRetry asserts no client-side dedup: the
// scheduler re-emits FailedScheduling on every retry with an advancing count, and each
// observation is its own row. dakr keys on (cluster, namespace, pod, timestamp) and
// handles genuine re-delivery on its end.
func TestHandleEvent_EmitsOneResourcePerObservedRetry(t *testing.T) {
	c, batchChan := newTestEventCollector()

	firstSeen := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	message := "0/3 nodes are available: 3 Insufficient cpu."
	c.handleEvent(failedSchedulingEvent("payments", "checkout-x2k9l", message, 1, firstSeen), EventTypeAdd)
	c.handleEvent(failedSchedulingEvent("payments", "checkout-x2k9l", message, 2, firstSeen.Add(30*time.Second)), EventTypeUpdate)

	resources := drainResources(t, batchChan)
	require.Len(t, resources[PodUnschedulableEvent], 2)

	first := resources[PodUnschedulableEvent][0].Object.(map[string]interface{})
	second := resources[PodUnschedulableEvent][1].Object.(map[string]interface{})
	assert.Equal(t, uint32(1), first["retry_count"])
	assert.Equal(t, uint32(2), second["retry_count"])
	assert.NotEqual(t, first["timestamp"], second["timestamp"])
	// The timestamp is part of the resource key, so the two retries cannot collapse into
	// one another anywhere downstream.
	assert.NotEqual(t, resources[PodUnschedulableEvent][0].Key, resources[PodUnschedulableEvent][1].Key)
}

func TestHandleEvent_SkipsNonSchedulingFailures(t *testing.T) {
	c, batchChan := newTestEventCollector()

	event := failedSchedulingEvent("payments", "checkout-x2k9l", "Successfully assigned payments/checkout-x2k9l to ip-10-0-1-5.ec2.internal", 1, time.Now())
	event.Reason = "Scheduled"
	c.handleEvent(event, EventTypeAdd)

	resources := drainResources(t, batchChan)
	assert.Len(t, resources[Event], 1)
	assert.Empty(t, resources[PodUnschedulableEvent])
}

// TestHandleEvent_SkipsNonPodInvolvedObjects covers batch schedulers (Volcano, the
// coscheduling plugin) that raise FailedScheduling against a group object, which this
// pod-keyed signal has no row shape for.
func TestHandleEvent_SkipsNonPodInvolvedObjects(t *testing.T) {
	c, batchChan := newTestEventCollector()

	event := failedSchedulingEvent("batch", "training-job", "0/3 nodes are available: 3 Insufficient cpu.", 1, time.Now())
	event.InvolvedObject.Kind = "PodGroup"
	c.handleEvent(event, EventTypeAdd)

	resources := drainResources(t, batchChan)
	assert.Len(t, resources[Event], 1)
	assert.Empty(t, resources[PodUnschedulableEvent])
}

// TestHandleEvent_SkipsDeletes covers event GC: a delete carries the state the preceding
// add/update already reported.
func TestHandleEvent_SkipsDeletes(t *testing.T) {
	c, batchChan := newTestEventCollector()

	event := failedSchedulingEvent("payments", "checkout-x2k9l", "0/3 nodes are available: 3 Insufficient cpu.", 3, time.Now())
	c.handleEvent(event, EventTypeDelete)

	resources := drainResources(t, batchChan)
	assert.Len(t, resources[Event], 1)
	assert.Empty(t, resources[PodUnschedulableEvent])
}

// TestFailedSchedulingRetryCount covers the floor and the events/v1 series field, since
// `count` is optional and a 0 would silently vanish from every summed retry count.
func TestFailedSchedulingRetryCount(t *testing.T) {
	t.Run("unset count floors to one", func(t *testing.T) {
		event := failedSchedulingEvent("default", "pod", "", 0, time.Now())
		assert.Equal(t, uint32(1), failedSchedulingRetryCount(event))
	})

	t.Run("series count wins when larger", func(t *testing.T) {
		event := failedSchedulingEvent("default", "pod", "", 1, time.Now())
		event.Series = &corev1.EventSeries{Count: 12}
		assert.Equal(t, uint32(12), failedSchedulingRetryCount(event))
	})
}

// TestFailedSchedulingObservedAt pins the timestamp precedence. An aggregated event stops
// advancing LastTimestamp once the series takes over, so preferring the series is what
// keeps a long pending episode's duration from freezing.
func TestFailedSchedulingObservedAt(t *testing.T) {
	lastSeen := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)

	t.Run("prefers series last observed time", func(t *testing.T) {
		event := failedSchedulingEvent("default", "pod", "", 1, lastSeen)
		event.Series = &corev1.EventSeries{
			Count:            4,
			LastObservedTime: metav1.NewMicroTime(lastSeen.Add(5 * time.Minute)),
		}

		observedAt, ok := failedSchedulingObservedAt(event)
		require.True(t, ok)
		assert.True(t, observedAt.Equal(lastSeen.Add(5*time.Minute)), "got %s", observedAt)
	})

	t.Run("falls back through to creation timestamp", func(t *testing.T) {
		event := &corev1.Event{
			ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(lastSeen)},
		}

		observedAt, ok := failedSchedulingObservedAt(event)
		require.True(t, ok)
		assert.True(t, observedAt.Equal(lastSeen), "got %s", observedAt)
	})

	t.Run("reports no usable timestamp", func(t *testing.T) {
		_, ok := failedSchedulingObservedAt(&corev1.Event{})
		assert.False(t, ok)
	})
}

// TestPodUnschedulableEventProtoType pins the ResourceType -> proto mapping, which is
// what routes these rows to dakr's pod_unschedulable_events converter.
func TestPodUnschedulableEventProtoType(t *testing.T) {
	assert.Equal(
		t,
		"RESOURCE_TYPE_POD_UNSCHEDULABLE_EVENT",
		PodUnschedulableEvent.ProtoType().String(),
	)
	assert.Equal(t, "pod_unschedulable_event", PodUnschedulableEvent.String())
}

// BenchmarkClassifyFailedSchedulingMessage measures the per-event cost of the classifier,
// which runs inline on the informer callback for every FailedScheduling event — the hot
// path in a cluster whose scheduling is backed up, i.e. exactly when this signal fires
// most.
func BenchmarkClassifyFailedSchedulingMessage(b *testing.B) {
	message := "0/5 nodes are available: 2 node(s) had untolerated taint {dedicated: gpu}, 3 Insufficient cpu. preemption: 0/5 nodes are available: 5 No preemption victims found for incoming pod."

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = classifyFailedSchedulingMessage(message)
	}
}
