package collector

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// newTestKarpenterCollector builds a minimal KarpenterCollector sufficient to exercise
// processNodeClaim's lifecycle-transition emission in isolation, without the informer /
// dynamic-client / batcher dependency graph NewKarpenterCollector needs.
func newTestKarpenterCollector() (*KarpenterCollector, chan CollectedResource) {
	batchChan := make(chan CollectedResource, 32)
	return &KarpenterCollector{
		batchChan:           batchChan,
		logger:              logr.Discard(),
		nodeClaimConditions: make(map[string]map[string]nodeClaimConditionState),
	}, batchChan
}

// nodeClaimCondition is a NodeClaim status condition as it arrives from the dynamic
// informer: an untyped map, not a typed Go struct.
func nodeClaimCondition(condType, status, lastTransitionTime string) map[string]interface{} {
	return map[string]interface{}{
		"type":               condType,
		"status":             status,
		"lastTransitionTime": lastTransitionTime,
	}
}

// nodeClaim builds an unstructured Karpenter NodeClaim with the given conditions.
// nodeName is omitted when empty, matching a NodeClaim that has not yet bound a Node.
func nodeClaim(name, nodeName string, conditions ...map[string]interface{}) *unstructured.Unstructured {
	raw := make([]interface{}, 0, len(conditions))
	for _, c := range conditions {
		raw = append(raw, c)
	}

	status := map[string]interface{}{"conditions": raw}
	if nodeName != "" {
		status["nodeName"] = nodeName
	}

	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "karpenter.sh/v1",
			"kind":       "NodeClaim",
			"metadata": map[string]interface{}{
				"name": name,
				"labels": map[string]interface{}{
					"node.kubernetes.io/instance-type": "m7g.large",
					"karpenter.sh/capacity-type":       "spot",
				},
			},
			"status": status,
		},
	}
}

// drainTransitions returns every NodeLifecycleTransition currently buffered on the
// batch channel, keyed by condition type. Non-lifecycle resources (the NodeClaim's own
// Karpenter row) are ignored — those are emitted by handleKarpenterResourceEvent, not
// by the code under test.
func drainTransitions(t *testing.T, batchChan chan CollectedResource) map[string]map[string]interface{} {
	t.Helper()

	transitions := make(map[string]map[string]interface{})
	for {
		select {
		case resource := <-batchChan:
			if resource.ResourceType != NodeLifecycleTransition {
				continue
			}
			object, ok := resource.Object.(map[string]interface{})
			require.True(t, ok, "transition object should be a map, got %T", resource.Object)
			condition, ok := object["condition"].(string)
			require.True(t, ok, "transition should carry a condition")
			_, duplicate := transitions[condition]
			require.False(t, duplicate, "condition %q emitted more than once", condition)
			transitions[condition] = object
		default:
			return transitions
		}
	}
}

