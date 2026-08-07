// internal/collector/cluster_autoscaler_status.go
package collector

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"
)

// Where the Cluster Autoscaler publishes its status, and under which key.
//
// Verified against k8s.io/autoscaler cluster-autoscaler/clusterstate/utils/status.go
// (WriteStatusConfigMap): the document goes in data["status"], and the ConfigMap carries a
// cluster-autoscaler.kubernetes.io/last-updated annotation with the same instant. The name
// is configurable through --status-config-map-name; the default below is what every
// managed distribution ships with, and it is the only one this collector watches.
const (
	casStatusConfigMapNamespace = "kube-system"
	casStatusConfigMapName      = "cluster-autoscaler-status"
	casStatusDataKey            = "status"
)

// The Cluster Autoscaler half of the disruption-block wire contract.
const (
	// clusterAutoscalerReportingController is the reportingComponent stamped on every
	// observation this collector synthesizes. It is the discriminator dakr's read path
	// filters on to tell a Cluster Autoscaler row from a Karpenter one, so it is contract,
	// not decoration.
	clusterAutoscalerReportingController = "cluster-autoscaler"

	// scaleDownBlockedReason is the event reason for a synthesized Cluster Autoscaler
	// block observation. Cluster Autoscaler has no per-node "I could not remove this"
	// event to piggyback on, so this reason is ours — deliberately one CAS never emits
	// itself, so a raw k8s_events reader can tell a synthesized row from a real CAS event.
	// See models.EventReasonScaleDownBlocked.
	scaleDownBlockedReason = "ScaleDownBlocked"

	// casBlockOwnerKindNodeGroup marks an observation that is NOT attributable to a single
	// node. dakr excludes these from the live blocked-NODE list and counts them separately
	// in the breakdown, which is what keeps a node group from masquerading as a node an
	// operator can go click on. See models.DisruptionBlockOwnerKindNodeGroup.
	casBlockOwnerKindNodeGroup = "NodeGroup"
)

// Status values from the ConfigMap that this collector reads. Verified against
// cluster-autoscaler/clusterstate/api/types.go, where they are spelled identically in the
// pre-1.30 (ClusterAutoscalerConditionStatus) and post-1.30 (machine-readable) formats.
const (
	casScaleDownCandidatesPresent = "CandidatesPresent"
)

const (
	// casMinBlockHold is how long a node group's scale-down must have been sitting on
	// candidates before this collector calls it blocked.
	//
	// It exists because CandidatesPresent on its own is NORMAL: Cluster Autoscaler marks a
	// node unneeded and then waits out --scale-down-unneeded-time (default 10m) before
	// removing it, so every healthy scale-down passes through this state. Reporting from
	// the first observation would flag routine scale-downs as blocked and — because dakr
	// keeps a node group in the live blocked list for an hour after the last observation —
	// leave them there long after the node was actually removed.
	//
	// The value matches Cluster Autoscaler's own --scale-down-unneeded-time default. It
	// cannot be read from the cluster (it is a process flag, not an API object), so a
	// cluster configured with a longer value will produce some early observations; that
	// direction is recoverable (the raw text is on the row) whereas suppressing a real
	// block is not.
	casMinBlockHold = 10 * time.Minute

	// casMaxMessageBytes bounds the raw text carried on an observation. The whole point of
	// the message is that it is the only recourse for an Other-classified row, so it is
	// generous — but a status document on a cluster with hundreds of node groups is
	// unbounded, and this rides the k8s_events pipeline alongside real events.
	casMaxMessageBytes = 4096
)

// casUIDNamespace is the UUIDv5 namespace for synthesized Cluster Autoscaler block
// observation UIDs. Any fixed UUID would do; this one is arbitrary and permanent.
//
// It must never change: the UID is dakr's fold key (GROUP BY cluster_id, uid), so
// regenerating it would split every in-flight block streak in two.
var casUIDNamespace = uuid.MustParse("6f0b1a3e-9a1a-4c2f-8f2a-5a2d0f8c1b74")

