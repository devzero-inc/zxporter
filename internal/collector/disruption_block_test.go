package collector

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The Karpenter behaviour these tests pin comes from sigs.k8s.io/karpenter v1.12.1, not
// from the docs:
//
//   - pkg/controllers/disruption/events.Blocked publishes DisruptionBlocked against the
//     Node AND the NodeClaim, never against the pod that caused the block — which is why
//     resolveDisruptedNode has to accept both kinds and why the blocking pod is re-derived
//     from the node rather than read off the event.
//   - pkg/controllers/state.StateNode.ValidatePodsDisruptable checks every pod's
//     do-not-disrupt annotation and returns on the first hit, and only then calls
//     pdb.Limits.CanEvictPods. That is the order classifyDisruptionBlocked reproduces.
//   - pkg/utils/pdb.Limits.isEvictable is what findBlockingPDB mirrors: same-namespace
//     only, LabelSelectorAsSelector over spec.selector, >1 matching PDB blocks outright,
//     and otherwise status.disruptionsAllowed == 0 blocks.
//   - pkg/controllers/nodeclaim/lifecycle stamps
//     karpenter.sh/nodeclaim-termination-timestamp with deletionTimestamp +
//     spec.terminationGracePeriod, which is the deadline terminationDue reads.

// stubCollector satisfies the parts of ResourceCollector the registry lookup needs but
// that this classification never calls. Only the lookup methods on the embedding types
// carry behaviour.
type stubCollector struct {
	collectorType string
}

func (s *stubCollector) Start(context.Context) error                    { return nil }
func (s *stubCollector) Stop() error                                    { return nil }
func (s *stubCollector) GetResourceChannel() <-chan []CollectedResource { return nil }
func (s *stubCollector) GetType() string                                { return s.collectorType }
func (s *stubCollector) IsAvailable(context.Context) bool               { return true }
func (s *stubCollector) AddResource(resource interface{}) error         { return nil }

type stubPDBCollector struct {
	stubCollector
	byNamespace map[string][]*policyv1.PodDisruptionBudget
}

func (s *stubPDBCollector) PDBsInNamespace(namespace string) []*policyv1.PodDisruptionBudget {
	return s.byNamespace[namespace]
}

type stubNodeCollector struct {
	stubCollector
	byNode map[string][]*corev1.Pod
}

func (s *stubNodeCollector) PodsOnNode(nodeName string) []*corev1.Pod {
	return s.byNode[nodeName]
}

type stubKarpenterCollector struct {
	stubCollector
	byName map[string]*unstructured.Unstructured
}

func (s *stubKarpenterCollector) NodeClaimByName(name string) *unstructured.Unstructured {
	return s.byName[name]
}

func (s *stubKarpenterCollector) NodeClaimForNode(nodeName string) *unstructured.Unstructured {
	for _, nodeClaim := range s.byName {
		if claimed, _, _ := unstructured.NestedString(nodeClaim.Object, "status", "nodeName"); claimed == nodeName {
			return nodeClaim
		}
	}
	return nil
}

// stubRegistry stands in for *CollectionManager. A missing entry returns nil, which is how
// a disabled or not-yet-started collector presents.
type stubRegistry map[string]ResourceCollector

func (r stubRegistry) GetCollector(collectorType string) ResourceCollector {
	return r[collectorType]
}

// clusterState assembles the three collector caches a DisruptionBlocked event is
// classified against.
type clusterState struct {
	pdbs       map[string][]*policyv1.PodDisruptionBudget
	podsByNode map[string][]*corev1.Pod
	nodeClaims map[string]*unstructured.Unstructured
}

func (c clusterState) sources() *disruptionSources {
	registry := stubRegistry{}
	if c.pdbs != nil {
		registry["pod_disruption_budget"] = &stubPDBCollector{
			stubCollector: stubCollector{collectorType: "pod_disruption_budget"},
			byNamespace:   c.pdbs,
		}
	}
	if c.podsByNode != nil {
		registry["node"] = &stubNodeCollector{
			stubCollector: stubCollector{collectorType: "node"},
			byNode:        c.podsByNode,
		}
	}
	if c.nodeClaims != nil {
		registry["karpenter"] = &stubKarpenterCollector{
			stubCollector: stubCollector{collectorType: "karpenter"},
			byName:        c.nodeClaims,
		}
	}
	return &disruptionSources{registry: registry}
}

