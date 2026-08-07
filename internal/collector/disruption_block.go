// internal/collector/disruption_block.go
package collector

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
)

// Karpenter's event vocabulary for the disruption controller. Source of truth:
// sigs.k8s.io/karpenter/pkg/events/reason.go and pkg/controllers/disruption/events.
const (
	// karpenterReportingController is the reportingController Karpenter stamps on every
	// event it writes. dzkarp, DevZero's distribution, reports under the same value.
	karpenterReportingController = "karpenter"
	// disruptionBlockedReason is emitted when a node was considered for voluntary
	// disruption and rejected. Karpenter emits it against the Node and again against the
	// NodeClaim, never against the pod that caused the block.
	disruptionBlockedReason = "DisruptionBlocked"
)

// Karpenter object keys this classification reads. Source of truth:
// sigs.k8s.io/karpenter/pkg/apis/v1/labels.go.
const (
	// doNotDisruptAnnotation opts a pod — or a whole Node/NodeClaim — out of voluntary
	// disruption. Only the literal "true" counts, matching Karpenter's own check.
	doNotDisruptAnnotation = "karpenter.sh/do-not-disrupt"
	// nodeClaimTerminationTimestampAnnotation is stamped on a NodeClaim being deleted
	// whose NodePool set a terminationGracePeriod, and holds the RFC3339 instant
	// (deletionTimestamp + terminationGracePeriod) at which Karpenter stops respecting
	// PDBs and do-not-disrupt and force-terminates the remaining pods.
	nodeClaimTerminationTimestampAnnotation = "karpenter.sh/nodeclaim-termination-timestamp"
)

// Cluster Autoscaler's equivalent opt-out. Source of truth:
// k8s.io/autoscaler/cluster-autoscaler/utils/drain (PodSafeToEvictKey), vendored in this
// repo at autoscale/clusterautoscaler/utils/drain/drain.go.
const (
	// safeToEvictAnnotation, set to the literal "false", makes Cluster Autoscaler refuse
	// to drain the pod, which blocks scale-down of the whole node it runs on. It is the
	// direct analogue of karpenter.sh/do-not-disrupt: CAS records it as the
	// BlockedByPod/NotSafeToEvictAnnotation unremovable reason.
	//
	// Only the literal "false" counts. The annotation is also used with "true" to opt a
	// pod INTO eviction (e.g. kube-system pods that would otherwise be unmovable), which
	// is the opposite fact and must not be read as a block.
	safeToEvictAnnotation = "cluster-autoscaler.kubernetes.io/safe-to-evict"
)

// The disruption_block_reason values. These strings are the wire contract: they are what
// zxporter puts on the RESOURCE_TYPE_EVENT payload, what dakr's converter gates against
// models.DisruptionBlockReason, and what lands verbatim in the
// k8s_events.disruption_block_reason ClickHouse column.
//
// Note what is NOT here: an "unclassified" value. Every DisruptionBlocked event gets one
// of these values, so an empty column means "ingested before this classification existed",
// never "we looked and gave up" — that case is disruptionBlockReasonOther.
//
// The set is shared by both autoscalers and dakr distinguishes them by the row's
// reporting_controller, not by a second enum (see models.DisruptionBlockReason). Three of
// the buckets are single-autoscaler by construction: ForceDisrupted only Karpenter can
// produce, NotUnneededLongEnough and NoPlaceToMovePods only Cluster Autoscaler can.
//
// dakr REJECTS an event carrying any other value — k8sconverters gates the field against
// models.AllDisruptionBlockReasons and drops the whole item on an unknown one — so these
// strings are exact, case-sensitive contract, not labels.
const (
	disruptionBlockReasonPDBViolation           = "PDBViolation"
	disruptionBlockReasonDoNotDisruptAnnotation = "DoNotDisruptAnnotation"
	disruptionBlockReasonForceDisrupted         = "ForceDisrupted"
	// disruptionBlockReasonNotUnneededLongEnough mirrors Cluster Autoscaler's
	// NotUnneededLongEnough UnremovableReason: the node is underutilized but has not been
	// so for --scale-down-unneeded-time yet. A waiting state, not a misconfiguration.
	disruptionBlockReasonNotUnneededLongEnough = "NotUnneededLongEnough"
	// disruptionBlockReasonNoPlaceToMovePods mirrors Cluster Autoscaler's
	// NoPlaceToMovePods UnremovableReason: the drain simulation found nowhere for the
	// node's pods to go. Spelled exactly as upstream spells it (see the vendored
	// autoscale/clusterautoscaler/simulator/cluster.go) — NOT "NoPlaceToMoveToPods",
	// which is not a string Cluster Autoscaler uses anywhere.
	disruptionBlockReasonNoPlaceToMovePods = "NoPlaceToMovePods"
	disruptionBlockReasonOther             = "Other"
)

