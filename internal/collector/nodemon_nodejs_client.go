package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// NodemonNodeJSMetrics represents a single Node.js detection entry returned by the
// nodemon GET /container/nodejs-metrics endpoint. Mirrors nodemon.NodeJSMetric but
// is defined here to avoid importing the nodemon package.
type NodemonNodeJSMetrics struct {
	NodeName    string `json:"node_name"`
	Pod         string `json:"pod"`
	Namespace   string `json:"namespace"`
	Container   string `json:"container"`
	ContainerID string `json:"container_id"`
	PidHost     int    `json:"pid_host"`
	PidNS       int    `json:"pid_ns"`

	NodeVersion       string `json:"node_version,omitempty"`
	NodeVersionSource string `json:"node_version_source,omitempty"`

	RawCmdline string    `json:"raw_cmdline,omitempty"`
	Timestamp  time.Time `json:"timestamp"`
}

// FetchAllNodeJSMetrics discovers all nodemon pods and fetches Node.js metrics from
// each, merging the results into a single slice.
func (c *NodemonClient) FetchAllNodeJSMetrics(ctx context.Context) ([]NodemonNodeJSMetrics, error) {
	nodeToIP, err := c.refreshCache(ctx)
	if err != nil {
		return nil, err
	}
	if len(nodeToIP) == 0 {
		return nil, nil
	}

	var all []NodemonNodeJSMetrics
	for nodeName, podIP := range nodeToIP {
		url := fmt.Sprintf("http://%s:%d/container/nodejs-metrics", podIP, c.port)
		metrics, fetchErr := c.fetchNodeJSMetrics(ctx, url)
		if fetchErr != nil {
			c.log.Error(fetchErr, "Failed to fetch Node.js metrics from exporter pod", "node", nodeName, "podIP", podIP)
			continue
		}
		all = append(all, metrics...)
	}
	return all, nil
}

func (c *NodemonClient) fetchNodeJSMetrics(ctx context.Context, url string) ([]NodemonNodeJSMetrics, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request to nodemon failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("nodemon returned status %d", resp.StatusCode)
	}

	const maxResponseBytes = 16 << 20 // 16MiB safety cap
	var metrics []NodemonNodeJSMetrics
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&metrics); err != nil {
		return nil, fmt.Errorf("decoding nodemon nodejs response: %w", err)
	}
	return metrics, nil
}

// IndexNodeJSMetricsByContainer indexes Node.js metrics by (pod, container, namespace)
// for O(1) lookup.
func IndexNodeJSMetricsByContainer(metrics []NodemonNodeJSMetrics) map[gpuContainerKey]NodemonNodeJSMetrics {
	index := make(map[gpuContainerKey]NodemonNodeJSMetrics)
	for _, m := range metrics {
		key := gpuContainerKey{Pod: m.Pod, Container: m.Container, Namespace: m.Namespace}
		// If multiple Node.js processes in one container, keep the first for now.
		if _, exists := index[key]; !exists {
			index[key] = m
		}
	}
	return index
}

// NodeJSMetricsFromNodemon converts a Node.js metric entry into a flat
// map[string]interface{} for attachment to ContainerMetricsSnapshot.
func NodeJSMetricsFromNodemon(m NodemonNodeJSMetrics) map[string]interface{} {
	return map[string]interface{}{
		"NodeJsDetected":      true,
		"NodeJsVersion":       m.NodeVersion,
		"NodeJsVersionSource": m.NodeVersionSource,
		"RawCmdline":          m.RawCmdline,
	}
}
