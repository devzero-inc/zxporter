package nodemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

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

	idx := &PodContainerIndex{containerMap: map[string]containerInfo{}}
	c := NewRuntimeCollector("node-1", idx, testr.New(t))
	c.procRoot = procRoot

	metrics, err := c.QueryRuntimeMetrics(context.Background())
	require.NoError(t, err)
	require.Len(t, metrics.Runtimes, 1)
	assert.Equal(t, containerID, metrics.Runtimes[0].ContainerID)
	assert.Equal(t, "20.11.1", metrics.Runtimes[0].Version)

	// Confirm a second query still resolves via the collector's cache.
	metrics, err = c.QueryRuntimeMetrics(context.Background())
	require.NoError(t, err)
	require.Len(t, metrics.Runtimes, 1)
	assert.Equal(t, "20.11.1", metrics.Runtimes[0].Version)
}

func TestRuntimeCollector_QueryRuntimeMetrics_NoProcesses(t *testing.T) {
	idx := &PodContainerIndex{containerMap: map[string]containerInfo{}}
	c := NewRuntimeCollector("node-1", idx, testr.New(t))
	c.procRoot = t.TempDir()

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

	idx := &PodContainerIndex{containerMap: map[string]containerInfo{}}
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