// pdbSource, podSource and nodeClaimSource are the read-only views the classifier needs
// of caches that live in three other collectors. Declared as interfaces so the classifier
// depends on the three lookups rather than on the collectors themselves, and so a test
// can supply cluster state without an informer.
type pdbSource interface {
	PDBsInNamespace(namespace string) []*policyv1.PodDisruptionBudget
}

type podSource interface {
	PodsOnNode(nodeName string) []*corev1.Pod
}

type nodeClaimSource interface {
	NodeClaimByName(name string) *unstructured.Unstructured
	NodeClaimForNode(nodeName string) *unstructured.Unstructured
}

// collectorRegistry is the subset of *CollectionManager the classifier uses.
type collectorRegistry interface {
	GetCollector(collectorType string) ResourceCollector
}

// disruptionSources resolves the three lookups through the CollectionManager on every
// call rather than holding the collectors directly.
//
// That indirection is the point: the reconciler replaces individual collectors in place
// when a CollectionPolicy changes (see restartCollectors), so a pointer captured at wiring
// time would silently go stale and start reading a stopped informer's frozen cache. A
// GetCollector lookup is a mutex-guarded map read, and the only caller is a
// DisruptionBlocked event, so resolving per event costs nothing worth optimising.
//
// A nil *disruptionSources, or any individual collector being absent or disabled, degrades
// to "that input is unknown" rather than failing — see classifyDisruptionBlocked.
type disruptionSources struct {
	registry collectorRegistry
}

func (s *disruptionSources) pdbs() pdbSource {
	if s == nil || s.registry == nil {
		return nil
	}
	source, _ := s.registry.GetCollector("pod_disruption_budget").(pdbSource)
	return source
}

func (s *disruptionSources) pods() podSource {
	if s == nil || s.registry == nil {
		return nil
	}
	source, _ := s.registry.GetCollector("node").(podSource)
	return source
}

func (s *disruptionSources) nodeClaims() nodeClaimSource {
	if s == nil || s.registry == nil {
		return nil
	}
	source, _ := s.registry.GetCollector("karpenter").(nodeClaimSource)
	return source
}

// disruptionBlockClassification is the outcome of classifying one DisruptionBlocked event.
type disruptionBlockClassification struct {
	// Reason is always one of the four disruptionBlockReason* values.
	Reason string
	// BlockingPDBName is "namespace/name" of the PDB that blocked eviction, set only
	// alongside PDBViolation. Nil rather than "" so dakr can write a real NULL: "no PDB was
	// involved" is not the same fact as "a PDB with an empty name".
	BlockingPDBName *string
}

// isKarpenterDisruptionBlocked reports whether an event is the Karpenter DisruptionBlocked
// event this classification applies to.
func isKarpenterDisruptionBlocked(event *corev1.Event) bool {
	return event != nil &&
		event.ReportingController == karpenterReportingController &&
		event.Reason == disruptionBlockedReason
}

