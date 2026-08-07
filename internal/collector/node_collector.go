// internal/collector/node_collector.go
package collector

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	gpuconst "github.com/NVIDIA/KAI-scheduler/pkg/common/constants"
	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	telemetry_logger "github.com/devzero-inc/zxporter/internal/logger"
	"github.com/devzero-inc/zxporter/internal/version"
	"github.com/go-logr/logr"
	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	metricsv1 "k8s.io/metrics/pkg/client/clientset/versioned"
)

// NodeCollectorConfig holds configuration for the node collector
type NodeCollectorConfig struct {
	// UpdateInterval specifies how often to collect metrics
	UpdateInterval time.Duration

	// DisableGPUMetrics determines whether to disable GPU metrics collection
	// Default is false, so metrics are collected by default
	DisableGPUMetrics bool

	// MaxConcurrentNodeCollections bounds how many nodes' metrics are
	// collected in parallel per sweep. Non-positive falls back to
	// defaultMaxConcurrentNodeCollections. See
	// Policies.NodeMetricsConcurrency's doc comment (api/v1/collectionpolicy_types.go)
	// for the full reasoning.
	MaxConcurrentNodeCollections int

	// NodemonRequestTimeout bounds each HTTP call to a node's nodemon pod.
	// Non-positive falls back to defaultNodemonRequestTimeout.
	NodemonRequestTimeout time.Duration

	// KubeletFallbackTimeout bounds each call to the kubelet Summary API
	// fallback. Non-positive falls back to defaultKubeletFallbackTimeout.
	KubeletFallbackTimeout time.Duration
}

// NodeCollector collects node events and resource metrics
type NodeCollector struct {
	k8sClient       kubernetes.Interface
	metricsClient   *metricsv1.Clientset
	nodemonClient   *NodemonClient
	kubeletClient   *KubeletSummaryClient
	informerFactory informers.SharedInformerFactory
	nodeInformer    cache.SharedIndexInformer
	batchChan       chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan    chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher         *ResourcesBatcher
	stopCh          chan struct{}
	ticker          *time.Ticker
	config          NodeCollectorConfig
	excludedNodes   map[string]bool
	logger          logr.Logger
	metrics         *TelemetryMetrics
	telemetryLogger telemetry_logger.Logger
	mu              sync.RWMutex
	nodeToPodsMap   map[string]map[string]*corev1.Pod // Maps node name -> pod key -> pod object
	podInformer     cache.SharedIndexInformer
	podMapMutex     sync.RWMutex

	// loopWG tracks collectNodeResourcesLoop's goroutine so Stop() can wait
	// for an in-flight sweep to finish before closing batchChan. Closing
	// stopCh alone only stops the loop from starting its *next* sweep — a
	// sweep already in progress (now up to config.MaxConcurrentNodeCollections
	// workers, each blocking on a send to batchChan) doesn't observe stopCh
	// until it returns to the loop's select. Without waiting here, Stop()
	// could close batchChan while a worker is still sending on it, which
	// panics the whole process.
	loopWG sync.WaitGroup

	// nodeClaimSources resolves the Karpenter collector's NodeClaim cache through the
	// CollectionManager, so the lifecycle fallback below can tell a Karpenter-managed
	// Node (already reported in full by karpenter_collector.go) from one this collector
	// is the only source for. Set by the reconciler via SetNodeClaimSource; nil-safe.
	//
	// Atomic for the same reason the event collector's equivalent is: the reconciler
	// re-wires a replaced collector after its informer is already dispatching, so the
	// write genuinely races the read.
	nodeClaimSources atomic.Pointer[disruptionSources]

	// nodeLifecycle tracks what has already been reported per Node, keyed by node name,
	// so an informer update storm does not re-report the same transition. Guarded by
	// lifecycleMu, which is separate from mu so it never contends with the metrics sweep.
	lifecycleMu   sync.Mutex
	nodeLifecycle map[string]*nodeLifecycleState

	// chanMu guards the resourceChan send/close race for handleNodeEvent and
	// sendNodeLifecycleTransition, which send DIRECTLY to resourceChan (bypassing the
	// batcher) from the Node informer's callback goroutine. loopWG above only tracks the
	// periodic collectNodeResourcesLoop's workers, which send on batchChan, not this path
	// — an informer callback racing resourceChan's close (via c.batcher.stop() in Stop())
	// is a separate, unguarded race. Same pattern as karpenter_collector.go's chanMu:
	// every sender takes the read lock and checks stopped before sending; Stop() takes the
	// write lock to flip stopped before triggering the channel's close, so a sender either
	// completes its send first or observes stopped=true and returns without sending.
	//
	// HARD INVARIANT this depends on: a sender holds the read lock for the DURATION of its
	// (possibly blocking) channel send, so Stop's write-lock acquisition cannot proceed
	// until that send completes — which requires resourceChan to still have a reader at
	// that moment. CollectionManager.processCollectorChannel is that reader, and it is
	// structurally safe: its `for resources := range resourceChan` loop has no independent
	// exit — the ONLY way it stops is resourceChan closing, which cannot happen before this
	// write-lock section runs. If that loop is ever changed to exit on some other signal
	// (e.g. a select against its own stop channel) before Stop is called here, a sender
	// blocked on a full resourceChan would hold the read lock forever and Stop would
	// deadlock acquiring the write lock. Flagged by automated review; verified the invariant
	// holds against the current manager.go rather than just documenting the risk.
	chanMu  sync.RWMutex
	stopped bool
}

// nodeLifecycleState is what the Cluster Autoscaler time-to-Ready fallback remembers
// about one Node.
type nodeLifecycleState struct {
	// launchedEmitted records that the Launched row has gone out. The Node's
	// creationTimestamp never changes, so it is reported exactly once.
	launchedEmitted bool
	// readyEmitted records that the Ready row has gone out.
	readyEmitted bool
	// observedNotReady records that this collector watched the Node while it was NOT
	// Ready. That is what distinguishes a Ready transition we witnessed — where the
	// timestamp is unambiguously the first one — from a Node that was already Ready when
	// the informer first listed it. See emitNodeLifecycleFallback.
	observedNotReady bool
}

