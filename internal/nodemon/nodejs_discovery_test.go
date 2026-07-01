package nodemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsNodeProcess(t *testing.T) {
	tests := []struct {
		comm    string
		cmdline string
		want    bool
	}{
		{"node", "", true},
		{"node", "/usr/local/bin/node server.js", true},
		{"", "/usr/local/bin/node server.js", true},
		{"", "/usr/local/bin/node18 server.js", false},
		{"python3", "/usr/bin/python3 app.py", false},
		{"", "", false},
		{"nodejs", "/usr/bin/nodejs app.js", false},
	}

	for _, tc := range tests {
		t.Run(tc.comm+"|"+tc.cmdline, func(t *testing.T) {
			assert.Equal(t, tc.want, isNodeProcess(tc.comm, tc.cmdline))
		})
	}
}

// newFakePidDir builds a /proc/<pid>-like directory: an "environ" file with the
// given NUL-separated content, and (if exeTarget is non-empty) an "exe" symlink
// pointing at exeTarget with binaryContent written under root/<exeTarget>.
func newFakePidDir(t *testing.T, environ string, exeTarget string, binaryContent string) string {
	t.Helper()
	pidDir := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(pidDir, "environ"), []byte(environ), 0o644))

	if exeTarget == "" {
		return pidDir
	}

	require.NoError(t, os.Symlink(exeTarget, filepath.Join(pidDir, "exe")))

	rootPath := filepath.Join(pidDir, "root", exeTarget)
	require.NoError(t, os.MkdirAll(filepath.Dir(rootPath), 0o755))
	require.NoError(t, os.WriteFile(rootPath, []byte(binaryContent), 0o644))

	return pidDir
}

func TestResolveNodeVersion_FromEnv(t *testing.T) {
	pidDir := newFakePidDir(t, "PATH=/usr/bin\x00NODE_VERSION=20.11.1\x00OTHER=x\x00", "", "")

	version, source := resolveNodeVersion(pidDir)
	assert.Equal(t, "20.11.1", version)
	assert.Equal(t, runtimeVersionSourceEnv, source)
}

func TestResolveNodeVersion_FromBinaryScan(t *testing.T) {
	binary := "some junk bytes ... https://nodejs.org/download/release/v18.19.1/node-v18.19.1.tar.gz ... more junk"
	pidDir := newFakePidDir(t, "PATH=/usr/bin\x00", "/usr/local/bin/node", binary)

	version, source := resolveNodeVersion(pidDir)
	assert.Equal(t, "18.19.1", version)
	assert.Equal(t, runtimeVersionSourceBinaryScan, source)
}

func TestResolveNodeVersion_EnvTakesPrecedenceOverBinaryScan(t *testing.T) {
	binary := "https://nodejs.org/download/release/v16.0.0/node-v16.0.0.tar.gz"
	pidDir := newFakePidDir(t, "NODE_VERSION=20.11.1\x00", "/usr/local/bin/node", binary)

	version, source := resolveNodeVersion(pidDir)
	assert.Equal(t, "20.11.1", version, "env var must win over binary scan")
	assert.Equal(t, runtimeVersionSourceEnv, source)
}

func TestResolveNodeVersion_Unknown(t *testing.T) {
	tests := []struct {
		name      string
		environ   string
		exeTarget string
		binary    string
	}{
		{
			name:    "no env, no exe symlink",
			environ: "PATH=/usr/bin\x00",
		},
		{
			name:      "no env, binary lacks release-URL string",
			environ:   "PATH=/usr/bin\x00",
			exeTarget: "/usr/local/bin/node",
			binary:    "not a match here",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pidDir := newFakePidDir(t, tc.environ, tc.exeTarget, tc.binary)

			version, source := resolveNodeVersion(pidDir)
			assert.Empty(t, version)
			assert.Empty(t, source)
		})
	}
}

func TestResolveNodeExePath(t *testing.T) {
	pidDir := t.TempDir()
	require.NoError(t, os.Symlink("/usr/local/bin/node", filepath.Join(pidDir, "exe")))

	assert.Equal(t, filepath.Join(pidDir, "root", "usr", "local", "bin", "node"), resolveExePath(pidDir))
}

func TestResolveNodeExePath_MissingSymlink(t *testing.T) {
	pidDir := t.TempDir()
	assert.Empty(t, resolveExePath(pidDir))
}
