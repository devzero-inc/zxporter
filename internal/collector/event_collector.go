// internal/collector/event_collector.go
package collector

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	telemetry_logger "github.com/devzero-inc/zxporter/internal/logger"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// EventCollector watches for event events and collects event data
type EventCollector struct {
	client           kubernetes.Interface
	informerFactory  informers.SharedInformerFactory
	eventInformer    cache.SharedIndexInformer
	batchChan        chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan     chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher          *ResourcesBatcher
	stopCh           chan struct{}
	namespaces       []string
	excludedEvents   map[types.NamespacedName]bool
	maxEventsPerType int            // Limit events per type to prevent overwhelming the channel
	eventCounts      map[string]int // Track number of events per type
	retentionPeriod  time.Duration  // How long to keep events in memory
	logger           logr.Logger
	telemetryLogger  telemetry_logger.Logger
	mu               sync.RWMutex
	cDHelper         ChangeDetectionHelper

	// disruptionSources is the read-only view of the PDB, node and Karpenter collectors'
	// caches used to classify Karpenter DisruptionBlocked events. Set by the reconciler via
	// SetDisruptionSources, and nil-safe: unset, every DisruptionBlocked event classifies
	// as "Other".
	//
	// Atomic rather than mutex-guarded because the write can land while the informer is
	// already dispatching (see SetDisruptionSources), and a plain RWMutex read would put a
	// lock acquire on the path of every event just to serve the rare DisruptionBlocked one.
	disruptionSources atomic.Pointer[disruptionSources]
}

// NewEventCollector creates a new collector for event resources
func NewEventCollector(
	client kubernetes.Interface,
	namespaces []string,
	excludedEvents []ExcludedEvent,
	maxEventsPerType int,
	retentionPeriod time.Duration,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
) *EventCollector {
	// Convert excluded events to a map for quicker lookups
	excludedEventsMap := make(map[types.NamespacedName]bool)
	for _, event := range excludedEvents {
		excludedEventsMap[types.NamespacedName{
			Namespace: event.Namespace,
			Name:      event.Name,
		}] = true
	}

	// Set default values if not specified
	if maxEventsPerType <= 0 {
		maxEventsPerType = 1000 // Default to 1000 events per type
	}

	if retentionPeriod <= 0 {
		retentionPeriod = 1 * time.Hour // Default to 1 hour retention
	}

	// Create channels
	batchChan := make(chan CollectedResource, 1000)     // Keep high buffer for individual events
	resourceChan := make(chan []CollectedResource, 100) // Buffer for batches

	// Create the batcher
	batcher := NewResourcesBatcher(
		maxBatchSize,
		maxBatchTime,
		batchChan,
		resourceChan,
		logger,
	)

	newLogger := logger.WithName("event-collector")
	return &EventCollector{
		client:           client,
		batchChan:        batchChan,
		resourceChan:     resourceChan,
		batcher:          batcher,
		stopCh:           make(chan struct{}),
		namespaces:       namespaces,
		excludedEvents:   excludedEventsMap,
		maxEventsPerType: maxEventsPerType,
		eventCounts:      make(map[string]int),
		retentionPeriod:  retentionPeriod,
		logger:           newLogger,
		telemetryLogger:  telemetryLogger,
		cDHelper:         ChangeDetectionHelper{logger: newLogger},
	}
}

