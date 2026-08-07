package collector

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PROVENANCE OF THE FIXTURES IN THIS FILE.
//
// casStatusYAMLFixture is k8s.io/autoscaler's own golden file for the status document,
// cluster-autoscaler/clusterstate/utils/status_test.yaml on branch
// cluster-autoscaler-release-1.36, with two edits, both noted inline: the node group's
// scaleDown status is flipped to CandidatesPresent (upstream's copy says NoCandidates,
// which is not the case under test) and a second node group is added to cover the
// multi-group split. Every key and every timestamp format is upstream's.
//
// The document is written by clusterstate/utils/status.go WriteStatusConfigMap as
// yaml.Marshal(api.ClusterAutoscalerStatus) into ConfigMap data["status"], and the struct
// it marshals is clusterstate/api/types.go — checked field by field against release
// branches 1.30 (where the machine-readable format was introduced) through 1.36.
//
// casStatusLegacyFixture is the pre-1.30 rendering, produced by
// ClusterAutoscalerStatus.GetReadableString() in clusterstate/api/utils.go on branch
// cluster-autoscaler-release-1.29 — reproduced here by following that function's
// fmt.Sprintf layout exactly (two-space prefix, labels padded to 12 columns, the
// LastProbeTime/LastTransitionTime pair indented under each condition, %v on a metav1.Time
// giving Go's default time rendering including the monotonic-clock suffix).
//
// NOTHING in either format carries per-node unremovable reasons — that data lives in
// ScaleDownStatus.UnremovableNodes, which the default ScaleDownStatusProcessor discards,
// and in the logs. So the tests below expect NODE GROUP granularity throughout, and never
// a node name.

const casStatusYAMLFixture = `time: "2023-11-24 04:28:19.546750398 +0000 UTC"
message: "TEST_MSG"
autoscalerStatus: "Running"
clusterWide:
  health:
    status: "Healthy"
    nodeCounts:
      registered:
        total: 11
        ready: 4
        notStarted: 3
      longUnregistered: 1
      unregistered: 2
    lastProbeTime: "2023-11-24T04:28:19Z"
    lastTransitionTime: "2023-11-23T14:52:02Z"
  scaleUp:
    status: "NoActivity"
    lastProbeTime: "2023-11-24T04:28:19Z"
    lastTransitionTime: "2023-11-23T14:52:02Z"
  scaleDown:
    status: "CandidatesPresent"
    candidates: 2
    lastProbeTime: "2023-11-24T04:28:19Z"
    lastTransitionTime: "2023-11-23T14:52:02Z"
nodeGroups:
  -
    name: "sample-node-group"
    health:
      status: "Healthy"
      nodeCounts:
        registered:
          total: 11
          ready: 4
          notStarted: 3
        longUnregistered: 1
        unregistered: 2
      cloudProviderTarget: 8
      minSize: 2
      maxSize: 12
      lastProbeTime: "2023-11-24T04:28:19Z"
      lastTransitionTime: "2023-11-23T14:52:02Z"
    scaleUp:
      status: "Backoff"
      backoffInfo:
        errorCode: "QUOTA_EXCEEDED"
        errorMessage: "Instance 'sample-node-group-40ce0341-t28s' creation failed: Quota 'CPUS' exceeded. Limit: 57.0 in region us-central1."
      lastProbeTime: "2023-11-24T04:28:19Z"
      lastTransitionTime: "2023-11-23T14:52:02Z"
    scaleDown:
      status: "CandidatesPresent"
      candidates: 2
      lastProbeTime: "2023-11-24T04:28:19Z"
      lastTransitionTime: "2023-11-23T14:52:02Z"
  -
    name: "idle-node-group"
    health:
      status: "Healthy"
      cloudProviderTarget: 3
      minSize: 1
      maxSize: 6
      lastProbeTime: "2023-11-24T04:28:19Z"
      lastTransitionTime: "2023-11-23T14:52:02Z"
    scaleDown:
      status: "NoCandidates"
      candidates: 0
      lastProbeTime: "2023-11-24T04:28:19Z"
      lastTransitionTime: "2023-11-23T14:52:02Z"
`