// disruptionBlockedEvent builds the event exactly as Karpenter writes it: reported by
// "karpenter", involvedObject is the Node (the NodeClaim copy is the other half of the
// same publish), and the message names the blocker only in free text this classification
// deliberately ignores.
func disruptionBlockedEvent(nodeName, message string) *corev1.Event {
	return &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      nodeName + ".17ca11b2c3d4e5f6",
		},
		InvolvedObject: corev1.ObjectReference{
			Kind: "Node",
			Name: nodeName,
		},
		ReportingController: "karpenter",
		Reason:              "DisruptionBlocked",
		Type:                corev1.EventTypeNormal,
		Message:             message,
		Count:               1,
		LastTimestamp:       metav1.NewTime(time.Now()),
	}
}

func blockedPod(namespace, name string, podLabels, annotations map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   namespace,
			Name:        name,
			Labels:      podLabels,
			Annotations: annotations,
		},
	}
}

func blockingPDB(namespace, name string, matchLabels map[string]string, disruptionsAllowed int32) *policyv1.PodDisruptionBudget {
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: matchLabels},
		},
		Status: policyv1.PodDisruptionBudgetStatus{DisruptionsAllowed: disruptionsAllowed},
	}
}

// disruptionNodeClaimNodeName is the bound node name every test fixture in this file uses —
// none of the classification behavior under test depends on its value, only on whether a
// NodeClaim resolves to a Node at all.
const disruptionNodeClaimNodeName = "ip-10-0-1-5"

func disruptionNodeClaim(name string, annotations map[string]string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "karpenter.sh/v1",
			"kind":       "NodeClaim",
			"metadata": map[string]interface{}{
				"name":        name,
				"annotations": toUnstructuredMap(annotations),
			},
			"status": map[string]interface{}{"nodeName": disruptionNodeClaimNodeName},
		},
	}
}