// NewNodeCollector creates a new collector for node resources
func NewNodeCollector(
	k8sClient kubernetes.Interface,
	metricsClient *metricsv1.Clientset,
	config NodeCollectorConfig,
	excludedNodes []string,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	metrics *TelemetryMetrics,
	telemetryLogger telemetry_logger.Logger,
) *NodeCollector {
	// Convert excluded nodes to a map for quicker lookups
	excludedNodesMap := make(map[string]bool)
	for _, node := range excludedNodes {
		excludedNodesMap[node] = true
	}

	// Default update interval if not specified
	if config.UpdateInterval <= 0 {
		config.UpdateInterval = 10 * time.Second
	}

	// Create channels
	batchChan := make(chan CollectedResource, 100)      // For metrics
	resourceChan := make(chan []CollectedResource, 100) // For events and batched metrics

	// Create the batcher for metrics
	batcher := NewResourcesBatcher(
		maxBatchSize,
		maxBatchTime,
		batchChan,
		resourceChan, // Batcher output goes to the same channel as direct events
		logger,
	)

	ns := os.Getenv("POD_NAMESPACE")
	if ns == "" {
		ns = defaultNamespace
	}
	nodemonClient := NewNodemonClient(k8sClient, ns, logger, config.NodemonRequestTimeout)
	kubeletClient := NewKubeletSummaryClient(k8sClient, logger, config.KubeletFallbackTimeout)

	return &NodeCollector{
		k8sClient:       k8sClient,
		metricsClient:   metricsClient,
		nodemonClient:   nodemonClient,
		kubeletClient:   kubeletClient,
		batchChan:       batchChan,
		resourceChan:    resourceChan,
		batcher:         batcher,
		stopCh:          make(chan struct{}),
		config:          config,
		excludedNodes:   excludedNodesMap,
		logger:          logger.WithName("node-collector"),
		metrics:         metrics,
		telemetryLogger: telemetryLogger,
		nodeToPodsMap:   make(map[string]map[string]*corev1.Pod),
		nodeLifecycle:   make(map[string]*nodeLifecycleState),
	}
}

// SetNodeClaimSource gives the collector its view of the Karpenter collector's NodeClaim
// cache, which is what lets the time-to-Ready fallback stay out of the way on
// Karpenter-managed nodes.
//
// Optional by design. Left unset — as it is on a cluster with no Karpenter, and in every
// test that does not exercise it — the fallback relies on the Node's own Karpenter labels
// instead, which is the check that actually matters (see nodeManagedByKarpenter).
//
// Safe to call on a running collector, for the same reason
// EventCollector.SetDisruptionSources is.
func (c *NodeCollector) SetNodeClaimSource(registry collectorRegistry) {
	c.nodeClaimSources.Store(&disruptionSources{registry: registry})
}