const casStatusLegacyFixture = `Cluster-wide:
  Health:      Healthy (ready=6 unready=0 notStarted=0 longNotStarted=0 registered=6 longUnregistered=0)
               LastProbeTime:      2023-11-24 04:28:19.546750398 +0000 UTC m=+3600.123456789
               LastTransitionTime: 2023-11-23 14:52:02.123456789 +0000 UTC m=+0.000000001
  ScaleUp:     NoActivity (ready=6 registered=6)
               LastProbeTime:      2023-11-24 04:28:19.546750398 +0000 UTC m=+3600.123456789
               LastTransitionTime: 2023-11-23 14:52:02.123456789 +0000 UTC m=+0.000000001
  ScaleDown:   CandidatesPresent (candidates=1)
               LastProbeTime:      2023-11-24 04:28:19.546750398 +0000 UTC m=+3600.123456789
               LastTransitionTime: 2023-11-23 14:52:02.123456789 +0000 UTC m=+0.000000001

NodeGroups:
  Name:        eks-workers-2c1f9d3b-a8e2-4f11-9c3e-8b7a6d5e4f30
  Health:      Healthy (ready=3 unready=0 notStarted=0 longNotStarted=0 registered=3 longUnregistered=0 cloudProviderTarget=3 (minSize=1, maxSize=10))
               LastProbeTime:      2023-11-24 04:28:19.546750398 +0000 UTC m=+3600.123456789
               LastTransitionTime: 2023-11-23 14:52:02.123456789 +0000 UTC m=+0.000000001
  ScaleUp:     NoActivity (ready=3 cloudProviderTarget=3)
               LastProbeTime:      2023-11-24 04:28:19.546750398 +0000 UTC m=+3600.123456789
               LastTransitionTime: 2023-11-23 14:52:02.123456789 +0000 UTC m=+0.000000001
  ScaleDown:   CandidatesPresent (candidates=1)
               LastProbeTime:      2023-11-24 04:28:19.546750398 +0000 UTC m=+3600.123456789
               LastTransitionTime: 2023-11-23 14:52:02.123456789 +0000 UTC m=+0.000000001

  Name:        eks-spot-9f2b1c7d-3a4e-4c8b-b1d6-2e9f0a7c5b41
  Health:      Healthy (ready=3 unready=0 notStarted=0 longNotStarted=0 registered=3 longUnregistered=0 cloudProviderTarget=3 (minSize=0, maxSize=20))
               LastProbeTime:      2023-11-24 04:28:19.546750398 +0000 UTC m=+3600.123456789
               LastTransitionTime: 2023-11-23 14:52:02.123456789 +0000 UTC m=+0.000000001
  ScaleDown:   NoCandidates (candidates=0)
               LastProbeTime:      2023-11-24 04:28:19.546750398 +0000 UTC m=+3600.123456789
               LastTransitionTime: 2023-11-23 14:52:02.123456789 +0000 UTC m=+0.000000001
`

// casFixtureNow is well past both fixtures' lastTransitionTime, so the minimum-hold gate
// is satisfied and the parser's own behaviour is what is under test.
var casFixtureNow = time.Date(2023, 11, 24, 4, 30, 0, 0, time.UTC)

