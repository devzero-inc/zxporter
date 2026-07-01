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
// /proc walk, across every supported runtime.
type RuntimeMetrics struct {
	JVM    []JVMMetric    `json:"jvm"`
	NodeJS []NodeJSMetric `json:"nodejs"`
	// Runtimes carries the generic-runtime detections (.NET, Go, GraalVM
	// native-image, Python, Ruby, Deno, Bun) — existence + best-effort version.
	Runtimes []RuntimeProcessMetric `json:"runtimes"`
}

// RuntimeCollector performs a single /proc walk per query and builds metrics for
// every discovered runtime (JVM, Node.js), instead of running an independent
// JVMCollector and NodeJSCollector walk. This is what backs the combined
// /container/runtime-metrics endpoint, which the zxporter collector uses instead
// of issuing two separate per-cycle HTTP fetches.
type RuntimeCollector struct {
	nodeName string
	index    *PodContainerIndex
	procRoot string
	log      logr.Logger

	mu               sync.Mutex
	nodeVersionCache map[string]nodeVersionInfo
	// runtimeVersionCache caches generic-runtime version resolution keyed by
	// containerID+"/"+runtime, with the same bounded-retry semantics as the
	// Node.js cache (a container can host processes of more than one runtime, so
	// containerID alone is not a sufficient key).
	runtimeVersionCache map[string]nodeVersionInfo
}

// NewRuntimeCollector creates a RuntimeCollector. index must already be started
// (or be started concurrently) — RuntimeCollector only reads from it. procRoot
// defaults to "/proc".
func NewRuntimeCollector(nodeName string, index *PodContainerIndex, log logr.Logger) *RuntimeCollector {
	return &RuntimeCollector{
		nodeName:            nodeName,
		index:               index,
		procRoot:            "/proc",
		log:                 log.WithName("runtime-collector"),
		nodeVersionCache:    make(map[string]nodeVersionInfo),
		runtimeVersionCache: make(map[string]nodeVersionInfo),
	}
}

// QueryRuntimeMetrics returns JVM, Node.js, and generic-runtime metrics for all
// discovered containers on this node, from a single /proc walk.
func (c *RuntimeCollector) QueryRuntimeMetrics(ctx context.Context) (RuntimeMetrics, error) {
	start := time.Now()
	javaProcs, nodeProcs, runtimeProcs, err := discoverRuntimeProcesses(c.procRoot)
	if err != nil {
		return RuntimeMetrics{}, fmt.Errorf("discovering runtime processes: %w", err)
	}
	c.log.Info("Discovered runtime processes",
		"java", len(javaProcs), "nodejs", len(nodeProcs), "other", len(runtimeProcs),
		"took", time.Since(start).String())

	// Always attempt every build, even if one is cancelled/errors — a slow JVM
	// hsperfdata read (many Java containers) must not starve Node.js or
	// generic-runtime visibility for the cycle, and vice versa, even though they
	// share one /proc walk.
	jvmMetrics, jvmErr := buildJVMMetrics(ctx, javaProcs, c.index, c.nodeName, c.log)

	c.mu.Lock()
	defer c.mu.Unlock()

	nodeJSMetrics, newCache, nodeJSErr := buildNodeJSMetrics(ctx, nodeProcs, c.index, c.nodeName, c.nodeVersionCache, c.log)
	if nodeJSErr == nil {
		c.nodeVersionCache = newCache
	}

	runtimeMetrics, newRuntimeCache, runtimeErr := buildRuntimeProcessMetrics(ctx, runtimeProcs, c.index, c.nodeName, c.runtimeVersionCache, c.log)
	if runtimeErr == nil {
		c.runtimeVersionCache = newRuntimeCache
	}

	return RuntimeMetrics{JVM: jvmMetrics, NodeJS: nodeJSMetrics, Runtimes: runtimeMetrics},
		errors.Join(jvmErr, nodeJSErr, runtimeErr)
}

// buildRuntimeProcessMetrics resolves (with caching and the same bounded-retry
// semantics as buildNodeJSMetrics) the version for each discovered
// generic-runtime process and builds the corresponding RuntimeProcessMetric.
// Returns the updated cache (rebuilt to only retain currently-running
// container/runtime pairs) alongside the metrics.
func buildRuntimeProcessMetrics(
	ctx context.Context,
	procs []RuntimeProcess,
	index *PodContainerIndex,
	nodeName string,
	cache map[string]nodeVersionInfo,
	log logr.Logger,
) ([]RuntimeProcessMetric, map[string]nodeVersionInfo, error) {
	newCache := make(map[string]nodeVersionInfo, len(procs))
	metrics := make([]RuntimeProcessMetric, 0, len(procs))
	for _, proc := range procs {
		select {
		case <-ctx.Done():
			log.Info("Runtime process metrics query cancelled", "collected", len(metrics), "remaining", len(procs)-len(metrics))
			return metrics, newCache, ctx.Err()
		default:
		}

		cacheKey := proc.ContainerID + "/" + proc.Runtime
		info, cached := cache[cacheKey]
		if !cached || (info.Version == "" && info.Attempts < maxNodeVersionResolveAttempts) {
			version, source := resolveRuntimeVersion(proc.Kind, proc.PidDir)
			info = nodeVersionInfo{Version: version, Source: source, Attempts: info.Attempts + 1}
		}
		newCache[cacheKey] = info

		containerInfo, _ := index.Lookup(proc.ContainerID)
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