func toUnstructuredMap(in map[string]string) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func TestClassifyDisruptionBlocked(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

	t.Run("pdb with no disruptions allowed", func(t *testing.T) {
		state := clusterState{
			podsByNode: map[string][]*corev1.Pod{
				"ip-10-0-1-5": {blockedPod("payments", "checkout-7d9f8b", map[string]string{"app": "checkout"}, nil)},
			},
			pdbs: map[string][]*policyv1.PodDisruptionBudget{
				"payments": {blockingPDB("payments", "checkout-pdb", map[string]string{"app": "checkout"}, 0)},
			},
		}

		got := classifyDisruptionBlocked(
			disruptionBlockedEvent("ip-10-0-1-5", "Cannot disrupt Node: pdb prevents pod evictions"),
			state.sources(), now,
		)

		assert.Equal(t, disruptionBlockReasonPDBViolation, got.Reason)
		require.NotNil(t, got.BlockingPDBName)
		assert.Equal(t, "payments/checkout-pdb", *got.BlockingPDBName)
	})

	// A strict PDB that is currently satisfied is not blocking anything. Reading
	// status.disruptionsAllowed rather than the spec is what keeps it out of the bucket.
	t.Run("pdb with budget to spare does not block", func(t *testing.T) {
		state := clusterState{
			podsByNode: map[string][]*corev1.Pod{
				"ip-10-0-1-5": {blockedPod("payments", "checkout-7d9f8b", map[string]string{"app": "checkout"}, nil)},
			},
			pdbs: map[string][]*policyv1.PodDisruptionBudget{
				"payments": {blockingPDB("payments", "checkout-pdb", map[string]string{"app": "checkout"}, 2)},
			},
		}

		got := classifyDisruptionBlocked(disruptionBlockedEvent("ip-10-0-1-5", ""), state.sources(), now)

		assert.Equal(t, disruptionBlockReasonOther, got.Reason)
		assert.Nil(t, got.BlockingPDBName)
	})

	// A PDB never selects across namespaces, so an identically-labelled pod elsewhere is
	// not covered by it.
	t.Run("pdb in another namespace does not block", func(t *testing.T) {
		state := clusterState{
			podsByNode: map[string][]*corev1.Pod{
				"ip-10-0-1-5": {blockedPod("payments", "checkout-7d9f8b", map[string]string{"app": "checkout"}, nil)},
			},
			pdbs: map[string][]*policyv1.PodDisruptionBudget{
				"search": {blockingPDB("search", "checkout-pdb", map[string]string{"app": "checkout"}, 0)},
			},
		}

		got := classifyDisruptionBlocked(disruptionBlockedEvent("ip-10-0-1-5", ""), state.sources(), now)

		assert.Equal(t, disruptionBlockReasonOther, got.Reason)
	})

	// matchExpressions is why the selector is compiled rather than compared as a map.
	t.Run("pdb selector matchExpressions", func(t *testing.T) {
		selectorPDB := blockingPDB("payments", "tier-pdb", nil, 0)
		selectorPDB.Spec.Selector = &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      "tier",
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{"critical", "high"},
			}},
		}

		state := clusterState{
			podsByNode: map[string][]*corev1.Pod{
				"ip-10-0-1-5": {blockedPod("payments", "checkout-7d9f8b", map[string]string{"tier": "critical"}, nil)},
			},
			pdbs: map[string][]*policyv1.PodDisruptionBudget{"payments": {selectorPDB}},
		}

		got := classifyDisruptionBlocked(disruptionBlockedEvent("ip-10-0-1-5", ""), state.sources(), now)

		assert.Equal(t, disruptionBlockReasonPDBViolation, got.Reason)
		require.NotNil(t, got.BlockingPDBName)
		assert.Equal(t, "payments/tier-pdb", *got.BlockingPDBName)
	})

	// The eviction API refuses to evict a pod covered by more than one PDB no matter what
	// those PDBs allow, so both having budget is still a block.
	t.Run("pod covered by two pdbs is blocked regardless of budget", func(t *testing.T) {
		state := clusterState{
			podsByNode: map[string][]*corev1.Pod{
				"ip-10-0-1-5": {blockedPod("payments", "checkout-7d9f8b", map[string]string{"app": "checkout"}, nil)},
			},
			pdbs: map[string][]*policyv1.PodDisruptionBudget{
				"payments": {
					blockingPDB("payments", "checkout-pdb", map[string]string{"app": "checkout"}, 5),
					blockingPDB("payments", "blanket-pdb", map[string]string{"app": "checkout"}, 5),
				},
			},
		}

		got := classifyDisruptionBlocked(disruptionBlockedEvent("ip-10-0-1-5", ""), state.sources(), now)

		assert.Equal(t, disruptionBlockReasonPDBViolation, got.Reason)
		require.NotNil(t, got.BlockingPDBName)
	})

	// AlwaysAllow lets an unready pod be evicted outside the budget, so a zero budget is
	// not a block for that pod.
	t.Run("unhealthy pod eviction policy AlwaysAllow releases an unready pod", func(t *testing.T) {
		alwaysAllow := policyv1.AlwaysAllow
		lenient := blockingPDB("payments", "checkout-pdb", map[string]string{"app": "checkout"}, 0)
		lenient.Spec.UnhealthyPodEvictionPolicy = &alwaysAllow

		unready := blockedPod("payments", "checkout-7d9f8b", map[string]string{"app": "checkout"}, nil)
		unready.Status.Conditions = []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionFalse},
		}

		state := clusterState{
			podsByNode: map[string][]*corev1.Pod{"ip-10-0-1-5": {unready}},
			pdbs:       map[string][]*policyv1.PodDisruptionBudget{"payments": {lenient}},
		}

		got := classifyDisruptionBlocked(disruptionBlockedEvent("ip-10-0-1-5", ""), state.sources(), now)

		assert.Equal(t, disruptionBlockReasonOther, got.Reason)
	})

	t.Run("pod with do-not-disrupt annotation", func(t *testing.T) {
		state := clusterState{
			podsByNode: map[string][]*corev1.Pod{
				"ip-10-0-1-5": {blockedPod("payments", "ledger-0", nil, map[string]string{
					"karpenter.sh/do-not-disrupt": "true",
				})},
			},
		}

		got := classifyDisruptionBlocked(disruptionBlockedEvent("ip-10-0-1-5", ""), state.sources(), now)

		assert.Equal(t, disruptionBlockReasonDoNotDisruptAnnotation, got.Reason)
		assert.Nil(t, got.BlockingPDBName)
	})

	// Only the literal "true" opts out, matching Karpenter's own check — "false" or a
	// typo'd value must not silently protect a pod.
	t.Run("do-not-disrupt annotation set to false does not block", func(t *testing.T) {
		state := clusterState{
			podsByNode: map[string][]*corev1.Pod{
				"ip-10-0-1-5": {blockedPod("payments", "ledger-0", nil, map[string]string{
					"karpenter.sh/do-not-disrupt": "false",
				})},
			},
		}

		got := classifyDisruptionBlocked(disruptionBlockedEvent("ip-10-0-1-5", ""), state.sources(), now)

		assert.Equal(t, disruptionBlockReasonOther, got.Reason)
	})

	// Cluster Autoscaler's spelling of the same opt-out
	// (cluster-autoscaler.kubernetes.io/safe-to-evict: "false", which CAS records as the
	// BlockedByPod/NotSafeToEvictAnnotation unremovable reason) shares the
	// DoNotDisruptAnnotation bucket rather than getting one of its own: the annotation
	// differs per autoscaler but the operator-facing fact is identical, and splitting them
	// would make one misconfiguration look like two problems on a cluster running both.
	//
	// It is checked on the Karpenter path too because the annotation is a property of the
	// POD — it predates Karpenter and plenty of charts set it unconditionally — so a pod
	// carrying it on a Karpenter-managed node really is one an operator asked not to move.
	t.Run("pod with safe-to-evict false", func(t *testing.T) {
		state := clusterState{
			podsByNode: map[string][]*corev1.Pod{
				"ip-10-0-1-5": {blockedPod("payments", "ledger-0", nil, map[string]string{
					"cluster-autoscaler.kubernetes.io/safe-to-evict": "false",
				})},
			},
		}

		got := classifyDisruptionBlocked(disruptionBlockedEvent("ip-10-0-1-5", ""), state.sources(), now)

		assert.Equal(t, disruptionBlockReasonDoNotDisruptAnnotation, got.Reason)
		assert.Nil(t, got.BlockingPDBName)
	})

	// "true" is the OPPOSITE instruction — it opts a pod INTO eviction, which is how
	// kube-system pods that would otherwise be unmovable get drained — so anything looser
	// than an exact "false" match would invert the meaning of a very common annotation.
	t.Run("safe-to-evict true does not block", func(t *testing.T) {
		state := clusterState{
			podsByNode: map[string][]*corev1.Pod{
				"ip-10-0-1-5": {blockedPod("kube-system", "coredns-0", nil, map[string]string{
					"cluster-autoscaler.kubernetes.io/safe-to-evict": "true",
				})},
			},
		}

		got := classifyDisruptionBlocked(disruptionBlockedEvent("ip-10-0-1-5", ""), state.sources(), now)

		assert.Equal(t, disruptionBlockReasonOther, got.Reason)
	})

	// One blocking pod blocks the whole node, whichever annotation it used and wherever it
	// sits in the node's pod list.
	t.Run("safe-to-evict false alongside unannotated pods", func(t *testing.T) {
		state := clusterState{
			podsByNode: map[string][]*corev1.Pod{
				"ip-10-0-1-5": {
					blockedPod("payments", "checkout-0", nil, nil),
					blockedPod("payments", "ledger-0", nil, map[string]string{
						"cluster-autoscaler.kubernetes.io/safe-to-evict": "false",
					}),
				},
			},
		}

		got := classifyDisruptionBlocked(disruptionBlockedEvent("ip-10-0-1-5", ""), state.sources(), now)

		assert.Equal(t, disruptionBlockReasonDoNotDisruptAnnotation, got.Reason)
	})

	// ValidateNodeDisruptable checks the node-level annotation before it ever looks at
	// pods, so a node opted out with no interesting pods on it is still the annotation.
	t.Run("do-not-disrupt annotation on the nodeclaim", func(t *testing.T) {
		state := clusterState{
			podsByNode: map[string][]*corev1.Pod{"ip-10-0-1-5": {}},
			nodeClaims: map[string]*unstructured.Unstructured{
				"default-abc12": disruptionNodeClaim("default-abc12", map[string]string{
					"karpenter.sh/do-not-disrupt": "true",
				}),
			},
		}

		got := classifyDisruptionBlocked(disruptionBlockedEvent("ip-10-0-1-5", ""), state.sources(), now)

		assert.Equal(t, disruptionBlockReasonDoNotDisruptAnnotation, got.Reason)
	})

	// Karpenter's own order: ValidatePodsDisruptable returns on the first annotated pod
	// and only reaches the PDB check if there is none.
	t.Run("do-not-disrupt outranks a blocking pdb", func(t *testing.T) {
		state := clusterState{
			podsByNode: map[string][]*corev1.Pod{
				"ip-10-0-1-5": {
					blockedPod("payments", "ledger-0", nil, map[string]string{"karpenter.sh/do-not-disrupt": "true"}),
				},
			},
			pdbs: map[string][]*policyv1.PodDisruptionBudget{
				"payments": {blockingPDB("payments", "ledger-pdb", map[string]string{}, 0)},
			},
		}

		got := classifyDisruptionBlocked(disruptionBlockedEvent("ip-10-0-1-5", ""), state.sources(), now)

		assert.Equal(t, disruptionBlockReasonDoNotDisruptAnnotation, got.Reason)
		assert.Nil(t, got.BlockingPDBName, "no PDB name when the PDB is not the reported reason")
	})

	t.Run("termination grace period already elapsed", func(t *testing.T) {
		state := clusterState{
			podsByNode: map[string][]*corev1.Pod{
				"ip-10-0-1-5": {blockedPod("payments", "ledger-0", nil, map[string]string{
					"karpenter.sh/do-not-disrupt": "true",
				})},
			},
			nodeClaims: map[string]*unstructured.Unstructured{
				"default-abc12": disruptionNodeClaim("default-abc12", map[string]string{
					"karpenter.sh/nodeclaim-termination-timestamp": now.Add(-time.Minute).Format(time.RFC3339),
				}),
			},
		}

		got := classifyDisruptionBlocked(disruptionBlockedEvent("ip-10-0-1-5", ""), state.sources(), now)

		// Outranks the do-not-disrupt pod on the same node: past the deadline Karpenter
		// force-terminates through it.
		assert.Equal(t, disruptionBlockReasonForceDisrupted, got.Reason)
	})

	// The deadline having been *set* is not the same as it having passed — until then
	// Karpenter still respects the block, so the real reason must still be reported.
	t.Run("termination grace period still running reports the real blocker", func(t *testing.T) {
		state := clusterState{
			podsByNode: map[string][]*corev1.Pod{
				"ip-10-0-1-5": {blockedPod("payments", "ledger-0", nil, map[string]string{
					"karpenter.sh/do-not-disrupt": "true",
				})},
			},
			nodeClaims: map[string]*unstructured.Unstructured{
				"default-abc12": disruptionNodeClaim("default-abc12", map[string]string{
					"karpenter.sh/nodeclaim-termination-timestamp": now.Add(time.Hour).Format(time.RFC3339),
				}),
			},
		}

		got := classifyDisruptionBlocked(disruptionBlockedEvent("ip-10-0-1-5", ""), state.sources(), now)

		assert.Equal(t, disruptionBlockReasonDoNotDisruptAnnotation, got.Reason)
	})

	t.Run("unparseable termination timestamp is not a past-due deadline", func(t *testing.T) {
		state := clusterState{
			podsByNode: map[string][]*corev1.Pod{"ip-10-0-1-5": {}},
			nodeClaims: map[string]*unstructured.Unstructured{
				"default-abc12": disruptionNodeClaim("default-abc12", map[string]string{
					"karpenter.sh/nodeclaim-termination-timestamp": "not-a-timestamp",
				}),
			},
		}

		got := classifyDisruptionBlocked(disruptionBlockedEvent("ip-10-0-1-5", ""), state.sources(), now)

		assert.Equal(t, disruptionBlockReasonOther, got.Reason)
	})

	// Karpenter publishes the same block against the NodeClaim as well as the Node. Both
	// copies must classify identically, or one node's block would be counted twice under
	// two different reasons.
	t.Run("nodeclaim-involved copy classifies the same as the node copy", func(t *testing.T) {
		state := clusterState{
			podsByNode: map[string][]*corev1.Pod{
				"ip-10-0-1-5": {blockedPod("payments", "checkout-7d9f8b", map[string]string{"app": "checkout"}, nil)},
			},
			pdbs: map[string][]*policyv1.PodDisruptionBudget{
				"payments": {blockingPDB("payments", "checkout-pdb", map[string]string{"app": "checkout"}, 0)},
			},
			nodeClaims: map[string]*unstructured.Unstructured{
				"default-abc12": disruptionNodeClaim("default-abc12", nil),
			},
		}

		event := disruptionBlockedEvent("ip-10-0-1-5", "")
		event.InvolvedObject.Kind = "NodeClaim"
		event.InvolvedObject.Name = "default-abc12"

		got := classifyDisruptionBlocked(event, state.sources(), now)

		assert.Equal(t, disruptionBlockReasonPDBViolation, got.Reason)
		require.NotNil(t, got.BlockingPDBName)
		assert.Equal(t, "payments/checkout-pdb", *got.BlockingPDBName)
	})

	// The node-level blocks Karpenter rejects on before it ever looks at pods —
	// uninitialized, deleting, nominated for a pending pod. Nothing in the caches explains
	// them, and Other is the honest answer.
	t.Run("nothing on the node explains the block", func(t *testing.T) {
		state := clusterState{
			podsByNode: map[string][]*corev1.Pod{
				"ip-10-0-1-5": {blockedPod("payments", "checkout-7d9f8b", map[string]string{"app": "checkout"}, nil)},
			},
			pdbs: map[string][]*policyv1.PodDisruptionBudget{},
		}

		got := classifyDisruptionBlocked(
			disruptionBlockedEvent("ip-10-0-1-5", "Cannot disrupt Node: state node is nominated for a pending pod"),
			state.sources(), now,
		)

		assert.Equal(t, disruptionBlockReasonOther, got.Reason)
		assert.Nil(t, got.BlockingPDBName)
	})
}