// Start begins the event collection process
func (c *EventCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting event collector",
		"namespaces", c.namespaces,
		"maxEventsPerType", c.maxEventsPerType,
		"retentionPeriod", c.retentionPeriod)

	// Create informer factory based on namespace configuration
	c.informerFactory = newInformerFactory(c.client, c.namespaces)

	// Create event informer
	c.eventInformer = c.informerFactory.Core().V1().Events().Informer()

	// Add event handlers
	_, err := c.eventInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			event := obj.(*corev1.Event)
			c.handleEvent(event, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldEvent := oldObj.(*corev1.Event)
			newEvent := newObj.(*corev1.Event)

			// Only handle meaningful updates
			if c.eventChanged(oldEvent, newEvent) {
				c.handleEvent(newEvent, EventTypeUpdate)
			}
		},
		DeleteFunc: func(obj interface{}) {
			event := obj.(*corev1.Event)
			c.handleEvent(event, EventTypeDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factories
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.eventInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Start the batcher after the cache is synced
	c.logger.Info("Starting resources batcher for Events")
	c.batcher.start()

	// Start a goroutine to clean up old events
	go c.periodicCleanup(ctx)

	// Keep this goroutine alive until context cancellation or stop
	stopCh := c.stopCh
	go func() {
		select {
		case <-ctx.Done():
			c.Stop()
		case <-stopCh:
			// Channel was closed by Stop() method
		}
	}()

	return nil
}

// handleEvent processes event events
func (c *EventCollector) handleEvent(event *corev1.Event, eventType EventType) {
	if c.isExcluded(event) {
		return
	}

	// Generate a type key for counting/grouping
	typeKey := fmt.Sprintf("%s/%s/%s", event.InvolvedObject.Kind, event.Type, event.Reason)

	// Check if we've hit the limit for this event type
	c.mu.Lock()
	count := c.eventCounts[typeKey]
	if count >= c.maxEventsPerType && eventType == EventTypeAdd {
		c.mu.Unlock()
		c.logger.V(5).Info("Skipping event due to per-type limit",
			"namespace", event.Namespace,
			"name", event.Name,
			"reason", event.Reason,
			"count", count,
			"limit", c.maxEventsPerType)
		return
	}
	c.eventCounts[typeKey]++
	c.mu.Unlock()

	// Send the raw event object to the batch channel
	c.batchChan <- CollectedResource{
		ResourceType: Event,
		Object:       c.eventPayload(event), // The entire event object, as-is
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", event.Namespace, event.Name),
	}

	// Additionally — never instead of — emit the classified form of a scheduling
	// failure. The raw event above still lands in k8s_events exactly as before; this
	// second resource is what feeds pod_unschedulable_events, where the reason is a
	// structured bucket instead of free text.
	c.emitPodUnschedulableEvent(event, eventType)
}

// enrichedEvent is the wire shape of an Event resource: the core/v1 Event exactly as the
// API server wrote it, plus the classification only this collector can make.
//
// The two fields sit BESIDE the embedded Event rather than inside it because a core/v1
// Event has nowhere to put a value Kubernetes did not produce, and stuffing one into its
// annotations would mean deep-copying the informer's cached object on every event just to
// avoid corrupting the shared store. Embedding a pointer inlines the Event's own JSON, so
// the payload an unenriched event produces is byte-identical to what this collector sent
// before this field existed — which is what keeps a new collector safe against an older
// dakr, and dakr's matching EnrichedEvent safe against an older collector.
//
// Mirrors k8sconverters.EnrichedEvent on the dakr side; the json tags are the contract.
type enrichedEvent struct {
	*corev1.Event

	DisruptionBlockReason string  `json:"disruptionBlockReason,omitempty"`
	BlockingPDBName       *string `json:"blockingPdbName,omitempty"`
}

// eventPayload returns what to put on the wire for an event.
//
// Everything except a Karpenter DisruptionBlocked event goes out as the bare core/v1
// object, unchanged and un-wrapped.
func (c *EventCollector) eventPayload(event *corev1.Event) interface{} {
	if !isKarpenterDisruptionBlocked(event) {
		return event
	}

	classification := classifyDisruptionBlocked(event, c.disruptionSources.Load(), time.Now())
	c.logger.V(5).Info("Classified Karpenter DisruptionBlocked event",
		"involvedObjectKind", event.InvolvedObject.Kind,
		"involvedObjectName", event.InvolvedObject.Name,
		"reason", classification.Reason)

	return &enrichedEvent{
		Event:                 event,
		DisruptionBlockReason: classification.Reason,
		BlockingPDBName:       classification.BlockingPDBName,
	}
}

// SetDisruptionSources gives the collector its view of the PDB, node and Karpenter
// collectors' caches, which is what lets it classify a DisruptionBlocked event
// structurally instead of parsing Karpenter's message.
//
// Optional by design: left unset — as it is in every test that does not exercise the
// classification, and on a cluster where those collectors are disabled — DisruptionBlocked
// events still flow, classified as "Other".
//
// Safe to call on a running collector. The reconciler re-wires a *replaced* EventCollector
// from restartCollectors, which happens after that collector's informer is already
// dispatching events, so this write genuinely races the read in eventPayload.
func (c *EventCollector) SetDisruptionSources(registry collectorRegistry) {
	c.disruptionSources.Store(&disruptionSources{registry: registry})
}

// failedSchedulingReason is the Event reason kube-scheduler stamps on a pod it could not
// place. It re-emits this on every scheduling retry, so a pod that sits pending produces
// a series of these.
const failedSchedulingReason = "FailedScheduling"

// emitPodUnschedulableEvent classifies a FailedScheduling event and emits it as its own
// resource. Every observed event becomes its own row: dakr keys the table on
// (cluster_id, namespace, pod_name, timestamp) and handles duplicate delivery on its end,
// so deduplicating here would only hide genuine retries.
func (c *EventCollector) emitPodUnschedulableEvent(event *corev1.Event, eventType EventType) {
	if event.Reason != failedSchedulingReason {
		return
	}

	// A delete carries the object's last-known state, which by definition was already
	// reported by the add/update that preceded it.
	if eventType == EventTypeDelete {
		return
	}

	// FailedScheduling is also emitted against batch-scheduler group objects (Volcano
	// PodGroups, the coscheduling plugin's), which this pod-keyed signal has no place
	// for. The pod's own identity comes from the involved object, not from the Event's
	// metadata: the Event is named after the pod but with a suffix.
	if event.InvolvedObject.Kind != "Pod" {
		return
	}

	podName := event.InvolvedObject.Name
	namespace := event.InvolvedObject.Namespace
	if namespace == "" {
		namespace = event.Namespace
	}
	if podName == "" || namespace == "" {
		return
	}

	observedAt, ok := failedSchedulingObservedAt(event)
	if !ok {
		// Without a timestamp there is nothing to measure a pending duration from, and
		// substituting the collection time would fabricate one. In practice unreachable —
		// the API server always stamps creationTimestamp.
		c.logger.V(5).Info("Skipping FailedScheduling event with no usable timestamp",
			"namespace", namespace,
			"pod", podName)
		return
	}

	c.batchChan <- CollectedResource{
		ResourceType: PodUnschedulableEvent,
		Object: map[string]interface{}{
			"namespace": namespace,
			"pod_name":  podName,
			"timestamp": observedAt.UTC().Format(time.RFC3339Nano),
			// The classification. Lossy by construction, which is why raw_message rides
			// along on every row regardless of whether it matched.
			"reason_bucket": classifyFailedSchedulingMessage(event.Message),
			"raw_message":   event.Message,
			"retry_count":   failedSchedulingRetryCount(event),
		},
		Timestamp: time.Now(),
		EventType: eventType,
		// The timestamp is part of the identity: two retries of the same pod are two
		// distinct observations, not one resource seen twice.
		Key: fmt.Sprintf("pod-unschedulable/%s/%s/%d", namespace, podName, observedAt.UnixNano()),
	}
}

// failedSchedulingObservedAt returns when the cluster last saw this scheduling failure,
// preferring the most specific timestamp the Event carries. Series.LastObservedTime is
// first because an aggregated event stops advancing LastTimestamp once it starts
// aggregating; EventTime and the metadata timestamps are the fallbacks for events written
// through the events/v1 API, which leaves the legacy fields zero.
func failedSchedulingObservedAt(event *corev1.Event) (time.Time, bool) {
	if event.Series != nil && !event.Series.LastObservedTime.IsZero() {
		return event.Series.LastObservedTime.Time, true
	}
	if !event.LastTimestamp.IsZero() {
		return event.LastTimestamp.Time, true
	}
	if !event.EventTime.IsZero() {
		return event.EventTime.Time, true
	}
	if !event.FirstTimestamp.IsZero() {
		return event.FirstTimestamp.Time, true
	}
	if !event.CreationTimestamp.IsZero() {
		return event.CreationTimestamp.Time, true
	}
	return time.Time{}, false
}

// failedSchedulingRetryCount returns how many scheduling attempts the Event represents.
//
// Both count fields are optional and events/v1-style events carry the total on
// Series.Count instead of Count, so take whichever is larger. The floor of 1 matches
// dakr's converter: an event observed at all is at least one attempt, and a 0 would
// silently vanish from every summed retry count.
func failedSchedulingRetryCount(event *corev1.Event) uint32 {
	count := event.Count
	if event.Series != nil && event.Series.Count > count {
		count = event.Series.Count
	}
	if count < 1 {
		return 1
	}
	return uint32(count)
}

// Pod-unschedulable reason buckets. These strings are the wire contract with dakr's
// pod_unschedulable_events table (models.PodUnschedulableReasonBucket) — dakr rejects any
// value not in this set, so they must match verbatim.
const (
	podUnschedulableReasonInsufficientCPU    = "InsufficientCPU"
	podUnschedulableReasonInsufficientMemory = "InsufficientMemory"
	podUnschedulableReasonInsufficientGPU    = "InsufficientGPU"
	podUnschedulableReasonNodeAffinity       = "NodeAffinity"
	podUnschedulableReasonTaints             = "Taints"
	podUnschedulableReasonTopologySpread     = "TopologySpread"
	podUnschedulableReasonVolumeBinding      = "VolumeBinding"
	podUnschedulableReasonOther              = "Other"
)

// failedSchedulingBucketPriority is the deterministic tie-break order used when two
// buckets block the same number of nodes. Resource exhaustion sorts first because it is
// the actionable-by-scaling case this report exists to surface; the remaining order is
// arbitrary but fixed, so the same message always classifies the same way.
var failedSchedulingBucketPriority = []string{
	podUnschedulableReasonInsufficientCPU,
	podUnschedulableReasonInsufficientMemory,
	podUnschedulableReasonInsufficientGPU,
	podUnschedulableReasonTaints,
	podUnschedulableReasonNodeAffinity,
	podUnschedulableReasonTopologySpread,
	podUnschedulableReasonVolumeBinding,
}

// preemptionSectionPattern marks where the scheduler's filter reasons end and its
// preemption attempt's own reasons begin. FitError.Error() appends the second as
// ". preemption: 0/N nodes are available: ...", and those node counts describe why
// preemption could not help, not why the pod is unschedulable — folding them into the
// classification would double-count.
var preemptionSectionPattern = regexp.MustCompile(`(?i)\bpreemption:`)

// insufficientResourcePattern matches the noderesources plugin's reason, which is
// fmt.Sprintf("Insufficient %v", resourceName) — the resource is any Kubernetes resource
// name, including extended ones like nvidia.com/gpu. Group 1 is the node count the
// scheduler prefixes ("3 Insufficient cpu"); group 2 is the resource name. The name is
// matched so it cannot end in '.', which is what keeps the sentence-ending period out of
// it.
var insufficientResourcePattern = regexp.MustCompile(
	`(?i)(?:(\d+)\s+)?Insufficient\s+([a-z0-9](?:[-a-z0-9_./]*[a-z0-9])?)`,
)

// acceleratorResourcePattern decides which extended resources count as GPU. It covers the
// vendor names in use — nvidia.com/gpu, amd.com/gpu, gpu.intel.com/i915,
// nvidia.com/gpu.shared — plus MIG slices and Habana Gaudi, which do not carry "gpu" in
// their name. Anything else insufficient (ephemeral-storage, pods, hugepages) has no
// bucket and is deliberately left to fall through to Other rather than being guessed into
// one of these.
var acceleratorResourcePattern = regexp.MustCompile(`(?i)gpu|nvidia\.com/mig-|habana\.ai/gaudi`)

// failedSchedulingRule maps one bucket to the scheduler phrases that imply it. Group 1 of
// every pattern is the optional node-count prefix.
//
// Each bucket is ONE regex with the phrasings as alternatives, rather than a list of
// regexes: the classifier runs inline on the informer callback for every FailedScheduling
// event, so a message is scanned once per bucket instead of once per phrasing.
//
// The phrases are the ErrReason constants from kubernetes/kubernetes
// pkg/scheduler/framework/plugins/* (tainttoleration, nodeaffinity, interpodaffinity,
// podtopologyspread, volumebinding, nodevolumelimits, volumezone) as of v1.31, with the
// pre-v1.24 phrasings kept alongside them — customer clusters run a wide range of
// versions, and an unmatched message is a silent slide into Other rather than a loud
// failure.
type failedSchedulingRule struct {
	bucket  string
	pattern *regexp.Regexp
}

var failedSchedulingRules = []failedSchedulingRule{
	{
		bucket: podUnschedulableReasonTaints,
		// v1.24+: "node(s) had untolerated taint {key: value}".
		// Pre-v1.24: "node(s) had taints that the pod didn't tolerate".
		pattern: regexp.MustCompile(
			`(?i)(?:(\d+)\s+)?node\(s\) had (?:untolerated taint|taints that the pod didn't tolerate)`,
		),
	},
	{
		bucket: podUnschedulableReasonNodeAffinity,
		// First alternative: v1.24+ merged node affinity and the node selector into one
		// reason; the two older phrasings are still emitted by older clusters.
		// Second: inter-pod affinity/anti-affinity, split into two reasons in v1.22+ and
		// one combined phrase before that.
		// Third: the nodename plugin — a pod pinned by .spec.nodeName to a node that does
		// not exist is the same class of "you asked for a node that isn't there".
		pattern: regexp.MustCompile(
			`(?i)(?:(\d+)\s+)?node\(s\) didn't match ` +
				`(?:(?:Pod's )?node (?:affinity(?:/selector)?|selector)` +
				`|pod (?:affinity/anti-affinity|affinity|anti-affinity) rules` +
				`|the requested (?:node name|hostname))`,
		),
	},
	{
		bucket: podUnschedulableReasonTopologySpread,
		// Also matches ErrReasonNodeLabelNotMatch, which is this string plus
		// " (missing required label)".
		pattern: regexp.MustCompile(
			`(?i)(?:(\d+)\s+)?node\(s\) didn't match pod topology spread constraints`,
		),
	},
	{
		bucket: podUnschedulableReasonVolumeBinding,
		// The two non-"node(s)" alternatives are PreFilter rejections, which carry no node
		// count — they fail the pod before any node is considered — so they weigh 1. That
		// only ever matters if a filter reason appeared alongside them, which it cannot:
		// PreFilter short-circuits.
		pattern: regexp.MustCompile(
			`(?i)(?:(\d+)\s+)?` +
				`(?:node\(s\) (?:didn't find available persistent volumes to bind` +
				`|had volume node affinity conflict` +
				`|exceed max volume count` +
				`|had no available volume zone` +
				`|unavailable due to one or more pvc\(s\) bound to non-existent pv\(s\)` +
				`|did not have enough free storage)` +
				`|pod has unbound immediate PersistentVolumeClaims` +
				`|persistentvolumeclaim\s+"[^"]*"\s+(?:not found|bound to non-existent persistentvolume))`,
		),
	},
}

// classifyFailedSchedulingMessage buckets a FailedScheduling message by the reason that
// blocked the MOST nodes.
//
// A real message is usually a histogram, not a single reason —
// "0/5 nodes are available: 3 Insufficient cpu, 2 node(s) had untolerated taint" — and
// dakr's table holds one bucket per event, so one has to win. The node count is the only
// signal in the message about which blocker dominates; clause order carries none, because
// FitError.Error() sorts the histogram lexicographically. Ties break on
// failedSchedulingBucketPriority so the result is deterministic.
//
// Anything unrecognised returns Other. The message is never guessed at, and it is stored
// verbatim in raw_message either way, so a bucket this classifier gets wrong (or has not
// learned yet) can be re-derived later from the stored text.
func classifyFailedSchedulingMessage(message string) string {
	scope := failedSchedulingFilterScope(message)
	if scope == "" {
		return podUnschedulableReasonOther
	}

	weights := make(map[string]int, len(failedSchedulingBucketPriority))

	// "Insufficient <resource>" is one scheduler reason covering every resource, so the
	// resource name — not the phrase — picks the bucket.
	for _, match := range insufficientResourcePattern.FindAllStringSubmatch(scope, -1) {
		bucket, ok := insufficientResourceBucket(match[2])
		if !ok {
			continue
		}
		weights[bucket] += failedSchedulingNodeCount(match[1])
	}

	for _, rule := range failedSchedulingRules {
		for _, match := range rule.pattern.FindAllStringSubmatch(scope, -1) {
			// The same reason can appear more than once in one message (two different
			// taints, say), and those node counts add up.
			weights[rule.bucket] += failedSchedulingNodeCount(match[1])
		}
	}

	best := podUnschedulableReasonOther
	bestWeight := 0
	for _, bucket := range failedSchedulingBucketPriority {
		// Strict >, walking the priority order, means the first bucket listed wins a tie.
		if weights[bucket] > bestWeight {
			best, bestWeight = bucket, weights[bucket]
		}
	}

	return best
}

// insufficientResourceBucket maps a Kubernetes resource name to its bucket. Resources
// with no bucket (ephemeral-storage, pods, hugepages-*) report false so the caller leaves
// them out of the classification entirely.
func insufficientResourceBucket(resource string) (string, bool) {
	switch strings.ToLower(resource) {
	case "cpu":
		return podUnschedulableReasonInsufficientCPU, true
	case "memory":
		return podUnschedulableReasonInsufficientMemory, true
	}

	if acceleratorResourcePattern.MatchString(resource) {
		return podUnschedulableReasonInsufficientGPU, true
	}

	return "", false
}

// failedSchedulingFilterScope trims the trailing preemption section, so only the reasons
// describing why the pod is unschedulable are classified.
func failedSchedulingFilterScope(message string) string {
	if location := preemptionSectionPattern.FindStringIndex(message); location != nil {
		return message[:location[0]]
	}
	return message
}

// failedSchedulingNodeCount reads the node count the scheduler prefixes to a reason. A
// reason with no prefix (a PreFilter rejection) weighs 1 — it still happened once.
func failedSchedulingNodeCount(raw string) int {
	if raw == "" {
		return 1
	}

	count, err := strconv.Atoi(raw)
	if err != nil || count < 1 {
		return 1
	}
	return count
}

// eventChanged detects meaningful changes in an event
func (c *EventCollector) eventChanged(oldEvent, newEvent *corev1.Event) bool {
	changed := c.cDHelper.objectMetaChanged(
		c.GetType(),
		oldEvent.Name,
		oldEvent.ObjectMeta,
		newEvent.ObjectMeta,
	)
	if changed != IgnoreChanges {
		return changed == PushChanges
	}

	// Check for count changes
	if oldEvent.Count != newEvent.Count {
		return true
	}

	// Check for last timestamp changes
	if !oldEvent.LastTimestamp.Equal(&newEvent.LastTimestamp) {
		return true
	}

	// Check for message changes
	if oldEvent.Message != newEvent.Message {
		return true
	}

	// No significant changes detected
	return false
}

// isExcluded checks if an event should be excluded from collection
func (c *EventCollector) isExcluded(event *corev1.Event) bool {
	// Check if monitoring specific namespaces and this event isn't in them
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		found := false
		for _, ns := range c.namespaces {
			if ns == event.Namespace {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}

	// Check if event is specifically excluded
	c.mu.RLock()
	defer c.mu.RUnlock()

	key := types.NamespacedName{
		Namespace: event.Namespace,
		Name:      event.Name,
	}
	return c.excludedEvents[key]
}

// periodicCleanup runs periodically to reset event counters for rate limiting
func (c *EventCollector) periodicCleanup(ctx context.Context) {
	ticker := time.NewTicker(c.retentionPeriod / 2)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done(): // Context cancellation from Start
			c.logger.Info("Context done, stopping periodic cleanup")
			return
		case <-c.stopCh: // Stop signal from Stop() method
			c.logger.Info("Stop signal received, stopping periodic cleanup")
			return
		case <-ticker.C:
			c.mu.Lock()
			// Reset all event counts
			c.eventCounts = make(map[string]int)
			c.mu.Unlock()

			c.logger.Info("Reset event rate limiting counters")
		}
	}
}

// Stop gracefully shuts down the event collector
func (c *EventCollector) Stop() error {
	c.logger.Info("Stopping event collector")

	// 1. Signal the informer factory and cleanup goroutine to stop by closing stopCh.
	select {
	case <-c.stopCh:
		c.logger.Info("Event collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed event collector stop channel")
	}

	// 2. Close the batchChan (input to the batcher).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed event collector batch input channel")
	}

	// 3. Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("Event collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *EventCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *EventCollector) GetType() string {
	return "event"
}

// IsAvailable checks if Event resources can be accessed in the cluster
func (c *EventCollector) IsAvailable(ctx context.Context) bool {
	return true
}

// AddResource manually adds an event resource to be processed by the collector
func (c *EventCollector) AddResource(resource interface{}) error {
	event, ok := resource.(*corev1.Event)
	if !ok {
		return fmt.Errorf("expected *corev1.Event, got %T", resource)
	}

	c.handleEvent(event, EventTypeAdd)
	return nil
}