// casBlockObservation is one "the autoscaler wanted to remove a node and could not"
// observation extracted from the status document. It is the collector-internal shape; the
// wire shape is the enrichedEvent that casBlockEvent builds from it.
type casBlockObservation struct {
	// ObjectKind is "Node" when the document names a specific node, and
	// casBlockOwnerKindNodeGroup when it only describes a whole group.
	//
	// Upstream Cluster Autoscaler always produces the latter — see
	// parseClusterAutoscalerStatus — and NO code path here ever synthesizes a node name to
	// get the former. A node name that is not a real node would corrupt the live blocked
	// node panel with entries an operator cannot go look at.
	ObjectKind string
	// ObjectName is the node group identifier (or node name) exactly as the document
	// spells it. Cloud providers spell these very differently — an ASG name on AWS, a full
	// instance-group URL on GCE — and it is passed through untouched.
	ObjectName string
	// Reason is one of the disruptionBlockReason* values. Never empty.
	Reason string
	// Message is the verbatim section of the status document this observation came from.
	// It is never dropped: for an Other-classified row it is the only place the detail
	// exists.
	Message string
	// StreakStart is when Cluster Autoscaler says this scale-down condition last changed,
	// i.e. when the block began. Zero when the document did not carry a usable timestamp,
	// in which case the collector substitutes its own first-observation time.
	//
	// It is part of the UID, which is what makes a block that clears and later recurs a
	// second streak rather than one long one — and, because it comes from the document
	// rather than from the collector's memory, keeps the UID stable across a zxporter
	// restart.
	StreakStart time.Time
}

// casStatus is the machine-readable status document Cluster Autoscaler 1.30+ writes.
// Mirrors cluster-autoscaler/clusterstate/api.ClusterAutoscalerStatus, narrowed to the
// fields this collector reads; unknown keys are ignored, which is what keeps a newer CAS
// from breaking the parse.
type casStatus struct {
	Time             string         `json:"time"`
	AutoscalerStatus string         `json:"autoscalerStatus"`
	ClusterWide      casClusterWide `json:"clusterWide"`
	NodeGroups       []casNodeGroup `json:"nodeGroups"`
}

type casClusterWide struct {
	Health    casHealth    `json:"health"`
	ScaleDown casScaleDown `json:"scaleDown"`
}

type casHealth struct {
	Status        string      `json:"status"`
	LastProbeTime metav1.Time `json:"lastProbeTime"`
}

type casNodeGroup struct {
	Name      string       `json:"name"`
	ScaleDown casScaleDown `json:"scaleDown"`
}

type casScaleDown struct {
	Status             string      `json:"status"`
	Candidates         int         `json:"candidates"`
	LastProbeTime      metav1.Time `json:"lastProbeTime"`
	LastTransitionTime metav1.Time `json:"lastTransitionTime"`
}

// casParseResult is everything one sweep learned from the status document.
type casParseResult struct {
	// Observations are the blocks to report, already classified.
	Observations []casBlockObservation
	// ObservedAt is the instant the document itself was written, when it said so, and the
	// collector's clock otherwise. It becomes the event's lastTimestamp, which is what
	// dakr's "blocked now" freshness check reads — so a Cluster Autoscaler that has
	// stopped writing correctly ages its blocks out instead of pinning them live forever.
	ObservedAt time.Time
	// Unparseable is true when neither format could be read at all. The raw document is
	// still reported (see parseClusterAutoscalerStatus) rather than dropped.
	Unparseable bool
	// Legacy is true when the document was read with the pre-1.30 text scanner. Reported
	// so the collector can log which path a cluster is on.
	Legacy bool
}