// TestClassifyDisruptionBlocked_NeverUnclassified is the contract the empty ClickHouse
// column depends on: "" means the event predates this classification, so a live collector
// must never emit it. Degraded inputs have to land in Other, not in nothing.
func TestClassifyDisruptionBlocked_NeverUnclassified(t *testing.T) {
	now := time.Now()

	cases := []struct {
		name    string
		sources *disruptionSources
		event   *corev1.Event
	}{
		{
			name:    "no sources wired at all",
			sources: nil,
			event:   disruptionBlockedEvent("ip-10-0-1-5", ""),
		},
		{
			name:    "registry present but every collector disabled",
			sources: &disruptionSources{registry: stubRegistry{}},
			event:   disruptionBlockedEvent("ip-10-0-1-5", ""),
		},
		{
			name:    "sources present but node unknown to them",
			sources: clusterState{podsByNode: map[string][]*corev1.Pod{}}.sources(),
			event:   disruptionBlockedEvent("ip-10-0-9-9", ""),
		},
		{
			name:    "nodeclaim-involved event with no karpenter collector",
			sources: clusterState{podsByNode: map[string][]*corev1.Pod{}}.sources(),
			event: func() *corev1.Event {
				e := disruptionBlockedEvent("ip-10-0-1-5", "")
				e.InvolvedObject.Kind = "NodeClaim"
				e.InvolvedObject.Name = "default-abc12"
				return e
			}(),
		},
		{
			name:    "involved object is neither a Node nor a NodeClaim",
			sources: clusterState{podsByNode: map[string][]*corev1.Pod{}}.sources(),
			event: func() *corev1.Event {
				e := disruptionBlockedEvent("ip-10-0-1-5", "")
				e.InvolvedObject.Kind = "NodePool"
				return e
			}(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyDisruptionBlocked(tc.event, tc.sources, now)
			assert.Equal(t, disruptionBlockReasonOther, got.Reason)
			assert.NotEmpty(t, got.Reason)
		})
	}
}

