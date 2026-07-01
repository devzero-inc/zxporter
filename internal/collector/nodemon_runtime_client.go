package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// NodemonRuntimeMetrics bundles the combined JVM + generic-runtime metrics
// returned by the nodemon GET /container/runtime-metrics endpoint — one HTTP
// round-trip and one /proc walk per node. JVM has its own bucket (richer,
// hsperfdata-backed payload); every other runtime shares the generic shape.
type NodemonRuntimeMetrics struct {
	JVM      []NodemonJVMMetrics            `json:"jvm"`
	Runtimes []NodemonRuntimeProcessMetrics `json:"runtimes"`
}

// FetchAllRuntimeMetrics discovers all nodemon pods and fetches combined JVM +
// Node.js metrics from each, merging the results across nodes.
func (c *NodemonClient) FetchAllRuntimeMetrics(ctx context.Context) (NodemonRuntimeMetrics, error) {
	nodeToIP, err := c.refreshCache(ctx)
	if err != nil {
		return NodemonRuntimeMetrics{}, err
	}
	if len(nodeToIP) == 0 {
		return NodemonRuntimeMetrics{}, nil
	}

	var all NodemonRuntimeMetrics
	for nodeName, podIP := range nodeToIP {
		url := fmt.Sprintf("http://%s:%d/container/runtime-metrics", podIP, c.port)
		metrics, fetchErr := c.fetchRuntimeMetrics(ctx, url)
		if fetchErr != nil {
			c.log.Error(fetchErr, "Failed to fetch runtime metrics from exporter pod", "node", nodeName, "podIP", podIP)
			continue
		}
		all.JVM = append(all.JVM, metrics.JVM...)
		all.Runtimes = append(all.Runtimes, metrics.Runtimes...)
	}
	return all, nil
}

func (c *NodemonClient) fetchRuntimeMetrics(ctx context.Context, url string) (NodemonRuntimeMetrics, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return NodemonRuntimeMetrics{}, fmt.Errorf("creating request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return NodemonRuntimeMetrics{}, fmt.Errorf("HTTP request to nodemon failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return NodemonRuntimeMetrics{}, fmt.Errorf("nodemon returned status %d", resp.StatusCode)
	}

	const maxResponseBytes = 16 << 20 // 16MiB safety cap
	var metrics NodemonRuntimeMetrics
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&metrics); err != nil {
		return NodemonRuntimeMetrics{}, fmt.Errorf("decoding nodemon runtime response: %w", err)
	}
	return metrics, nil
}
