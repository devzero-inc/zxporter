// internal/collector/cluster_autoscaler_status_collector.go
package collector

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	telemetry_logger "github.com/devzero-inc/zxporter/internal/logger"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

const (
	// casDefaultSweepInterval is how often the collector re-evaluates the status document
	// on its own, independently of informer traffic. See the sweep loop for why this
	// exists at all.
	casDefaultSweepInterval = time.Minute

	// casDefaultReEmitInterval is the minimum gap between two emissions of the SAME block
	// streak.
	//
	// Both bounds matter. It has to be well under dakr's one-hour freshness window, since
	// a Cluster Autoscaler block clears there purely by going quiet — there is no CAS event
	// meaning "the scale-down I was blocked on went ahead" to key a resolution off, unlike
	// Karpenter — so a collector that stops re-emitting makes a still-blocked node group
	// silently vanish from the live view within the hour.
	//
	// And it has to be well above Cluster Autoscaler's write cadence: CAS rewrites the
	// status ConfigMap every scan (default 10s, and the embedded timestamp changes every
	// time, so every write is a real informer update). Emitting per update would put ~8.6k
	// rows per node group per day onto the k8s_events pipeline to say one unchanging thing.
	casDefaultReEmitInterval = 5 * time.Minute
)

// ClusterAutoscalerStatusCollector watches the kube-system/cluster-autoscaler-status
// ConfigMap and synthesizes disruption-block observations from it.
//
// It is the Cluster Autoscaler analogue of the Karpenter DisruptionBlocked classification
// in disruption_block.go, and emits into the same place: an EnrichedEvent on the ordinary
// Event resource type, carrying reason=ScaleDownBlocked and a disruption_block_reason.
// dakr reads Karpenter's and CAS's blocks from k8s_events in a single query, so a cluster
// running either — or, mid-migration, both — produces one coherent report.
//
// Watching a ConfigMap at all is the compromise the autoscaler forces: CAS has no
// NodeClaim-equivalent CRD, no per-node "unremovable" event, and a status document whose
// schema is versioned by convention rather than by API. Everything this collector cannot
// read confidently degrades to Other with the raw text attached, never to a guess.
type ClusterAutoscalerStatusCollector struct {
	client            kubernetes.Interface
	informerFactory   informers.SharedInformerFactory
	configMapInformer cache.SharedIndexInformer
	batchChan         chan CollectedResource
	resourceChan      chan []CollectedResource
	batcher           *ResourcesBatcher
	stopCh            chan struct{}
	logger            logr.Logger
	telemetryLogger   telemetry_logger.Logger

	sweepInterval  time.Duration
	reEmitInterval time.Duration

	// now is the clock, injectable so tests can drive the re-emit interval and the
	// minimum-hold window without sleeping.
	now func() time.Time

	// mu guards streaks. Sweeps run from both the informer callbacks and the ticker
	// goroutine, so two can genuinely overlap.
	mu sync.Mutex
	// streaks is the live block state, keyed by (involved object kind, name, reason) —
	// the identity of the thing being blocked, not of the streak, so that a streak
	// starting over is recognisable as a change to an entry rather than a new one. It is
	// what makes the emitted count monotonic and the re-emit interval enforceable, and it
	// is pruned every sweep so a node group that stops being blocked stops occupying
	// memory.
	streaks map[string]*casBlockStreak

	// sweepWG tracks the ticker goroutine, so Stop can wait for sweepLoop to actually exit
	// before returning. It does NOT by itself make the batchChan close safe — see chanMu.
	sweepWG sync.WaitGroup

	// chanMu guards the batchChan send/close race. Sweeps run from both the informer
	// callbacks (synchronously, on whatever goroutine client-go dispatches the event from)
	// and the ticker goroutine, and only the latter is covered by sweepWG — an informer
	// callback can still be inside emitBlockObservation's send when Stop runs. Every sender
	// takes the read lock (so concurrent sends don't block each other); Stop takes the
	// write lock to flip stopped and close the channel as one atomic step, so a sender that
	// hasn't yet acquired its RLock will see stopped=true and skip the send instead of
	// racing the close.
	//
	// HARD INVARIANT this depends on (same shape as karpenter_collector.go's chanMu, which
	// has the fuller version of this note): a sender holds the read lock for as long as its
	// send blocks, so Stop's write-lock acquisition cannot proceed while a sender is still
	// waiting for batchChan to have room. The batcher goroutine drains it via a `select`
	// with two exit paths — batchChan closing, or its own b.stopCh closing — and only the
	// first matters here because Stop closes c.batchChan before calling c.batcher.stop()
	// (which is what closes b.stopCh), so the batcher is still draining batchChan for the
	// entire time this section can be waiting on an in-flight sender. If a future change
	// ever closed b.stopCh before this section runs, a sender blocked on a full batchChan
	// would hold the read lock forever and Stop would deadlock. Flagged by automated review
	// on the karpenter_collector.go copy of this pattern; verified it applies identically
	// here rather than assuming it does.
	chanMu  sync.RWMutex
	stopped bool
}

