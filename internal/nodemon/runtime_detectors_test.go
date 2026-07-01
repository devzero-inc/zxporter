package nodemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writePidFile writes a file into a fake /proc/<pid> dir.
func writePidFile(t *testing.T, pidDir, name string, content []byte) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(pidDir, name), content, 0o644))
}

func TestResolveDotnetVersion(t *testing.T) {
	t.Run("from env", func(t *testing.T) {
		pidDir := t.TempDir()
		writePidFile(t, pidDir, "environ", []byte("DOTNET_VERSION=8.0.11\x00PATH=/usr/bin\x00"))
		v, src := resolveDotnetVersion(pidDir)
		assert.Equal(t, "8.0.11", v)
		assert.Equal(t, runtimeVersionSourceEnv, src)
	})

	t.Run("from maps", func(t *testing.T) {
		pidDir := t.TempDir()
		writePidFile(t, pidDir, "environ", []byte("PATH=/usr/bin\x00"))
		writePidFile(t, pidDir, "maps", []byte(
			"7f0000000000-7f0000001000 r-xp 00000000 00:00 1 /usr/share/dotnet/shared/Microsoft.NETCore.App/9.0.4/libcoreclr.so\n"))
		v, src := resolveDotnetVersion(pidDir)
		assert.Equal(t, "9.0.4", v)
		assert.Equal(t, runtimeVersionSourceMaps, src)
	})

	t.Run("unresolvable", func(t *testing.T) {
		pidDir := t.TempDir()
		v, src := resolveDotnetVersion(pidDir)
		assert.Empty(t, v)
		assert.Empty(t, src)
	})
}

func TestResolveGoVersion_FromTestBinaryBuildInfo(t *testing.T) {
	pidDir := t.TempDir()
	self, err := os.Executable()
	require.NoError(t, err)
	require.NoError(t, os.Symlink(self, filepath.Join(pidDir, "exe")))

	v, src := resolveGoVersion(pidDir)
	assert.NotEmpty(t, v)
	assert.NotContains(t, v, "go", "version should have the go prefix stripped")
	assert.Equal(t, runtimeVersionSourceBuildInfo, src)
}

func TestResolvePythonVersion(t *testing.T) {
	t.Run("from env", func(t *testing.T) {
		pidDir := t.TempDir()
		writePidFile(t, pidDir, "environ", []byte("PYTHON_VERSION=3.12.4\x00"))
		v, src := resolvePythonVersion(pidDir)
		assert.Equal(t, "3.12.4", v)
		assert.Equal(t, runtimeVersionSourceEnv, src)
	})

	t.Run("from exe path", func(t *testing.T) {
		pidDir := t.TempDir()
		// Fake target binary under the pid's /root overlay so readlink+join resolves.
		binDir := filepath.Join(pidDir, "root", "usr", "local", "bin")
		require.NoError(t, os.MkdirAll(binDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(binDir, "python3.11"), []byte{}, 0o755))
		require.NoError(t, os.Symlink("/usr/local/bin/python3.11", filepath.Join(pidDir, "exe")))

		v, src := resolvePythonVersion(pidDir)
		assert.Equal(t, "3.11", v)
		assert.Equal(t, runtimeVersionSourceExePath, src)
	})

	t.Run("from comm", func(t *testing.T) {
		pidDir := t.TempDir()
		writePidFile(t, pidDir, "comm", []byte("python3.9\n"))
		v, src := resolvePythonVersion(pidDir)
		assert.Equal(t, "3.9", v)
		assert.Equal(t, runtimeVersionSourceComm, src)
	})
}

func TestResolveRubyVersion(t *testing.T) {
	t.Run("from env", func(t *testing.T) {
		pidDir := t.TempDir()
		writePidFile(t, pidDir, "environ", []byte("RUBY_VERSION=3.3.4\x00"))
		v, src := resolveRubyVersion(pidDir)
		assert.Equal(t, "3.3.4", v)
		assert.Equal(t, runtimeVersionSourceEnv, src)
	})

	t.Run("from maps", func(t *testing.T) {
		pidDir := t.TempDir()
		writePidFile(t, pidDir, "maps", []byte(
			"7f0000000000-7f0000001000 r-xp 00000000 00:00 1 /usr/local/lib/libruby.so.3.2.4\n"))
		v, src := resolveRubyVersion(pidDir)
		assert.Equal(t, "3.2.4", v)
		assert.Equal(t, runtimeVersionSourceMaps, src)
	})
}

