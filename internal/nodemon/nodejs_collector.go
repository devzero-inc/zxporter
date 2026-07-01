package nodemon

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
)

// nodeVersionInfo is the cached result of resolving a container's Node.js version.
type nodeVersionInfo struct {
	Version string
	Source  string
}

// NodeJSCollector detects Node.js processes via /proc and resolves their version
// on a best-effort, no-opt-in basis (env var, else a read-only binary scan). It
// does not read any live V8 heap stats — V8 has no hsperfdata-equivalent counters
// file, so that remains out of scope for now.
type NodeJSCollector struct {
	nodeName string
	index    *PodContainerIndex
	procRoot string
	log      logr.Logger

	mu sync.Mutex
	// versionCache avoids re-resolving (and, on the binary-scan path, re-reading a
	// multi-MB file) every scrape cycle for containers we've already resolved.
	// Rebuilt each cycle to only retain currently-running containers.
	versionCache map[string]nodeVersionInfo
}

// NewNodeJSCollector creates a NodeJSCollector. index must already be started (or
// be started concurrently) — NodeJSCollector only reads from it. procRoot defaults
// to "/proc".
func NewNodeJSCollector(nodeName string, index *PodContainerIndex, log logr.Logger) *NodeJSCollector {
	return &NodeJSCollector{
		nodeName:     nodeName,
		index:        index,
		procRoot:     "/proc",
		log:          log.WithName("nodejs-collector"),
		versionCache: make(map[string]nodeVersionInfo),
	}
}

// QueryNodeJSMetrics returns Node.js detection metrics for all discovered Node.js
// containers on this node.
func (c *NodeJSCollector) QueryNodeJSMetrics(ctx context.Context) ([]NodeJSMetric, error) {
	start := time.Now()
	procs, err := discoverNodeProcesses(c.procRoot)
	if err != nil {
		return nil, fmt.Errorf("discovering node processes: %w", err)
	}
	c.log.Info("Discovered node processes", "count", len(procs), "took", time.Since(start).String())

	c.mu.Lock()
	defer c.mu.Unlock()

	// Only adopt the rebuilt cache on a completed (non-cancelled) pass — on
	// cancellation buildNodeJSMetrics returns a partial cache covering only the
	// containers processed so far, and swapping it in would drop cached entries
	// for containers not yet reached this cycle.
	metrics, newCache, err := buildNodeJSMetrics(ctx, procs, c.index, c.nodeName, c.versionCache, c.log)
	if err == nil {
		c.versionCache = newCache
	}
	return metrics, err
}

// buildNodeJSMetrics resolves (with caching) the Node.js version for each
// discovered process and builds the corresponding NodeJSMetric. Shared by
// NodeJSCollector (the legacy /container/nodejs-metrics path) and
// RuntimeCollector (the combined /container/runtime-metrics path), so there is
// one implementation of the per-process build step regardless of which /proc
// walk discovered procs. Returns the updated cache (rebuilt to only retain
// currently-running containers) alongside the metrics.
func buildNodeJSMetrics(
	ctx context.Context,
	procs []NodeJSProcess,
	index *PodContainerIndex,
	nodeName string,
	cache map[string]nodeVersionInfo,
	log logr.Logger,
) ([]NodeJSMetric, map[string]nodeVersionInfo, error) {
	newCache := make(map[string]nodeVersionInfo, len(procs))
	metrics := make([]NodeJSMetric, 0, len(procs))
	for _, proc := range procs {
		select {
		case <-ctx.Done():
			log.Info("Node.js metrics query cancelled", "collected", len(metrics), "remaining", len(procs)-len(metrics))
			return metrics, newCache, ctx.Err()
		default:
		}

		// Only treat a cache hit as authoritative if it actually resolved a version —
		// an unresolved result (transient /proc read failure, process not yet fully
		// started, release-URL string beyond the scan window) is retried on the next
		// cycle rather than being stuck as "unknown" for the container's lifetime.
		info, cached := cache[proc.ContainerID]
		if !cached || info.Version == "" {
			version, source := resolveNodeVersion(proc.PidDir)
			info = nodeVersionInfo{Version: version, Source: source}
		}
		newCache[proc.ContainerID] = info

		containerInfo, _ := index.Lookup(proc.ContainerID)
		metrics = append(metrics, NodeJSMetric{
			NodeName:          nodeName,
			Pod:               containerInfo.Pod,
			Namespace:         containerInfo.Namespace,
			Container:         containerInfo.Container,
			ContainerID:       proc.ContainerID,
			PidHost:           proc.PidHost,
			PidNS:             proc.PidNS,
			NodeVersion:       info.Version,
			NodeVersionSource: info.Source,
			RawCmdline:        proc.CmdLine,
			Timestamp:         time.Now().UTC(),
		})
	}

	return metrics, newCache, nil
}