// parseClusterAutoscalerStatus turns the raw contents of the status ConfigMap's `status`
// key into block observations.
//
// WHAT THIS DOCUMENT ACTUALLY CONTAINS, because the plan's premise was optimistic. Neither
// format carries per-node unremovable reasons:
//
//   - 1.30+ writes yaml.Marshal(api.ClusterAutoscalerStatus)
//     (clusterstate/utils/status.go). That struct is cluster-wide health + per-node-group
//     health/scaleUp/scaleDown conditions, and nothing else — verified field by field
//     against clusterstate/api/types.go on release branches 1.30 through 1.36, and against
//     the upstream golden fixture clusterstate/utils/status_test.yaml.
//   - <=1.29 writes api.ClusterAutoscalerStatus.GetReadableString()
//     (clusterstate/api/utils.go), the same information rendered as indented text.
//   - The unremovable-node reasons (simulator.UnremovableReason) live in
//     ScaleDownStatus.UnremovableNodes, which is handed to a ScaleDownStatusProcessor that
//     is a no-op by default, exported as an aggregate Prometheus counter, and logged. The
//     upstream FAQ says so directly: "check Cluster Autoscaler logs ... including why it
//     considers a pod unremovable".
//
// So no invented `unremovableNodes` key is looked for here — inventing a schema is the
// same class of mistake as inventing a node name. What IS available, and what this reads,
// is the per-node-group scaleDown condition: a group sitting on scale-down candidates it
// has not removed is exactly "the autoscaler wanted to remove a node and could not". That
// is reported per NODE GROUP, because the document names no nodes.
//
// The reason is then classified by scanning the group's raw text for Cluster Autoscaler's
// own reason vocabulary (classifyCASBlockReason). Upstream text contains none of those
// tokens and therefore classifies as Other with the raw section attached, which is the
// documented fallback — but a distribution or future version that does surface a reason is
// picked up without a code change, and never guessed at.
//
// Never returns an error: an unreadable document produces a single Unparseable observation
// carrying the raw text, because "we cannot read this cluster's status" is itself
// information the operator needs, and dropping it silently is the one thing the plan rules
// out.
func parseClusterAutoscalerStatus(raw string, now time.Time) casParseResult {
	status, ok := parseCASStatusYAML(raw)
	if ok {
		return casParseResult{
			Observations: casObservationsFromStatus(status, raw, now),
			ObservedAt:   casObservedAt(status, now),
		}
	}

	if groups, ok := parseCASStatusLegacy(raw); ok {
		return casParseResult{
			Observations: casObservationsFromLegacy(groups),
			ObservedAt:   now,
			Legacy:       true,
		}
	}

	return casParseResult{
		Observations: []casBlockObservation{casUnparseableObservation(raw)},
		ObservedAt:   now,
		Unparseable:  true,
	}
}

// parseCASStatusYAML reads the 1.30+ machine-readable document.
//
// The second return is false both when the YAML is invalid AND when it parsed into
// nothing recognisable. That second check is load-bearing: the pre-1.30 readable text is
// close enough to YAML that it can unmarshal cleanly into a completely empty struct, which
// would otherwise look like "a healthy cluster with no node groups" instead of "wrong
// format, try the other parser".
func parseCASStatusYAML(raw string) (casStatus, bool) {
	var status casStatus
	if err := yaml.Unmarshal([]byte(raw), &status); err != nil {
		return casStatus{}, false
	}
	if len(status.NodeGroups) == 0 &&
		status.AutoscalerStatus == "" &&
		status.ClusterWide.Health.Status == "" &&
		status.ClusterWide.ScaleDown.Status == "" {
		return casStatus{}, false
	}
	return status, true
}

// casObservedAt returns when Cluster Autoscaler wrote the document, falling back to the
// collector's clock. Preferring the document's own instant is what makes a stalled
// autoscaler's blocks go stale in dakr rather than staying pinned to the live list.
func casObservedAt(status casStatus, now time.Time) time.Time {
	if !status.ClusterWide.Health.LastProbeTime.IsZero() {
		return status.ClusterWide.Health.LastProbeTime.Time
	}
	if t, ok := parseCASTime(status.Time); ok {
		return t
	}
	return now
}

// casObservationsFromStatus turns each stalled node group into one observation.
func casObservationsFromStatus(status casStatus, raw string, now time.Time) []casBlockObservation {
	sections := splitCASNodeGroupSections(raw)

	observations := make([]casBlockObservation, 0, len(status.NodeGroups))
	for _, group := range status.NodeGroups {
		if group.Name == "" {
			// No identifier means nothing to attribute the block to, and naming it
			// anything would be an invention. Skipped rather than guessed.
			continue
		}
		if group.ScaleDown.Status != casScaleDownCandidatesPresent {
			continue
		}

		streakStart := group.ScaleDown.LastTransitionTime.Time
		if !streakStart.IsZero() && now.Sub(streakStart) < casMinBlockHold {
			// Candidates were only just identified: this is a scale-down in progress, not
			// a blocked one. See casMinBlockHold.
			continue
		}

		section := sections[group.Name]
		if section == "" {
			// The document parsed but the raw slice for this group did not; carrying the
			// structured facts is better than carrying nothing.
			section = fmt.Sprintf("nodeGroup %s: scaleDown status %s, candidates %d",
				group.Name, group.ScaleDown.Status, group.ScaleDown.Candidates)
		}

		observations = append(observations, casBlockObservation{
			ObjectKind:  casBlockOwnerKindNodeGroup,
			ObjectName:  group.Name,
			Reason:      classifyCASBlockReason(section),
			Message:     truncateCASMessage(section),
			StreakStart: streakStart,
		})
	}
	return observations
}