// Start begins the node collection process
func (c *NodeCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting node collector",
		"updateInterval", c.config.UpdateInterval,
		"disableGPUMetrics", c.config.DisableGPUMetrics)

	// Create informer factory (StripMetadataTransform applied via newInformerFactory).
	c.informerFactory = newInformerFactory(c.k8sClient, nil)

	// Create node informer
	c.nodeInformer = c.informerFactory.Core().V1().Nodes().Informer()

	c.podInformer = c.informerFactory.Core().V1().Pods().Informer()

	// Add pod event handlers
	_, err := c.podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod := obj.(*corev1.Pod)
			c.handlePodEvent(pod, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldPod := oldObj.(*corev1.Pod)
			newPod := newObj.(*corev1.Pod)
			c.handlePodEvent(newPod, EventTypeUpdate)

			// If node assignment changed, handle as delete for old node
			if oldPod.Spec.NodeName != newPod.Spec.NodeName {
				c.removePodFromNode(oldPod)
			}
		},
		DeleteFunc: func(obj interface{}) {
			pod := obj.(*corev1.Pod)
			c.handlePodEvent(pod, EventTypeDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add pod event handler: %w", err)
	}

	// Add event handlers
	_, err = c.nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			node := obj.(*corev1.Node)
			c.handleNodeEvent(node, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldNode := oldObj.(*corev1.Node)
			newNode := newObj.(*corev1.Node)

			// Only send updates if there's a meaningful change
			if c.nodeStatusChanged(oldNode, newNode) {
				c.handleNodeEvent(newNode, EventTypeUpdate)
			}
		},
		DeleteFunc: func(obj interface{}) {
			node := obj.(*corev1.Node)
			c.handleNodeEvent(node, EventTypeDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start the informer factory
	c.informerFactory.Start(c.stopCh)

	// Wait for cache sync
	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.nodeInformer.HasSynced, c.podInformer.HasSynced) {
		if c.telemetryLogger != nil {
			c.telemetryLogger.Report(
				gen.LogLevel_LOG_LEVEL_ERROR,
				"NodeCollector",
				"Timed out waiting for caches to sync",
				fmt.Errorf("cache sync timeout"),
				map[string]string{
					"excluded_nodes":   fmt.Sprintf("%v", c.excludedNodes),
					"zxporter_version": version.Get().String(),
				},
			)
		}
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	// Start the batcher (for metrics) after the cache is synced
	c.logger.Info("Starting resources batcher for node metrics")
	c.batcher.start()

	// Start a ticker to collect resource metrics at regular intervals
	c.ticker = time.NewTicker(c.config.UpdateInterval)

	// Start the resource collection loop. loopWG lets Stop() wait for an
	// in-flight sweep to finish before closing batchChan — see the comment
	// on loopWG's field declaration.
	c.loopWG.Add(1)
	go c.collectNodeResourcesLoop(ctx)

	// Monitor for context cancellation
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

// Add these new methods for pod event handling
func (c *NodeCollector) handlePodEvent(pod *corev1.Pod, eventType EventType) {
	// Skip pods not assigned to nodes yet
	if pod.Spec.NodeName == "" {
		return
	}

	// Skip excluded nodes
	if c.isExcluded(pod.Spec.NodeName) {
		return
	}

	switch eventType {
	case EventTypeAdd, EventTypeUpdate:
		c.addPodToNode(pod)
	case EventTypeDelete:
		c.removePodFromNode(pod)
	}
}

// addPodToNode add pod to node
func (c *NodeCollector) addPodToNode(pod *corev1.Pod) {
	c.podMapMutex.Lock()
	defer c.podMapMutex.Unlock()

	nodeName := pod.Spec.NodeName
	podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)

	// Initialize the pod map for this node if it doesn't exist
	if _, exists := c.nodeToPodsMap[nodeName]; !exists {
		c.nodeToPodsMap[nodeName] = make(map[string]*corev1.Pod)
	}

	c.nodeToPodsMap[nodeName][podKey] = pod
}

// removePodFromNode removes pod from existing node
func (c *NodeCollector) removePodFromNode(pod *corev1.Pod) {
	c.podMapMutex.Lock()
	defer c.podMapMutex.Unlock()

	nodeName := pod.Spec.NodeName
	podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)

	// Remove the pod from the node map
	if podMap, exists := c.nodeToPodsMap[nodeName]; exists {
		delete(podMap, podKey)
	}
}

// PodsOnNode returns the pods the collector currently tracks for a node.
//
// This reuses nodeToPodsMap — the node→pod correlation this collector already maintains
// off its pod informer for resource accounting — instead of standing up a second pod
// cache. The slice is fresh, but the pod pointers are the informer's cached objects and
// MUST be treated as read-only.
//
// Returns nil for an unknown node, which is indistinguishable from "node has no pods".
// Both mean "no pod here can be blocking", so the distinction has no caller.
func (c *NodeCollector) PodsOnNode(nodeName string) []*corev1.Pod {
	c.podMapMutex.RLock()
	defer c.podMapMutex.RUnlock()

	podMap, exists := c.nodeToPodsMap[nodeName]
	if !exists {
		return nil
	}

	pods := make([]*corev1.Pod, 0, len(podMap))
	for _, pod := range podMap {
		pods = append(pods, pod)
	}
	return pods
}

// Calculate resource requests and limits for a node
func (c *NodeCollector) calculateNodeWorkloadResources(nodeName string) map[string]interface{} {
	c.podMapMutex.RLock()
	defer c.podMapMutex.RUnlock()

	result := map[string]interface{}{
		"cpuRequestsMillis":   int64(0),
		"cpuLimitsMillis":     int64(0),
		"memoryRequestsBytes": int64(0),
		"memoryLimitsBytes":   int64(0),
		"gpuRequestCount":     int64(0),
		"gpuLimitCount":       int64(0),
	}

	// Check if we have pods for this node
	podMap, exists := c.nodeToPodsMap[nodeName]
	if !exists {
		return result
	}

	// Calculate total requests and limits
	for _, pod := range podMap {
		// Skip pods not in Running or Pending phase
		if pod.Status.Phase != corev1.PodRunning && pod.Status.Phase != corev1.PodPending {
			continue
		}

		// Calculate resources for containers
		for _, container := range pod.Spec.Containers {
			// CPU requests
			if val, ok := container.Resources.Requests[corev1.ResourceCPU]; ok {
				result["cpuRequestsMillis"] = result["cpuRequestsMillis"].(int64) + val.MilliValue()
			}

			// CPU limits
			if val, ok := container.Resources.Limits[corev1.ResourceCPU]; ok {
				result["cpuLimitsMillis"] = result["cpuLimitsMillis"].(int64) + val.MilliValue()
			}

			// Memory requests
			if val, ok := container.Resources.Requests[corev1.ResourceMemory]; ok {
				result["memoryRequestsBytes"] = result["memoryRequestsBytes"].(int64) + val.Value()
			}

			// Memory limits
			if val, ok := container.Resources.Limits[corev1.ResourceMemory]; ok {
				result["memoryLimitsBytes"] = result["memoryLimitsBytes"].(int64) + val.Value()
			}

			// GPU requests
			if val, ok := container.Resources.Requests[gpuconst.GpuResource]; ok {
				result["gpuRequestCount"] = result["gpuRequestCount"].(int64) + val.Value()
			}

			// GPU limits
			if val, ok := container.Resources.Limits[gpuconst.GpuResource]; ok {
				result["gpuLimitCount"] = result["gpuLimitCount"].(int64) + val.Value()
			}
		}
	}

	return result
}

// sendResourceEvent puts one batch directly on resourceChan, or drops it silently if Stop
// has already run. See chanMu's doc comment for why this check-then-send must be atomic
// with Stop's close.
func (c *NodeCollector) sendResourceEvent(batch []CollectedResource) {
	c.chanMu.RLock()
	defer c.chanMu.RUnlock()
	if c.stopped {
		return
	}
	c.resourceChan <- batch
}

// handleNodeEvent processes node add, update, and delete events
func (c *NodeCollector) handleNodeEvent(node *corev1.Node, eventType EventType) {
	if c.isExcluded(node.Name) {
		return
	}

	// Send node events directly to resourceChan as a single-item batch
	c.sendResourceEvent([]CollectedResource{
		{
			ResourceType: Node,
			Object:       node,
			Timestamp:    time.Now(),
			EventType:    eventType,
			Key:          node.Name,
		},
	})

	// Additionally — never instead of — report the node's lifecycle timings when nothing
	// else will. The Node resource above is unchanged.
	c.emitNodeLifecycleFallback(node, eventType)
}

// Node labels the lifecycle fallback reads.
const (
	// karpenterNodePoolLabel is stamped by Karpenter on every Node it registers. Its
	// presence means karpenter_collector.go is already reporting that node's full
	// four-phase lifecycle off the NodeClaim, so this fallback must stay away.
	// Source: sigs.k8s.io/karpenter/pkg/apis/v1/labels.go.
	karpenterNodePoolLabel = "karpenter.sh/nodepool"
	// karpenterRegisteredLabel is the other half of that marker, set by the NodeClaim
	// lifecycle controller once it has claimed the Node.
	karpenterRegisteredLabel = "karpenter.sh/registered"
	// instanceTypeLabel is the well-known Kubernetes label every cloud provider's kubelet
	// sets. Reported alongside the transition because the read path groups by it.
	// Source: kubernetes.io/docs/reference/labels-annotations-taints.
	instanceTypeLabel = "node.kubernetes.io/instance-type"
	// legacyInstanceTypeLabel is its pre-1.17 spelling, still present on older clusters.
	legacyInstanceTypeLabel = "beta.kubernetes.io/instance-type"
)

// nodeReadySeedWindow bounds how long after a Node's creation its Ready transition may be
// and still be taken as the FIRST one, when this collector did not witness the transition
// itself.
//
// Every Node in the cluster arrives already-Ready on the informer's initial list, and for
// almost all of them the Ready condition's lastTransitionTime is still the original one —
// which is exactly the number this signal wants, and seeding it is what gives a freshly
// installed zxporter any history at all. But for a node that has since flapped
// NotReady→Ready, that timestamp is the flap, not the boot; reporting it as time-to-Ready
// would fabricate a duration of hours or days that looks entirely legitimate downstream.
//
// An hour is far longer than any real node takes to become Ready (seconds to a few
// minutes) and far shorter than a plausible flap-after-boot gap, so the window separates
// the two cleanly. Its failure mode is declining to report, never reporting a wrong
// number.
const nodeReadySeedWindow = time.Hour

// emitNodeLifecycleFallback reports a Node's own time-to-Ready when no NodeClaim covers
// it — the Cluster Autoscaler (and unmanaged-node) path.
//
// It emits the SAME NodeLifecycleTransition resource Karpenter's collector emits, into the
// same ClickHouse table, with the Node's name standing in for node_claim_name. That is
// deliberate: the read path pivots per node_claim_name and derives whichever phase
// durations it finds, so one query serves both autoscalers and a cluster mid-migration
// between them, instead of a second table and a union at the call site.
//
// Only two of the four phases are reported:
//
//   - Launched, from the Node's creationTimestamp. An approximation, and knowingly so: it
//     is when the kubelet registered with the API server, which is after the instance
//     actually booted. Cluster Autoscaler exposes nothing earlier — it has no
//     NodeClaim-equivalent object — so this is the earliest instant that exists.
//   - Ready, from the Ready condition's lastTransitionTime.
//
// Registered and Initialized are left ABSENT, not zero: they are Karpenter NodeClaim
// concepts (registration with the cluster, startup taints removed, allocatable populated)
// with no Cluster Autoscaler equivalent, and a fabricated value for either would show up
// as a real phase duration in the report. dakr's GetTimeToReady already yields NULL phase
// durations and a correct total for a two-row node.
func (c *NodeCollector) emitNodeLifecycleFallback(node *corev1.Node, eventType EventType) {
	if eventType == EventTypeDelete {
		c.forgetNodeLifecycle(node.Name)
		return
	}

	if c.nodeManagedByKarpenter(node) {
		return
	}

	created := node.CreationTimestamp.Time
	if created.IsZero() {
		// Nothing to measure from. Unreachable through the API server, which always
		// stamps creationTimestamp.
		return
	}

	readyAt, isReady := nodeReadyTransition(node)

	c.lifecycleMu.Lock()
	state, known := c.nodeLifecycle[node.Name]
	if !known {
		state = &nodeLifecycleState{}
		c.nodeLifecycle[node.Name] = state
	}

	emitLaunched := !state.launchedEmitted
	state.launchedEmitted = true

	witnessedTransition := state.observedNotReady
	if !isReady {
		state.observedNotReady = true
	}

	emitReady := isReady && !state.readyEmitted &&
		(witnessedTransition || readyAt.Sub(created) <= nodeReadySeedWindow)
	if emitReady {
		state.readyEmitted = true
	}
	c.lifecycleMu.Unlock()

	if emitLaunched {
		c.sendNodeLifecycleTransition(node, "Launched", created)
	}
	if emitReady {
		c.sendNodeLifecycleTransition(node, "Ready", readyAt)
	}
}

// nodeManagedByKarpenter reports whether karpenter_collector.go is already the source of
// truth for this Node's lifecycle.
//
// The Node's own Karpenter labels are checked FIRST and are what the decision really
// rests on. Consulting the NodeClaim cache alone would be a race: on a Karpenter cluster
// a Node can reach this handler before the NodeClaim informer has listed the claim that
// owns it, and the fallback would then emit a duplicate, differently-keyed lifecycle for
// a node Karpenter is about to report in full. The label is written by Karpenter as part
// of registering the Node, so it is present on the object from the first time this
// collector ever sees it.
//
// The NodeClaim lookup is still consulted as a backstop for a Node whose labels were
// stripped or not yet propagated.
func (c *NodeCollector) nodeManagedByKarpenter(node *corev1.Node) bool {
	if _, ok := node.Labels[karpenterNodePoolLabel]; ok {
		return true
	}
	if _, ok := node.Labels[karpenterRegisteredLabel]; ok {
		return true
	}

	nodeClaims := c.nodeClaimSources.Load().nodeClaims()
	if nodeClaims == nil {
		return false
	}
	return nodeClaims.NodeClaimForNode(node.Name) != nil
}

// nodeReadyTransition returns when the Node's Ready condition last became True.
func nodeReadyTransition(node *corev1.Node) (time.Time, bool) {
	for _, condition := range node.Status.Conditions {
		if condition.Type != corev1.NodeReady {
			continue
		}
		if condition.Status != corev1.ConditionTrue {
			return time.Time{}, false
		}
		if condition.LastTransitionTime.IsZero() {
			return time.Time{}, false
		}
		return condition.LastTransitionTime.Time, true
	}
	return time.Time{}, false
}

// sendNodeLifecycleTransition puts one lifecycle row on the wire.
//
// The payload matches the Karpenter collector's byte for byte, minus the keys this path
// has no value for. Absent keys become NULL columns in dakr, which is why they are omitted
// rather than sent empty: reservation_type in particular is a Karpenter capacity-type
// label with no portable Cluster Autoscaler equivalent (each cloud spells its
// spot/on-demand marker differently), and normalising three vendor labels into one value
// would be a guess.
func (c *NodeCollector) sendNodeLifecycleTransition(node *corev1.Node, condition string, at time.Time) {
	object := map[string]interface{}{
		"node_claim_name":      node.Name,
		"node_name":            node.Name,
		"condition":            condition,
		"status":               "True",
		"last_transition_time": at.UTC().Format(time.RFC3339Nano),
	}
	if instanceType := nodeInstanceType(node); instanceType != "" {
		object["instance_type"] = instanceType
	}

	c.sendResourceEvent([]CollectedResource{
		{
			ResourceType: NodeLifecycleTransition,
			Object:       object,
			Timestamp:    time.Now(),
			EventType:    EventTypeAdd,
			Key:          fmt.Sprintf("node-lifecycle/%s/%s", node.Name, condition),
		},
	})
}

func nodeInstanceType(node *corev1.Node) string {
	if instanceType, ok := node.Labels[instanceTypeLabel]; ok {
		return instanceType
	}
	return node.Labels[legacyInstanceTypeLabel]
}

// forgetNodeLifecycle drops the tracked state for a deleted Node, so the map cannot grow
// with cluster churn.
func (c *NodeCollector) forgetNodeLifecycle(nodeName string) {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	delete(c.nodeLifecycle, nodeName)
}

// nodeStatusChanged checks if there have been meaningful changes to node status
func (c *NodeCollector) nodeStatusChanged(oldNode, newNode *corev1.Node) bool {
	// Check if conditions changed
	if len(oldNode.Status.Conditions) != len(newNode.Status.Conditions) {
		return true
	}

	// Create map of old conditions for quick lookup
	oldConditions := make(map[corev1.NodeConditionType]corev1.NodeCondition)
	for _, condition := range oldNode.Status.Conditions {
		oldConditions[condition.Type] = condition
	}

	// Check if any condition changed
	for _, newCondition := range newNode.Status.Conditions {
		oldCondition, exists := oldConditions[newCondition.Type]
		if !exists || oldCondition.Status != newCondition.Status ||
			oldCondition.Reason != newCondition.Reason ||
			oldCondition.Message != newCondition.Message {
			return true
		}
	}

	// Check if allocatable resources changed (CPU, memory, GPU)
	if !oldNode.Status.Allocatable.Cpu().Equal(*newNode.Status.Allocatable.Cpu()) ||
		!oldNode.Status.Allocatable.Memory().Equal(*newNode.Status.Allocatable.Memory()) {
		return true
	}
	oldAllocGPU := oldNode.Status.Allocatable[corev1.ResourceName(gpuconst.GpuResource)]
	newAllocGPU := newNode.Status.Allocatable[corev1.ResourceName(gpuconst.GpuResource)]
	if !oldAllocGPU.Equal(newAllocGPU) {
		return true
	}

	// Check if capacity changed (CPU, memory, GPU)
	if !oldNode.Status.Capacity.Cpu().Equal(*newNode.Status.Capacity.Cpu()) ||
		!oldNode.Status.Capacity.Memory().Equal(*newNode.Status.Capacity.Memory()) {
		return true
	}
	oldCapGPU := oldNode.Status.Capacity[corev1.ResourceName(gpuconst.GpuResource)]
	newCapGPU := newNode.Status.Capacity[corev1.ResourceName(gpuconst.GpuResource)]
	if !oldCapGPU.Equal(newCapGPU) {
		return true
	}

	if !reflect.DeepEqual(oldNode.Labels, newNode.Labels) {
		return true
	}

	if !reflect.DeepEqual(oldNode.Annotations, newNode.Annotations) {
		return true
	}

	if !reflect.DeepEqual(oldNode.UID, newNode.UID) {
		return true
	}

	// No significant changes detected
	return false
}

// collectNodeResourcesLoop collects node resource metrics at regular intervals
func (c *NodeCollector) collectNodeResourcesLoop(ctx context.Context) {
	defer c.loopWG.Done()

	// Collect immediately on start
	c.collectAllNodeResources(ctx)

	// Then collect based on ticker
	for {
		select {
		case <-c.stopCh:
			return
		case <-c.ticker.C:
			c.collectAllNodeResources(ctx)
		}
	}
}

// collectAllNodeResources collects resource metrics for all nodes.
//
// defaultMaxConcurrentNodeCollections bounds how many nodes' metrics are
// collected in parallel per sweep when NodeCollectorConfig.MaxConcurrentNodeCollections
// isn't set. collectAllNodeResources used to do this serially — see
// https://github.com/devzero-inc/services/issues/9410 — where a
// several-hundred-node, high-churn fleet made sweeps balloon to 60-90s+
// against the 10s collection tick, smearing a single sweep's node
// timestamps across multiple wall-clock minutes. Bounded (rather than
// unbounded) fan-out avoids hammering the API server (the kubelet fallback
// path goes through it) or every nodemon pod at once on very large fleets.
// This default hasn't been empirically load-tested against very large
// fleets — see Policies.NodeMetricsConcurrency's doc comment
// (api/v1/collectionpolicy_types.go) for how to tune it per cluster.
const defaultMaxConcurrentNodeCollections = 20

// nodeCollectionOutcome is one node's coverage classification, reported back
// from the parallel per-node worker for aggregate telemetry after the fact.
type nodeCollectionOutcome struct {
	nodeName string
	coverage nodeCoverage
	// usedLegacy is true when the composite /v2/node/snapshot request had to
	// fall back to the legacy /node/metrics + /container/metrics endpoints for
	// this node (e.g. a nodemon pod that predates the composite contract). It
	// means this node paid the old 2-calls-per-node cost instead of 1, and is
	// aggregated into a per-sweep signal so a stalled rollout is visible.
	usedLegacy     bool
	fallbackReason string
	// gpuStale is true when this node's composite GPU section was stale (nodemon's
	// DCGM refresh is failing) and therefore dropped. Aggregated per sweep so the
	// DCGM problem is visible in DAKR telemetry, not only in nodemon's logs.
	gpuStale bool
}

type nodeCoverage int

const (
	nodeCoverageNodemon nodeCoverage = iota
	nodeCoverageKubeletFallback
	nodeCoverageUncovered
)

func (c *NodeCollector) collectAllNodeResources(ctx context.Context) {
	// Skip if metrics client is unavailable
	if c.metricsClient == nil {
		c.logger.Info("Metrics client not available, skipping node metrics collection")
		c.telemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_ERROR,
			"NodeCollector",
			"Metrics client not available, skipping node metrics collection",
			fmt.Errorf("metrics server client not available or properly set"),
			map[string]string{
				"collector_type":   c.GetType(),
				"error_type":       "nil_metrics_server_client",
				"zxporter_version": version.Get().String(),
			},
		)
		return
	}

	// Captured once, up front — the per-node work below does real
	// nodemon/kubelet network round trips and can take real wall-clock time,
	// so it must never re-read the informer cache; that second read is what
	// used to race against concurrent node deletions (e.g. spot instance
	// terminations) and silently drop the node's containers for the cycle.
	nodes := c.nodeInformer.GetIndexer().List()
	nodeObjs := make([]*corev1.Node, 0, len(nodes))
	for _, obj := range nodes {
		if node, ok := obj.(*corev1.Node); ok {
			nodeObjs = append(nodeObjs, node)
		}
	}

	outcomes := make([]nodeCollectionOutcome, len(nodeObjs))

	concurrency := c.config.MaxConcurrentNodeCollections
	if concurrency <= 0 {
		concurrency = defaultMaxConcurrentNodeCollections
	}

	var g errgroup.Group
	g.SetLimit(concurrency)
	for i, node := range nodeObjs {
		i, node := i, node
		g.Go(func() error {
			outcomes[i] = c.collectSingleNodeResources(ctx, node)
			return nil // per-node failures are logged/reported, never abort the sweep
		})
	}
	_ = g.Wait() // collectSingleNodeResources never returns an error

	// Coverage accounting: nodemon is the primary source; nodes without a
	// nodemon pod fall back to the kubelet Summary API. Nodes where both
	// fail are true gaps.
	var nodemonCovered, kubeletFallback int
	var uncoveredNodes []string
	// Nodes that fell back from the composite /v2/node/snapshot to the legacy
	// per-metric endpoints — i.e. paid the old 2-calls-per-node cost. Tracked
	// per sweep so a mixed-version rollout that never converges to the composite
	// path is visible instead of silently slow.
	var legacyFallbackNodes []string
	legacyReasons := map[string]int{}
	// Nodes whose GPU section was stale (nodemon's DCGM refresh failing) and thus
	// dropped — surfaced as a per-sweep signal so a DCGM problem is visible in
	// DAKR telemetry, not just nodemon logs.
	var gpuStaleNodes []string
	for _, o := range outcomes {
		switch o.coverage {
		case nodeCoverageNodemon:
			nodemonCovered++
		case nodeCoverageKubeletFallback:
			kubeletFallback++
		case nodeCoverageUncovered:
			uncoveredNodes = append(uncoveredNodes, o.nodeName)
		}
		if o.usedLegacy {
			legacyFallbackNodes = append(legacyFallbackNodes, o.nodeName)
			legacyReasons[o.fallbackReason]++
		}
		if o.gpuStale {
			gpuStaleNodes = append(gpuStaleNodes, o.nodeName)
		}
	}
	c.logger.V(1).Info("Built node metrics",
		"nodes", len(outcomes),
		"nodemonCovered", nodemonCovered,
		"kubeletFallback", kubeletFallback,
		"uncovered", len(uncoveredNodes))

	if c.telemetryLogger != nil {
		c.telemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_INFO,
			"NodeCollector",
			"Fetched node metrics",
			nil,
			map[string]string{
				"node_count":       fmt.Sprintf("%d", len(outcomes)),
				"nodemon_covered":  fmt.Sprintf("%d", nodemonCovered),
				"kubelet_fallback": fmt.Sprintf("%d", kubeletFallback),
				"legacy_fallback":  fmt.Sprintf("%d", len(legacyFallbackNodes)),
				"excluded_nodes":   fmt.Sprintf("%v", c.excludedNodes),
				"event_type":       "node_metrics_query_success",
				"zxporter_version": version.Get().String(),
			},
		)
	}

	// Signal when nodemon pods are serving the legacy per-metric endpoints
	// instead of the composite /v2/node/snapshot — this node collector is paying
	// the old 2-calls-per-node cost. Expected transiently during a nodemon
	// rolling upgrade; a persistent nonzero count means the fleet never
	// converged to the composite path.
	if len(legacyFallbackNodes) > 0 {
		c.logger.Info("Node metrics served via legacy fallback (composite snapshot unavailable)",
			"count", len(legacyFallbackNodes), "reasons", legacyReasons)
		if c.telemetryLogger != nil {
			// Cap the sample list so this one field can't bloat on a large fleet;
			// legacy_fallback always carries the true total.
			const maxLegacyNodesSample = 20
			sample := legacyFallbackNodes
			suffix := ""
			if len(sample) > maxLegacyNodesSample {
				suffix = fmt.Sprintf(" (+%d more)", len(sample)-maxLegacyNodesSample)
				sample = sample[:maxLegacyNodesSample]
			}
			c.telemetryLogger.Report(
				gen.LogLevel_LOG_LEVEL_WARN,
				"NodeCollector",
				"Node metrics served via legacy fallback instead of composite snapshot",
				nil,
				map[string]string{
					"legacy_fallback":  fmt.Sprintf("%d", len(legacyFallbackNodes)),
					"fallback_reasons": fmt.Sprintf("%v", legacyReasons),
					"sample_nodes":     fmt.Sprintf("%v", sample) + suffix,
					"event_type":       "nodemon_legacy_fallback",
					"zxporter_version": version.Get().String(),
				},
			)
		}
	}

	// Signal when GPU metrics were dropped because nodemon is serving a stale
	// DCGM snapshot (its scrape is failing). Without this the DCGM problem is
	// only visible in nodemon's own logs, never in DAKR telemetry.
	if len(gpuStaleNodes) > 0 {
		c.logger.Info("GPU metrics dropped for nodes with a stale nodemon DCGM snapshot",
			"count", len(gpuStaleNodes))
		if c.telemetryLogger != nil {
			const maxGPUStaleSample = 20
			sample := gpuStaleNodes
			suffix := ""
			if len(sample) > maxGPUStaleSample {
				suffix = fmt.Sprintf(" (+%d more)", len(sample)-maxGPUStaleSample)
				sample = sample[:maxGPUStaleSample]
			}
			c.telemetryLogger.Report(
				gen.LogLevel_LOG_LEVEL_WARN,
				"NodeCollector",
				"GPU metrics dropped: nodemon DCGM snapshot is stale",
				nil,
				map[string]string{
					"gpu_dropped_stale": fmt.Sprintf("%d", len(gpuStaleNodes)),
					"sample_nodes":      fmt.Sprintf("%v", sample) + suffix,
					"event_type":        "gpu_dropped_stale",
					"zxporter_version":  version.Get().String(),
				},
			)
		}
	}

	// Surface coverage gaps: nodes where neither nodemon nor the kubelet fallback
	// produced metrics. This converts previously-silent blind spots into a signal.
	if len(uncoveredNodes) > 0 {
		c.logger.Info("Nodes without any metrics coverage (no nodemon pod and kubelet fallback failed)",
			"count", len(uncoveredNodes), "nodes", uncoveredNodes)
		if c.telemetryLogger != nil {
			c.telemetryLogger.Report(
				gen.LogLevel_LOG_LEVEL_WARN,
				"NodeCollector",
				"Nodes without metrics coverage",
				nil,
				map[string]string{
					"uncovered_count":  fmt.Sprintf("%d", len(uncoveredNodes)),
					"uncovered_nodes":  fmt.Sprintf("%v", uncoveredNodes),
					"event_type":       "node_metrics_coverage_gap",
					"zxporter_version": version.Get().String(),
				},
			)
		}
	}
}

