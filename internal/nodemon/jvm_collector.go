package nodemon

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
)

// JVMCollector collects JVM metrics from hsperfdata files via /proc.
// Requires the pod to run with hostPID: true and as UID 0 to read
// /proc/<pid>/root/tmp/hsperfdata_*/<nsPid> for other containers.
type JVMCollector struct {
	nodeName string
	index    *PodContainerIndex
	procRoot string
	log      logr.Logger

	mu sync.RWMutex
	// cachedMetrics/cachedErr are the result of the last completed Collect —
	// what QueryJVMMetrics serves. Collection runs on a ticker
	// (StartCollectionLoop), off the HTTP request path, so a request never
	// pays for a live /proc walk.
	cachedMetrics []JVMMetric
	cachedErr     error

	// discover and build are seams over the package-level functions,
	// overridable in tests. Production code always uses the real functions.
	discover func(procRoot string) ([]JavaProcess, error)
	build    func(ctx context.Context, procs []JavaProcess, index *PodContainerIndex, nodeName string, log logr.Logger) ([]JVMMetric, error)
}

// NewJVMCollector creates a JVMCollector. index must already be started (or be
// started concurrently) — JVMCollector only reads from it. procRoot defaults to
// "/proc".
func NewJVMCollector(nodeName string, index *PodContainerIndex, log logr.Logger) *JVMCollector {
	return &JVMCollector{
		nodeName:  nodeName,
		index:     index,
		procRoot:  "/proc",
		log:       log.WithName("jvm-collector"),
		cachedErr: errMetricsNotYetCollected,
		discover:  discoverJavaProcesses,
		build:     buildJVMMetrics,
	}
}

// QueryJVMMetrics returns the JVM metrics from the last completed background
// Collect — see StartCollectionLoop. It never does its own /proc walk, so
// it's safe to call on every HTTP request.
func (c *JVMCollector) QueryJVMMetrics(_ context.Context) ([]JVMMetric, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cachedMetrics, c.cachedErr
}

// StartCollectionLoop runs Collect immediately, then on every tick. Call in a
// goroutine. ctx is expected to be long-lived (cancelled only at shutdown),
// so each individual cycle gets its own bounded sub-context — otherwise a
// slow or stuck cycle (e.g. a wedged hsperfdata read) would run forever,
// permanently freezing the cache and preventing any future tick from ever
// running, since the loop is single-threaded.
func (c *JVMCollector) StartCollectionLoop(ctx context.Context, interval time.Duration) {
	c.collectOneCycle(ctx, interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.collectOneCycle(ctx, interval)
		}
	}
}

// collectOneCycle runs Collect under a deadline derived from parentCtx,
// bounded by interval, so one cycle can never run longer than the loop's own
// tick period.
func (c *JVMCollector) collectOneCycle(parentCtx context.Context, interval time.Duration) {
	cycleCtx, cancel := context.WithTimeout(parentCtx, interval)
	defer cancel()
	c.Collect(cycleCtx)
}

// Collect performs a single /proc walk, builds JVM metrics, and publishes the
// result for QueryJVMMetrics to serve. A cycle that produces nothing usable
// (an error and no metrics) keeps the last good snapshot instead of blanking
// it — a transient failure would otherwise erase good data for a full
// refresh interval. The error itself is still published either way, so
// QueryJVMMetrics reflects that the most recent cycle had a problem.
func (c *JVMCollector) Collect(ctx context.Context) {
	metrics, err := c.collect(ctx)
	if err != nil {
		c.log.Error(err, "JVM metrics collection cycle failed")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.cachedErr = err
	if err != nil && len(metrics) == 0 {
		return
	}
	c.cachedMetrics = metrics
}

// collect performs the live /proc walk and build. Only Collect should call
// this — QueryJVMMetrics reads the cache Collect publishes.
func (c *JVMCollector) collect(ctx context.Context) ([]JVMMetric, error) {
	start := time.Now()
	procs, err := c.discover(c.procRoot)
	if err != nil {
		return nil, fmt.Errorf("discovering java processes: %w", err)
	}
	c.log.Info("Discovered java processes", "count", len(procs), "took", time.Since(start).String())

	return c.build(ctx, procs, c.index, c.nodeName, c.log)
}

// buildJVMMetrics reads hsperfdata for each discovered Java process and builds
// the corresponding JVMMetric. Shared by JVMCollector (the legacy
// /container/jvm-metrics path) and RuntimeCollector (the combined
// /container/runtime-metrics path), so there is one implementation of the
// per-process build step regardless of which /proc walk discovered procs.
func buildJVMMetrics(ctx context.Context, procs []JavaProcess, index *PodContainerIndex, nodeName string, log logr.Logger) ([]JVMMetric, error) {
	start := time.Now()
	metrics := make([]JVMMetric, 0, len(procs))
	for _, proc := range procs {
		select {
		case <-ctx.Done():
			log.Info("JVM metrics query cancelled", "collected", len(metrics), "remaining", len(procs)-len(metrics))
			return metrics, ctx.Err()
		default:
		}

		log.Info("Reading hsperfdata", "pid", proc.PidHost, "path", proc.HsperfDataPath)
		if st, err := os.Stat(proc.HsperfDataPath); err == nil {
			log.Info("hsperfdata stat", "pid", proc.PidHost, "sizeBytes", st.Size())
		} else {
			log.Error(err, "hsperfdata stat failed", "pid", proc.PidHost, "path", proc.HsperfDataPath)
		}
		readStart := time.Now()
		counters, err := readHsperfdata(proc.HsperfDataPath)
		if err != nil {
			log.Error(err, "Failed to read hsperfdata", "pid", proc.PidHost, "path", proc.HsperfDataPath)
			continue
		}
		log.Info("Read hsperfdata", "pid", proc.PidHost, "counters", len(counters), "took", time.Since(readStart).String())

		info, _ := index.Lookup(proc.ContainerID)
		metrics = append(metrics, buildJVMMetric(counters, proc, info, nodeName))
	}
	log.Info("Built JVM metrics", "count", len(metrics), "took", time.Since(start).String())

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