// splitCASNodeGroupSections slices the raw document into the verbatim text of each entry
// under `nodeGroups:`, keyed by that entry's name.
//
// The raw slice is used rather than re-marshalling the parsed struct so the message is
// what Cluster Autoscaler actually wrote — including any key this parser does not model,
// which is the whole reason the message exists.
//
// A best-effort scan: it keys on the indentation of the `nodeGroups:` list, which is fixed
// by yaml.Marshal. A section it cannot slice comes back missing and the caller falls back
// to a rendering of the structured fields.
func splitCASNodeGroupSections(raw string) map[string]string {
	sections := make(map[string]string)

	lines := strings.Split(raw, "\n")
	inNodeGroups := false
	// Sized for a typical node group's section; grows if a document is more verbose.
	current := make([]string, 0, 24)
	currentName := ""

	flush := func() {
		if currentName != "" && len(current) > 0 {
			sections[currentName] = strings.TrimRight(strings.Join(current, "\n"), " \n")
		}
		current = current[:0]
		currentName = ""
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		if !inNodeGroups {
			if trimmed == "nodeGroups:" {
				inNodeGroups = true
			}
			continue
		}

		// A top-level key ends the nodeGroups list.
		if trimmed != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "-") {
			break
		}

		// A new list entry starts a new section.
		if strings.HasPrefix(trimmed, "- ") || trimmed == "-" {
			flush()
		}
		current = append(current, line)

		if name, ok := casYAMLScalar(trimmed, "name"); ok && currentName == "" {
			currentName = name
		}
	}
	flush()

	return sections
}

// casYAMLScalar pulls `key: value` out of one already-trimmed YAML line, tolerating the
// leading "- " of a list entry and the quotes yaml.Marshal puts around strings.
func casYAMLScalar(trimmed, key string) (string, bool) {
	trimmed = strings.TrimPrefix(trimmed, "- ")
	prefix := key + ":"
	if !strings.HasPrefix(trimmed, prefix) {
		return "", false
	}
	value := strings.TrimSpace(strings.TrimPrefix(trimmed, prefix))
	value = strings.Trim(value, `"'`)
	if value == "" {
		return "", false
	}
	return value, true
}

// casLegacyNodeGroup is one node group scraped out of the pre-1.30 readable text.
type casLegacyNodeGroup struct {
	name            string
	scaleDownStatus string
	streakStart     time.Time
	section         string
}

