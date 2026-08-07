// internal/collector/node_lifecycle_fallback_test.go
package collector

import (
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// These tests cover the Cluster Autoscaler side of node time-to-Ready: the rows a Node
// with no NodeClaim produces. They are the emission half of the contract whose read half
// is dakr's GetTimeToReady, which pivots per node_claim_name and derives whichever phases
// it finds — so what matters here is that exactly the two phases Cluster Autoscaler can
// evidence go out, keyed on the Node's own name, and that the other two are ABSENT rather
// than zero-valued.

// newLifecycleTestNodeCollector builds a collector with only the fields the fallback
// touches. resourceChan is generously buffered: the fallback sends on it directly, the
// same way handleNodeEvent does.
func newLifecycleTestNodeCollector() (*NodeCollector, chan []CollectedResource) {
	resourceChan := make(chan []CollectedResource, 16)
	return &NodeCollector{
		resourceChan:  resourceChan,
		excludedNodes: map[string]bool{},
		logger:        logr.Discard(),
		nodeToPodsMap: make(map[string]map[string]*corev1.Pod),
		nodeLifecycle: make(map[string]*nodeLifecycleState),
	}, resourceChan
}

// lifecycleNode builds a Node as the API server presents one: a creationTimestamp and a
// Ready condition with its own lastTransitionTime.
func lifecycleNode(name string, created time.Time, ready bool, readyAt time.Time) *corev1.Node {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			CreationTimestamp: metav1.NewTime(created),
			Labels:            map[string]string{instanceTypeLabel: "m5.large"},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:               corev1.NodeMemoryPressure,
					Status:             corev1.ConditionFalse,
					LastTransitionTime: metav1.NewTime(created),
				},
				{
					Type:               corev1.NodeReady,
					Status:             status,
					LastTransitionTime: metav1.NewTime(readyAt),
				},
			},
		},
	}
}

func drainLifecycleTransitions(t *testing.T, resourceChan chan []CollectedResource) []map[string]interface{} {
	t.Helper()

	var transitions []map[string]interface{}
	for {
		select {
		case batch := <-resourceChan:
			for _, resource := range batch {
				if resource.ResourceType != NodeLifecycleTransition {
					continue
				}
				object, ok := resource.Object.(map[string]interface{})
				require.True(t, ok, "expected a map payload, got %T", resource.Object)
				transitions = append(transitions, object)
			}
		default:
			return transitions
		}
	}
}

func transitionByCondition(transitions []map[string]interface{}, condition string) map[string]interface{} {
	for _, transition := range transitions {
		if transition["condition"] == condition {
			return transition
		}
	}
	return nil
}

// TestNodeLifecycleFallback_EmitsTwoPhasesForCASNode is the shape dakr's two-timestamp
// path expects: Launched from the Node's creationTimestamp, Ready from the Ready
// condition, keyed on the Node's name because there is no NodeClaim to name.
func TestNodeLifecycleFallback_EmitsTwoPhasesForCASNode(t *testing.T) {
	c, resourceChan := newLifecycleTestNodeCollector()

	created := time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC)
	readyAt := created.Add(95 * time.Second)

	// First observation while the node is still coming up, then the transition to Ready.
	c.emitNodeLifecycleFallback(lifecycleNode("ip-10-0-2-9", created, false, created), EventTypeAdd)
	c.emitNodeLifecycleFallback(lifecycleNode("ip-10-0-2-9", created, true, readyAt), EventTypeUpdate)

	transitions := drainLifecycleTransitions(t, resourceChan)
	require.Len(t, transitions, 2, "exactly two phases: Cluster Autoscaler evidences no others")

	launched := transitionByCondition(transitions, "Launched")
	require.NotNil(t, launched)
	assert.Equal(t, "ip-10-0-2-9", launched["node_claim_name"],
		"with no NodeClaim, the Node's own name is the pivot key")
	assert.Equal(t, "ip-10-0-2-9", launched["node_name"])
	assert.Equal(t, "True", launched["status"])
	assert.Equal(t, created.Format(time.RFC3339Nano), launched["last_transition_time"])
	assert.Equal(t, "m5.large", launched["instance_type"])

	ready := transitionByCondition(transitions, "Ready")
	require.NotNil(t, ready)
	assert.Equal(t, readyAt.Format(time.RFC3339Nano), ready["last_transition_time"],
		"the 95s time-to-Ready has to be derivable from these two rows alone")

	// The two phases Cluster Autoscaler cannot evidence must be ABSENT from the payload,
	// not present and empty: dakr maps a missing key to a NULL column, and a zero value
	// would show up as a real phase duration in the report.
	for _, transition := range transitions {
		assert.NotContains(t, transition, "reservation_type")
		assert.NotEqual(t, "Registered", transition["condition"])
		assert.NotEqual(t, "Initialized", transition["condition"])
	}
}