// casBlockStreak is one node group's (or node's) ongoing block.
type casBlockStreak struct {
	// streakStart is when the block began: Cluster Autoscaler's own lastTransitionTime
	// where the document had one, and otherwise the instant this collector first saw the
	// block. Either way it is fixed for the life of the streak, which is what makes the
	// derived UID stable across re-emissions.
	streakStart time.Time
	// lastEmitted is when an observation for this streak last went on the wire, which is
	// what the re-emit interval is measured against.
	lastEmitted time.Time
	// count is the running number of emissions in this streak. dakr reads max(count) per
	// UID, so this must only ever increase within a streak.
	count int32
	// seen marks the streak as still present in the current sweep, so streaks that
	// disappeared can be pruned at the end of it.
	seen bool
}

// casStreakKey identifies the thing being blocked. Reason is part of it because dakr
// aggregates by reason: a block that changes classification is a different streak, and
// folding both under one UID would make one of the two invisible.
func casStreakKey(observation casBlockObservation) string {
	return observation.ObjectKind + "|" + observation.ObjectName + "|" + observation.Reason
}

// NewClusterAutoscalerStatusCollector creates a collector for the Cluster Autoscaler
// status ConfigMap.
func NewClusterAutoscalerStatusCollector(
	client kubernetes.Interface,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
) *ClusterAutoscalerStatusCollector {
	batchChan := make(chan CollectedResource, 100)
	resourceChan := make(chan []CollectedResource, 100)

	batcher := NewResourcesBatcher(maxBatchSize, maxBatchTime, batchChan, resourceChan, logger)

	return &ClusterAutoscalerStatusCollector{
		client:          client,
		batchChan:       batchChan,
		resourceChan:    resourceChan,
		batcher:         batcher,
		stopCh:          make(chan struct{}),
		logger:          logger.WithName("cluster-autoscaler-status-collector"),
		telemetryLogger: telemetryLogger,
		sweepInterval:   casDefaultSweepInterval,
		reEmitInterval:  casDefaultReEmitInterval,
		now:             time.Now,
		streaks:         make(map[string]*casBlockStreak),
	}
}

// Start begins watching the status ConfigMap.
//
// The informer is field-selected down to the single object, so a cluster with thousands of
// ConfigMaps caches exactly one and a cluster with no Cluster Autoscaler caches none. That
// is what makes it safe to run this collector unconditionally rather than gating it on
// autoscaler detection, which is a dakr-side, after-the-fact answer.
func (c *ClusterAutoscalerStatusCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting cluster-autoscaler status collector",
		"namespace", casStatusConfigMapNamespace,
		"configMap", casStatusConfigMapName,
		"sweepInterval", c.sweepInterval,
		"reEmitInterval", c.reEmitInterval)

	c.informerFactory = informers.NewSharedInformerFactoryWithOptions(
		c.client,
		0, // No resync: the sweep ticker below is this collector's periodic path.
		informers.WithTransform(StripMetadataTransform),
		informers.WithNamespace(casStatusConfigMapNamespace),
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.FieldSelector = fields.OneTermEqualSelector(
				"metadata.name", casStatusConfigMapName,
			).String()
		}),
	)

	c.configMapInformer = c.informerFactory.Core().V1().ConfigMaps().Informer()

	_, err := c.configMapInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if configMap, ok := obj.(*corev1.ConfigMap); ok {
				c.sweep(configMap)
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if configMap, ok := newObj.(*corev1.ConfigMap); ok {
				c.sweep(configMap)
			}
		},
		DeleteFunc: func(obj interface{}) {
			// Cluster Autoscaler was uninstalled, or its status ConfigMap was deleted.
			// Nothing is emitted to say so — the blocks simply stop being re-observed and
			// age out of dakr's live view on their own.
			c.forgetAllStreaks()
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add cluster-autoscaler status event handler: %w", err)
	}

	c.informerFactory.Start(c.stopCh)

	c.logger.Info("Waiting for cluster-autoscaler status informer cache to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.configMapInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Cluster-autoscaler status informer cache synced")

	c.batcher.start()

	c.sweepWG.Add(1)
	go c.sweepLoop(ctx)

	stopCh := c.stopCh
	go func() {
		select {
		case <-ctx.Done():
			c.Stop() //nolint:errcheck // Stop only reports channel-close bookkeeping.
		case <-stopCh:
		}
	}()

	return nil
}

