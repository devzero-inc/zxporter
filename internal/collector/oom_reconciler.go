package collector

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// oomReconcileInterval is how often the reconciler sweeps pods for missed OOM events.
	// 30s balances latency (operator gets missed OOMs within half a minute) against
	// K8s API load (one List call per namespace per sweep).
	oomReconcileInterval = 30 * time.Second

	// oomDeduplicationTTL is how long a seen OOM entry is kept in the dedup map.
	// After this, the entry is evicted so a new OOM on the same container (after pod
	// recycling) can be detected. 10 minutes covers several reconciliation cycles
	// and the operator's emergency response cooldown (default 10s).
	oomDeduplicationTTL = 10 * time.Minute
)

// OOMReconcilerMarker is the interface used by PodCollector to mark OOMs as seen,
// preventing the periodic sweep from re-publishing events already sent via the
// real-time informer path.
type OOMReconcilerMarker interface {
	MarkSeen(namespace, podName, containerName string, restartCount int32)
}

// oomSeenKey uniquely identifies an OOM event for deduplication.
type oomSeenKey struct {
	namespace     string
	podName       string
	containerName string
}

type oomSeenEntry struct {
	restartCount int32
	seenAt       time.Time
}

// OOMReconciler periodically sweeps pods for OOM termination states that the
// informer-based PodCollector may have missed (informer coalescing, rapid
// restart-then-recovery, zxporter restart). Detected OOMs are published directly
// to the MPA stream, bypassing the lossy combinedChannel pipeline.
type OOMReconciler struct {
	client       kubernetes.Interface
	namespaces   []string
	mpaPublisher MpaMetricsPublisher
	logger       logr.Logger

	mu   sync.Mutex
	seen map[oomSeenKey]oomSeenEntry
}

// NewOOMReconciler creates a new OOM reconciler.
func NewOOMReconciler(
	client kubernetes.Interface,
	namespaces []string,
	mpaPublisher MpaMetricsPublisher,
	logger logr.Logger,
) *OOMReconciler {
	return &OOMReconciler{
		client:       client,
		namespaces:   namespaces,
		mpaPublisher: mpaPublisher,
		logger:       logger.WithName("oom-reconciler"),
		seen:         make(map[oomSeenKey]oomSeenEntry),
	}
}

// MarkSeen records that an OOM event has already been published (by the PodCollector's
// real-time path), so the periodic sweep will skip it.
func (r *OOMReconciler) MarkSeen(namespace, podName, containerName string, restartCount int32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := oomSeenKey{namespace: namespace, podName: podName, containerName: containerName}
	r.seen[key] = oomSeenEntry{restartCount: restartCount, seenAt: time.Now()}
}

// Start runs the periodic OOM reconciliation loop. Blocks until ctx is cancelled.
func (r *OOMReconciler) Start(ctx context.Context) {
	r.logger.Info("Starting OOM reconciler", "interval", oomReconcileInterval, "namespaces", r.namespaces)
	ticker := time.NewTicker(oomReconcileInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			r.logger.Info("OOM reconciler stopped")
			return
		case <-ticker.C:
			r.sweep(ctx)
			r.evictStaleEntries()
		}
	}
}

// sweep lists pods in all watched namespaces and checks for OOM termination states.
func (r *OOMReconciler) sweep(ctx context.Context) {
	namespaces := r.namespaces
	// Empty or single empty string means all namespaces
	if len(namespaces) == 0 || (len(namespaces) == 1 && namespaces[0] == "") {
		namespaces = []string{""}
	}

	for _, ns := range namespaces {
		if err := r.sweepNamespace(ctx, ns); err != nil {
			r.logger.Error(err, "Failed to sweep namespace for OOM events", "namespace", ns)
		}
	}
}