// parseCASStatusLegacy scrapes the pre-1.30 readable rendering
// (clusterstate/api/utils.go GetReadableString), which looks like:
//
//	Cluster-wide:
//	  Health:      Healthy (ready=3 unready=0 ...)
//	               LastProbeTime:      2023-11-24 04:28:19.546 +0000 UTC
//	               LastTransitionTime: 2023-11-23 14:52:02.123 +0000 UTC
//	  ScaleDown:   NoCandidates (candidates=0)
//	               ...
//
//	NodeGroups:
//	  Name:        my-node-group
//	  Health:      Healthy (...)
//	  ScaleDown:   CandidatesPresent (candidates=2)
//	               LastProbeTime:      ...
//	               LastTransitionTime: 2023-11-23 14:52:02.123 +0000 UTC
//
// Line-oriented and forgiving on purpose: this is a fmt.Sprintf rendering with no version
// guarantee, so the scan keys only on the two labels that have never moved (`Name:` and
// `ScaleDown:`) and treats everything else as opaque text to carry along.
func parseCASStatusLegacy(raw string) ([]casLegacyNodeGroup, bool) {
	lines := strings.Split(raw, "\n")

	inNodeGroups := false
	var groups []casLegacyNodeGroup
	var current *casLegacyNodeGroup
	// Sized for one node group's rendered block (name plus three conditions, each with
	// two timestamp lines); grows if a document is more verbose.
	section := make([]string, 0, 16)
	awaitingScaleDownTransition := false

	flush := func() {
		if current != nil {
			current.section = strings.TrimRight(strings.Join(section, "\n"), " \n")
			groups = append(groups, *current)
		}
		current = nil
		section = section[:0]
		awaitingScaleDownTransition = false
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		if !inNodeGroups {
			if trimmed == "NodeGroups:" {
				inNodeGroups = true
			}
			continue
		}

		if name, ok := casLegacyField(trimmed, "Name"); ok {
			flush()
			current = &casLegacyNodeGroup{name: name}
			section = append(section, line)
			continue
		}
		if current == nil {
			continue
		}
		section = append(section, line)

		if status, ok := casLegacyField(trimmed, "ScaleDown"); ok {
			// "CandidatesPresent (candidates=2)" — the status is the first word, the
			// parenthetical is the condition's free-text message.
			current.scaleDownStatus = strings.Fields(status)[0]
			awaitingScaleDownTransition = true
			continue
		}
		if awaitingScaleDownTransition {
			if value, ok := casLegacyField(trimmed, "LastTransitionTime"); ok {
				if t, parsed := parseCASTime(value); parsed {
					current.streakStart = t
				}
				awaitingScaleDownTransition = false
			}
		}
	}
	flush()

	return groups, len(groups) > 0
}

// casLegacyField pulls the value out of one `Label:   value` line of the readable
// rendering. The renderer pads labels to a fixed width, so the split is on the colon and
// the value is whitespace-trimmed.
func casLegacyField(trimmed, label string) (string, bool) {
	prefix := label + ":"
	if !strings.HasPrefix(trimmed, prefix) {
		return "", false
	}
	value := strings.TrimSpace(strings.TrimPrefix(trimmed, prefix))
	if value == "" {
		return "", false
	}
	return value, true
}

// casObservationsFromLegacy applies the same rule as the YAML path: a node group sitting
// on scale-down candidates is blocked.
//
// The hold check uses the scraped LastTransitionTime when there was one. When there was
// not, the observation goes out with a zero StreakStart and the collector applies the hold
// against its own first-observation time instead — deferring rather than skipping, so a
// cluster whose timestamps did not scrape still reports, just later.
func casObservationsFromLegacy(groups []casLegacyNodeGroup) []casBlockObservation {
	observations := make([]casBlockObservation, 0, len(groups))
	for _, group := range groups {
		if group.name == "" || group.scaleDownStatus != casScaleDownCandidatesPresent {
			continue
		}
		observations = append(observations, casBlockObservation{
			ObjectKind:  casBlockOwnerKindNodeGroup,
			ObjectName:  group.name,
			Reason:      classifyCASBlockReason(group.section),
			Message:     truncateCASMessage(group.section),
			StreakStart: group.streakStart,
		})
	}
	return observations
}

// casUnparseableObservation is what a document neither parser could read becomes.
//
// It is attributed to the status ConfigMap itself — a real object, with its real name —
// and carries the aggregate NodeGroup kind so dakr keeps it out of the live blocked-NODE
// list. No node name and no node group name is invented for it.
//
// Emitting rather than dropping is deliberate: an unreadable status document on a
// customer's cluster is exactly the case where the raw text is the only thing anyone can
// act on, and the plan's rule is never guess, fall back to raw.
//
// It carries no StreakStart, so the collector holds it for casMinBlockHold against its own
// clock like any other timestamp-less observation. That is the right behaviour here too: a
// document that is unreadable for one sweep is most likely a torn read of a ConfigMap
// mid-write, while one that is still unreadable ten minutes later is a real format
// mismatch worth reporting.
func casUnparseableObservation(raw string) casBlockObservation {
	return casBlockObservation{
		ObjectKind: casBlockOwnerKindNodeGroup,
		ObjectName: casStatusConfigMapName,
		Reason:     disruptionBlockReasonOther,
		Message: truncateCASMessage(
			"unparseable cluster-autoscaler-status document:\n" + raw,
		),
	}
}

