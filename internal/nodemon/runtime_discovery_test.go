package nodemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassifyRuntimeProcess(t *testing.T) {
	tests := []struct {
		name    string
		comm    string
		cmdline string
		want    processKind
	}{
		{"java by comm", "java", "", processKindJava},
		{"java by cmdline", "", "/usr/bin/java -jar app.jar", processKindJava},
		{"node by comm", "node", "", processKindNode},
		{"node by cmdline", "", "/usr/local/bin/node server.js", processKindNode},
		{"neither", "python3", "/usr/bin/python3 app.py", processKindUnknown},
		{"empty", "", "", processKindUnknown},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, classifyRuntimeProcess(tc.comm, tc.cmdline))
		})
	}
}

// buildFakeProcTree constructs a /proc/<pid> directory for either a Java or a
// Node.js process (selected by comm), returning the resulting container ID.
func buildFakeProcTree(t *testing.T, procRoot string, pid int, comm string) (containerID string) {
	t.Helper()

	containerID = fmt.Sprintf("%064x", pid)

	pidDir := filepath.Join(procRoot, strconv.Itoa(pid))
	require.NoError(t, os.MkdirAll(pidDir, 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(pidDir, "comm"), []byte(comm+"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(pidDir, "cmdline"), []byte(comm+"\x00app\x00"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(pidDir, "cgroup"),
		[]byte("0::/kubepods/burstable/pod123/cri-containerd-"+containerID+".scope\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(pidDir, "status"), []byte("NSpid:\t"+strconv.Itoa(pid)+"\t7\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(pidDir, "environ"), []byte("PATH=/usr/bin\x00"), 0o644))

	if comm == javaComm {
		hsperfDir := filepath.Join(pidDir, "root", "tmp", "hsperfdata_root")
		require.NoError(t, os.MkdirAll(hsperfDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(hsperfDir, "7"), []byte{}, 0o644))
	}

	return containerID
}

func TestDiscoverRuntimeProcesses_SingleWalkBucketsBothKinds(t *testing.T) {
	procRoot := t.TempDir()
	javaContainerID := buildFakeProcTree(t, procRoot, 100, javaComm)
	nodeContainerID := buildFakeProcTree(t, procRoot, 200, nodeComm)

	javaProcs, nodeProcs, err := discoverRuntimeProcesses(procRoot)
	require.NoError(t, err)

	require.Len(t, javaProcs, 1)
	assert.Equal(t, javaContainerID, javaProcs[0].ContainerID)

	require.Len(t, nodeProcs, 1)
	assert.Equal(t, nodeContainerID, nodeProcs[0].ContainerID)
}

func TestDiscoverRuntimeProcesses_NoMatches(t *testing.T) {
	procRoot := t.TempDir()
	javaProcs, nodeProcs, err := discoverRuntimeProcesses(procRoot)
	require.NoError(t, err)
	assert.Empty(t, javaProcs)
	assert.Empty(t, nodeProcs)
}
