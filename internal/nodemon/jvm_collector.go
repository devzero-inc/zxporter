package nodemon

import (
	"context"
	"fmt"
	"os"
	"strings"
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

// JVMCollector collects JVM metrics from hsperfdata files via /proc.
// Requires the pod to run with hostPID: true and as UID 0 to read
// /proc/<pid>/root/tmp/hsperfdata_*/<nsPid> for other containers.
type JVMCollector struct {
	nodeName        string
	k8sClient       kubernetes.Interface
	informerFactory informers.SharedInformerFactory
	procRoot        string
	log             logr.Logger

	mu           sync.RWMutex
	containerMap map[string]containerInfo // containerID (hex) -> pod metadata
	stopCh       chan struct{}
}

// NewJVMCollector creates a JVMCollector. procRoot defaults to "/proc".
func NewJVMCollector(nodeName string, k8sClient kubernetes.Interface, log logr.Logger) *JVMCollector {
	return &JVMCollector{
		nodeName:     nodeName,
		k8sClient:    k8sClient,
		procRoot:     "/proc",
		log:          log.WithName("jvm-collector"),
		containerMap: make(map[string]containerInfo),
		stopCh:       make(chan struct{}),
	}
}

// checkHostPIDVisibility logs a warning if the collector cannot see PIDs outside
// its own PID namespace (i.e. hostPID is not enabled or procRoot is wrong).
// This turns the silent count:0 failure into a clear diagnostic signal.
func (c *JVMCollector) checkHostPIDVisibility() {
	entries, err := os.ReadDir(c.procRoot)
	if err != nil {
		c.log.Error(err, "Cannot read procRoot — JVM discovery will not work", "procRoot", c.procRoot)
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
		if _, err := fmt.Sscanf(e.Name(), "%d", new(int)); err == nil {
			pidCount++
		}
	}

	if pidCount < 20 {
		c.log.Info("WARNING: JVM collector can only see a small number of PIDs — hostPID may not be enabled. "+
			"JVM discovery will likely find 0 Java processes. "+
			"Ensure jvmMetrics.enabled=true in Helm values so the pod runs with hostPID: true.",
			"procRoot", c.procRoot, "visiblePIDs", pidCount)
	} else {
		c.log.Info("Host PID namespace visibility confirmed", "procRoot", c.procRoot, "visiblePIDs", pidCount)
	}
}

// startInformer creates the informer, starts it, and waits for cache sync
// with a 30-second timeout. Returns an error on failure and cleans up
// informer goroutines so the caller can retry.
func (c *JVMCollector) startInformer() error {
	c.informerFactory = informers.NewSharedInformerFactoryWithOptions(
		c.k8sClient,
		0,
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			if c.nodeName != "" {
				opts.FieldSelector = "spec.nodeName=" + c.nodeName
			}
		}),
	)

	podInformer := c.informerFactory.Core().V1().Pods().Informer()

	_, err := podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				return
			}
			c.updateContainerMap(pod)
		},
		UpdateFunc: func(_, newObj interface{}) {
			pod, ok := newObj.(*corev1.Pod)
			if !ok {
				return
			}
			c.updateContainerMap(pod)
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
			c.removeFromContainerMap(pod)
		},
	})
	if err != nil {
		c.informerFactory = nil
		return fmt.Errorf("adding pod event handler: %w", err)
	}

	c.informerFactory.Start(c.stopCh)

	syncCtx, syncCancel := context.WithTimeout(
		context.Background(), 30*time.Second,
	)
	defer syncCancel()

	syncDone := make(chan bool, 1)
	go func() {
		syncDone <- cache.WaitForCacheSync(
			c.stopCh, podInformer.HasSynced,
		)
	}()

	select {
	case synced := <-syncDone:
		if !synced {
			c.cleanupInformer()
			return fmt.Errorf("pod informer cache failed to sync")
		}
	case <-syncCtx.Done():
		c.cleanupInformer()
		return fmt.Errorf(
			"pod informer cache sync timed out after 30s",
		)
	}

	return nil
}

// cleanupInformer stops the informer factory and resets state so
// startInformer can be called again on retry.
func (c *JVMCollector) cleanupInformer() {
	c.Stop()
	// Reset so we can retry — Stop() closes stopCh, so make a new one.
	c.stopCh = make(chan struct{})
	c.informerFactory = nil
}