// collectSingleNodeResources does all the work for one node: resolve its
// metrics (nodemon, falling back to the kubelet Summary API), fetch GPU
// metrics, compute utilization, and send the result to the batch channel.
// Safe to call concurrently for different nodes — it neither mutates nor
// re-reads the informer cache (node is passed in, already captured by the
// caller) and every other piece of shared state it touches (NodemonClient's
// discovery cache, KubeletSummaryClient, calculateNodeWorkloadResources'
// nodeToPodsMap, the telemetry logger, batchChan) is already safe for
// concurrent use.
//
// Excluded nodes are still fetched (matching prior behavior, so coverage
// telemetry counts stay comparable) but never enriched or sent.
func (c *NodeCollector) collectSingleNodeResources(ctx context.Context, node *corev1.Node) nodeCollectionOutcome {
	outcome := nodeCollectionOutcome{nodeName: node.Name}

	// Fetch node-level and GPU metrics from nodemon in a single composite
	// request (was two: /node/metrics + /container/metrics). The node and GPU
	// sections are independently usable, and FetchNodeSnapshotByNode falls back
	// to the legacy endpoints only for a nodemon pod that predates the composite
	// contract. A non-nil snapshot can accompany a non-nil error when only one
	// legacy fallback section failed, so we use whatever sections came back.
	var nodeMetric *UnifiedNodeMetric
	var gpuMetrics map[string]interface{}
	if c.nodemonClient != nil {
		snapshot, err := c.nodemonClient.FetchNodeSnapshotByNode(ctx, node.Name)
		if err != nil {
			c.logger.V(1).Info("nodemon node snapshot returned an error", "node", node.Name, "error", err)
		}
		if snapshot != nil {
			nodeMetric = snapshot.NodeMetric
			// The composite always carries a GPU summary; honor the collector's
			// opt-out by only attaching it when GPU metrics are enabled.
			if !c.config.DisableGPUMetrics {
				gpuMetrics = snapshot.GPUMetrics
			}
			outcome.usedLegacy = snapshot.UsedLegacy
			outcome.fallbackReason = snapshot.FallbackReason
			outcome.gpuStale = snapshot.GPUStale
		}
	}

	fromNodemon := nodeMetric != nil
	if !fromNodemon {
		// No usable node section from nodemon — fall back to the kubelet Summary API.
		if km, kerr := c.kubeletClient.FetchNodeMetricsByNode(ctx, node.Name); kerr != nil {
			c.logger.V(1).Info("Kubelet node-metrics fallback failed", "node", node.Name, "error", kerr)
			outcome.coverage = nodeCoverageUncovered
		} else if km != nil {
			nodeMetric = km
			outcome.coverage = nodeCoverageKubeletFallback
		}
	} else {
		outcome.coverage = nodeCoverageNodemon
	}

	if c.isExcluded(node.Name) {
		return outcome
	}

	usage := corev1.ResourceList{}
	if nodeMetric != nil {
		cpuMillis := int64(nodeMetric.CPUUsageNanoCores / 1_000_000)
		usage[corev1.ResourceCPU] = *resource.NewMilliQuantity(cpuMillis, resource.DecimalSI)
		usage[corev1.ResourceMemory] = *resource.NewQuantity(int64(nodeMetric.MemoryWorkingSet), resource.BinarySI)
	}

	// Extract CPU/memory usage in millicores/bytes.
	cpuUsage := usage.Cpu().MilliValue()
	memoryUsage := usage.Memory().Value()

	// Get allocatable and capacity resources from the node.
	cpuAllocatable := node.Status.Allocatable.Cpu().MilliValue()
	memoryAllocatable := node.Status.Allocatable.Memory().Value()
	cpuCapacity := node.Status.Capacity.Cpu().MilliValue()
	memoryCapacity := node.Status.Capacity.Memory().Value()

	// Calculate utilization percentages.
	cpuUtilizationPercent := float64(cpuUsage) / float64(cpuAllocatable) * 100
	memoryUtilizationPercent := float64(memoryUsage) / float64(memoryAllocatable) * 100

	// Network/IO metrics: derived from the nodemon-sourced nodeMetric already
	// fetched above instead of a second, redundant nodemon call (the fix for
	// the other half of #9410 — this used to re-fetch the exact same
	// /node/metrics data a second time). Only populated when nodemon itself
	// was the source (kubelet's Summary API doesn't carry network/disk
	// rates), matching the exact set of fields the old redundant fetch would
	// have produced for a kubelet-fallback or fully uncovered node: none.
	var networkMetrics map[string]float64
	if fromNodemon {
		networkMetrics = networkIOMetricsFromNodeMetric(nodeMetric)
	}

	// GPU metrics (gpuMetrics) were already populated from the composite node
	// snapshot above when GPU collection is enabled and the GPU section was
	// usable; nothing further to fetch here.

	// Create resource data
	resourceData := map[string]interface{}{
		// Node identification
		"nodeName": node.Name,

		// Resource usage
		"cpuUsageMillis":         cpuUsage,
		"memoryUsageBytes":       memoryUsage,
		"cpuAllocatableMillis":   cpuAllocatable,
		"memoryAllocatableBytes": memoryAllocatable,
		"cpuCapacityMillis":      cpuCapacity,
		"memoryCapacityBytes":    memoryCapacity,

		// Utilization percentages
		"cpuUtilizationPercent":    cpuUtilizationPercent,
		"memoryUtilizationPercent": memoryUtilizationPercent,

		// Node properties
		"labels":                  node.Labels,
		"taints":                  node.Spec.Taints,
		"conditions":              node.Status.Conditions,
		"kubeletVersion":          node.Status.NodeInfo.KubeletVersion,
		"osImage":                 node.Status.NodeInfo.OSImage,
		"kernelVersion":           node.Status.NodeInfo.KernelVersion,
		"containerRuntimeVersion": node.Status.NodeInfo.ContainerRuntimeVersion,

		// Include the full node object for any other needed details
		"node": node,
	}

	// Add network metrics if available
	if len(networkMetrics) > 0 {
		resourceData["networkReceiveBytes"] = networkMetrics["NetworkReceiveBytes"]
		resourceData["networkTransmitBytes"] = networkMetrics["NetworkTransmitBytes"]
		resourceData["networkReceivePackets"] = networkMetrics["NetworkReceivePackets"]
		resourceData["networkTransmitPackets"] = networkMetrics["NetworkTransmitPackets"]
		resourceData["networkReceiveErrors"] = networkMetrics["NetworkReceiveErrors"]
		resourceData["networkTransmitErrors"] = networkMetrics["NetworkTransmitErrors"]
		resourceData["networkReceiveDropped"] = networkMetrics["NetworkReceiveDropped"]
		resourceData["networkTransmitDropped"] = networkMetrics["NetworkTransmitDropped"]
		resourceData["fsReadBytes"] = networkMetrics["FSReadBytes"]
		resourceData["fsWriteBytes"] = networkMetrics["FSWriteBytes"]
		resourceData["fsReads"] = networkMetrics["FSReads"]
		resourceData["fsWrites"] = networkMetrics["FSWrites"]
	}

	// Add GPU metrics if available
	if len(gpuMetrics) > 0 {
		// Basic GPU counts and utilization
		resourceData["gpuCount"] = gpuMetrics["GPUCount"]
		resourceData["gpuInstanceCount"] = gpuMetrics["GPUInstanceCount"]
		resourceData["gpuUtilizationAvg"] = gpuMetrics["GPUUtilizationAvg"]
		resourceData["gpuUtilizationMax"] = gpuMetrics["GPUUtilizationMax"]

		// GPU memory
		resourceData["gpuMemoryUsedTotal"] = gpuMetrics["GPUMemoryUsedTotal"]
		resourceData["gpuMemoryFreeTotal"] = gpuMetrics["GPUMemoryFreeTotal"]
		resourceData["gpuMemoryTotalMb"] = gpuMetrics["GPUMemoryTotalMb"]

		// GPU power and temperature
		resourceData["gpuPowerUsageTotal"] = gpuMetrics["GPUPowerUsageTotal"]
		resourceData["gpuTemperatureAvg"] = gpuMetrics["GPUTemperatureAvg"]
		resourceData["gpuTemperatureMax"] = gpuMetrics["GPUTemperatureMax"]
		resourceData["gpuMemoryTemperatureAvg"] = gpuMetrics["GPUMemoryTemperatureAvg"]
		resourceData["gpuMemoryTemperatureMax"] = gpuMetrics["GPUMemoryTemperatureMax"]

		// GPU utilization details
		resourceData["gpuTensorUtilizationAvg"] = gpuMetrics["GPUTensorUtilizationAvg"]
		resourceData["gpuDramUtilizationAvg"] = gpuMetrics["GPUDramUtilizationAvg"]
		resourceData["gpuPCIeTxBytesTotal"] = gpuMetrics["GPUPCIeTxBytesTotal"]
		resourceData["gpuPCIeRxBytesTotal"] = gpuMetrics["GPUPCIeRxBytesTotal"]

		// Graphic utilization
		resourceData["gpuGraphicsUtilizationAvg"] = gpuMetrics["GPUGraphicsUtilizationAvg"]

		// GPU models and identifiers
		resourceData["gpuModels"] = gpuMetrics["GPUModels"]
		resourceData["gpuUUIDs"] = gpuMetrics["GPUUUIDs"]
		resourceData["gpuUsage"] = gpuMetrics["GPUUsage"]
		resourceData["gpuMigInstances"] = gpuMetrics["GPUMigInstances"]
	}

	workloadResources := c.calculateNodeWorkloadResources(node.Name)

	for k, v := range workloadResources {
		resourceData[k] = v
	}

	// Send node resource metrics to the batch channel for batching
	c.batchChan <- CollectedResource{
		ResourceType: NodeResource,
		Object:       resourceData,
		Timestamp:    time.Now(),
		EventType:    EventTypeMetrics,
		Key:          node.Name,
	}

	return outcome
}

