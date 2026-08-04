package nodemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildFakeNodeProc builds a procRoot containing one containerized node process
// whose exe points at a binary with the given content. Returns the procRoot and
// container ID.
func buildFakeNodeProc(t *testing.T, pid int, exeTarget, binaryContent string) (procRoot, containerID string) {
	t.Helper()
	procRoot = t.TempDir()
	containerID = buildFakeProcTree(t, procRoot, pid, nodeComm)

	pidDir := filepath.Join(procRoot, strconv.Itoa(pid))
	require.NoError(t, os.Symlink(exeTarget, filepath.Join(pidDir, "exe")))
	rootPath := filepath.Join(pidDir, "root", exeTarget)
	require.NoError(t, os.MkdirAll(filepath.Dir(rootPath), 0o755))
	require.NoError(t, os.WriteFile(rootPath, []byte(binaryContent), 0o644))
	return procRoot, containerID
}

func findRuntime(t *testing.T, metrics []RuntimeProcessMetric, runtime string) RuntimeProcessMetric {
	t.Helper()
	for _, m := range metrics {
		if m.Runtime == runtime {
			return m
		}
	}
	t.Fatalf("no %s entry in runtimes; got %+v", runtime, metrics)
	return RuntimeProcessMetric{}
}

func TestRuntimeCollector_QueryRuntimeMetrics_SingleWalkCoversAllKinds(t *testing.T) {
	procRoot := t.TempDir()
	nodeContainerID := buildFakeProcTree(t, procRoot, 200, nodeComm)
	pythonContainerID := buildFakeProcTree(t, procRoot, 300, "python3")

	idx := &PodContainerIndex{
		containerMap: map[string]containerInfo{
			nodeContainerID:   {Pod: "my-node-app", Namespace: "default", Container: "app"},
			pythonContainerID: {Pod: "my-python-app", Namespace: "default", Container: "app"},
		},
	}

	c := NewRuntimeCollector("node-1", idx, testr.New(t))
	c.procRoot = procRoot

	c.Collect(context.Background())
	metrics, err := c.QueryRuntimeMetrics(context.Background())
	require.NoError(t, err)

	require.Len(t, metrics.Runtimes, 2)
	nodeMetric := findRuntime(t, metrics.Runtimes, runtimeNameNodeJS)
	assert.Equal(t, nodeContainerID, nodeMetric.ContainerID)
	assert.Equal(t, "my-node-app", nodeMetric.Pod)
	// The fixture's comm-only process has no resolvable env/binary, so version
	// resolution legitimately comes back empty — still reported as detected.
	assert.Empty(t, nodeMetric.Version)

	pythonMetric := findRuntime(t, metrics.Runtimes, runtimeNamePython)
	assert.Equal(t, pythonContainerID, pythonMetric.ContainerID)
	assert.Equal(t, "my-python-app", pythonMetric.Pod)
}

func TestRuntimeCollector_QueryRuntimeMetrics_CachesNodeVersionAcrossCalls(t *testing.T) {
	binary := "junk https://nodejs.org/download/release/v20.11.1/node-v20.11.1.tar.gz junk"
	procRoot, containerID := buildFakeNodeProc(t, 300, "/usr/local/bin/node", binary)

	idx := &PodContainerIndex{containerMap: map[string]containerInfo{
		containerID: {Pod: "node-app", Namespace: "default", Container: "app"},
	}}
	c := NewRuntimeCollector("node-1", idx, testr.New(t))
	c.procRoot = procRoot

	c.Collect(context.Background())
	metrics, err := c.QueryRuntimeMetrics(context.Background())
	require.NoError(t, err)
	require.Len(t, metrics.Runtimes, 1)
	assert.Equal(t, containerID, metrics.Runtimes[0].ContainerID)
	assert.Equal(t, "20.11.1", metrics.Runtimes[0].Version)

	// Confirm a second collection cycle still resolves via the collector's
	// persisted version cache.
	c.Collect(context.Background())
	metrics, err = c.QueryRuntimeMetrics(context.Background())
	require.NoError(t, err)
	require.Len(t, metrics.Runtimes, 1)
	assert.Equal(t, "20.11.1", metrics.Runtimes[0].Version)
}

func TestRuntimeCollector_QueryRuntimeMetrics_NoProcesses(t *testing.T) {
	idx := &PodContainerIndex{containerMap: map[string]containerInfo{}}
	c := NewRuntimeCollector("node-1", idx, testr.New(t))
	c.procRoot = t.TempDir()

	c.Collect(context.Background())
	metrics, err := c.QueryRuntimeMetrics(context.Background())
	require.NoError(t, err)
	assert.Empty(t, metrics.JVM)
	assert.Empty(t, metrics.Runtimes)
}