// sweepLoop re-evaluates the cached status document on a timer.
//
// THIS IS WHAT KEEPS A BLOCK ALIVE IN DAKR, and it is not redundant with the informer
// handlers. A Cluster Autoscaler block has no resolution event to key off, so dakr treats
// an hour of silence as "the block cleared" — which is only sound if a still-blocked node
// group keeps being re-observed. The informer alone cannot promise that: the shared
// factory is built with no resync period, so if CAS's writes were ever to stop changing
// the object (or the watch were to sit idle), a real, ongoing block would go quiet and
// silently disappear from the live view.
//
// Re-reading the informer's cached object rather than replaying a parsed snapshot keeps
// the ticker honest: it re-emits what the cluster currently says, and if the document has
// stopped being updated, the observation timestamp it carries is the stale one CAS last
// wrote, so dakr ages the block out exactly as it should.
func (c *ClusterAutoscalerStatusCollector) sweepLoop(ctx context.Context) {
	defer c.sweepWG.Done()

	ticker := time.NewTicker(c.sweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.sweep(nil)
		case <-c.stopCh:
			return
		case <-ctx.Done():
			return
		}
	}
}

// sweep parses the status document and emits every block that is due.
//
// A nil configMap means "read whatever the informer currently holds", which is how the
// ticker path calls it.
func (c *ClusterAutoscalerStatusCollector) sweep(configMap *corev1.ConfigMap) {
	if configMap == nil {
		configMap = c.cachedStatusConfigMap()
		if configMap == nil {
			return
		}
	}

	raw, ok := configMap.Data[casStatusDataKey]
	if !ok || strings.TrimSpace(raw) == "" {
		c.logger.V(5).Info("Cluster-autoscaler status ConfigMap has no status data key",
			"configMap", configMap.Name)
		return
	}

	now := c.now()
	result := parseClusterAutoscalerStatus(raw, now)

	if result.Unparseable {
		// Loud, because it means every CAS cluster on this version reports Other forever.
		// Still emitted (with the raw document attached) rather than dropped — see
		// casUnparseableObservation.
		c.logger.Info("Could not parse cluster-autoscaler status document; reporting raw",
			"configMap", configMap.Name,
			"bytes", len(raw))
	}

	for _, observation := range result.Observations {
		c.emitBlockObservation(observation, result.ObservedAt, now)
	}

	c.pruneStreaks()
}

// cachedStatusConfigMap returns the informer's copy of the status ConfigMap, or nil when
// the cluster has no Cluster Autoscaler (or the collector has not started).
func (c *ClusterAutoscalerStatusCollector) cachedStatusConfigMap() *corev1.ConfigMap {
	if c.configMapInformer == nil {
		return nil
	}
	key := casStatusConfigMapNamespace + "/" + casStatusConfigMapName
	obj, exists, err := c.configMapInformer.GetStore().GetByKey(key)
	if err != nil || !exists {
		return nil
	}
	configMap, ok := obj.(*corev1.ConfigMap)
	if !ok {
		return nil
	}
	return configMap
}

