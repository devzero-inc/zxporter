package snap

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/devzero-inc/zxporter/internal/collector"
	"github.com/devzero-inc/zxporter/internal/transport"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ClusterSnapshotter takes periodic snapshots and computes deltas
type ClusterSnapshotter struct {
	client        kubernetes.Interface
	logger        logr.Logger
	sender        transport.DirectSender
	stopCh        chan struct{}
	ticker        *time.Ticker
	interval      time.Duration
	mu            sync.RWMutex
	namespaces    []string
	excludedPods  map[string]bool
	excludedNodes map[string]bool
}

func NewClusterSnapshotter(
	client kubernetes.Interface,
	interval time.Duration,
	sender transport.DirectSender,
	namespaces []string,
	excludedPods []collector.ExcludedPod,
	excludedNodes []string,
	logger logr.Logger,
) *ClusterSnapshotter {
	if interval <= 0 {
		interval = 15 * time.Minute // Default to 15 minutes
	}

	excludedPodsMap := make(map[string]bool)
	for _, pod := range excludedPods {
		key := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
		excludedPodsMap[key] = true
	}

	excludedNodesMap := make(map[string]bool)
	for _, node := range excludedNodes {
		excludedNodesMap[node] = true
	}

	return &ClusterSnapshotter{
		client:        client,
		logger:        logger.WithName("cluster-snapshotter"),
		sender:        sender,
		stopCh:        make(chan struct{}),
		interval:      interval,
		namespaces:    namespaces,
		excludedPods:  excludedPodsMap,
		excludedNodes: excludedNodesMap,
	}
}

func (c *ClusterSnapshotter) Start(ctx context.Context) error {
	c.logger.Info("Starting cluster snapshotter", "interval", c.interval)

	c.ticker = time.NewTicker(c.interval)

	go func() {
		c.takeSnapshot(ctx)
	}()

	go c.snapshotLoop(ctx)

	go func() {
		select {
		case <-ctx.Done():
			c.Stop()
		case <-c.stopCh:
		}
	}()

	return nil
}

func (c *ClusterSnapshotter) snapshotLoop(ctx context.Context) {
	for {
		select {
		case <-c.stopCh:
			return
		case <-c.ticker.C:
			c.takeSnapshot(ctx)
		}
	}
}

func (c *ClusterSnapshotter) takeSnapshot(ctx context.Context) {
	c.logger.Info("Taking cluster snapshot")
	startTime := time.Now()

	snapshot, err := c.captureClusterState(ctx)
	if err != nil {
		c.logger.Error(err, "Failed to capture cluster state")
		return
	}

	c.sendSnapshot(ctx, snapshot, true)

	c.logger.Info("Snapshot completed", "duration", time.Since(startTime))
}

func (c *ClusterSnapshotter) captureClusterState(ctx context.Context) (*ClusterSnapshot, error) {
	snapshot := &ClusterSnapshot{
		Nodes:         make(map[string]*NodeData),
		Namespaces:    make(map[string]*Namespace),
		ClusterScoped: &ClusterScopedSnapshot{},
		Timestamp:     time.Now(),
		SnapshotID:    fmt.Sprintf("snapshot-%d", time.Now().UnixNano()),
	}

	if err := c.captureClusterInfo(ctx, snapshot); err != nil {
		return nil, fmt.Errorf("failed to capture cluster info: %w", err)
	}

	if err := c.captureNodes(ctx, snapshot); err != nil {
		return nil, fmt.Errorf("failed to capture nodes: %w", err)
	}

	if err := c.captureNamespaces(ctx, snapshot); err != nil {
		return nil, fmt.Errorf("failed to capture namespaces: %w", err)
	}

	if err := c.captureClusterScopedResources(ctx, snapshot); err != nil {
		return nil, fmt.Errorf("failed to capture cluster-scoped resources: %w", err)
	}

	return snapshot, nil
}

func (c *ClusterSnapshotter) sendSnapshot(ctx context.Context, snapshot *ClusterSnapshot, isFullSnapshot bool) {
	// dont send multiple snapshots at once
	// even if multiple runs are executing in parallel, this will ensure that same resources arent sent to the control plane simultaneously (control plane implementation detail)
	c.mu.Lock()
	defer c.mu.Unlock()

	snapshotType := "delta"
	if isFullSnapshot {
		snapshotType = "full"
	}

	c.logger.Info("Sending cluster snapshot",
		"type", snapshotType,
		"snapshotId", snapshot.SnapshotID,
		"nodes", len(snapshot.Nodes),
		"namespaces", len(snapshot.Namespaces))

	// Use the dedicated cluster snapshot endpoint directly via the DirectSender interface
	if _, err := c.sender.SendClusterSnapshot(ctx, snapshot, snapshot.SnapshotID, snapshot.Timestamp); err != nil {
		c.logger.Error(err, "Failed to send cluster snapshot via dedicated endpoint")
		// Fallback to the old method if the dedicated endpoint fails
		// c.sendSnapshotFallback(ctx, snapshot)
	} else {
		c.logger.Info("Successfully sent cluster snapshot via dedicated endpoint", "type", snapshotType)
	}
}

// // sendSnapshotFallback sends the cluster snapshot using the old generic resource method
// func (c *ClusterSnapshotter) sendSnapshotFallback(ctx context.Context, snapshot *ClusterSnapshot) {
// 	res := collector.CollectedResource{
// 		ResourceType: collector.ClusterSnapshot,
// 		Object:       snapshot,
// 		Timestamp:    snapshot.Timestamp,
// 		EventType:    collector.EventTypeSnapshot,
// 		Key:          snapshot.SnapshotID,
// 	}

// 	if _, err := c.sender.Send(ctx, res); err != nil {
// 		c.logger.Error(err, "Failed to send cluster snapshot via fallback method")
// 	} else {
// 		c.logger.Info("Successfully sent cluster snapshot via fallback method")
// 	}
// }

func (c *ClusterSnapshotter) Stop() error {
	c.logger.Info("Stopping cluster snapshotter")

	if c.ticker != nil {
		c.ticker.Stop()
		c.logger.Info("Stopped cluster snapshotter ticker")
	}

	select {
	case <-c.stopCh:
		c.logger.Info("Cluster snapshotter stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed cluster snapshotter stop channel")
	}

	return nil
}

func (c *ClusterSnapshotter) GetResourceChannel() <-chan []collector.CollectedResource {
	return nil
}

func (c *ClusterSnapshotter) GetType() string {
	return "cluster_snapshot"
}

func (c *ClusterSnapshotter) IsAvailable(ctx context.Context) bool {
	_, err := c.client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{Limit: 1})
	return err == nil
}
