package collector

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"

	"golang.org/x/sync/errgroup"
)

const (
	containerSnapshotPath          = "/v2/container/snapshot"
	containerSnapshotResponseLimit = 32 << 20
)

// ContainerCollectionSnapshot is the cluster-wide result of one bounded
// composite request wave.
type ContainerCollectionSnapshot struct {
	ContainerMetrics     []UnifiedContainerMetric
	RuntimeMetrics       NodemonRuntimeMetrics
	FailedContainerNodes map[string]struct{}
	CompositeCount       int
	LegacyFallbackCount  int
}

type containerNodeSnapshot struct {
	containerMetrics []UnifiedContainerMetric
	runtimeMetrics   NodemonRuntimeMetrics
	containerFailed  bool
	usedLegacy       bool
	containerErr     error
	runtimeErr       error
}

// FetchAllContainerSnapshots fetches one composite cache snapshot from every
// discovered nodemon pod with the existing bounded concurrency limit.
func (c *NodemonClient) FetchAllContainerSnapshots(
	ctx context.Context,
) (ContainerCollectionSnapshot, error) {
	nodeToIP, err := c.refreshCache(ctx)
	if err != nil {
		return ContainerCollectionSnapshot{}, err
	}

	output := ContainerCollectionSnapshot{
		FailedContainerNodes: make(map[string]struct{}),
		CompositeCount:       len(nodeToIP),
	}
	if len(nodeToIP) == 0 {
		return output, nil
	}

	results := fetchAllNodesConcurrently(ctx, nodeToIP, func(ctx context.Context, podIP string) (containerNodeSnapshot, error) {
		baseURL := fmt.Sprintf("http://%s:%d", podIP, c.port)
		return c.fetchContainerSnapshot(ctx, baseURL)
	})
	sort.Slice(results, func(i, j int) bool {
		return results[i].nodeName < results[j].nodeName
	})

	for _, result := range results {
		if result.err != nil {
			output.FailedContainerNodes[result.nodeName] = struct{}{}
			c.log.Error(result.err, "Failed to fetch container snapshot from nodemon pod",
				"node", result.nodeName, "podIP", result.podIP)
			continue
		}

		value := result.value
		if value.usedLegacy {
			output.LegacyFallbackCount++
		}
		if value.containerFailed {
			output.FailedContainerNodes[result.nodeName] = struct{}{}
		} else {
			output.ContainerMetrics = append(output.ContainerMetrics, value.containerMetrics...)
		}
		output.RuntimeMetrics.JVM = append(output.RuntimeMetrics.JVM, value.runtimeMetrics.JVM...)
		output.RuntimeMetrics.Runtimes = append(output.RuntimeMetrics.Runtimes, value.runtimeMetrics.Runtimes...)

		if value.containerErr != nil {
			c.log.Error(value.containerErr, "Legacy container snapshot fallback failed",
				"node", result.nodeName, "podIP", result.podIP)
		}
		if value.runtimeErr != nil && !errors.Is(value.runtimeErr, errRuntimeMetricsDisabled) {
			c.log.Error(value.runtimeErr, "Legacy runtime snapshot fallback failed",
				"node", result.nodeName, "podIP", result.podIP)
		}
	}
	return output, nil
}

func (c *NodemonClient) fetchContainerSnapshot(
	ctx context.Context,
	baseURL string,
) (containerNodeSnapshot, error) {
	snapshot, err := c.fetchCompositeContainerSnapshot(ctx, baseURL)
	if err == nil {
		return snapshot, nil
	}
	if _, fallback := fallbackReasonFromError(err); !fallback {
		return containerNodeSnapshot{}, err
	}
	return c.fetchLegacyContainerSnapshot(ctx, baseURL), nil
}

func (c *NodemonClient) fetchCompositeContainerSnapshot(
	ctx context.Context,
	baseURL string,
) (containerNodeSnapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+containerSnapshotPath, nil)
	if err != nil {
		return containerNodeSnapshot{}, fmt.Errorf("creating container snapshot request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return containerNodeSnapshot{}, fmt.Errorf("HTTP request to nodemon container snapshot failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return containerNodeSnapshot{}, &snapshotFallbackError{reason: fallbackNotFound}
	case http.StatusServiceUnavailable:
		return containerNodeSnapshot{}, &snapshotFallbackError{reason: fallbackNotReady}
	default:
		return containerNodeSnapshot{}, fmt.Errorf("nodemon container snapshot returned status %d", resp.StatusCode)
	}

	var response containerSnapshotResponse
	if err := decodeLimitedSnapshotJSON(resp.Body, containerSnapshotResponseLimit, &response); err != nil {
		return containerNodeSnapshot{}, err
	}
	if response.SchemaVersion != snapshotSchemaVersion {
		return containerNodeSnapshot{}, &snapshotFallbackError{
			reason: fallbackUnsupportedSchema,
			cause:  fmt.Errorf("got schema version %d", response.SchemaVersion),
		}
	}

	result := containerNodeSnapshot{}
	if snapshotSectionHasData(response.Sections.Containers.State) {
		result.containerMetrics = response.ContainerMetrics
	} else {
		result.containerFailed = true
	}
	if snapshotSectionHasData(response.Sections.Runtime.State) {
		result.runtimeMetrics = response.RuntimeMetrics
	}
	return result, nil
}

func (c *NodemonClient) fetchLegacyContainerSnapshot(
	ctx context.Context,
	baseURL string,
) containerNodeSnapshot {
	result := containerNodeSnapshot{usedLegacy: true}
	var group errgroup.Group
	group.Go(func() error {
		result.containerMetrics, result.containerErr = c.fetchContainerMetrics(ctx, baseURL)
		return nil
	})
	group.Go(func() error {
		result.runtimeMetrics, result.runtimeErr = c.fetchRuntimeMetrics(ctx, baseURL+"/container/runtime-metrics")
		return nil
	})
	_ = group.Wait()

	result.containerFailed = result.containerErr != nil
	if errors.Is(result.runtimeErr, errRuntimeMetricsDisabled) {
		result.runtimeErr = nil
		result.runtimeMetrics = NodemonRuntimeMetrics{}
	}
	return result
}