// TestNodeLifecycleFallback_SkipsKarpenterManagedNodes pins the guard that keeps this
// path from double-reporting. karpenter_collector.go already emits a full four-phase
// lifecycle for these nodes, keyed on the NodeClaim name; a second, differently-keyed
// two-phase lifecycle for the same physical node would show up as an extra node in every
// percentile.
func TestNodeLifecycleFallback_SkipsKarpenterManagedNodes(t *testing.T) {
	created := time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC)

	t.Run("recognised by the node's own label, with no NodeClaim cache wired", func(t *testing.T) {
		c, resourceChan := newLifecycleTestNodeCollector()

		node := lifecycleNode("ip-10-0-1-5", created, true, created.Add(30*time.Second))
		node.Labels[karpenterNodePoolLabel] = "default"

		c.emitNodeLifecycleFallback(node, EventTypeAdd)

		assert.Empty(t, drainLifecycleTransitions(t, resourceChan),
			"the label is on the Node from the first time it is ever seen, so this cannot race the NodeClaim informer")
	})

	t.Run("recognised by the NodeClaim cache when labels are absent", func(t *testing.T) {
		c, resourceChan := newLifecycleTestNodeCollector()
		c.SetNodeClaimSource(clusterState{
			nodeClaims: map[string]*unstructured.Unstructured{
				"nodeclaim-abc12": disruptionNodeClaim("nodeclaim-abc12", nil),
			},
		}.sources().registry)

		c.emitNodeLifecycleFallback(
			lifecycleNode("ip-10-0-1-5", created, true, created.Add(30*time.Second)), EventTypeAdd)

		assert.Empty(t, drainLifecycleTransitions(t, resourceChan))
	})

	t.Run("an unmanaged node with no Karpenter anywhere is still reported", func(t *testing.T) {
		c, resourceChan := newLifecycleTestNodeCollector()

		c.emitNodeLifecycleFallback(
			lifecycleNode("ip-10-0-1-6", created, true, created.Add(30*time.Second)), EventTypeAdd)

		assert.Len(t, drainLifecycleTransitions(t, resourceChan), 2)
	})
}

// TestNodeLifecycleFallback_ReportsEachTransitionOnce covers the informer's actual
// behaviour: a Node produces a stream of updates, most of them irrelevant to this signal.
func TestNodeLifecycleFallback_ReportsEachTransitionOnce(t *testing.T) {
	c, resourceChan := newLifecycleTestNodeCollector()

	created := time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC)
	readyAt := created.Add(45 * time.Second)

	c.emitNodeLifecycleFallback(lifecycleNode("ip-10-0-3-7", created, false, created), EventTypeAdd)
	for i := 0; i < 5; i++ {
		c.emitNodeLifecycleFallback(lifecycleNode("ip-10-0-3-7", created, true, readyAt), EventTypeUpdate)
	}

	transitions := drainLifecycleTransitions(t, resourceChan)
	assert.Len(t, transitions, 2, "one Launched and one Ready, however many updates arrive")
}

// TestNodeLifecycleFallback_SeedsAlreadyReadyNodes covers the informer's initial list,
// where every node in the cluster arrives already Ready. Seeding those is what gives a
// freshly installed zxporter any history at all — but only when the Ready timestamp can
// still be the original one.
func TestNodeLifecycleFallback_SeedsAlreadyReadyNodes(t *testing.T) {
	created := time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC)

	t.Run("a node whose Ready timestamp is plausibly its first", func(t *testing.T) {
		c, resourceChan := newLifecycleTestNodeCollector()

		c.emitNodeLifecycleFallback(
			lifecycleNode("ip-10-0-4-1", created, true, created.Add(80*time.Second)), EventTypeAdd)

		transitions := drainLifecycleTransitions(t, resourceChan)
		require.Len(t, transitions, 2)
		require.NotNil(t, transitionByCondition(transitions, "Ready"))
	})

	t.Run("a node that has since flapped reports Launched only", func(t *testing.T) {
		c, resourceChan := newLifecycleTestNodeCollector()

		// Created three days ago, Ready since an hour ago: that Ready timestamp is a
		// NotReady→Ready flap, not the boot. Reporting it would fabricate a ~3 day
		// time-to-Ready that looks entirely legitimate downstream.
		c.emitNodeLifecycleFallback(
			lifecycleNode("ip-10-0-4-2", created, true, created.Add(72*time.Hour)), EventTypeAdd)

		transitions := drainLifecycleTransitions(t, resourceChan)
		require.Len(t, transitions, 1)
		assert.Equal(t, "Launched", transitions[0]["condition"],
			"declining to report is the right failure mode; reporting a wrong duration is not")
	})

	t.Run("a flap witnessed live is reported, however late", func(t *testing.T) {
		c, resourceChan := newLifecycleTestNodeCollector()

		// Same node, but this time the collector watched it go NotReady→Ready, so the
		// timestamp is unambiguous rather than inferred.
		c.emitNodeLifecycleFallback(lifecycleNode("ip-10-0-4-3", created, false, created), EventTypeAdd)
		c.emitNodeLifecycleFallback(
			lifecycleNode("ip-10-0-4-3", created, true, created.Add(72*time.Hour)), EventTypeUpdate)

		transitions := drainLifecycleTransitions(t, resourceChan)
		require.Len(t, transitions, 2)
		require.NotNil(t, transitionByCondition(transitions, "Ready"))
	})
}

// TestNodeLifecycleFallback_ForgetsDeletedNodes pins that the tracking map cannot grow
// with cluster churn.
func TestNodeLifecycleFallback_ForgetsDeletedNodes(t *testing.T) {
	c, resourceChan := newLifecycleTestNodeCollector()

	created := time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC)
	node := lifecycleNode("ip-10-0-5-3", created, true, created.Add(30*time.Second))

	c.emitNodeLifecycleFallback(node, EventTypeAdd)
	require.Len(t, drainLifecycleTransitions(t, resourceChan), 2)

	c.emitNodeLifecycleFallback(node, EventTypeDelete)
	assert.Empty(t, drainLifecycleTransitions(t, resourceChan),
		"a delete carries the last-known state, which was already reported")

	c.lifecycleMu.Lock()
	_, tracked := c.nodeLifecycle["ip-10-0-5-3"]
	c.lifecycleMu.Unlock()
	assert.False(t, tracked)
}