// sweepNamespace checks all pods in a single namespace for OOM termination.
func (r *OOMReconciler) sweepNamespace(ctx context.Context, namespace string) error {
	listOpts := metav1.ListOptions{}
	var podList *corev1.PodList
	var err error

	if namespace == "" {
		podList, err = r.client.CoreV1().Pods("").List(ctx, listOpts)
	} else {
		podList, err = r.client.CoreV1().Pods(namespace).List(ctx, listOpts)
	}
	if err != nil {
		return err
	}

	for i := range podList.Items {
		pod := &podList.Items[i]
		r.checkPodForOOM(pod)
	}
	return nil
}

// checkPodForOOM inspects container statuses for OOM termination and publishes
// to the MPA stream if the OOM hasn't been seen before.
func (r *OOMReconciler) checkPodForOOM(pod *corev1.Pod) {
	for _, cs := range pod.Status.ContainerStatuses {
		if !isOOMTermination(cs) {
			continue
		}

		key := oomSeenKey{
			namespace:     pod.Namespace,
			podName:       pod.Name,
			containerName: cs.Name,
		}

		r.mu.Lock()
		entry, exists := r.seen[key]
		alreadySeen := exists && entry.restartCount >= cs.RestartCount
		if !alreadySeen {
			r.seen[key] = oomSeenEntry{restartCount: cs.RestartCount, seenAt: time.Now()}
		}
		r.mu.Unlock()

		if alreadySeen {
			continue
		}

		r.publishOOM(pod, cs)
	}
}

// isOOMTermination returns true if the container's last termination was OOM-related.
func isOOMTermination(cs corev1.ContainerStatus) bool {
	terminated := cs.LastTerminationState.Terminated
	if terminated == nil {
		return false
	}
	if terminated.Reason == ReasonOOMKilled {
		return true
	}
	// Kubernetes reports init-container OOM as StartError with "oom" in message
	if terminated.Reason == ReasonStartError && strings.Contains(strings.ToLower(terminated.Message), "oom") {
		return true
	}
	return false
}

// publishOOM sends a synthetic OOM metric snapshot directly to the MPA stream.
func (r *OOMReconciler) publishOOM(pod *corev1.Pod, cs corev1.ContainerStatus) {
	if r.mpaPublisher == nil {
		return
	}

	workloadName, workloadKind := getWorkloadInfo(pod)
	requestBytes, limitBytes := getContainerResources(pod, cs.Name)

	var cpuRequestMillis, cpuLimitMillis int64
	for _, c := range pod.Spec.Containers {
		if c.Name == cs.Name {
			if req := c.Resources.Requests.Cpu(); req != nil {
				cpuRequestMillis = req.MilliValue()
			}
			if lim := c.Resources.Limits.Cpu(); lim != nil {
				cpuLimitMillis = lim.MilliValue()
			}
			break
		}
	}

	snapshot := &ContainerMetricsSnapshot{
		ContainerName:         cs.Name,
		PodName:               pod.Name,
		Namespace:             pod.Namespace,
		NodeName:              pod.Spec.NodeName,
		WorkloadName:          workloadName,
		WorkloadKind:          workloadKind,
		CpuRequestMillis:      cpuRequestMillis,
		CpuLimitMillis:        cpuLimitMillis,
		MemoryUsageBytes:      limitBytes,
		MemoryRequestBytes:    requestBytes,
		MemoryLimitBytes:      limitBytes,
		PodLabels:             pod.Labels,
		ContainerRunning:      cs.State.Running != nil,
		ContainerRestarts:     cs.RestartCount > 0,
		RestartCount:          int64(cs.RestartCount),
		LastTerminationReason: ReasonOOMKilled,
	}

	r.mpaPublisher.PublishMetrics(snapshot, time.Now())

	r.logger.Info("OOM reconciler: published missed OOM event to MPA stream",
		"namespace", pod.Namespace,
		"pod", pod.Name,
		"container", cs.Name,
		"restartCount", cs.RestartCount)
}

// evictStaleEntries removes dedup entries older than oomDeduplicationTTL.
func (r *OOMReconciler) evictStaleEntries() {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := time.Now().Add(-oomDeduplicationTTL)
	for key, entry := range r.seen {
		if entry.seenAt.Before(cutoff) {
			delete(r.seen, key)
		}
	}
}