// Start creates a node-scoped pod informer and waits for the cache to
// sync. Retries up to 3 times with exponential backoff (5s, 10s, 20s)
// on transient failures.
func (c *JVMCollector) Start() error {
	if c.informerFactory != nil {
		return fmt.Errorf("JVMCollector already started")
	}

	const maxRetries = 3
	backoff := 5 * time.Second

	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		c.log.Info("Starting pod informer",
			"attempt", attempt, "maxRetries", maxRetries)

		if err := c.startInformer(); err != nil {
			lastErr = err
			c.log.Error(err, "Pod informer failed to start",
				"attempt", attempt, "retryIn", backoff.String())
			if attempt < maxRetries {
				time.Sleep(backoff)
				backoff *= 2
			}
			continue
		}

		c.log.Info("Pod informer cache synced")
		c.checkHostPIDVisibility()
		return nil
	}

	return fmt.Errorf(
		"JVM collector failed after %d attempts: %w",
		maxRetries, lastErr,
	)
}

// Stop shuts down the informer factory. Safe to call multiple times.
func (c *JVMCollector) Stop() {
	select {
	case <-c.stopCh:
		// already closed
	default:
		close(c.stopCh)
	}
}

func (c *JVMCollector) updateContainerMap(pod *corev1.Pod) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, cs := range pod.Status.ContainerStatuses {
		id := stripContainerIDScheme(cs.ContainerID)
		if id == "" {
			continue
		}
		c.containerMap[id] = containerInfo{
			Pod:       pod.Name,
			Namespace: pod.Namespace,
			Container: cs.Name,
		}
	}
}

func (c *JVMCollector) removeFromContainerMap(pod *corev1.Pod) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, cs := range pod.Status.ContainerStatuses {
		id := stripContainerIDScheme(cs.ContainerID)
		if id == "" {
			continue
		}
		delete(c.containerMap, id)
	}
}

// QueryJVMMetrics returns JVM metrics for all discovered Java containers on this node.
func (c *JVMCollector) QueryJVMMetrics(ctx context.Context) ([]JVMMetric, error) {
	c.mu.RLock()
	containerMap := make(map[string]containerInfo, len(c.containerMap))
	for k, v := range c.containerMap {
		containerMap[k] = v
	}
	c.mu.RUnlock()
	c.log.Info("Container map snapshot", "count", len(containerMap))

	start := time.Now()
	procs, err := discoverJavaProcesses(c.procRoot)
	if err != nil {
		return nil, fmt.Errorf("discovering java processes: %w", err)
	}
	c.log.Info("Discovered java processes", "count", len(procs), "took", time.Since(start).String())

	start = time.Now()
	metrics := make([]JVMMetric, 0, len(procs))
	for _, proc := range procs {
		select {
		case <-ctx.Done():
			c.log.Info("JVM metrics query cancelled", "collected", len(metrics), "remaining", len(procs)-len(metrics))
			return metrics, ctx.Err()
		default:
		}

		c.log.Info("Reading hsperfdata", "pid", proc.PidHost, "path", proc.HsperfDataPath)
		if st, err := os.Stat(proc.HsperfDataPath); err == nil {
			c.log.Info("hsperfdata stat", "pid", proc.PidHost, "sizeBytes", st.Size())
		} else {
			c.log.Error(err, "hsperfdata stat failed", "pid", proc.PidHost, "path", proc.HsperfDataPath)
		}
		readStart := time.Now()
		counters, err := readHsperfdata(proc.HsperfDataPath)
		if err != nil {
			c.log.Error(err, "Failed to read hsperfdata", "pid", proc.PidHost, "path", proc.HsperfDataPath)
			continue
		}
		c.log.Info("Read hsperfdata", "pid", proc.PidHost, "counters", len(counters), "took", time.Since(readStart).String())

		info := containerMap[proc.ContainerID]
		metrics = append(metrics, buildJVMMetric(counters, proc, info, c.nodeName))
	}
	c.log.Info("Built JVM metrics", "count", len(metrics), "took", time.Since(start).String())

	return metrics, nil
}

// sumSpaceCounters sums sun.gc.generation.*.space.*.{metric} across all generations
// and spaces. This handles JVMs where the aggregate sun.gc.heap.{metric} counter
// doesn't exist (Serial GC, some G1 configurations).
func sumSpaceCounters(counters map[string]any, metric string) int64 {
	var total int64
	for gen := 0; gen < 4; gen++ {
		for space := 0; space < 4; space++ {
			key := fmt.Sprintf("sun.gc.generation.%d.space.%d.%s", gen, space, metric)
			if v, ok := hsInt(counters, key); ok {
				total += v
			}
		}
	}
	return total
}