func TestBuildRuntimeProcessMetrics_StopsRetryingAfterMaxAttempts(t *testing.T) {
	// A node process whose binary never yields a version: resolution should be
	// attempted maxVersionResolveAttempts times total, then pinned as unresolved.
	procRoot := t.TempDir()
	containerID := buildFakeProcTree(t, procRoot, 400, nodeComm)
	pidDir := filepath.Join(procRoot, "400")
	exeTarget := "/usr/local/bin/node"
	require.NoError(t, os.Symlink(exeTarget, filepath.Join(pidDir, "exe")))
	rootPath := filepath.Join(pidDir, "root", exeTarget)
	require.NoError(t, os.MkdirAll(filepath.Dir(rootPath), 0o755))
	require.NoError(t, os.WriteFile(rootPath, []byte("no version marker here"), 0o644))

	proc := RuntimeProcess{
		Kind:        processKindNode,
		Runtime:     runtimeNameNodeJS,
		PidHost:     400,
		PidNS:       7,
		ContainerID: containerID,
		CmdLine:     "node app.js",
		PidDir:      pidDir,
	}

	idx := &PodContainerIndex{containerMap: map[string]containerInfo{
		containerID: {Pod: "node-app", Namespace: "default", Container: "app"},
	}}
	cache := map[string]versionResolveInfo(nil)
	for cycle := 0; cycle < maxVersionResolveAttempts+3; cycle++ {
		metrics, newCache, err := buildRuntimeProcessMetrics(
			context.Background(), []RuntimeProcess{proc}, idx, "node-1", cache, testr.New(t))
		require.NoError(t, err)
		require.Len(t, metrics, 1)
		assert.Empty(t, metrics[0].Version)
		cache = newCache
	}

	key := fmt.Sprintf("%s/%s", containerID, runtimeNameNodeJS)
	require.Contains(t, cache, key)
	assert.Equal(t, maxVersionResolveAttempts, cache[key].Attempts,
		"attempts must be capped at maxVersionResolveAttempts")
}

// TestRuntimeCollector_Collect_BuildsConcurrently proves the JVM and
// generic-runtime builds run concurrently rather than one after another.
// buildJVM blocks until buildRuntime has started, and buildRuntime only
// starts once buildJVM has already started: if the two ran sequentially,
// buildJVM would run to completion (never unblocking, since buildRuntime
// never gets a chance to run) and the whole call would hang. A bounded wait
// turns that hang into a clear failure instead of stalling the test suite.
func TestRuntimeCollector_Collect_BuildsConcurrently(t *testing.T) {
	procRoot := t.TempDir() // empty: no /proc entries, so discovery yields no processes

	c := NewRuntimeCollector("node-1", nil, testr.New(t))
	c.procRoot = procRoot

	jvmStarted := make(chan struct{})
	release := make(chan struct{})

	c.buildJVM = func(_ context.Context, _ []JavaProcess, _ *PodContainerIndex, _ string, _ logr.Logger) ([]JVMMetric, error) {
		close(jvmStarted)
		<-release
		return nil, nil
	}
	c.buildRuntime = func(_ context.Context, _ []RuntimeProcess, _ *PodContainerIndex, _ string, cache map[string]versionResolveInfo, _ logr.Logger) ([]RuntimeProcessMetric, map[string]versionResolveInfo, error) {
		<-jvmStarted
		close(release)
		return nil, cache, nil
	}

	done := make(chan struct{})
	go func() {
		c.Collect(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Collect did not return within 2s — buildJVM and buildRuntime are not running concurrently")
	}

	_, err := c.QueryRuntimeMetrics(context.Background())
	require.NoError(t, err)
}

// TestRuntimeCollector_QueryRuntimeMetrics_ServesCachedSnapshot proves
// QueryRuntimeMetrics reads the snapshot populated by the last Collect call
// instead of doing a live /proc walk + build on every call — the whole point
// of backgrounding collection is that HTTP requests never pay walk/build cost.
func TestRuntimeCollector_QueryRuntimeMetrics_ServesCachedSnapshot(t *testing.T) {
	procRoot := t.TempDir()
	c := NewRuntimeCollector("node-1", nil, testr.New(t))
	c.procRoot = procRoot

	var calls int32
	c.buildJVM = func(_ context.Context, _ []JavaProcess, _ *PodContainerIndex, _ string, _ logr.Logger) ([]JVMMetric, error) {
		atomic.AddInt32(&calls, 1)
		return nil, nil
	}
	c.buildRuntime = func(_ context.Context, _ []RuntimeProcess, _ *PodContainerIndex, _ string, cache map[string]versionResolveInfo, _ logr.Logger) ([]RuntimeProcessMetric, map[string]versionResolveInfo, error) {
		return nil, cache, nil
	}

	c.Collect(context.Background())
	require.EqualValues(t, 1, atomic.LoadInt32(&calls), "Collect should invoke the build functions exactly once")

	for range 5 {
		_, err := c.QueryRuntimeMetrics(context.Background())
		require.NoError(t, err)
	}
	assert.EqualValues(t, 1, atomic.LoadInt32(&calls),
		"QueryRuntimeMetrics must serve the cached snapshot from Collect, not recompute on every call")
}

// TestRuntimeCollector_StartCollectionLoop_RefreshesPeriodically proves the
// background loop keeps the cache warm on a tick, not just at startup.
func TestRuntimeCollector_StartCollectionLoop_RefreshesPeriodically(t *testing.T) {
	procRoot := t.TempDir()
	c := NewRuntimeCollector("node-1", nil, testr.New(t))
	c.procRoot = procRoot

	var calls int32
	c.buildJVM = func(_ context.Context, _ []JavaProcess, _ *PodContainerIndex, _ string, _ logr.Logger) ([]JVMMetric, error) {
		atomic.AddInt32(&calls, 1)
		return nil, nil
	}
	c.buildRuntime = func(_ context.Context, _ []RuntimeProcess, _ *PodContainerIndex, _ string, cache map[string]versionResolveInfo, _ logr.Logger) ([]RuntimeProcessMetric, map[string]versionResolveInfo, error) {
		return nil, cache, nil
	}

	// Join the background loop before the test returns: otherwise it can still
	// be inside a Collect() → logger.Info() call (writing to the testr logger's
	// *testing.T) during teardown, which the race detector flags.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.StartCollectionLoop(ctx, 10*time.Millisecond)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&calls) >= 3
	}, time.Second, 5*time.Millisecond, "expected multiple collection cycles from the ticker")
}