// casReasonTokens maps Cluster Autoscaler's own vocabulary onto the shared
// disruption_block_reason buckets.
//
// Both spellings of each reason are listed: the Go identifier
// (simulator.UnremovableReason / drain.BlockingPodReason, which is what a structured field
// would carry) and the human-readable UnremovableReason.String() (which is what rendered
// text carries). Source of truth is vendored in this repo —
// autoscale/clusterautoscaler/simulator/cluster.go and
// autoscale/clusterautoscaler/utils/drain/drain.go.
//
// Everything absent from this table — NodeGroupMinSizeReached, NotUnderutilized,
// CurrentlyBeingDeleted, MinimalResourceLimitExceeded, and every reason a future version
// adds — falls to Other with the raw text preserved. That is required, not merely polite:
// dakr gates the field against the known set and DROPS the whole observation on an unknown
// value, so inventing a bucket here loses the data entirely.
//
// BlockedByPod is deliberately NOT a key. On its own it means "some pod blocked this",
// which spans two different buckets depending on the BlockingPodReason; the two blocking
// reasons that map cleanly are keys instead, and a bare BlockedByPod stays Other.
var casReasonTokens = []struct {
	token  string
	reason string
}{
	// Node-level opt-out: cluster-autoscaler.kubernetes.io/scale-down-disabled on a Node.
	{"ScaleDownDisabledAnnotation", disruptionBlockReasonDoNotDisruptAnnotation},
	{"Scale down disabled annotation", disruptionBlockReasonDoNotDisruptAnnotation},
	// Pod-level opt-out: cluster-autoscaler.kubernetes.io/safe-to-evict: "false".
	{"NotSafeToEvictAnnotation", disruptionBlockReasonDoNotDisruptAnnotation},
	// Pod-level PDB block.
	{"NotEnoughPdb", disruptionBlockReasonPDBViolation},
	// CAS-only waiting state.
	{"NotUnneededLongEnough", disruptionBlockReasonNotUnneededLongEnough},
	{"Not unneeded long enough", disruptionBlockReasonNotUnneededLongEnough},
	// CAS-only wedged state.
	{"NoPlaceToMovePods", disruptionBlockReasonNoPlaceToMovePods},
	{"No place to move pods", disruptionBlockReasonNoPlaceToMovePods},
}

// classifyCASBlockReason picks a bucket by looking for Cluster Autoscaler's reason
// vocabulary in the raw section.
//
// A token scan rather than a field read, because the status document has no reason field
// (see parseClusterAutoscalerStatus) and inventing one to parse would be guessing at a
// schema. Scanning the text CAS actually wrote costs nothing, never fabricates, and picks
// up any version or distribution that does surface a reason without a code change here.
//
// Order matters only for a section mentioning several reasons, which upstream never
// produces; casReasonTokens is ordered most-specific-first for that case.
//
// Never returns an empty string.
func classifyCASBlockReason(section string) string {
	for _, entry := range casReasonTokens {
		if strings.Contains(section, entry.token) {
			return entry.reason
		}
	}
	return disruptionBlockReasonOther
}

// casTimeLayouts are the two shapes a timestamp takes in this document: RFC3339 from the
// machine-readable format's metav1.Time, and Go's default time.Time rendering from the
// readable format's %v (and from the top-level `time` field, which
// clusterstate/utils/status.go formats with exactly this layout).
var casTimeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02 15:04:05.999999999 -0700 MST",
	"2006-01-02 15:04:05 -0700 MST",
}