// classifyDisruptionBlocked works out why Karpenter could not disrupt a node, from live
// cluster state rather than from the event's message.
//
// The message is not used on purpose. Karpenter does name the blocking pod or PDB in it,
// but the phrasing is unversioned and has changed shape across releases (it is built by
// fmt.Sprintf over an error string), and customer clusters span a wide version range. The
// three inputs below are typed API fields that have not moved.
//
// The event itself identifies the *node*, never the pod: Karpenter publishes
// DisruptionBlocked against the Node and the NodeClaim (see
// pkg/controllers/disruption/events.Blocked), so which pod blocked has to be re-derived
// from the pods on that node.
//
// Precedence, and why:
//
//  1. ForceDisrupted — the NodeClaim's terminationGracePeriod deadline has already
//     elapsed, at which point Karpenter force-terminates past both PDBs and
//     do-not-disrupt. Whatever else is true about the node, the block is about to stop
//     mattering, so this outranks the reasons it overrides.
//  2. DoNotDisruptAnnotation (either autoscaler's spelling — see
//     podOptedOutOfDisruption), then 3. PDBViolation — in that order because it is the order
//     Karpenter itself evaluates them (state.StateNode.ValidatePodsDisruptable checks every
//     pod's annotation and returns on the first hit, and only then consults PDBs), so a
//     node with both produces the annotation event upstream too.
//  4. Other — the node-level blocks that are neither: uninitialized, already deleting,
//     nominated for a pending pod, missing its NodePool label. Also where an
//     unresolvable node, an unavailable collector, or a stale cache lands. It is a real
//     classification, not a failure: the verbatim message survives in k8s_events.message,
//     which is the only place the detail exists.
//
// Never returns an empty Reason.
func classifyDisruptionBlocked(
	event *corev1.Event,
	sources *disruptionSources,
	now time.Time,
) disruptionBlockClassification {
	other := disruptionBlockClassification{Reason: disruptionBlockReasonOther}

	nodeName, nodeClaim := resolveDisruptedNode(event, sources.nodeClaims())

	// The node-level annotation blocks disruption on its own, without any pod being
	// involved, and Karpenter checks it (ValidateNodeDisruptable) before it ever looks at
	// pods. Check it against the NodeClaim, which is where the annotation is propagated.
	nodeClaimBlocked := nodeClaim != nil &&
		nodeClaim.GetAnnotations()[doNotDisruptAnnotation] == "true"

	if terminationDue(nodeClaim, now) {
		return disruptionBlockClassification{Reason: disruptionBlockReasonForceDisrupted}
	}

	if nodeName == "" {
		return other
	}

	podLookup := sources.pods()
	if podLookup == nil {
		if nodeClaimBlocked {
			return disruptionBlockClassification{Reason: disruptionBlockReasonDoNotDisruptAnnotation}
		}
		return other
	}
	pods := podLookup.PodsOnNode(nodeName)

	for _, pod := range pods {
		if podOptedOutOfDisruption(pod) {
			return disruptionBlockClassification{Reason: disruptionBlockReasonDoNotDisruptAnnotation}
		}
	}
	if nodeClaimBlocked {
		return disruptionBlockClassification{Reason: disruptionBlockReasonDoNotDisruptAnnotation}
	}

	if pdbName, ok := findBlockingPDB(pods, sources.pdbs()); ok {
		return disruptionBlockClassification{
			Reason:          disruptionBlockReasonPDBViolation,
			BlockingPDBName: &pdbName,
		}
	}

	return other
}

// podOptedOutOfDisruption reports whether a pod has been pinned in place by an operator,
// under either autoscaler's spelling of that opt-out. One such pod blocks its whole node.
//
// Both spellings land in the same DoNotDisruptAnnotation bucket deliberately: the
// annotation differs per autoscaler but the operator-facing fact ("somebody pinned this
// workload") is identical, and splitting them would make one misconfiguration look like
// two different problems on a cluster running both autoscalers — which is a first-class
// case here, not an edge case. See models.DisruptionBlockReasonDoNotDisruptAnnotation.
//
// Checking the Cluster Autoscaler annotation on the Karpenter path is not a mismatch: the
// annotation is a property of the POD, it long predates Karpenter, and plenty of charts
// set it unconditionally. A pod carrying it on a Karpenter-managed node really is one an
// operator asked not to move.
func podOptedOutOfDisruption(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	if pod.Annotations[doNotDisruptAnnotation] == "true" {
		return true
	}
	// Only the literal "false" blocks. "true" is the opposite instruction — it opts a pod
	// INTO eviction — so a substring or non-empty check here would invert the meaning.
	return pod.Annotations[safeToEvictAnnotation] == "false"
}

