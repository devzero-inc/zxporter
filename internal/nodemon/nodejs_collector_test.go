package nodemon

import (
	"context"
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
func buildFakeNodeProc(t *testing.T, pid int, environ, exeTarget, binaryContent string) (procRoot, containerID string) {
	t.Helper()

	procRoot = t.TempDir()
	containerID = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

	pidDir := filepath.Join(procRoot, strconv.Itoa(pid))
	require.NoError(t, os.MkdirAll(pidDir, 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(pidDir, "comm"), []byte("node\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(pidDir, "cmdline"), []byte("node\x00server.js\x00"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(pidDir, "cgroup"),
		[]byte("0::/kubepods/burstable/pod123/cri-containerd-"+containerID+".scope\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(pidDir, "status"), []byte("NSpid:\t"+strconv.Itoa(pid)+"\t7\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(pidDir, "environ"), []byte(environ), 0o644))

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
	procRoot, containerID := buildFakeNodeProc(t, 4242, "PATH=/usr/bin\x00", "/usr/local/bin/node", binary)

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
	procRoot, _ := buildFakeNodeProc(t, 4242, "PATH=/usr/bin\x00", "/usr/local/bin/node", binary)

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