func TestParseClusterAutoscalerStatus_MachineReadable(t *testing.T) {
	result := parseClusterAutoscalerStatus(casStatusYAMLFixture, casFixtureNow)

	require.False(t, result.Unparseable)
	require.False(t, result.Legacy)

	// The document's own probe time, not the collector's clock: this becomes the event's
	// lastTimestamp, which is what makes a stalled autoscaler's blocks age out in dakr.
	assert.Equal(t,
		time.Date(2023, 11, 24, 4, 28, 19, 0, time.UTC),
		result.ObservedAt.UTC())

	require.Len(t, result.Observations, 1,
		"only the group sitting on scale-down candidates is blocked")

	got := result.Observations[0]
	assert.Equal(t, casBlockOwnerKindNodeGroup, got.ObjectKind,
		"the status document names no nodes, so a node name must never be invented")
	assert.Equal(t, "sample-node-group", got.ObjectName)
	assert.Equal(t, disruptionBlockReasonOther, got.Reason,
		"the document carries no unremovable reason, so Other with the raw text is the only honest answer")
	assert.Equal(t,
		time.Date(2023, 11, 23, 14, 52, 2, 0, time.UTC),
		got.StreakStart.UTC())

	// The raw section is the only recourse for an Other-classified row, so it has to be
	// the text CAS actually wrote — including the keys this parser does not model.
	assert.Contains(t, got.Message, "name: \"sample-node-group\"")
	assert.Contains(t, got.Message, "QUOTA_EXCEEDED")
	assert.NotContains(t, got.Message, "idle-node-group")
}

func TestParseClusterAutoscalerStatus_HoldsBackFreshCandidates(t *testing.T) {
	// A group that acquired candidates a minute ago is mid-scale-down, not blocked:
	// Cluster Autoscaler waits out --scale-down-unneeded-time before removing a node, so
	// every healthy scale-down passes through CandidatesPresent.
	fresh := strings.Replace(casStatusYAMLFixture,
		`      status: "CandidatesPresent"
      candidates: 2
      lastProbeTime: "2023-11-24T04:28:19Z"
      lastTransitionTime: "2023-11-23T14:52:02Z"`,
		`      status: "CandidatesPresent"
      candidates: 2
      lastProbeTime: "2023-11-24T04:28:19Z"
      lastTransitionTime: "2023-11-24T04:29:00Z"`, 1)
	require.NotEqual(t, casStatusYAMLFixture, fresh, "fixture edit must apply")

	result := parseClusterAutoscalerStatus(fresh, casFixtureNow)

	require.False(t, result.Unparseable)
	assert.Empty(t, result.Observations)
}

func TestParseClusterAutoscalerStatus_Legacy(t *testing.T) {
	result := parseClusterAutoscalerStatus(casStatusLegacyFixture, casFixtureNow)

	require.False(t, result.Unparseable)
	require.True(t, result.Legacy, "the pre-1.30 readable text must not be read as YAML")

	require.Len(t, result.Observations, 1)
	got := result.Observations[0]
	assert.Equal(t, casBlockOwnerKindNodeGroup, got.ObjectKind)
	assert.Equal(t, "eks-workers-2c1f9d3b-a8e2-4f11-9c3e-8b7a6d5e4f30", got.ObjectName)
	assert.Equal(t, disruptionBlockReasonOther, got.Reason)

	// Go's default time rendering, monotonic suffix and all, has to parse.
	assert.Equal(t,
		time.Date(2023, 11, 23, 14, 52, 2, 123456789, time.UTC),
		got.StreakStart.UTC())

	assert.Contains(t, got.Message, "ScaleDown:   CandidatesPresent (candidates=1)")
	assert.NotContains(t, got.Message, "eks-spot-")
}

func TestParseClusterAutoscalerStatus_Unparseable(t *testing.T) {
	// SYNTHETIC: no Cluster Autoscaler version writes this. It stands in for a future
	// format, a truncated write, or a third-party distribution's own document.
	garbage := "\x00\x01 not a status document at all: [[[ }}}\nsecond line"

	result := parseClusterAutoscalerStatus(garbage, casFixtureNow)

	require.True(t, result.Unparseable)
	require.Len(t, result.Observations, 1,
		"an unreadable document is reported, not silently dropped")

	got := result.Observations[0]
	assert.Equal(t, disruptionBlockReasonOther, got.Reason)
	assert.Equal(t, casBlockOwnerKindNodeGroup, got.ObjectKind,
		"an unattributable block must never masquerade as a node in the live blocked list")
	assert.Equal(t, casStatusConfigMapName, got.ObjectName,
		"attributed to the real object it came from, with no invented node or group name")
	assert.Contains(t, got.Message, "not a status document at all",
		"the raw document is the whole point of the fallback")
	assert.Contains(t, got.Message, "second line")
}

