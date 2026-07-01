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

	newCache := make(map[string]nodeVersionInfo, len(procs))
	metrics := make([]NodeJSMetric, 0, len(procs))
	for _, proc := range procs {
		select {
		case <-ctx.Done():
			c.log.Info("Node.js metrics query cancelled", "collected", len(metrics), "remaining", len(procs)-len(metrics))
			return metrics, ctx.Err()
		default:
		}

		info, cached := c.versionCache[proc.ContainerID]
		if !cached {
			version, source := resolveNodeVersion(proc.PidDir)
			info = nodeVersionInfo{Version: version, Source: source}
		}
		newCache[proc.ContainerID] = info

		containerInfo, _ := c.index.Lookup(proc.ContainerID)
		metrics = append(metrics, NodeJSMetric{
			NodeName:          c.nodeName,
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
	c.versionCache = newCache

	return metrics, nil
}
