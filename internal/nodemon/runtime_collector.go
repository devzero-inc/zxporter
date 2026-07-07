package nodemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
)

// RuntimeMetrics bundles the process-introspection metrics collected in a single
// /proc walk, across every supported runtime. JVM has its own bucket because its
// payload is hsperfdata-backed heap metrics + flag extraction; every other
// runtime (Node.js, .NET, Go, GraalVM native-image, Python, Ruby, Deno, Bun) is
// existence + best-effort version and shares the generic shape.
type RuntimeMetrics struct {
	JVM      []JVMMetric            `json:"jvm"`
	Runtimes []RuntimeProcessMetric `json:"runtimes"`
}

// versionResolveInfo is the cached result of resolving a container's runtime
// version.
type versionResolveInfo struct {
	Version string
	Source  string
	// Attempts counts unresolved resolution attempts so far. Bounds retries for
	// containers that will never resolve (custom/stripped binary, or the marker
	// string genuinely beyond the scan window) — without this, an unresolved
	// result would trigger a fresh (up to 64MiB) binary scan on every single
	// scrape cycle for the container's entire lifetime.
	Attempts int
}

// maxVersionResolveAttempts caps how many scrape cycles retry an unresolved
// version (and an unknown probe classification) before giving up. Small enough
// to bound worst-case scan cost, large enough to ride out a few cycles of
// "process still starting" transient failures.
const maxVersionResolveAttempts = 5

// RuntimeCollector performs a single /proc walk per query and builds metrics
// for every discovered runtime. This backs the combined
// /container/runtime-metrics endpoint, which the zxporter collector polls once
// per cycle.
type RuntimeCollector struct {
	nodeName string
	index    *PodContainerIndex
	procRoot string
	log      logr.Logger

	mu sync.Mutex
	// versionCache caches version resolution keyed by containerID+"/"+runtime,
	// with bounded retries (a container can host processes of more than one
	// runtime, so containerID alone is not a sufficient key). Rebuilt each
	// completed cycle to only retain currently-running containers.
	versionCache map[string]versionResolveInfo
	// probeCache memoizes executable-probe classifications (see newMemoizedProbe)
	// so long-lived unclassifiable processes aren't re-inspected every cycle.
	probeCache map[string]probeCacheEntry
}

// NewRuntimeCollector creates a RuntimeCollector. index must already be started
// (or be started concurrently) — RuntimeCollector only reads from it. procRoot
// defaults to "/proc".
func NewRuntimeCollector(nodeName string, index *PodContainerIndex, log logr.Logger) *RuntimeCollector {
	return &RuntimeCollector{
		nodeName:     nodeName,
		index:        index,
		procRoot:     "/proc",
		log:          log.WithName("runtime-collector"),
		versionCache: make(map[string]versionResolveInfo),
		probeCache:   make(map[string]probeCacheEntry),
	}
}