func TestParseClusterAutoscalerStatus_TruncatesOversizedMessage(t *testing.T) {
	result := parseClusterAutoscalerStatus(strings.Repeat("x", casMaxMessageBytes*2), casFixtureNow)

	require.True(t, result.Unparseable)
	require.Len(t, result.Observations, 1)
	assert.LessOrEqual(t, len(result.Observations[0].Message), casMaxMessageBytes+len("\n... [truncated]"))
	assert.Contains(t, result.Observations[0].Message, "[truncated]")
}

// TestClassifyCASBlockReason covers the mapping from Cluster Autoscaler's own reason
// vocabulary onto the shared disruption_block_reason buckets. The inputs are the
// UnremovableReason identifiers and their String() renderings from the vendored
// autoscale/clusterautoscaler/simulator/cluster.go, and the BlockingPodReason names from
// autoscale/clusterautoscaler/utils/drain/drain.go.
func TestClassifyCASBlockReason(t *testing.T) {
	cases := []struct {
		name    string
		section string
		want    string
	}{
		{
			name:    "node scale-down-disabled annotation",
			section: "unremovable: ScaleDownDisabledAnnotation",
			want:    disruptionBlockReasonDoNotDisruptAnnotation,
		},
		{
			name:    "node scale-down-disabled annotation, readable spelling",
			section: "unremovable: Scale down disabled annotation",
			want:    disruptionBlockReasonDoNotDisruptAnnotation,
		},
		{
			name:    "pod safe-to-evict false",
			section: "BlockedByPod: NotSafeToEvictAnnotation",
			want:    disruptionBlockReasonDoNotDisruptAnnotation,
		},
		{
			name:    "pod PDB",
			section: "BlockedByPod: NotEnoughPdb",
			want:    disruptionBlockReasonPDBViolation,
		},
		{
			name:    "waiting out scale-down-unneeded-time",
			section: "unremovable: NotUnneededLongEnough",
			want:    disruptionBlockReasonNotUnneededLongEnough,
		},
		{
			name:    "nowhere for the pods to go",
			section: "unremovable: NoPlaceToMovePods",
			want:    disruptionBlockReasonNoPlaceToMovePods,
		},
		{
			name:    "nowhere for the pods to go, readable spelling",
			section: "unremovable: No place to move pods",
			want:    disruptionBlockReasonNoPlaceToMovePods,
		},
		{
			name: "a reason with no bucket of its own falls to Other",
			// Real UnremovableReason, deliberately unmapped — see casReasonTokens.
			section: "unremovable: NodeGroupMinSizeReached",
			want:    disruptionBlockReasonOther,
		},
		{
			name:    "a bare BlockedByPod is ambiguous and stays Other",
			section: "unremovable: BlockedByPod",
			want:    disruptionBlockReasonOther,
		},
		{
			name:    "upstream's own document mentions no reason at all",
			section: "scaleDown:\n  status: CandidatesPresent\n  candidates: 2",
			want:    disruptionBlockReasonOther,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, classifyCASBlockReason(tc.section))
		})
	}
}

