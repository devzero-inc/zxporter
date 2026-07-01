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
		{"dotnet by comm", "dotnet", "", processKindDotnet},
		{"dotnet by cmdline", "", "/usr/bin/dotnet app.dll", processKindDotnet},
		{"python by comm", "python3", "/usr/bin/python3 app.py", processKindPython},
		{"python versioned comm", "python3.11", "", processKindPython},
		{"python by cmdline", "", "/usr/local/bin/python3.12 -m http.server", processKindPython},
		{"ruby by comm", "ruby", "", processKindRuby},
		{"deno by comm", "deno", "", processKindDeno},
		{"bun by comm", "bun", "", processKindBun},
		{"pythonish non-match", "pythonic", "/usr/bin/pythonic", processKindUnknown},
		{"go binary is not cmdline-classifiable", "myapp", "/app/myapp serve", processKindUnknown},
		{"neither", "bash", "/bin/bash -c sleep", processKindUnknown},
		{"empty", "", "", processKindUnknown},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, classifyRuntimeProcess(tc.comm, tc.cmdline))
		})
	}
}

// buildFakeProcTree constructs a /proc/<pid> directory for a process with the
// given comm, returning the resulting container ID.
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

func TestDiscoverRuntimeProcesses_SingleWalkBucketsAllKinds(t *testing.T) {
	procRoot := t.TempDir()
	javaContainerID := buildFakeProcTree(t, procRoot, 100, javaComm)
	nodeContainerID := buildFakeProcTree(t, procRoot, 200, nodeComm)
	pythonContainerID := buildFakeProcTree(t, procRoot, 300, "python3")

	// A containerized process with an app-named comm and an exe symlink to the
	// test binary itself (a Go binary with embedded build info) exercises the
	// probe stage.
	goContainerID := buildFakeProcTree(t, procRoot, 400, "myapp")
	self, err := os.Executable()
	require.NoError(t, err)
	require.NoError(t, os.Symlink(self, filepath.Join(procRoot, "400", "exe")))

	javaProcs, nodeProcs, runtimeProcs, err := discoverRuntimeProcesses(procRoot)
	require.NoError(t, err)

	require.Len(t, javaProcs, 1)
	assert.Equal(t, javaContainerID, javaProcs[0].ContainerID)

	require.Len(t, nodeProcs, 1)
	assert.Equal(t, nodeContainerID, nodeProcs[0].ContainerID)

	require.Len(t, runtimeProcs, 2)
	byRuntime := map[string]RuntimeProcess{}
	for _, p := range runtimeProcs {
		byRuntime[p.Runtime] = p
	}
	require.Contains(t, byRuntime, runtimeNamePython)
	assert.Equal(t, pythonContainerID, byRuntime[runtimeNamePython].ContainerID)
	require.Contains(t, byRuntime, runtimeNameGo)
	assert.Equal(t, goContainerID, byRuntime[runtimeNameGo].ContainerID)
}

func TestDiscoverRuntimeProcesses_UnprobeableUnknownIsDropped(t *testing.T) {
	procRoot := t.TempDir()
	buildFakeProcTree(t, procRoot, 500, "someapp") // no exe symlink: probe fails

	javaProcs, nodeProcs, runtimeProcs, err := discoverRuntimeProcesses(procRoot)
	require.NoError(t, err)
	assert.Empty(t, javaProcs)
	assert.Empty(t, nodeProcs)
	assert.Empty(t, runtimeProcs)
}

func TestDiscoverRuntimeProcesses_NoMatches(t *testing.T) {
	procRoot := t.TempDir()
	javaProcs, nodeProcs, runtimeProcs, err := discoverRuntimeProcesses(procRoot)
	require.NoError(t, err)
	assert.Empty(t, javaProcs)
	assert.Empty(t, nodeProcs)
	assert.Empty(t, runtimeProcs)
}
