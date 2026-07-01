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
}

// NewRuntimeCollector creates a RuntimeCollector. index must already be started
// (or be started concurrently) — RuntimeCollector only reads from it. procRoot
// defaults to "/proc".
func NewRuntimeCollector(nodeName string, index *PodContainerIndex, log logr.Logger) *RuntimeCollector {
	return &RuntimeCollector{
		nodeName:         nodeName,
		index:            index,
		procRoot:         "/proc",
		log:              log.WithName("runtime-collector"),
		nodeVersionCache: make(map[string]nodeVersionInfo),
	}
}

// QueryRuntimeMetrics returns JVM and Node.js metrics for all discovered
// containers on this node, from a single /proc walk.
func (c *RuntimeCollector) QueryRuntimeMetrics(ctx context.Context) (RuntimeMetrics, error) {
	start := time.Now()
	javaProcs, nodeProcs, err := discoverRuntimeProcesses(c.procRoot)
	if err != nil {
		return RuntimeMetrics{}, fmt.Errorf("discovering runtime processes: %w", err)
	}
	c.log.Info("Discovered runtime processes", "java", len(javaProcs), "nodejs", len(nodeProcs), "took", time.Since(start).String())

	// Always attempt both builds, even if one is cancelled/errors — a slow JVM
	// hsperfdata read (many Java containers) must not starve Node.js visibility
	// for the cycle, and vice versa, even though they share one /proc walk.
	jvmMetrics, jvmErr := buildJVMMetrics(ctx, javaProcs, c.index, c.nodeName, c.log)

	c.mu.Lock()
	defer c.mu.Unlock()

	nodeJSMetrics, newCache, nodeJSErr := buildNodeJSMetrics(ctx, nodeProcs, c.index, c.nodeName, c.nodeVersionCache, c.log)
	if nodeJSErr == nil {
		c.nodeVersionCache = newCache
	}

	return RuntimeMetrics{JVM: jvmMetrics, NodeJS: nodeJSMetrics}, errors.Join(jvmErr, nodeJSErr)
}