// TestRuntimeCollector_StartCollectionLoop_BoundsEachCycle proves each
// collection cycle runs under a per-cycle deadline rather than the loop's
// long-lived (shutdown-only) context. buildJVM blocks on its ctx; if Collect
// were still called with the unbounded loop context (the bug), it would
// never unblock and the ticker could never fire a second cycle.
func TestRuntimeCollector_StartCollectionLoop_BoundsEachCycle(t *testing.T) {
	procRoot := t.TempDir()
	c := NewRuntimeCollector("node-1", nil, testr.New(t))
	c.procRoot = procRoot

	var calls int32
	c.buildJVM = func(ctx context.Context, _ []JavaProcess, _ *PodContainerIndex, _ string, _ logr.Logger) ([]JVMMetric, error) {
		atomic.AddInt32(&calls, 1)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	c.buildRuntime = func(ctx context.Context, _ []RuntimeProcess, _ *PodContainerIndex, _ string, cache map[string]versionResolveInfo, _ logr.Logger) ([]RuntimeProcessMetric, map[string]versionResolveInfo, error) {
		<-ctx.Done()
		return nil, cache, ctx.Err()
	}

	// Join the background loop before the test returns (see the sibling
	// RefreshesPeriodically test for why).
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.StartCollectionLoop(ctx, 20*time.Millisecond)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&calls) >= 2
	}, 2*time.Second, 10*time.Millisecond,
		"expected the per-cycle context to expire so a second cycle can start")
}

// TestRuntimeCollector_Collect_KeepsLastGoodSnapshotOnTotalFailure proves a
// cycle that produces nothing usable (build functions return no metrics and
// an error) doesn't blank a previously-good cache — a transient failure
// shouldn't erase good data for a full refresh interval.
func TestRuntimeCollector_Collect_KeepsLastGoodSnapshotOnTotalFailure(t *testing.T) {
	c := NewRuntimeCollector("node-1", nil, testr.New(t))
	c.procRoot = t.TempDir()

	goodJVM := []JVMMetric{{NodeName: "node-1", Pod: "app"}}
	c.buildJVM = func(_ context.Context, _ []JavaProcess, _ *PodContainerIndex, _ string, _ logr.Logger) ([]JVMMetric, error) {
		return goodJVM, nil
	}
	c.buildRuntime = func(_ context.Context, _ []RuntimeProcess, _ *PodContainerIndex, _ string, cache map[string]versionResolveInfo, _ logr.Logger) ([]RuntimeProcessMetric, map[string]versionResolveInfo, error) {
		return nil, cache, nil
	}
	c.Collect(context.Background())
	metrics, err := c.QueryRuntimeMetrics(context.Background())
	require.NoError(t, err)
	require.Equal(t, goodJVM, metrics.JVM)

	c.buildJVM = func(_ context.Context, _ []JavaProcess, _ *PodContainerIndex, _ string, _ logr.Logger) ([]JVMMetric, error) {
		return nil, errors.New("boom")
	}
	c.buildRuntime = func(_ context.Context, _ []RuntimeProcess, _ *PodContainerIndex, _ string, cache map[string]versionResolveInfo, _ logr.Logger) ([]RuntimeProcessMetric, map[string]versionResolveInfo, error) {
		return nil, cache, errors.New("boom")
	}
	c.Collect(context.Background())

	metrics, err = c.QueryRuntimeMetrics(context.Background())
	assert.Error(t, err, "the failure should still be surfaced")
	assert.Equal(t, goodJVM, metrics.JVM, "a total failure must not blank the last good snapshot")
}

// TestRuntimeCollector_QueryRuntimeMetrics_NotYetCollected proves a fresh
// collector (no Collect run yet) reports an error rather than a silent,
// successful-looking empty result — the HTTP server can start accepting
// requests before the first background Collect completes.
func TestRuntimeCollector_QueryRuntimeMetrics_NotYetCollected(t *testing.T) {
	c := NewRuntimeCollector("node-1", nil, testr.New(t))
	_, err := c.QueryRuntimeMetrics(context.Background())
	assert.ErrorIs(t, err, errMetricsNotYetCollected)
}