// QueryRuntimeMetrics returns JVM and generic-runtime metrics for all
// discovered containers on this node, from a single /proc walk.
func (c *RuntimeCollector) QueryRuntimeMetrics(ctx context.Context) (RuntimeMetrics, error) {
	start := time.Now()

	c.mu.Lock()
	prevProbeCache := c.probeCache
	c.mu.Unlock()
	probe, nextProbeCache := newMemoizedProbe(prevProbeCache, probeRuntimeProcess)

	javaProcs, runtimeProcs, err := discoverRuntimeProcesses(c.procRoot, probe)
	if err != nil {
		return RuntimeMetrics{}, fmt.Errorf("discovering runtime processes: %w", err)
	}
	c.log.Info("Discovered runtime processes",
		"java", len(javaProcs), "other", len(runtimeProcs),
		"took", time.Since(start).String())

	// Always attempt both builds, even if one is cancelled/errors — a slow JVM
	// hsperfdata read (many Java containers) must not starve generic-runtime
	// visibility for the cycle, and vice versa, even though they share one
	// /proc walk.
	jvmMetrics, jvmErr := buildJVMMetrics(ctx, javaProcs, c.index, c.nodeName, c.log)

	c.mu.Lock()
	defer c.mu.Unlock()

	// The walk completed, so the rebuilt probe cache covers every currently
	// running container — adopt it (drops entries for dead containers).
	c.probeCache = nextProbeCache

	// Only swap in the rebuilt version cache wholesale on a completed pass — on
	// cancellation it covers only the containers processed so far, and swapping
	// would drop entries for containers not yet reached this cycle. But DO merge
	// the partial results per key: each contains an incremented Attempts counter,
	// and discarding those would reset the bounded-retry cap on every cancelled
	// cycle — on a node where scans routinely exceed the handler budget, the cap
	// would never be reached and unresolvable binaries would be re-scanned (up
	// to 64MiB each) every single cycle forever.
	runtimeMetrics, newCache, runtimeErr := buildRuntimeProcessMetrics(ctx, runtimeProcs, c.index, c.nodeName, c.versionCache, c.log)
	if runtimeErr == nil {
		c.versionCache = newCache
	} else {
		for k, v := range newCache {
			c.versionCache[k] = v
		}
	}

	return RuntimeMetrics{JVM: jvmMetrics, Runtimes: runtimeMetrics},
		errors.Join(jvmErr, runtimeErr)
}

// buildRuntimeProcessMetrics resolves (with caching and bounded retries) the
// version for each discovered generic-runtime process and builds the
// corresponding RuntimeProcessMetric. Returns the updated cache (rebuilt to
// only retain currently-running container/runtime pairs) alongside the metrics.
func buildRuntimeProcessMetrics(
	ctx context.Context,
	procs []RuntimeProcess,
	index *PodContainerIndex,
	nodeName string,
	cache map[string]versionResolveInfo,
	log logr.Logger,
) ([]RuntimeProcessMetric, map[string]versionResolveInfo, error) {
	newCache := make(map[string]versionResolveInfo, len(procs))
	metrics := make([]RuntimeProcessMetric, 0, len(procs))
	for _, proc := range procs {
		select {
		case <-ctx.Done():
			log.Info("Runtime process metrics query cancelled", "collected", len(metrics), "remaining", len(procs)-len(metrics))
			return metrics, newCache, ctx.Err()
		default:
		}

		// Only treat a cache hit as authoritative if it actually resolved a
		// version — an unresolved result (transient /proc read failure, process
		// not yet fully started, marker string beyond the scan window) is retried
		// on later cycles, capped so a genuinely-unresolvable container doesn't
		// pay a fresh scan cost every cycle forever.
		cacheKey := proc.ContainerID + "/" + proc.Runtime
		info, cached := cache[cacheKey]
		if !cached || (info.Version == "" && info.Attempts < maxVersionResolveAttempts) {
			version, source := resolveRuntimeVersion(proc.Kind, proc.PidDir)
			info = versionResolveInfo{Version: version, Source: source, Attempts: info.Attempts + 1}
		}
		newCache[cacheKey] = info

		// Skip processes whose container isn't (yet) in the pod index — host-level
		// or non-k8s containers, or pods the informer hasn't delivered. Emitting
		// them with empty pod/namespace metadata just ships unattributable
		// cmdlines the collector can never join to a container; a pod that is
		// merely not-yet-indexed is picked up on the next cycle.
		containerInfo, ok := index.Lookup(proc.ContainerID)
		if !ok {
			continue
		}
		metrics = append(metrics, RuntimeProcessMetric{
			Runtime:       proc.Runtime,
			NodeName:      nodeName,
			Pod:           containerInfo.Pod,
			Namespace:     containerInfo.Namespace,
			Container:     containerInfo.Container,
			ContainerID:   proc.ContainerID,
			PidHost:       proc.PidHost,
			PidNS:         proc.PidNS,
			Version:       info.Version,
			VersionSource: info.Source,
			RawCmdline:    proc.CmdLine,
			Timestamp:     time.Now().UTC(),
		})
	}

	return metrics, newCache, nil
}