// TestCASBlockUID_Deterministic is the property the whole re-emit design rests on. dakr
// folds block observations with GROUP BY (cluster_id, uid): every count it reports is per
// UID, the live blocked list reconstructs a streak as min..max over the rows sharing one,
// and a Cluster Autoscaler block clears by that UID going quiet. A UID that changed
// between two observations of the same block would inflate every count and make the block
// duration collapse to zero — silently, because nothing about the data would look wrong.
func TestCASBlockUID_Deterministic(t *testing.T) {
	streakStart := time.Date(2023, 11, 23, 14, 52, 2, 0, time.UTC)

	first := casBlockUID(casBlockOwnerKindNodeGroup, "sample-node-group", disruptionBlockReasonOther, streakStart)
	second := casBlockUID(casBlockOwnerKindNodeGroup, "sample-node-group", disruptionBlockReasonOther, streakStart)

	assert.Equal(t, first, second, "the same block observed twice must fold onto one UID")
	assert.NotEmpty(t, first)

	t.Run("the same instant in another zone is the same instant", func(t *testing.T) {
		elsewhere := streakStart.In(time.FixedZone("UTC-8", -8*60*60))
		assert.Equal(t, first,
			casBlockUID(casBlockOwnerKindNodeGroup, "sample-node-group", disruptionBlockReasonOther, elsewhere))
	})

	t.Run("a different node group is a different streak", func(t *testing.T) {
		assert.NotEqual(t, first,
			casBlockUID(casBlockOwnerKindNodeGroup, "other-node-group", disruptionBlockReasonOther, streakStart))
	})

	t.Run("a different reason is a different streak", func(t *testing.T) {
		assert.NotEqual(t, first,
			casBlockUID(casBlockOwnerKindNodeGroup, "sample-node-group", disruptionBlockReasonPDBViolation, streakStart))
	})

	t.Run("a block that cleared and recurred is a different streak", func(t *testing.T) {
		assert.NotEqual(t, first,
			casBlockUID(casBlockOwnerKindNodeGroup, "sample-node-group", disruptionBlockReasonOther,
				streakStart.Add(3*time.Hour)))
	})
}

func TestCASBlockEvent_WireContract(t *testing.T) {
	streakStart := time.Date(2023, 11, 23, 14, 52, 2, 0, time.UTC)
	observedAt := time.Date(2023, 11, 24, 4, 28, 19, 0, time.UTC)

	observation := casBlockObservation{
		ObjectKind:  casBlockOwnerKindNodeGroup,
		ObjectName:  "sample-node-group",
		Reason:      disruptionBlockReasonNotUnneededLongEnough,
		Message:     "scaleDown:\n  status: CandidatesPresent",
		StreakStart: streakStart,
	}

	event := casBlockEvent(observation, streakStart, observedAt, 7)

	// The discriminator dakr's queries filter on.
	assert.Equal(t, clusterAutoscalerReportingController, event.ReportingController)
	assert.Equal(t, scaleDownBlockedReason, event.Reason)
	assert.Equal(t, casBlockOwnerKindNodeGroup, event.InvolvedObject.Kind)
	assert.Equal(t, "sample-node-group", event.InvolvedObject.Name)
	assert.Empty(t, event.InvolvedObject.Namespace, "node groups and nodes are cluster-scoped")
	assert.Equal(t, disruptionBlockReasonNotUnneededLongEnough, event.DisruptionBlockReason)
	assert.Nil(t, event.BlockingPDBName, "must decode as NULL, not an empty string")
	assert.Equal(t, int32(7), event.Count)
	assert.True(t, observedAt.Equal(event.LastTimestamp.Time))
	assert.True(t, streakStart.Equal(event.FirstTimestamp.Time))
	assert.Equal(t,
		casBlockUID(observation.ObjectKind, observation.ObjectName, observation.Reason, streakStart),
		string(event.UID))
	assert.Contains(t, event.Message, "CandidatesPresent",
		"the raw section is never dropped")

	// The JSON keys ARE the contract: dakr decodes this into k8sconverters.EnrichedEvent,
	// so a renamed key does not fail — it silently decodes as "never classified".
	t.Run("wire keys", func(t *testing.T) {
		encoded, err := json.Marshal(event)
		require.NoError(t, err)

		var decoded map[string]interface{}
		require.NoError(t, json.Unmarshal(encoded, &decoded))

		assert.Equal(t, clusterAutoscalerReportingController, decoded["reportingComponent"],
			"the autoscaler discriminator dakr filters on")
		assert.Equal(t, scaleDownBlockedReason, decoded["reason"])
		assert.Equal(t, disruptionBlockReasonNotUnneededLongEnough, decoded["disruptionBlockReason"])
		assert.NotContains(t, decoded, "blockingPdbName",
			"omitted entirely so dakr writes NULL rather than an empty string")

		metadata, ok := decoded["metadata"].(map[string]interface{})
		require.True(t, ok)
		assert.Equal(t, string(event.UID), metadata["uid"],
			"metadata.uid is dakr's fold key")

		involved, ok := decoded["involvedObject"].(map[string]interface{})
		require.True(t, ok)
		assert.Equal(t, casBlockOwnerKindNodeGroup, involved["kind"])
		assert.Equal(t, "sample-node-group", involved["name"])
		assert.NotContains(t, involved, "namespace")

		assert.EqualValues(t, 7, decoded["count"])
		assert.Equal(t, observedAt.Format(time.RFC3339), decoded["lastTimestamp"])
	})
}