func TestIsKarpenterDisruptionBlocked(t *testing.T) {
	t.Run("karpenter DisruptionBlocked", func(t *testing.T) {
		assert.True(t, isKarpenterDisruptionBlocked(disruptionBlockedEvent("n", "")))
	})

	t.Run("another karpenter reason", func(t *testing.T) {
		event := disruptionBlockedEvent("n", "")
		event.Reason = "DisruptionTerminating"
		assert.False(t, isKarpenterDisruptionBlocked(event))
	})

	// The cluster autoscaler also emits block-ish events; only Karpenter's carry the
	// NodeClaim semantics this classification reads.
	t.Run("another controller", func(t *testing.T) {
		event := disruptionBlockedEvent("n", "")
		event.ReportingController = "cluster-autoscaler"
		assert.False(t, isKarpenterDisruptionBlocked(event))
	})
}

// TestHandleEvent_EnrichesKarpenterDisruptionBlocked covers the emission path: the
// classification has to reach the wire on the existing Event resource, not a new one.
func TestHandleEvent_EnrichesKarpenterDisruptionBlocked(t *testing.T) {
	c, batchChan := newTestEventCollector()
	c.SetDisruptionSources(clusterState{
		podsByNode: map[string][]*corev1.Pod{
			"ip-10-0-1-5": {blockedPod("payments", "checkout-7d9f8b", map[string]string{"app": "checkout"}, nil)},
		},
		pdbs: map[string][]*policyv1.PodDisruptionBudget{
			"payments": {blockingPDB("payments", "checkout-pdb", map[string]string{"app": "checkout"}, 0)},
		},
	}.sources().registry)

	c.handleEvent(disruptionBlockedEvent("ip-10-0-1-5", "Cannot disrupt Node: pdb prevents pod evictions"), EventTypeAdd)

	resources := drainResources(t, batchChan)
	require.Len(t, resources[Event], 1, "classification rides on the existing Event resource, not a new one")

	payload, ok := resources[Event][0].Object.(*enrichedEvent)
	require.True(t, ok, "expected *enrichedEvent, got %T", resources[Event][0].Object)
	assert.Equal(t, disruptionBlockReasonPDBViolation, payload.DisruptionBlockReason)
	require.NotNil(t, payload.BlockingPDBName)
	assert.Equal(t, "payments/checkout-pdb", *payload.BlockingPDBName)
	assert.Equal(t, "DisruptionBlocked", payload.Reason, "the core/v1 Event must survive intact")
}