// networkIOMetricsFromNodeMetric converts an already-fetched nodemon
// UnifiedNodeMetric into the network/IO metrics map collectSingleNodeResources
// needs. Pure and I/O-free — this replaced a second, redundant HTTP call to
// the exact same nodemon /node/metrics endpoint (see issue #9410).
func networkIOMetricsFromNodeMetric(m *UnifiedNodeMetric) map[string]float64 {
	return map[string]float64{
		"NetworkReceiveBytes":    m.NetworkRxBytesPerSec,
		"NetworkTransmitBytes":   m.NetworkTxBytesPerSec,
		"NetworkReceivePackets":  m.NetworkRxPacketsPerSec,
		"NetworkTransmitPackets": m.NetworkTxPacketsPerSec,
		"NetworkReceiveErrors":   m.NetworkRxErrorsPerSec,
		"NetworkTransmitErrors":  m.NetworkTxErrorsPerSec,
		"NetworkReceiveDropped":  m.NetworkRxDropsPerSec,
		"NetworkTransmitDropped": m.NetworkTxDropsPerSec,
		"FSReadBytes":            m.DiskReadBytesPerSec,
		"FSWriteBytes":           m.DiskWriteBytesPerSec,
		"FSReads":                m.DiskReadOpsPerSec,
		"FSWrites":               m.DiskWriteOpsPerSec,
	}
}