// newTestCASCollector builds a collector with no informer, driven directly through sweep.
// The channel is generously buffered so a test never has to drain concurrently.
func newTestCASCollector(clock func() time.Time) (*ClusterAutoscalerStatusCollector, chan CollectedResource) {
	batchChan := make(chan CollectedResource, 64)
	return &ClusterAutoscalerStatusCollector{
		batchChan:      batchChan,
		logger:         logr.Discard(),
		sweepInterval:  casDefaultSweepInterval,
		reEmitInterval: casDefaultReEmitInterval,
		now:            clock,
		streaks:        make(map[string]*casBlockStreak),
	}, batchChan
}

func casStatusConfigMap(status string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: casStatusConfigMapNamespace,
			Name:      casStatusConfigMapName,
		},
		Data: map[string]string{casStatusDataKey: status},
	}
}

func drainCASEvents(t *testing.T, batchChan chan CollectedResource) []*enrichedEvent {
	t.Helper()

	var events []*enrichedEvent
	for {
		select {
		case resource := <-batchChan:
			require.Equal(t, Event, resource.ResourceType,
				"block observations ride the existing Event pipeline, not a resource type of their own")
			event, ok := resource.Object.(*enrichedEvent)
			require.True(t, ok, "expected *enrichedEvent, got %T", resource.Object)
			events = append(events, event)
		default:
			return events
		}
	}
}

// TestCASCollector_ReEmitsWhileBlocked is the behaviour dakr's read path depends on and
// cannot compensate for. Cluster Autoscaler emits no event meaning "the scale-down I was
// blocked on went ahead", so dakr clears a CAS block purely by it going quiet for an hour.
// A collector that observed a block once and then stopped would make a still-blocked node
// group vanish from the live view within the hour.
func TestCASCollector_ReEmitsWhileBlocked(t *testing.T) {
	now := casFixtureNow
	c, batchChan := newTestCASCollector(func() time.Time { return now })
	configMap := casStatusConfigMap(casStatusYAMLFixture)

	c.sweep(configMap)
	first := drainCASEvents(t, batchChan)
	require.Len(t, first, 1)

	// Cluster Autoscaler rewrites this ConfigMap every scan (~10s). Those sweeps must not
	// each produce a row: the block has not changed, and dakr's freshness window is an
	// hour.
	for i := 0; i < 12; i++ {
		now = now.Add(10 * time.Second)
		c.sweep(configMap)
	}
	assert.Empty(t, drainCASEvents(t, batchChan),
		"an unchanging block is throttled to the re-emit interval")

	// Past the interval, and while the block still holds, it is re-observed.
	now = now.Add(casDefaultReEmitInterval)
	c.sweep(configMap)
	second := drainCASEvents(t, batchChan)
	require.Len(t, second, 1, "a still-held block must keep being re-observed")

	assert.Equal(t, first[0].UID, second[0].UID,
		"re-emissions of one streak must fold onto one UID")
	assert.Equal(t, int32(1), first[0].Count)
	assert.Equal(t, int32(2), second[0].Count,
		"count is cumulative across re-emissions; dakr reads max()")

	// And it keeps going, for as long as the block does.
	now = now.Add(casDefaultReEmitInterval)
	c.sweep(configMap)
	third := drainCASEvents(t, batchChan)
	require.Len(t, third, 1)
	assert.Equal(t, first[0].UID, third[0].UID)
	assert.Equal(t, int32(3), third[0].Count)
}

