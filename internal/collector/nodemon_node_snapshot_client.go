package collector

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"golang.org/x/sync/errgroup"
)

const (
	nodeSnapshotPath          = "/v2/node/snapshot"
	nodeSnapshotResponseLimit = 1 << 20
)

// NodeCollectionSnapshot contains the independently usable node and GPU
// sections returned by one nodemon request.
type NodeCollectionSnapshot struct {
	NodeMetric     *UnifiedNodeMetric
	GPUMetrics     map[string]interface{}
	UsedLegacy     bool
	FallbackReason string
	// GPUStale is true when the composite GPU section came back stale — nodemon
	// is serving a last-good snapshot because its DCGM refresh is failing — so we
	// dropped it rather than ingesting unbounded-age data. Surfaced so the
	// collector can emit a per-sweep telemetry signal (the DCGM problem is only
	// otherwise visible in nodemon's own logs).
	GPUStale bool
}

// FetchNodeSnapshotByNode fetches one composite cache snapshot for nodeName.
// Legacy endpoints are queried only when the nodemon does not support a usable
// composite contract; transport failures are returned without another request.
func (c *NodemonClient) FetchNodeSnapshotByNode(
	ctx context.Context,
	nodeName string,
) (*NodeCollectionSnapshot, error) {
	nodeToIP, err := c.refreshCache(ctx)
	if err != nil {
		return nil, err
	}

	podIP, ok := nodeToIP[nodeName]
	if !ok {
		return nil, nil
	}

	baseURL := fmt.Sprintf("http://%s:%d", podIP, c.port)
	snapshot, err := c.fetchNodeSnapshot(ctx, baseURL)
	if err == nil {
		return snapshot, nil
	}

	reason, fallback := fallbackReasonFromError(err)
	if !fallback {
		return nil, err
	}
	return c.fetchLegacyNodeSnapshot(ctx, baseURL, reason)
}

func (c *NodemonClient) fetchNodeSnapshot(
	ctx context.Context,
	baseURL string,
) (*NodeCollectionSnapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+nodeSnapshotPath, nil)
	if err != nil {
		return nil, fmt.Errorf("creating node snapshot request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request to nodemon node snapshot failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, &snapshotFallbackError{reason: fallbackNotFound}
	case http.StatusServiceUnavailable:
		return nil, &snapshotFallbackError{reason: fallbackNotReady}
	default:
		return nil, fmt.Errorf("nodemon node snapshot returned status %d", resp.StatusCode)
	}

	var response nodeSnapshotResponse
	if err := decodeLimitedSnapshotJSON(resp.Body, nodeSnapshotResponseLimit, &response); err != nil {
		return nil, err
	}
	if response.SchemaVersion != snapshotSchemaVersion {
		return nil, &snapshotFallbackError{
			reason: fallbackUnsupportedSchema,
			cause:  fmt.Errorf("got schema version %d", response.SchemaVersion),
		}
	}

	result := &NodeCollectionSnapshot{GPUMetrics: map[string]interface{}{}}
	if snapshotSectionHasData(response.Sections.Node.State) {
		result.NodeMetric = response.NodeMetrics
	}
	// Ingest GPU only when the section is fresh (ready), never stale. A stale
	// section means nodemon's DCGM refresh is failing and it is serving a
	// last-good snapshot of UNBOUNDED age (the state is a flag, not a max age).
	// Matching main's behavior during a DCGM outage, we emit no GPU for this
	// node rather than ingesting arbitrarily old values stamped as current. The
	// node section is never stale (the unified exporter always publishes ready),
	// so only GPU needs this guard.
	if response.Sections.GPU.State == snapshotStateReady {
		result.GPUMetrics = response.GPUSummary.downstreamMetrics()
	} else if response.Sections.GPU.State == snapshotStateStale {
		result.GPUStale = true
	}
	return result, nil
}

func (c *NodemonClient) fetchLegacyNodeSnapshot(
	ctx context.Context,
	baseURL string,
	reason snapshotFallbackReason,
) (*NodeCollectionSnapshot, error) {
	result := &NodeCollectionSnapshot{
		GPUMetrics:     map[string]interface{}{},
		UsedLegacy:     true,
		FallbackReason: string(reason),
	}

	var nodeMetric *UnifiedNodeMetric
	var gpuMetrics []NodemonMetric
	var nodeErr error
	var gpuErr error
	var group errgroup.Group
	group.Go(func() error {
		nodeMetric, nodeErr = c.fetchNodeMetrics(ctx, baseURL)
		return nil
	})
	group.Go(func() error {
		gpuMetrics, gpuErr = c.fetchMetrics(ctx, baseURL+"/container/metrics")
		return nil
	})
	_ = group.Wait()

	if nodeErr == nil {
		result.NodeMetric = nodeMetric
	}
	if gpuErr == nil {
		result.GPUMetrics = NodeGPUMetricsFromNodemon(gpuMetrics)
	}
	if nodeErr != nil || gpuErr != nil {
		return result, errors.Join(nodeErr, gpuErr)
	}
	return result, nil
}