// heapMaxBytes derives the JVM's maximum heap size from hsperfdata counters.
//
// Modern JVMs expose the aggregate sun.gc.heap.maxCapacity counter, which is
// authoritative. When it is absent we fall back to the per-generation
// maxCapacity counters, but how they combine depends on the collector:
//
//   - Generational collectors (Serial, Parallel) physically partition the heap
//     into a young and an old generation, so the heap max is gen0 + gen1.
//   - Region-based collectors (G1, ZGC, Shenandoah) manage one shared region
//     pool; each generation's maxCapacity reports ~the whole heap (any region
//     can belong to either generation), so summing double-counts. The heap max
//     is then a single generation's maxCapacity, i.e. the larger of the two.
//
// Summing under G1 was the original bug: it reported ~2x the real max heap,
// exceeding the container memory limit.
func heapMaxBytes(counters map[string]any) int64 {
	if maxCap, ok := hsInt(counters, "sun.gc.heap.maxCapacity"); ok {
		return maxCap
	}
	gen0, _ := hsInt(counters, "sun.gc.generation.0.maxCapacity")
	gen1, _ := hsInt(counters, "sun.gc.generation.1.maxCapacity")
	if isRegionBasedCollector(counters) {
		if gen0 > gen1 {
			return gen0
		}
		return gen1
	}
	return gen0 + gen1
}

// isRegionBasedCollector reports whether the JVM uses a collector that manages
// the heap as a single shared region pool (G1, ZGC, Shenandoah) rather than as
// physically separate young/old generations (Serial, Parallel). It inspects the
// sun.gc.collector.<n>.name counters published by the JVM.
func isRegionBasedCollector(counters map[string]any) bool {
	for i := 0; i < 8; i++ {
		name, ok := hsStr(counters, fmt.Sprintf("sun.gc.collector.%d.name", i))
		if !ok || name == "" {
			break
		}
		if strings.Contains(name, "G1") ||
			strings.Contains(name, "ZGC") ||
			strings.Contains(name, "Shenandoah") {
			return true
		}
	}
	return false
}

// stripContainerIDScheme strips the URL scheme (e.g., "containerd://") from a container ID.
func stripContainerIDScheme(raw string) string {
	if i := strings.LastIndex(raw, "://"); i >= 0 {
		return raw[i+3:]
	}
	return raw
}

// buildJVMMetric assembles a JVMMetric from parsed hsperfdata counters and process metadata.
func buildJVMMetric(counters map[string]any, proc JavaProcess, info containerInfo, nodeName string) JVMMetric {
	m := JVMMetric{
		NodeName:    nodeName,
		Pod:         info.Pod,
		Namespace:   info.Namespace,
		Container:   info.Container,
		ContainerID: proc.ContainerID,
		PidHost:     proc.PidHost,
		PidNS:       proc.PidNS,
		RawCmdline:  proc.CmdLine,
		// FlagsExtracted / FlagSources are filled after hsperf parsing.
		Timestamp: time.Now().UTC(),
	}

	m.JavaCommand, _ = hsStr(counters, "sun.rt.javaCommand")
	m.JavaVersion, _ = hsStr(counters, "java.property.java.version")

	// Heap capacity: try aggregate heap counter first, then sum per-space counters.
	if cap, ok := hsInt(counters, "sun.gc.heap.capacity"); ok {
		m.HeapSizeBytes = cap
	} else {
		m.HeapSizeBytes = sumSpaceCounters(counters, "capacity")
	}

	if used, ok := hsInt(counters, "sun.gc.heap.used"); ok {
		m.HeapUsedBytes = used
	} else {
		// Aggregate heap counter missing (common with Serial/G1 GC).
		// Sum all generation.*.space.*.used counters instead.
		m.HeapUsedBytes = sumSpaceCounters(counters, "used")
	}

	m.HeapMaxSizeBytes = heapMaxBytes(counters)

	// Convert GC time ticks to seconds using the JVM's high-resolution timer frequency.
	freq, _ := hsInt(counters, "sun.os.hrt.frequency")
	if freq <= 0 {
		freq = 1_000_000_000 // nanosecond fallback
	}

	m.GCTimeSecondsTotal = make(map[string]float64)
	for i := 0; i < 8; i++ {
		name, ok := hsStr(counters, fmt.Sprintf("sun.gc.collector.%d.name", i))
		if !ok || name == "" {
			break
		}
		ticks, _ := hsInt(counters, fmt.Sprintf("sun.gc.collector.%d.time", i))
		m.GCTimeSecondsTotal[name] = float64(ticks) / float64(freq)
	}

	safeTicks, _ := hsInt(counters, "sun.rt.safepointTime")
	syncTicks, _ := hsInt(counters, "sun.rt.safepointSyncTime")
	// HotSpot stores safepoint counters in milliseconds (not HRT ticks like GC collector time).
	// See: sun.rt.safepointTime, sun.rt.safepointSyncTime
	m.SafepointTimeSecondsTotal = float64(safeTicks) / 1000.0
	m.SafepointSyncTimeSecondsTotal = float64(syncTicks) / 1000.0

	flags, sources, effectiveCmd := ParseJVMFlagsWithSources(proc.CmdLine, proc.EnvJavaOpts)
	m.FlagsExtracted = flags
	m.FlagSources = sources
	m.RawCmdline = effectiveCmd

	return m
}