// TestCASCollector_ClearedBlockStartsANewStreak pins the other half: a block that goes
// away and comes back is two streaks, not one long one, so dakr does not report a
// duration spanning the quiet gap.
func TestCASCollector_ClearedBlockStartsANewStreak(t *testing.T) {
	now := casFixtureNow
	c, batchChan := newTestCASCollector(func() time.Time { return now })

	c.sweep(casStatusConfigMap(casStatusYAMLFixture))
	first := drainCASEvents(t, batchChan)
	require.Len(t, first, 1)

	// The group stops having candidates: nothing is emitted to say so, the streak is just
	// forgotten.
	now = now.Add(time.Hour)
	c.sweep(casStatusConfigMap(strings.Replace(
		casStatusYAMLFixture,
		`    scaleDown:
      status: "CandidatesPresent"
      candidates: 2`,
		`    scaleDown:
      status: "NoCandidates"
      candidates: 0`, 1)))
	assert.Empty(t, drainCASEvents(t, batchChan))

	// It comes back, with a new transition time — Cluster Autoscaler's own record that
	// this is a new period.
	now = now.Add(time.Hour)
	c.sweep(casStatusConfigMap(strings.Replace(
		casStatusYAMLFixture,
		`      lastTransitionTime: "2023-11-23T14:52:02Z"
  -
    name: "idle-node-group"`,
		`      lastTransitionTime: "2023-11-24T05:00:00Z"
  -
    name: "idle-node-group"`, 1)))
	second := drainCASEvents(t, batchChan)
	require.Len(t, second, 1)

	assert.NotEqual(t, first[0].UID, second[0].UID,
		"a recurrence is a second streak, not a continuation of the first")
	assert.Equal(t, int32(1), second[0].Count, "the new streak counts from one")
}

// TestCASCollector_ReportsAnUnreadableDocument closes the loop on the "never drop
// silently" rule: a status document neither parser can read still reaches dakr, carrying
// the raw text, so a format mismatch on a customer's cluster is visible rather than
// looking like a cluster with nothing to report.
func TestCASCollector_ReportsAnUnreadableDocument(t *testing.T) {
	now := casFixtureNow
	c, batchChan := newTestCASCollector(func() time.Time { return now })

	// SYNTHETIC: stands in for a future format or a third-party distribution's document.
	configMap := casStatusConfigMap("\x00 not a status document: [[[ }}}")

	// A single unreadable sweep is most likely a torn read of a ConfigMap mid-write.
	c.sweep(configMap)
	assert.Empty(t, drainCASEvents(t, batchChan))

	// Still unreadable ten minutes later: a real mismatch, and reported.
	now = now.Add(casMinBlockHold)
	c.sweep(configMap)

	events := drainCASEvents(t, batchChan)
	require.Len(t, events, 1)
	assert.Equal(t, disruptionBlockReasonOther, events[0].DisruptionBlockReason)
	assert.Equal(t, casBlockOwnerKindNodeGroup, events[0].InvolvedObject.Kind)
	assert.Contains(t, events[0].Message, "not a status document")
}

func TestCASCollector_SkipsConfigMapWithoutStatusKey(t *testing.T) {
	now := casFixtureNow
	c, batchChan := newTestCASCollector(func() time.Time { return now })

	c.sweep(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: casStatusConfigMapNamespace,
			Name:      casStatusConfigMapName,
		},
		Data: map[string]string{"other": "value"},
	})

	assert.Empty(t, drainCASEvents(t, batchChan))
}