// emitBlockObservation records the observation against its streak and, if it is due,
// sends it.
//
// Two gates, in order:
//
//  1. Minimum hold. A node group that has only just acquired scale-down candidates is
//     mid-scale-down, not blocked (see casMinBlockHold). The streak is still recorded
//     during the hold, so the moment it elapses the first emission carries the real streak
//     start rather than looking like a block that began just now.
//  2. Re-emit interval, which throttles an unchanging block down from Cluster
//     Autoscaler's ~10s write cadence.
func (c *ClusterAutoscalerStatusCollector) emitBlockObservation(
	observation casBlockObservation,
	observedAt time.Time,
	now time.Time,
) {
	key := casStreakKey(observation)

	c.mu.Lock()
	streak, known := c.streaks[key]
	if !known {
		streak = &casBlockStreak{}
		c.streaks[key] = streak
	}
	streak.seen = true

	// Resolve the streak start. Cluster Autoscaler's own lastTransitionTime is preferred
	// because it survives a zxporter restart mid-block; when the document has none, the
	// instant this collector first saw the block stands in, and is then held fixed.
	switch {
	case !observation.StreakStart.IsZero():
		if !observation.StreakStart.Equal(streak.streakStart) {
			// Cluster Autoscaler transitioned: the previous block ended and a new one
			// began. A new streak gets its own UID (so dakr does not report one block
			// spanning the quiet gap between them) and its own count.
			streak.streakStart = observation.StreakStart
			streak.count = 0
			streak.lastEmitted = time.Time{}
		}
	case streak.streakStart.IsZero():
		streak.streakStart = now
	}
	streakStart := streak.streakStart

	if now.Sub(streakStart) < casMinBlockHold {
		c.mu.Unlock()
		return
	}
	if !streak.lastEmitted.IsZero() && now.Sub(streak.lastEmitted) < c.reEmitInterval {
		c.mu.Unlock()
		return
	}

	streak.lastEmitted = now
	streak.count++
	count := streak.count
	c.mu.Unlock()

	event := casBlockEvent(observation, streakStart, observedAt, count)

	c.logger.V(5).Info("Reporting cluster-autoscaler scale-down block",
		"involvedObjectKind", observation.ObjectKind,
		"involvedObjectName", observation.ObjectName,
		"reason", observation.Reason,
		"count", count)

	c.chanMu.RLock()
	defer c.chanMu.RUnlock()
	if c.stopped {
		return
	}
	c.batchChan <- CollectedResource{
		ResourceType: Event,
		Object:       event,
		Timestamp:    now,
		EventType:    EventTypeAdd,
		Key:          fmt.Sprintf("%s/%s", event.Namespace, event.Name),
	}
}

// pruneStreaks drops streaks that were not observed in the sweep that just ran, so the
// next occurrence of the same block starts a fresh streak (and, when the document carries
// no timestamps, a fresh UID) instead of being folded into the old one.
func (c *ClusterAutoscalerStatusCollector) pruneStreaks() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for uid, streak := range c.streaks {
		if !streak.seen {
			delete(c.streaks, uid)
			continue
		}
		streak.seen = false
	}
}

func (c *ClusterAutoscalerStatusCollector) forgetAllStreaks() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.streaks = make(map[string]*casBlockStreak)
}

// Stop halts the collector.
func (c *ClusterAutoscalerStatusCollector) Stop() error {
	c.logger.Info("Stopping cluster-autoscaler status collector")

	select {
	case <-c.stopCh:
	default:
		close(c.stopCh)
	}

	// Wait for the ticker goroutine to exit. This does not by itself make closing
	// batchChan safe — an informer-callback-triggered sweep is not tracked here — see
	// chanMu, which is what actually prevents the send-after-close race below.
	c.sweepWG.Wait()

	c.chanMu.Lock()
	c.stopped = true
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
	}
	c.chanMu.Unlock()

	if c.batcher != nil {
		c.batcher.stop()
	}

	c.forgetAllStreaks()

	return nil
}

// GetResourceChannel returns the channel of batched resources.
func (c *ClusterAutoscalerStatusCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the collector's registry key.
func (c *ClusterAutoscalerStatusCollector) GetType() string {
	return ClusterAutoscalerStatus.String()
}

// IsAvailable always reports true.
//
// It deliberately does not probe for the ConfigMap. Cluster Autoscaler can be installed
// long after zxporter, and registration happens once at startup, so a probe would leave
// those clusters permanently unwatched. The field-selected informer costs a single watch
// that matches nothing on a cluster without CAS.
func (c *ClusterAutoscalerStatusCollector) IsAvailable(ctx context.Context) bool {
	return true
}

// AddResource is not supported: everything this collector reports is derived from the
// status document.
func (c *ClusterAutoscalerStatusCollector) AddResource(resource interface{}) error {
	return nil
}
