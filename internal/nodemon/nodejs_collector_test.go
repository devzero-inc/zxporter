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

// buildFakeNodeProc builds a fake /proc/<pid> tree for a Node.js process under a
// fresh procRoot and returns the procRoot and the (bare, hex) container ID it maps
// to via the cgroup file.
func buildFakeNodeProc(t *testing.T, pid int, exeTarget, binaryContent string) (procRoot, containerID string) {
	t.Helper()

	procRoot = t.TempDir()
	containerID = fmt.Sprintf("%064x", pid)

	pidDir := filepath.Join(procRoot, strconv.Itoa(pid))
	require.NoError(t, os.MkdirAll(pidDir, 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(pidDir, "comm"), []byte("node\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(pidDir, "cmdline"), []byte("node\x00server.js\x00"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(pidDir, "cgroup"),
		[]byte("0::/kubepods/burstable/pod123/cri-containerd-"+containerID+".scope\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(pidDir, "status"), []byte("NSpid:\t"+strconv.Itoa(pid)+"\t7\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(pidDir, "environ"), []byte("PATH=/usr/bin\x00"), 0o644))

	if exeTarget != "" {
		require.NoError(t, os.Symlink(exeTarget, filepath.Join(pidDir, "exe")))
		rootPath := filepath.Join(pidDir, "root", exeTarget)
		require.NoError(t, os.MkdirAll(filepath.Dir(rootPath), 0o755))
		require.NoError(t, os.WriteFile(rootPath, []byte(binaryContent), 0o644))
	}

	return procRoot, containerID
}

func TestNodeJSCollector_QueryNodeJSMetrics_DiscoversAndResolvesVersion(t *testing.T) {
	binary := "junk https://nodejs.org/download/release/v18.19.1/node-v18.19.1.tar.gz junk"
	procRoot, containerID := buildFakeNodeProc(t, 4242, "/usr/local/bin/node", binary)

	idx := &PodContainerIndex{
		containerMap: map[string]containerInfo{
			containerID: {Pod: "my-app", Namespace: "default", Container: "app"},
		},
	}

	c := NewNodeJSCollector("node-1", idx, testr.New(t))
	c.procRoot = procRoot

	metrics, err := c.QueryNodeJSMetrics(context.Background())
	require.NoError(t, err)
	require.Len(t, metrics, 1)

	m := metrics[0]
	assert.Equal(t, "node-1", m.NodeName)
	assert.Equal(t, "my-app", m.Pod)
	assert.Equal(t, "default", m.Namespace)
	assert.Equal(t, "app", m.Container)
	assert.Equal(t, containerID, m.ContainerID)
	assert.Equal(t, "18.19.1", m.NodeVersion)
	assert.Equal(t, nodeVersionSourceBinaryScan, m.NodeVersionSource)
}

func TestNodeJSCollector_QueryNodeJSMetrics_CachesResolvedVersion(t *testing.T) {
	binary := "junk https://nodejs.org/download/release/v18.19.1/node-v18.19.1.tar.gz junk"
	procRoot, _ := buildFakeNodeProc(t, 4242, "/usr/local/bin/node", binary)

	idx := &PodContainerIndex{containerMap: map[string]containerInfo{}}
	c := NewNodeJSCollector("node-1", idx, testr.New(t))
	c.procRoot = procRoot

	metrics, err := c.QueryNodeJSMetrics(context.Background())
	require.NoError(t, err)
	require.Len(t, metrics, 1)
	assert.Equal(t, "18.19.1", metrics[0].NodeVersion)

	// Corrupt the on-disk binary so a fresh resolve would fail to find the
	// release-URL string. A second query should still report the cached version,
	// proving it didn't re-scan the (now-broken) binary.
	rootPath := filepath.Join(procRoot, strconv.Itoa(4242), "root", "usr", "local", "bin", "node")
	require.NoError(t, os.WriteFile(rootPath, []byte("no match here"), 0o644))

	metrics, err = c.QueryNodeJSMetrics(context.Background())
	require.NoError(t, err)
	require.Len(t, metrics, 1)
	assert.Equal(t, "18.19.1", metrics[0].NodeVersion, "cached version must survive even if the binary changes")
}

func TestNodeJSCollector_QueryNodeJSMetrics_NoJavaProcessesFound(t *testing.T) {
	idx := &PodContainerIndex{containerMap: map[string]containerInfo{}}
	c := NewNodeJSCollector("node-1", idx, testr.New(t))
	c.procRoot = t.TempDir()

	metrics, err := c.QueryNodeJSMetrics(context.Background())
	require.NoError(t, err)
	assert.Empty(t, metrics)
}

func TestNodeJSCollector_QueryNodeJSMetrics_RetriesUnresolvedVersion(t *testing.T) {
	// No NODE_VERSION env and no exe symlink at all — first query can't resolve
	// anything (e.g. transient /proc read failure on a process that's still starting).
	procRoot, containerID := buildFakeNodeProc(t, 4242, "", "")

	idx := &PodContainerIndex{containerMap: map[string]containerInfo{}}
	c := NewNodeJSCollector("node-1", idx, testr.New(t))
	c.procRoot = procRoot

	metrics, err := c.QueryNodeJSMetrics(context.Background())
	require.NoError(t, err)
	require.Len(t, metrics, 1)
	assert.Empty(t, metrics[0].NodeVersion, "first scrape has nothing to resolve from")

	// Now the binary becomes readable — e.g. the process finished starting.
	binary := "https://nodejs.org/download/release/v20.11.1/node-v20.11.1.tar.gz"
	exeTarget := "/usr/local/bin/node"
	pidDir := filepath.Join(procRoot, strconv.Itoa(4242))
	require.NoError(t, os.Symlink(exeTarget, filepath.Join(pidDir, "exe")))
	rootPath := filepath.Join(pidDir, "root", exeTarget)
	require.NoError(t, os.MkdirAll(filepath.Dir(rootPath), 0o755))
	require.NoError(t, os.WriteFile(rootPath, []byte(binary), 0o644))

	metrics, err = c.QueryNodeJSMetrics(context.Background())
	require.NoError(t, err)
	require.Len(t, metrics, 1)
	assert.Equal(t, containerID, metrics[0].ContainerID)
	assert.Equal(t, "20.11.1", metrics[0].NodeVersion, "an unresolved result must be retried, not cached forever")
}

func TestNodeJSCollector_QueryNodeJSMetrics_StopsRetryingAfterMaxAttempts(t *testing.T) {
	// No NODE_VERSION env and no exe symlink — genuinely unresolvable, e.g. a
	// custom/stripped binary that will never contain the release-URL string.
	procRoot, _ := buildFakeNodeProc(t, 4242, "", "")

	idx := &PodContainerIndex{containerMap: map[string]containerInfo{}}
	c := NewNodeJSCollector("node-1", idx, testr.New(t))
	c.procRoot = procRoot

	for i := 0; i < maxNodeVersionResolveAttempts; i++ {
		metrics, err := c.QueryNodeJSMetrics(context.Background())
		require.NoError(t, err)
		require.Len(t, metrics, 1)
		assert.Empty(t, metrics[0].NodeVersion)
	}

	// Retries are now exhausted. Even though the binary becomes resolvable, the
	// container must not be rescanned — this is the whole point of the cap: a
	// permanently-unresolvable container stops incurring scan cost eventually.
	binary := "https://nodejs.org/download/release/v20.11.1/node-v20.11.1.tar.gz"
	exeTarget := "/usr/local/bin/node"
	pidDir := filepath.Join(procRoot, strconv.Itoa(4242))
	require.NoError(t, os.Symlink(exeTarget, filepath.Join(pidDir, "exe")))
	rootPath := filepath.Join(pidDir, "root", exeTarget)
	require.NoError(t, os.MkdirAll(filepath.Dir(rootPath), 0o755))
	require.NoError(t, os.WriteFile(rootPath, []byte(binary), 0o644))

	metrics, err := c.QueryNodeJSMetrics(context.Background())
	require.NoError(t, err)
	require.Len(t, metrics, 1)
	assert.Empty(t, metrics[0].NodeVersion, "retries must stop once the attempt cap is reached")
}