// TestHandleEvent_LeavesOtherEventsUnwrapped pins that this is not a change to the shape
// of every event: only Karpenter DisruptionBlocked events get the envelope.
func TestHandleEvent_LeavesOtherEventsUnwrapped(t *testing.T) {
	c, batchChan := newTestEventCollector()

	event := disruptionBlockedEvent("ip-10-0-1-5", "")
	event.Reason = "DisruptionTerminating"
	c.handleEvent(event, EventTypeAdd)

	resources := drainResources(t, batchChan)
	require.Len(t, resources[Event], 1)
	_, ok := resources[Event][0].Object.(*corev1.Event)
	assert.True(t, ok, "expected a bare *corev1.Event, got %T", resources[Event][0].Object)
}

// TestSetDisruptionSources_ConcurrentWithEvents covers the one ordering the reconciler
// cannot avoid: restartCollectors re-wires a *replaced* EventCollector after that
// collector's informer is already dispatching, so the write lands while handleEvent is
// reading. Only meaningful under -race.
func TestSetDisruptionSources_ConcurrentWithEvents(t *testing.T) {
	c, batchChan := newTestEventCollector()

	// Drain continuously; the channel is smaller than the event count.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range batchChan {
		}
	}()

	registry := clusterState{
		podsByNode: map[string][]*corev1.Pod{
			"ip-10-0-1-5": {blockedPod("payments", "ledger-0", nil, map[string]string{
				"karpenter.sh/do-not-disrupt": "true",
			})},
		},
	}.sources().registry

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			c.handleEvent(disruptionBlockedEvent("ip-10-0-1-5", ""), EventTypeAdd)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			c.SetDisruptionSources(registry)
		}
	}()
	wg.Wait()

	close(batchChan)
	<-done
}