func TestResolveDenoAndBunVersion_FromBinaryScan(t *testing.T) {
	makePidDirWithExeContent := func(t *testing.T, content string) string {
		pidDir := t.TempDir()
		binDir := filepath.Join(pidDir, "root", "usr", "bin")
		require.NoError(t, os.MkdirAll(binDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(binDir, "rt"),
			[]byte("\x00junk\x00"+content+"\x00more"), 0o755))
		require.NoError(t, os.Symlink("/usr/bin/rt", filepath.Join(pidDir, "exe")))
		return pidDir
	}

	t.Run("deno", func(t *testing.T) {
		pidDir := makePidDirWithExeContent(t, "Deno/1.44.2")
		v, src := resolveDenoVersion(pidDir)
		assert.Equal(t, "1.44.2", v)
		assert.Equal(t, runtimeVersionSourceBinaryScan, src)
	})

	t.Run("bun", func(t *testing.T) {
		pidDir := makePidDirWithExeContent(t, "Bun/1.1.20")
		v, src := resolveBunVersion(pidDir)
		assert.Equal(t, "1.1.20", v)
		assert.Equal(t, runtimeVersionSourceBinaryScan, src)
	})

	t.Run("deno env precedence", func(t *testing.T) {
		pidDir := makePidDirWithExeContent(t, "Deno/1.44.2")
		writePidFile(t, pidDir, "environ", []byte("DENO_VERSION=1.45.0\x00"))
		v, src := resolveDenoVersion(pidDir)
		assert.Equal(t, "1.45.0", v)
		assert.Equal(t, runtimeVersionSourceEnv, src)
	})
}

func TestScanFileSubmatch_MatchSpanningChunkBoundary(t *testing.T) {
	// Place the match straddling the 4MiB chunk boundary to exercise the
	// carried-overlap path.
	const chunkSize = 4 << 20
	path := filepath.Join(t.TempDir(), "bin")
	content := make([]byte, 0, chunkSize+64)
	content = append(content, make([]byte, chunkSize-5)...)
	content = append(content, []byte("Deno/1.2.3 trailing")...)
	require.NoError(t, os.WriteFile(path, content, 0o644))

	assert.Equal(t, "1.2.3", scanFileSubmatch(path, 64<<20, denoUserAgentRe))
}

func TestScanFileSubmatch_RespectsMaxBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bin")
	content := append(make([]byte, 1<<20), []byte("Deno/1.2.3")...)
	require.NoError(t, os.WriteFile(path, content, 0o644))

	assert.Empty(t, scanFileSubmatch(path, 1<<20, denoUserAgentRe),
		"match beyond maxBytes must not be found")
	assert.Equal(t, "1.2.3", scanFileSubmatch(path, 2<<20, denoUserAgentRe))
}

func TestProbeRuntimeProcess(t *testing.T) {
	t.Run("go binary via buildinfo", func(t *testing.T) {
		pidDir := t.TempDir()
		self, err := os.Executable()
		require.NoError(t, err)
		require.NoError(t, os.Symlink(self, filepath.Join(pidDir, "exe")))
		assert.Equal(t, processKindGo, probeRuntimeProcess(procEntry{PidDir: pidDir}))
	})

	t.Run("script-named python process via exe basename", func(t *testing.T) {
		// gunicorn/celery pattern: comm is the script name, exe is the interpreter.
		pidDir := t.TempDir()
		binDir := filepath.Join(pidDir, "root", "usr", "local", "bin")
		require.NoError(t, os.MkdirAll(binDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(binDir, "python3.12"), []byte{}, 0o755))
		require.NoError(t, os.Symlink("/usr/local/bin/python3.12", filepath.Join(pidDir, "exe")))
		assert.Equal(t, processKindPython, probeRuntimeProcess(procEntry{PidDir: pidDir, Comm: "gunicorn"}))
	})

	t.Run("dotnet apphost via mapped libcoreclr", func(t *testing.T) {
		pidDir := t.TempDir()
		writePidFile(t, pidDir, "maps", []byte(
			"7f0000000000-7f0000001000 r-xp 00000000 00:00 1 /usr/share/dotnet/shared/Microsoft.NETCore.App/8.0.11/libcoreclr.so\n"))
		assert.Equal(t, processKindDotnet, probeRuntimeProcess(procEntry{PidDir: pidDir, Comm: "myapp"}))
	})

	t.Run("non-binary stays unknown", func(t *testing.T) {
		pidDir := t.TempDir()
		writePidFile(t, pidDir, "exe", []byte("just text, not an ELF"))
		assert.Equal(t, processKindUnknown, probeRuntimeProcess(procEntry{PidDir: pidDir}))
	})

	t.Run("missing exe stays unknown", func(t *testing.T) {
		assert.Equal(t, processKindUnknown, probeRuntimeProcess(procEntry{PidDir: t.TempDir()}))
	})
}

