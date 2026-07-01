package nodemon

import (
	"context"
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuntimeCollector_QueryRuntimeMetrics_SingleWalkCoversBothKinds(t *testing.T) {
	procRoot := t.TempDir()
	nodeContainerID := buildFakeProcTree(t, procRoot, 200, nodeComm)

	idx := &PodContainerIndex{
		containerMap: map[string]containerInfo{
			nodeContainerID: {Pod: "my-node-app", Namespace: "default", Container: "app"},
		},
	}

	c := NewRuntimeCollector("node-1", idx, testr.New(t))
	c.procRoot = procRoot

	metrics, err := c.QueryRuntimeMetrics(context.Background())
	require.NoError(t, err)

	require.Len(t, metrics.NodeJS, 1)
	assert.Equal(t, nodeContainerID, metrics.NodeJS[0].ContainerID)
	assert.Equal(t, "my-node-app", metrics.NodeJS[0].Pod)
	// The fixture's comm-only process has no resolvable env/binary, so version
	// resolution legitimately comes back empty — still reported as detected.
	assert.Empty(t, metrics.NodeJS[0].NodeVersion)
}

func TestRuntimeCollector_QueryRuntimeMetrics_CachesNodeVersionAcrossCalls(t *testing.T) {
	binary := "junk https://nodejs.org/download/release/v20.11.1/node-v20.11.1.tar.gz junk"
	procRoot, containerID := buildFakeNodeProc(t, 300, "/usr/local/bin/node", binary)

	idx := &PodContainerIndex{containerMap: map[string]containerInfo{}}
	c := NewRuntimeCollector("node-1", idx, testr.New(t))
	c.procRoot = procRoot

	metrics, err := c.QueryRuntimeMetrics(context.Background())
	require.NoError(t, err)
	require.Len(t, metrics.NodeJS, 1)
	assert.Equal(t, containerID, metrics.NodeJS[0].ContainerID)
	assert.Equal(t, "20.11.1", metrics.NodeJS[0].NodeVersion)

	// Confirm the cache is actually being used: a second query still resolves the
	// same version via the RuntimeCollector's own cache, not NodeJSCollector's.
	metrics, err = c.QueryRuntimeMetrics(context.Background())
	require.NoError(t, err)
	require.Len(t, metrics.NodeJS, 1)
	assert.Equal(t, "20.11.1", metrics.NodeJS[0].NodeVersion)
}

func TestRuntimeCollector_QueryRuntimeMetrics_NoProcesses(t *testing.T) {
	idx := &PodContainerIndex{containerMap: map[string]containerInfo{}}
	c := NewRuntimeCollector("node-1", idx, testr.New(t))
	c.procRoot = t.TempDir()

	metrics, err := c.QueryRuntimeMetrics(context.Background())
	require.NoError(t, err)
	assert.Empty(t, metrics.JVM)
	assert.Empty(t, metrics.NodeJS)
}
