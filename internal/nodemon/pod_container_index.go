package nodemon

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

type containerInfo struct {
	Pod       string
	Namespace string
	Container string
}

// PodContainerIndex maintains a node-scoped containerID -> pod/namespace/container
// mapping via a Pod informer. It requires hostPID: true to be useful, since the
// collectors that consult it (JVMCollector, RuntimeCollector) resolve process
// container IDs from /proc, which is only populated with cross-namespace PIDs when
// hostPID is enabled.
//
// A single PodContainerIndex is shared across all process-introspection collectors
// on a node rather than each owning its own Pod watch, since they all need the exact
// same mapping.
type PodContainerIndex struct {
	nodeName        string
	k8sClient       kubernetes.Interface
	informerFactory informers.SharedInformerFactory
	procRoot        string
	log             logr.Logger

	mu           sync.RWMutex
	containerMap map[string]containerInfo // containerID (hex) -> pod metadata
	stopCh       chan struct{}
}

// NewPodContainerIndex creates a PodContainerIndex. procRoot defaults to "/proc".
func NewPodContainerIndex(nodeName string, k8sClient kubernetes.Interface, log logr.Logger) *PodContainerIndex {
	return &PodContainerIndex{
		nodeName:     nodeName,
		k8sClient:    k8sClient,
		procRoot:     "/proc",
		log:          log.WithName("pod-container-index"),
		containerMap: make(map[string]containerInfo),
		stopCh:       make(chan struct{}),
	}
}

// Lookup returns the pod metadata for a given containerID (hex, no scheme prefix).
func (idx *PodContainerIndex) Lookup(containerID string) (containerInfo, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	info, ok := idx.containerMap[containerID]
	return info, ok
}

// checkProcRootVisibility logs a warning if the index cannot see PIDs outside its
// own PID namespace (i.e. hostPID is not enabled or procRoot is wrong). This turns
// the silent count:0 failure into a clear diagnostic signal.
func (idx *PodContainerIndex) checkProcRootVisibility() {
	entries, err := os.ReadDir(idx.procRoot)
	if err != nil {
		idx.log.Error(err, "Cannot read procRoot — process discovery will not work", "procRoot", idx.procRoot)
		return
	}

	// With hostPID, we see hundreds/thousands of PIDs from all namespaces.
	// Without it, we only see our own PID namespace (typically < 10 entries).
	// A threshold of 20 dir entries is a conservative heuristic.
	pidCount := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err == nil {
			pidCount++
		}
	}

	if pidCount < 20 {
		idx.log.Info("WARNING: process index can only see a small number of PIDs — hostPID may not be enabled. "+
			"Process discovery will likely find 0 processes. "+
			"Ensure runtimeMetrics.enabled=true in Helm values so the pod runs with hostPID: true.",
			"procRoot", idx.procRoot, "visiblePIDs", pidCount)
	} else {
		idx.log.Info("Host PID namespace visibility confirmed", "procRoot", idx.procRoot, "visiblePIDs", pidCount)
	}
}

// startInformer creates the informer, starts it, and waits for cache sync
// with a 30-second timeout. Returns an error on failure and cleans up
// informer goroutines so the caller can retry.
func (idx *PodContainerIndex) startInformer() error {
	idx.informerFactory = informers.NewSharedInformerFactoryWithOptions(
		idx.k8sClient,
		0,
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			if idx.nodeName != "" {
				opts.FieldSelector = "spec.nodeName=" + idx.nodeName
			}
		}),
	)

	podInformer := idx.informerFactory.Core().V1().Pods().Informer()

	_, err := podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				return
			}
			idx.updateContainerMap(pod)
		},
		UpdateFunc: func(_, newObj interface{}) {
			pod, ok := newObj.(*corev1.Pod)
			if !ok {
				return
			}
			idx.updateContainerMap(pod)
		},
		DeleteFunc: func(obj interface{}) {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					return
				}
				pod, ok = tombstone.Obj.(*corev1.Pod)
				if !ok {
					return
				}
			}
			idx.removeFromContainerMap(pod)
		},
	})
	if err != nil {
		idx.informerFactory = nil
		return fmt.Errorf("adding pod event handler: %w", err)
	}

	idx.informerFactory.Start(idx.stopCh)

	syncCtx, syncCancel := context.WithTimeout(
		context.Background(), 30*time.Second,
	)
	defer syncCancel()

	syncDone := make(chan bool, 1)
	go func() {
		syncDone <- cache.WaitForCacheSync(
			idx.stopCh, podInformer.HasSynced,
		)
	}()

	select {
	case synced := <-syncDone:
		if !synced {
			idx.cleanupInformer()
			return fmt.Errorf("pod informer cache failed to sync")
		}
	case <-syncCtx.Done():
		idx.cleanupInformer()
		return fmt.Errorf(
			"pod informer cache sync timed out after 30s",
		)
	}

	return nil
}

// cleanupInformer stops the informer factory and resets state so
// startInformer can be called again on retry.
func (idx *PodContainerIndex) cleanupInformer() {
	idx.Stop()
	// Reset so we can retry — Stop() closes stopCh, so make a new one.
	idx.stopCh = make(chan struct{})
	idx.informerFactory = nil
}

// Start creates a node-scoped pod informer and waits for the cache to
// sync. Retries up to 3 times with exponential backoff (5s, 10s, 20s)
// on transient failures.
func (idx *PodContainerIndex) Start() error {
	if idx.informerFactory != nil {
		return fmt.Errorf("PodContainerIndex already started")
	}

	const maxRetries = 3
	backoff := 5 * time.Second

	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		idx.log.Info("Starting pod informer",
			"attempt", attempt, "maxRetries", maxRetries)

		if err := idx.startInformer(); err != nil {
			lastErr = err
			idx.log.Error(err, "Pod informer failed to start",
				"attempt", attempt, "retryIn", backoff.String())
			if attempt < maxRetries {
				time.Sleep(backoff)
				backoff *= 2
			}
			continue
		}

		idx.log.Info("Pod informer cache synced")
		idx.checkProcRootVisibility()
		return nil
	}

	return fmt.Errorf(
		"pod container index failed after %d attempts: %w",
		maxRetries, lastErr,
	)
}

// Stop shuts down the informer factory. Safe to call multiple times.
func (idx *PodContainerIndex) Stop() {
	select {
	case <-idx.stopCh:
		// already closed
	default:
		close(idx.stopCh)
	}
}

func (idx *PodContainerIndex) updateContainerMap(pod *corev1.Pod) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	for _, cs := range pod.Status.ContainerStatuses {
		id := stripContainerIDScheme(cs.ContainerID)
		if id == "" {
			continue
		}
		idx.containerMap[id] = containerInfo{
			Pod:       pod.Name,
			Namespace: pod.Namespace,
			Container: cs.Name,
		}
	}
}

func (idx *PodContainerIndex) removeFromContainerMap(pod *corev1.Pod) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	for _, cs := range pod.Status.ContainerStatuses {
		id := stripContainerIDScheme(cs.ContainerID)
		if id == "" {
			continue
		}
		delete(idx.containerMap, id)
	}
}