// TestEnrichedEventWireCompatibility pins the property that lets a new collector and an
// old dakr (and vice versa) be deployed in either order: the envelope adds two keys and
// changes nothing else about the payload.
func TestEnrichedEventWireCompatibility(t *testing.T) {
	blocking := "payments/checkout-pdb"
	event := disruptionBlockedEvent("ip-10-0-1-5", "Cannot disrupt Node: pdb prevents pod evictions")

	t.Run("an old dakr decodes the envelope as a plain event", func(t *testing.T) {
		encoded, err := json.Marshal(&enrichedEvent{
			Event:                 event,
			DisruptionBlockReason: disruptionBlockReasonPDBViolation,
			BlockingPDBName:       &blocking,
		})
		require.NoError(t, err)

		var decoded corev1.Event
		require.NoError(t, json.Unmarshal(encoded, &decoded))
		assert.Equal(t, "DisruptionBlocked", decoded.Reason)
		assert.Equal(t, "ip-10-0-1-5", decoded.InvolvedObject.Name)
		assert.Equal(t, "karpenter", decoded.ReportingController)
	})

	t.Run("an unenriched event encodes identically to the bare object", func(t *testing.T) {
		bare, err := json.Marshal(event)
		require.NoError(t, err)
		wrapped, err := json.Marshal(&enrichedEvent{Event: event})
		require.NoError(t, err)

		assert.JSONEq(t, string(bare), string(wrapped))
	})
}