// parseCASTime parses a timestamp from either format, dropping the monotonic-clock suffix
// ("m=+123.45") that Go appends when a time.Time carrying a monotonic reading is printed.
func parseCASTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	if idx := strings.Index(value, " m=+"); idx >= 0 {
		value = value[:idx]
	}
	if idx := strings.Index(value, " m=-"); idx >= 0 {
		value = value[:idx]
	}
	value = strings.Trim(value, `"'`)

	for _, layout := range casTimeLayouts {
		if t, err := time.Parse(layout, value); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// truncateCASMessage bounds the raw text, marking where it was cut so nobody reads a
// truncated document as a complete one.
func truncateCASMessage(message string) string {
	if len(message) <= casMaxMessageBytes {
		return message
	}
	return message[:casMaxMessageBytes] + "\n... [truncated]"
}

// casBlockUID derives the event UID for a block streak.
//
// THIS IS THE FOLD KEY. dakr groups block observations by (cluster_id, uid): every count
// it reports is per-UID, the live blocked list reconstructs a streak as min..max of the
// rows sharing one, and a block clears by that UID going quiet. So the same streak
// re-observed on the next sweep MUST produce the same UID — a uuid.New() per sweep would
// turn one blocked node group into an ever-growing pile of one-observation blocks and
// inflate every count.
//
// Deterministic UUIDv5 over (kind, name, reason, streak start):
//
//   - kind+name is the involved object, so two node groups never share a streak.
//   - reason is included because dakr aggregates by it: a group whose block changes
//     classification is a different streak, not a mutation of the old one (the fold takes
//     argMax over the classification, so mixing them under one UID would make one of the
//     two invisible).
//   - streak start is what makes a block that clears and recurs a SECOND streak instead of
//     one long one; without it, a group blocked for 10 minutes, fine for three hours and
//     blocked again would report as blocked for three hours ten minutes. It is taken from
//     Cluster Autoscaler's own lastTransitionTime wherever the document has one, which
//     also makes the UID survive a zxporter restart mid-block.
//
// The cluster is NOT part of the input, and does not need to be: zxporter serves exactly
// one cluster, and dakr scopes every fold by cluster_id, so two clusters producing the same
// UID for the same node group name is not a collision anywhere it is read.
func casBlockUID(kind, name, reason string, streakStart time.Time) string {
	key := strings.Join([]string{
		kind,
		name,
		reason,
		streakStart.UTC().Format(time.RFC3339Nano),
	}, "|")
	return uuid.NewSHA1(casUIDNamespace, []byte(key)).String()
}

// casBlockEvent renders one observation as the enrichedEvent that goes on the wire.
//
// It rides the ordinary RESOURCE_TYPE_EVENT pipeline — the same envelope Task 3 uses for
// Karpenter — rather than a resource type of its own, because it is the same fact and dakr
// reads both from k8s_events in one query.
//
// count must be the running total of observations in this streak, monotonically
// increasing: dakr reads max(count) per UID, so it survives a collector restart resetting
// the counter (the older, larger value stays the max) but never recovers from a counter
// that goes backwards within a streak.
func casBlockEvent(obs casBlockObservation, streakStart time.Time, observedAt time.Time, count int32) *enrichedEvent {
	uid := casBlockUID(obs.ObjectKind, obs.ObjectName, obs.Reason, streakStart)

	event := &corev1.Event{
		TypeMeta: metav1.TypeMeta{Kind: "Event", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{
			// Kubernetes names an event after its involved object plus a suffix; the same
			// shape is kept here because dakr derives k8s_events.pod_name from the leading
			// dot-separated segment.
			Name: fmt.Sprintf("%s.%s", obs.ObjectName, uid[:8]),
			// Cluster-scoped: the involved object is a node group or a node, neither of
			// which lives in a namespace.
			Namespace:         "",
			UID:               types.UID(uid),
			CreationTimestamp: metav1.NewTime(streakStart),
		},
		InvolvedObject: corev1.ObjectReference{
			Kind: obs.ObjectKind,
			Name: obs.ObjectName,
		},
		Reason:  scaleDownBlockedReason,
		Message: obs.Message,
		Type:    corev1.EventTypeWarning,
		Source:  corev1.EventSource{Component: clusterAutoscalerReportingController},
		// The discriminator dakr filters on. Set to the autoscaler whose decision this
		// describes, not to the component that synthesized the row; ReportingInstance
		// records that instead, so a raw k8s_events reader can still tell.
		ReportingController: clusterAutoscalerReportingController,
		ReportingInstance:   "zxporter",
		FirstTimestamp:      metav1.NewTime(streakStart),
		LastTimestamp:       metav1.NewTime(observedAt),
		Count:               count,
	}

	return &enrichedEvent{
		Event:                 event,
		DisruptionBlockReason: obs.Reason,
		// Left nil, never "": dakr writes a real NULL for it, and "no PDB was involved" is
		// not the same fact as "a PDB with an empty name". The status document never names
		// a PDB, so this path has nothing to put here.
		BlockingPDBName: nil,
	}
}