// resolveDisruptedNode turns the event's involvedObject into the Node name the block is
// about and the NodeClaim backing it. Karpenter emits DisruptionBlocked twice for the same
// block — once against each — so both kinds arrive and either has to resolve to the same
// pair.
//
// Either half may come back zero: the Karpenter collector is not running, the CRDs are
// absent, or the NodeClaim has not bound a Node yet (which is itself one of the block
// reasons, and lands in Other).
const (
	involvedObjectKindNode      = "Node"
	involvedObjectKindNodeClaim = "NodeClaim"
)

func resolveDisruptedNode(
	event *corev1.Event,
	nodeClaims nodeClaimSource,
) (string, *unstructured.Unstructured) {
	switch event.InvolvedObject.Kind {
	case involvedObjectKindNode:
		nodeName := event.InvolvedObject.Name
		if nodeClaims == nil {
			return nodeName, nil
		}
		return nodeName, nodeClaims.NodeClaimForNode(nodeName)
	case involvedObjectKindNodeClaim:
		if nodeClaims == nil {
			return "", nil
		}
		nodeClaim := nodeClaims.NodeClaimByName(event.InvolvedObject.Name)
		if nodeClaim == nil {
			return "", nil
		}
		nodeName, _, _ := unstructured.NestedString(nodeClaim.Object, "status", "nodeName")
		return nodeName, nodeClaim
	default:
		return "", nil
	}
}

// terminationDue reports whether the NodeClaim's terminationGracePeriod deadline has
// already passed.
//
// It reads Karpenter's own precomputed deadline
// (karpenter.sh/nodeclaim-termination-timestamp = deletionTimestamp +
// spec.terminationGracePeriod, stamped by the nodeclaim lifecycle controller) rather than
// recomputing it from the NodePool. That avoids a NodePool lookup entirely and, more
// importantly, avoids disagreeing with Karpenter about the instant: the node health
// controller can stamp the same annotation with a different value for a forced
// termination, and that value is the one that governs.
//
// Absent annotation means either no terminationGracePeriod is configured or termination
// has not begun — in both cases nothing is being force-disrupted.
func terminationDue(nodeClaim *unstructured.Unstructured, now time.Time) bool {
	if nodeClaim == nil {
		return false
	}

	raw, ok := nodeClaim.GetAnnotations()[nodeClaimTerminationTimestampAnnotation]
	if !ok || raw == "" {
		return false
	}

	deadline, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		// An unparseable deadline is not a past-due one. Reporting Other keeps the
		// ForceDisrupted bucket meaning "we saw a deadline and it had passed".
		return false
	}
	return !deadline.After(now)
}

