package nodemon

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
)

// staleTestQuerier is a minimal MetricsQuerier returning a fixed snapshot.
type staleTestQuerier struct{}

func (staleTestQuerier) QueryMetrics(_ context.Context) ([]GPUMetric, error) {
	return []GPUMetric{{Pod: "train", GPUUtilization: 90}}, nil
}

// deadlineCapturingQuerier records whether the context it was called with
// carried a deadline — used to prove refreshCycle bounds each refresh.
type deadlineCapturingQuerier struct{ hadDeadline bool }

func (q *deadlineCapturingQuerier) QueryMetrics(ctx context.Context) ([]GPUMetric, error) {
	_, q.hadDeadline = ctx.Deadline()
	return nil, nil
}

// TestCachedGPUExporter_RefreshCycleBoundsEachRefresh proves refreshCycle wraps
// the refresh in a per-cycle deadline, so a hung dependency (e.g. getDCGMUrls'
// otherwise-unbounded k8s List) cannot wedge the refresher. The parent context
// has no deadline; the one the source sees must.
func TestCachedGPUExporter_RefreshCycleBoundsEachRefresh(t *testing.T) {
	src := &deadlineCapturingQuerier{}
	c := NewCachedGPUExporter(src, DefaultGPURefreshInterval, logr.Discard())

	_, parentHasDeadline := context.Background().Deadline()
	require.False(t, parentHasDeadline, "sanity: the parent context has no deadline")

	c.refreshCycle(context.Background())
	require.True(t, src.hadDeadline, "refreshCycle must give each refresh a per-cycle deadline")
}

// TestCachedGPUExporter_AgeBasedStaleness exercises the grace window
// deterministically by aging lastSuccess (only reachable from the in-package
// test). It confirms both reader paths agree: within the threshold the snapshot
// is served (ready / non-nil), beyond it the snapshot is withheld everywhere
// (QueryMetrics -> nil for the container/legacy path, QueryGPUSnapshot -> stale
// for the node path).
func TestCachedGPUExporter_AgeBasedStaleness(t *testing.T) {
	c := NewCachedGPUExporter(staleTestQuerier{}, DefaultGPURefreshInterval, logr.Discard())
	require.Equal(t, 20*time.Second, c.stalenessThreshold, "default 10s interval -> 20s threshold")

	c.Refresh(context.Background())

	// Within the grace window (aged 15s < 20s): served on both paths.
	c.mu.Lock()
	c.lastSuccess = time.Now().Add(-15 * time.Second)
	c.mu.Unlock()

	metrics, err := c.QueryMetrics(context.Background())
	require.NoError(t, err)
	require.Len(t, metrics, 1, "within grace, QueryMetrics serves the snapshot")
	_, status := c.QueryGPUSnapshot()
	require.Equal(t, SnapshotStateReady, status.State, "within grace, node path is ready")

	// Aged past the threshold (25s > 20s): withheld on both paths.
	c.mu.Lock()
	c.lastSuccess = time.Now().Add(-25 * time.Second)
	c.mu.Unlock()

	metrics, err = c.QueryMetrics(context.Background())
	require.NoError(t, err)
	require.Nil(t, metrics, "past threshold, QueryMetrics withholds (container/legacy path gaps)")
	_, status = c.QueryGPUSnapshot()
	require.Equal(t, SnapshotStateStale, status.State, "past threshold, node path is stale (dropped by collector)")
}

// TestCachedGPUExporter_StalenessThresholdFloor confirms the 20s floor holds
// even when the interval is clamped to its 5s minimum, and that a larger
// interval widens the threshold to 2x interval.
func TestCachedGPUExporter_StalenessThresholdFloor(t *testing.T) {
	atFloor := NewCachedGPUExporter(staleTestQuerier{}, MinGPURefreshInterval, logr.Discard())
	require.Equal(t, minGPUStalenessThreshold, atFloor.stalenessThreshold,
		"2*5s=10s is below the 20s floor, so the floor applies")

	wide := NewCachedGPUExporter(staleTestQuerier{}, 30*time.Second, logr.Discard())
	require.Equal(t, 60*time.Second, wide.stalenessThreshold,
		"a 30s interval must widen the threshold to 2x so normal operation is never stale")
}
