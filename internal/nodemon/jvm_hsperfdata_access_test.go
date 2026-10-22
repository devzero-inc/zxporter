package nodemon

import (
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// skipIfRoot skips tests that rely on chmod 000 denying access: root bypasses
// file mode checks, so the denial cannot be reproduced there.
func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod 000 does not deny access")
	}
}

// chmodRestored sets mode on path and restores 0o755 on cleanup so
// t.TempDir can remove the tree.
func chmodRestored(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	require.NoError(t, os.Chmod(path, mode))
	t.Cleanup(func() { _ = os.Chmod(path, 0o755) })
}

// capturingLogger returns a logger that records every Info message.
func capturingLogger() (logr.Logger, func() []string) {
	var mu sync.Mutex
	var lines []string
	log := funcr.New(func(prefix, args string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, prefix+" "+args)
	}, funcr.Options{})
	return log, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), lines...)
	}
}

func TestFindHsperfdata_PermissionDeniedVsNotFound(t *testing.T) {
	const nsPid = 7
	tests := []struct {
		name string
		// setup builds <pidDir>/root as needed and returns nothing.
		setup      func(t *testing.T, pidDir string)
		needsChmod bool
		wantFound  bool
		wantDenied bool
	}{
		{
			name: "found",
			setup: func(t *testing.T, pidDir string) {
				dir := filepath.Join(pidDir, "root", "tmp", "hsperfdata_app")
				require.NoError(t, os.MkdirAll(dir, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(dir, strconv.Itoa(nsPid)), nil, 0o644))
			},
			wantFound: true,
		},
		{
			name: "no /tmp is not found, not denied",
			setup: func(t *testing.T, pidDir string) {
				require.NoError(t, os.MkdirAll(filepath.Join(pidDir, "root"), 0o755))
			},
		},
		{
			name: "empty /tmp is not found, not denied",
			setup: func(t *testing.T, pidDir string) {
				require.NoError(t, os.MkdirAll(filepath.Join(pidDir, "root", "tmp"), 0o755))
			},
		},
		{
			name: "unreadable /tmp is denied",
			setup: func(t *testing.T, pidDir string) {
				tmp := filepath.Join(pidDir, "root", "tmp")
				dir := filepath.Join(tmp, "hsperfdata_app")
				require.NoError(t, os.MkdirAll(dir, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(dir, strconv.Itoa(nsPid)), nil, 0o644))
				chmodRestored(t, tmp, 0)
			},
			needsChmod: true,
			wantDenied: true,
		},
		{
			name: "unsearchable hsperfdata user dir is denied",
			setup: func(t *testing.T, pidDir string) {
				dir := filepath.Join(pidDir, "root", "tmp", "hsperfdata_app")
				require.NoError(t, os.MkdirAll(dir, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(dir, strconv.Itoa(nsPid)), nil, 0o644))
				chmodRestored(t, dir, 0)
			},
			needsChmod: true,
			wantDenied: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.needsChmod {
				skipIfRoot(t)
			}
			pidDir := filepath.Join(t.TempDir(), "100")
			tc.setup(t, pidDir)

			path, err := findHsperfdata(pidDir, nsPid)

			if tc.wantFound {
				assert.Equal(t, filepath.Join(pidDir, "root", "tmp", "hsperfdata_app", strconv.Itoa(nsPid)), path)
			} else {
				// A denial must not change the path result.
				assert.Empty(t, path)
			}
			if tc.wantDenied {
				assert.ErrorIs(t, err, fs.ErrPermission)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestHsperfDeniedReporter_LogsOncePerContainer(t *testing.T) {
	log, lines := capturingLogger()
	r := newHsperfDeniedReporter(log)

	a := procEntry{PidHost: 1, ContainerID: "aaa"}
	b := procEntry{PidHost: 2, ContainerID: "bbb"}
	noID := procEntry{PidHost: 3}

	assert.True(t, r.report(a, fs.ErrPermission))
	assert.False(t, r.report(a, fs.ErrPermission), "same container must not log again")
	assert.False(t, r.report(procEntry{PidHost: 9, ContainerID: "aaa"}, fs.ErrPermission),
		"another JVM in an already-reported container must not log again")
	assert.True(t, r.report(b, fs.ErrPermission))
	assert.True(t, r.report(noID, fs.ErrPermission))
	assert.False(t, r.report(noID, fs.ErrPermission), "without a container ID, dedupe falls back to host PID")

	assert.Len(t, lines(), 3)

	var nilReporter *hsperfDeniedReporter
	assert.False(t, nilReporter.report(a, fs.ErrPermission), "a nil reporter discards reports")
}

func TestHsperfDeniedReporter_BoundedSeenSet(t *testing.T) {
	r := newHsperfDeniedReporter(logr.Discard())
	for i := range hsperfDeniedReporterMaxSeen + 10 {
		r.report(procEntry{ContainerID: "c" + strconv.Itoa(i)}, fs.ErrPermission)
	}
	assert.LessOrEqual(t, len(r.seen), hsperfDeniedReporterMaxSeen)
}

func TestDiscoverRuntimeProcesses_DeniedHsperfdataIsReportedOnce(t *testing.T) {
	skipIfRoot(t)

	procRoot := t.TempDir()
	containerID := buildFakeProcTree(t, procRoot, 800, javaComm)
	chmodRestored(t, filepath.Join(procRoot, "800", "root", "tmp"), 0)

	log, lines := capturingLogger()
	denied := newHsperfDeniedReporter(log)

	for range 2 { // two collection ticks
		javaProcs, runtimeProcs, err := discoverRuntimeProcesses(procRoot, probeRuntimeProcess, denied)
		require.NoError(t, err)

		// Return behaviour is unchanged: no heap metrics, but the JVM is still
		// reported as a Java runtime.
		assert.Empty(t, javaProcs)
		require.Len(t, runtimeProcs, 1)
		assert.Equal(t, runtimeNameJava, runtimeProcs[0].Runtime)
	}

	got := lines()
	require.Len(t, got, 1, "a denial is logged once, not once per tick")
	assert.True(t, strings.Contains(got[0], containerID), got[0])
	assert.True(t, strings.Contains(got[0], "Permission denied"), got[0])
}