// BenchmarkParseClusterAutoscalerStatus sizes the one piece of this collector that runs
// on a timer: Cluster Autoscaler rewrites the status ConfigMap every scan (default 10s),
// and each write is a real informer update that lands here. The absolute cost is what
// matters, not a delta — there is no prior implementation to regress against.
func BenchmarkParseClusterAutoscalerStatus(b *testing.B) {
	b.Run("machine-readable", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = parseClusterAutoscalerStatus(casStatusYAMLFixture, casFixtureNow)
		}
	})

	b.Run("legacy-readable", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = parseClusterAutoscalerStatus(casStatusLegacyFixture, casFixtureNow)
		}
	})
}

func TestClusterAutoscalerStatusCollectorType(t *testing.T) {
	// The collector's registry key. Its OUTPUT rides the Event resource type — asserted
	// in drainCASEvents — so this value never reaches the wire.
	assert.Equal(t, "cluster_autoscaler_status", ClusterAutoscalerStatus.String())
}

// TestCASCollector_StopIsSafeAfterStopped pins the deterministic half of the
// send-after-close fix: once Stop has run, a sweep triggered by any path (including an
// informer callback firing after Stop closed batchChan) must no-op rather than send on
// the now-closed channel.
func TestCASCollector_StopIsSafeAfterStopped(t *testing.T) {
	c, _ := newTestCASCollector(func() time.Time { return casFixtureNow })
	c.stopCh = make(chan struct{})

	require.NoError(t, c.Stop())

	// Stop already closed batchChan — drainCASEvents assumes an open channel (its select
	// doesn't check the ok value on receive), so it cannot be used here. The assertion
	// that matters is that a post-Stop sweep does not touch that closed channel at all.
	assert.NotPanics(t, func() {
		c.sweep(casStatusConfigMap(casStatusYAMLFixture))
	}, "a sweep after Stop must be a no-op, not a send on the closed channel")
}

// TestCASCollector_StopDoesNotRaceConcurrentSweeps is the regression test for the bug an
// automated reviewer caught in this PR: AddFunc/UpdateFunc call sweep synchronously on the
// informer's own callback goroutine, which sweepWG (only ever Add(1)'d for sweepLoop, see
// the struct comment) does not track. Stop used to close batchChan right after sweepWG.Wait
// returned, so an informer-triggered sweep concurrently inside emitBlockObservation's send
// could panic with "send on closed channel". chanMu now makes the send and the close
// mutually exclusive.
//
// This drives real concurrency — many goroutines racing sweep against a concurrent Stop —
// rather than asserting the fix's mechanism directly, so it would have caught the original
// bug: run it against the pre-fix code (send unguarded, close unguarded) and it panics
// nearly every time under `-race`.
func TestCASCollector_StopDoesNotRaceConcurrentSweeps(t *testing.T) {
	c, _ := newTestCASCollector(func() time.Time { return casFixtureNow })
	c.stopCh = make(chan struct{})
	configMap := casStatusConfigMap(casStatusYAMLFixture)

	const sweepers = 32
	var wg sync.WaitGroup
	panics := make(chan any, sweepers)

	wg.Add(sweepers)
	for i := 0; i < sweepers; i++ {
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					panics <- r
				}
			}()
			// Simulate the informer dispatching a burst of Add/Update callbacks
			// concurrently with the collector shutting down.
			for j := 0; j < 50; j++ {
				c.sweep(configMap)
			}
		}()
	}

	// Give the sweepers a head start so at least some are genuinely in-flight when Stop
	// runs, rather than Stop trivially winning a race that never happens.
	time.Sleep(time.Millisecond)
	require.NoError(t, c.Stop())

	wg.Wait()
	close(panics)

	for p := range panics {
		t.Fatalf("sweep panicked concurrently with Stop: %v", p)
	}
	// Stop has already closed batchChan by this point — nothing further to drain, and
	// drainCASEvents assumes an open channel (see TestCASCollector_StopIsSafeAfterStopped).
}