// TestProcessNodeClaim_EmitsOneTransitionPerNewlyObservedCondition walks a NodeClaim
// through the sequence a real one takes — first observation, a new condition appearing,
// an existing condition flipping, and a resync that changes nothing — asserting each
// step emits exactly the transitions that are actually new. The unchanged-resync case
// is the important one: informers redeliver the same object indefinitely, and
// re-emitting every condition each time would flood the table with rows that all claim
// to be the same transition.
func TestProcessNodeClaim_EmitsOneTransitionPerNewlyObservedCondition(t *testing.T) {
	c, batchChan := newTestKarpenterCollector()

	launchedAt := "2026-08-03T10:00:00Z"
	registeredAt := "2026-08-03T10:00:42.5Z"
	readyAt := "2026-08-03T10:01:07Z"

	// First observation: both present conditions are new.
	firstSeen := nodeClaim("nodeclaim-abc", "",
		nodeClaimCondition("Launched", "True", launchedAt),
		nodeClaimCondition("Ready", "Unknown", launchedAt),
	)
	c.processNodeClaim(firstSeen, EventTypeAdd)

	transitions := drainTransitions(t, batchChan)
	require.Len(t, transitions, 2)
	assert.Equal(t, "True", transitions["Launched"]["status"])
	assert.Equal(t, launchedAt, transitions["Launched"]["last_transition_time"])
	assert.Equal(t, "nodeclaim-abc", transitions["Launched"]["node_claim_name"])
	assert.Equal(t, "m7g.large", transitions["Launched"]["instance_type"])
	assert.Equal(t, "spot", transitions["Launched"]["reservation_type"])
	// No Node bound yet, so node_name is absent rather than empty — dakr maps a
	// missing key to a NULL column.
	assert.NotContains(t, transitions["Launched"], "node_name")
	assert.Equal(t, "Unknown", transitions["Ready"]["status"])

	// A Node binds and Registered appears; Launched is unchanged and must not repeat.
	registered := nodeClaim("nodeclaim-abc", "ip-10-0-1-5.ec2.internal",
		nodeClaimCondition("Launched", "True", launchedAt),
		nodeClaimCondition("Registered", "True", registeredAt),
		nodeClaimCondition("Ready", "Unknown", launchedAt),
	)
	c.processNodeClaim(registered, EventTypeUpdate)

	transitions = drainTransitions(t, batchChan)
	require.Len(t, transitions, 1)
	assert.Equal(t, registeredAt, transitions["Registered"]["last_transition_time"])
	assert.Equal(t, "ip-10-0-1-5.ec2.internal", transitions["Registered"]["node_name"])

	// Ready flips Unknown -> True with a new timestamp: one transition, for Ready only.
	ready := nodeClaim("nodeclaim-abc", "ip-10-0-1-5.ec2.internal",
		nodeClaimCondition("Launched", "True", launchedAt),
		nodeClaimCondition("Registered", "True", registeredAt),
		nodeClaimCondition("Ready", "True", readyAt),
	)
	c.processNodeClaim(ready, EventTypeUpdate)

	transitions = drainTransitions(t, batchChan)
	require.Len(t, transitions, 1)
	assert.Equal(t, "True", transitions["Ready"]["status"])
	assert.Equal(t, readyAt, transitions["Ready"]["last_transition_time"])

	// A resync redelivering the identical object emits nothing.
	c.processNodeClaim(ready, EventTypeUpdate)
	assert.Empty(t, drainTransitions(t, batchChan))
}

// TestProcessNodeClaim_IgnoresNonLifecycleAndTimestamplessConditions asserts the
// filtering, since a NodeClaim carries conditions this signal has no column for
// (Drifted, Expired, ...) and dakr's Enum8 would reject them at insert time.
func TestProcessNodeClaim_IgnoresNonLifecycleAndTimestamplessConditions(t *testing.T) {
	c, batchChan := newTestKarpenterCollector()

	claim := nodeClaim("nodeclaim-def", "",
		nodeClaimCondition("Drifted", "True", "2026-08-03T10:00:00Z"),
		nodeClaimCondition("Expired", "False", "2026-08-03T10:00:00Z"),
		// A lifecycle condition with no lastTransitionTime has no timestamp to
		// measure a phase duration against.
		nodeClaimCondition("Initialized", "True", ""),
		nodeClaimCondition("Launched", "True", "2026-08-03T10:00:00Z"),
	)
	c.processNodeClaim(claim, EventTypeAdd)

	transitions := drainTransitions(t, batchChan)
	require.Len(t, transitions, 1)
	assert.Contains(t, transitions, "Launched")
}

// TestProcessNodeClaim_ReemitsAfterDeleteAndRecreate covers NodeClaim name reuse: once
// a NodeClaim is deleted its tracked state is dropped, so a later NodeClaim with the
// same name is a genuinely new node whose conditions must all be reported again.
func TestProcessNodeClaim_ReemitsAfterDeleteAndRecreate(t *testing.T) {
	c, batchChan := newTestKarpenterCollector()

	claim := nodeClaim("nodeclaim-ghi", "",
		nodeClaimCondition("Launched", "True", "2026-08-03T10:00:00Z"),
	)
	c.processNodeClaim(claim, EventTypeAdd)
	require.Len(t, drainTransitions(t, batchChan), 1)

	// The delete itself reports nothing — its conditions are already recorded.
	c.processNodeClaim(claim, EventTypeDelete)
	require.Empty(t, drainTransitions(t, batchChan))

	c.processNodeClaim(claim, EventTypeAdd)
	assert.Len(t, drainTransitions(t, batchChan), 1)
}

// TestNodeLifecycleTransitionProtoType pins the ResourceType -> proto mapping, which is
// what routes these rows to dakr's node_lifecycle_transitions converter.
func TestNodeLifecycleTransitionProtoType(t *testing.T) {
	assert.Equal(
		t,
		"RESOURCE_TYPE_NODE_LIFECYCLE_TRANSITION",
		NodeLifecycleTransition.ProtoType().String(),
	)
	assert.Equal(t, "node_lifecycle_transition", NodeLifecycleTransition.String())
}