// isExcluded checks if a node should be excluded from collection
func (c *NodeCollector) isExcluded(nodeName string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.excludedNodes[nodeName]
}

// Stop gracefully shuts down the node collector
func (c *NodeCollector) Stop() error {
	c.logger.Info("Stopping node collector")

	// 1. Stop the ticker
	if c.ticker != nil {
		c.ticker.Stop()
		c.logger.Info("Stopped node collector ticker")
	}

	// 2. Signal the informer factory and collection loop to stop by closing stopCh.
	select {
	case <-c.stopCh:
		c.logger.Info("Node collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed node collector stop channel")
	}

	// 2b. Wait for collectNodeResourcesLoop's goroutine to actually return.
	// Closing stopCh above only stops it from starting another sweep — a
	// sweep already in flight (with concurrent workers still possibly
	// blocked on batchChan sends) has to finish and observe stopCh on its
	// next loop iteration first. Waiting here before closing batchChan below
	// is what makes that safe.
	c.loopWG.Wait()

	// 2c. Flip stopped before closing batchChan below — see chanMu's doc comment. This
	// MUST happen before batchChan closes, not merely before the explicit batcher.stop()
	// call in step 4: closing batchChan causes the batcher's own goroutine to notice its
	// input channel closed and close resourceChan from its defer right then, independent
	// of when batcher.stop() (which signals shutdown a second way, via the batcher's own
	// stopCh) is called — so gating only around step 4 leaves the batcher's own
	// close-on-input-closed path to race a direct sender that hasn't seen stopped yet.
	// Taking the write lock here blocks until any handleNodeEvent /
	// sendNodeLifecycleTransition call already past its stopped-check has finished its
	// send, so no direct sender can still be in flight once resourceChan closes, no
	// matter which of the batcher's two shutdown paths gets there first.
	c.chanMu.Lock()
	c.stopped = true
	c.chanMu.Unlock()

	// 3. Close the batchChan (input to the batcher for metrics).
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed node collector batch input channel")
	}

	// 4. Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop() // This will close resourceChan when done
		c.logger.Info("Node collector batcher stopped")
	}

	// 5. Clear nodeToPodsMap
	c.podMapMutex.Lock()
	c.nodeToPodsMap = make(map[string]map[string]*corev1.Pod)
	c.podMapMutex.Unlock()

	// 5b. Clear the lifecycle-fallback state for the same reason.
	c.lifecycleMu.Lock()
	c.nodeLifecycle = make(map[string]*nodeLifecycleState)
	c.lifecycleMu.Unlock()

	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *NodeCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *NodeCollector) GetType() string {
	return "node"
}

// IsAvailable checks if Node resources can be accessed in the cluster.
// Always returns true — nodemon pods are discovered dynamically.
func (c *NodeCollector) IsAvailable(ctx context.Context) bool {
	return c.nodemonClient != nil
}

// AddResource manually adds a node resource to be processed by the collector
func (c *NodeCollector) AddResource(resource interface{}) error {
	node, ok := resource.(*corev1.Node)
	if !ok {
		return fmt.Errorf("expected *corev1.Node, got %T", resource)
	}

	c.handleNodeEvent(node, EventTypeAdd)
	return nil
}