func TestNewMemoizedProbe(t *testing.T) {
	makeEntry := func(t *testing.T, target string) procEntry {
		pidDir := t.TempDir()
		require.NoError(t, os.Symlink(target, filepath.Join(pidDir, "exe")))
		return procEntry{PidDir: pidDir, ContainerID: "c1"}
	}

	t.Run("classified result probed once across cycles", func(t *testing.T) {
		calls := 0
		inner := func(procEntry) processKind { calls++; return processKindGo }
		e := makeEntry(t, "/app/mybinary")

		probe, next := newMemoizedProbe(nil, inner)
		assert.Equal(t, processKindGo, probe(e))
		assert.Equal(t, processKindGo, probe(e))   // same cycle
		probe2, _ := newMemoizedProbe(next, inner) // next cycle, seeded
		assert.Equal(t, processKindGo, probe2(e))
		assert.Equal(t, 1, calls)
	})

	t.Run("unknown retried up to cap then pinned", func(t *testing.T) {
		calls := 0
		inner := func(procEntry) processKind { calls++; return processKindUnknown }
		e := makeEntry(t, "/usr/sbin/nginx")

		cache := map[string]probeCacheEntry(nil)
		for cycle := 0; cycle < maxVersionResolveAttempts+3; cycle++ {
			var probe func(procEntry) processKind
			probe, cache = newMemoizedProbe(cache, inner)
			assert.Equal(t, processKindUnknown, probe(e))
		}
		assert.Equal(t, maxVersionResolveAttempts, calls)
	})

	t.Run("different exe in same container probed separately", func(t *testing.T) {
		calls := 0
		inner := func(procEntry) processKind { calls++; return processKindGo }
		probe, _ := newMemoizedProbe(nil, inner)
		assert.Equal(t, processKindGo, probe(makeEntry(t, "/app/one")))
		assert.Equal(t, processKindGo, probe(makeEntry(t, "/app/two")))
		assert.Equal(t, 2, calls)
	})

	t.Run("unreadable exe probed but not cached", func(t *testing.T) {
		calls := 0
		inner := func(procEntry) processKind { calls++; return processKindUnknown }
		e := procEntry{PidDir: t.TempDir(), ContainerID: "c1"} // no exe link
		probe, next := newMemoizedProbe(nil, inner)
		probe(e)
		probe(e)
		assert.Equal(t, 2, calls, "transient unreadable exe must not be memoized")
		assert.Empty(t, next)
	})
}

func TestGraalVMVersionRe(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"GraalVM CE 22.3.1", "22.3.1"},
		{"Oracle GraalVM 21+35.1", ""}, // "Oracle GraalVM" prefix form: version follows directly
		{"GraalVM 21+35.1", "21+35.1"},
		{"nothing here", ""},
	}
	for _, tc := range tests {
		m := graalVMVersionRe.FindStringSubmatch(tc.in)
		if tc.want == "" && tc.in == "Oracle GraalVM 21+35.1" {
			// Documenting current behavior: the "Oracle GraalVM 21+35.1" form DOES
			// match via the bare "GraalVM " prefix.
			require.Len(t, m, 2)
			assert.Equal(t, "21+35.1", m[1])
			continue
		}
		if tc.want == "" {
			assert.Nil(t, m)
			continue
		}
		require.Len(t, m, 2)
		assert.Equal(t, tc.want, m[1])
	}
}