// findBlockingPDB returns the "namespace/name" of a PodDisruptionBudget that currently
// prevents one of the node's pods from being evicted.
//
// This mirrors sigs.k8s.io/karpenter/pkg/utils/pdb's CanEvictPods, so the classification
// agrees with the decision Karpenter actually made:
//
//   - Only PDBs in the pod's OWN namespace are considered; a PDB never selects across
//     namespaces.
//   - The PDB's spec.selector is compiled with metav1.LabelSelectorAsSelector and matched
//     against the pod's labels. That handles matchExpressions, and it makes an empty
//     selector ({}) match every pod in the namespace while a nil selector matches none —
//     the standard Kubernetes distinction, which a naive matchLabels comparison gets
//     backwards.
//   - A pod selected by MORE THAN ONE PDB is blocked regardless of what those PDBs allow,
//     because the eviction API refuses to evict such a pod at all. The first matching PDB
//     is named, since there is no single culprit.
//   - Otherwise the PDB blocks when status.disruptionsAllowed == 0 — the live budget, not
//     the spec'd minAvailable/maxUnavailable, so a PDB that is merely strict but currently
//     satisfied is correctly not reported.
//   - A PDB with unhealthyPodEvictionPolicy: AlwaysAllow does not block a pod that is not
//     Ready, since that pod can be evicted outside the budget.
//
// Pods are visited in the order the node collector holds them, which is map order and so
// not stable. With several blocking PDBs on one node the reported name can therefore vary
// between two events describing the same state — acceptable, because any of them is a
// true answer to "what is blocking this node", and the alternative (sorting the whole set
// on every event) buys determinism nothing consumes.
func findBlockingPDB(pods []*corev1.Pod, pdbs pdbSource) (string, bool) {
	if pdbs == nil {
		return "", false
	}

	// Selectors are compiled once per namespace, not once per (pod, PDB) pair. A full node
	// runs ~60 pods across a handful of namespaces, so the naive nesting recompiles the
	// same selector set dozens of times — and it does so precisely in the case that cannot
	// short-circuit (no PDB blocks anything), which is the common outcome for the
	// node-level blocks that end up as "Other".
	compiled := make(map[string][]compiledPDB, 4)

	for _, pod := range pods {
		if pod == nil {
			continue
		}

		candidates, seen := compiled[pod.Namespace]
		if !seen {
			candidates = compilePDBSelectors(pdbs.PDBsInNamespace(pod.Namespace))
			compiled[pod.Namespace] = candidates
		}

		podLabels := labels.Set(pod.Labels)

		var firstMatch *policyv1.PodDisruptionBudget
		var blocker *policyv1.PodDisruptionBudget
		matches := 0
		for _, candidate := range candidates {
			if !candidate.selector.Matches(podLabels) {
				continue
			}
			matches++
			if firstMatch == nil {
				firstMatch = candidate.pdb
			}
			if blocker == nil &&
				candidate.pdb.Status.DisruptionsAllowed == 0 &&
				!allowsEvictingUnreadyPod(candidate.pdb, pod) {
				blocker = candidate.pdb
			}
		}

		// More than one PDB on a pod blocks eviction outright, whatever those PDBs allow.
		if matches > 1 {
			return pdbKey(firstMatch), true
		}
		if blocker != nil {
			return pdbKey(blocker), true
		}
	}

	return "", false
}

// compiledPDB pairs a PDB with its compiled selector, so a namespace's selectors are
// parsed once per classification rather than once per pod.
type compiledPDB struct {
	pdb      *policyv1.PodDisruptionBudget
	selector labels.Selector
}

// compilePDBSelectors drops any PDB whose selector will not compile. That is unreachable
// through the API server, which validates the selector on write, so there is nothing
// actionable to report — and treating it as "matches everything" would invent a blocker.
func compilePDBSelectors(pdbs []*policyv1.PodDisruptionBudget) []compiledPDB {
	compiled := make([]compiledPDB, 0, len(pdbs))
	for _, pdb := range pdbs {
		if pdb == nil {
			continue
		}
		selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
		if err != nil {
			continue
		}
		compiled = append(compiled, compiledPDB{pdb: pdb, selector: selector})
	}
	return compiled
}

// allowsEvictingUnreadyPod reports whether the PDB lets this pod be evicted outside its
// budget because the pod is not Ready and the PDB opted into
// unhealthyPodEvictionPolicy: AlwaysAllow.
func allowsEvictingUnreadyPod(pdb *policyv1.PodDisruptionBudget, pod *corev1.Pod) bool {
	if pdb.Spec.UnhealthyPodEvictionPolicy == nil ||
		*pdb.Spec.UnhealthyPodEvictionPolicy != policyv1.AlwaysAllow {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionFalse {
			return true
		}
	}
	return false
}

func pdbKey(pdb *policyv1.PodDisruptionBudget) string {
	return pdb.Namespace + "/" + pdb.Name
}