// BenchmarkClassifyDisruptionBlocked measures the classification's cost on the informer
// callback, for a full node (60 pods) in a namespace with 10 PDBs.
//
// The two cases bracket the cost, because findBlockingPDB compiles a label selector per
// (pod, PDB) candidate and stops at the first blocker: "blocked" pays 10 compiles and
// returns on the first pod, "no blocker" pays all 600 and is the real ceiling. Both run
// only on Karpenter DisruptionBlocked events — a per-node, dedupe-limited signal, orders
// of magnitude rarer than the FailedScheduling path next door.
func BenchmarkClassifyDisruptionBlocked(b *testing.B) {
	pods := make([]*corev1.Pod, 0, 60)
	for i := 0; i < 60; i++ {
		pods = append(pods, blockedPod("payments", "worker", map[string]string{"app": "worker"}, nil))
	}
	unrelatedPDBs := make([]*policyv1.PodDisruptionBudget, 0, 10)
	for i := 0; i < 10; i++ {
		unrelatedPDBs = append(unrelatedPDBs, blockingPDB("payments", "unrelated", map[string]string{"app": "other"}, 3))
	}
	blockingPDBs := append(append([]*policyv1.PodDisruptionBudget{}, unrelatedPDBs[:9]...),
		blockingPDB("payments", "worker-pdb", map[string]string{"app": "worker"}, 0))

	event := disruptionBlockedEvent("ip-10-0-1-5", "Cannot disrupt Node: pdb prevents pod evictions")
	now := time.Now()

	for _, tc := range []struct {
		name string
		pdbs []*policyv1.PodDisruptionBudget
	}{
		{"blocked", blockingPDBs},
		{"no-blocker", unrelatedPDBs},
	} {
		sources := clusterState{
			podsByNode: map[string][]*corev1.Pod{"ip-10-0-1-5": pods},
			pdbs:       map[string][]*policyv1.PodDisruptionBudget{"payments": tc.pdbs},
		}.sources()

		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = classifyDisruptionBlocked(event, sources, now)
			}
		})
	}
}
