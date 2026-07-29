package health

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func TestReadCgroupMemory_V2WithLimit(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, cgroupV2ControllersFile), "cpuset cpu io memory pids\n")
	writeFile(t, filepath.Join(root, cgroupV2CurrentFile), "104857600\n") // 100Mi
	writeFile(t, filepath.Join(root, cgroupV2MaxFile), "209715200\n")     // 200Mi

	usage, ok, err := readCgroupMemory(root)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, uint64(104857600), usage.UsageBytes)
	assert.Equal(t, uint64(209715200), usage.LimitBytes)
}

func TestReadCgroupMemory_V2Unlimited(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, cgroupV2ControllersFile), "cpuset cpu io memory pids\n")
	writeFile(t, filepath.Join(root, cgroupV2CurrentFile), "104857600\n")
	writeFile(t, filepath.Join(root, cgroupV2MaxFile), "max\n")

	_, ok, err := readCgroupMemory(root)
	require.NoError(t, err)
	assert.False(t, ok, "unlimited v2 cgroup should be reported as not ok")
}

func TestReadCgroupMemory_V1WithLimit(t *testing.T) {
	root := t.TempDir()
	memDir := filepath.Join(root, cgroupV1MemoryDir)
	require.NoError(t, os.MkdirAll(memDir, 0o755))
	writeFile(t, filepath.Join(memDir, cgroupV1UsageFile), "52428800\n")  // 50Mi
	writeFile(t, filepath.Join(memDir, cgroupV1LimitFile), "104857600\n") // 100Mi

	usage, ok, err := readCgroupMemory(root)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, uint64(52428800), usage.UsageBytes)
	assert.Equal(t, uint64(104857600), usage.LimitBytes)
}

func TestReadCgroupMemory_V1Unlimited(t *testing.T) {
	root := t.TempDir()
	memDir := filepath.Join(root, cgroupV1MemoryDir)
	require.NoError(t, os.MkdirAll(memDir, 0o755))
	writeFile(t, filepath.Join(memDir, cgroupV1UsageFile), "52428800\n")
	// Real-world sentinel reported by the kernel for "no limit set".
	writeFile(t, filepath.Join(memDir, cgroupV1LimitFile), "9223372036854771712\n")

	_, ok, err := readCgroupMemory(root)
	require.NoError(t, err)
	assert.False(t, ok, "unlimited v1 cgroup should be reported as not ok")
}

func TestReadCgroupMemory_MissingFiles(t *testing.T) {
	root := t.TempDir() // no cgroup.controllers, no memory/ dir -> v1 path, missing files

	_, _, err := readCgroupMemory(root)
	assert.Error(t, err)
}

func TestReadCgroupMemory_V2MalformedCurrent(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, cgroupV2ControllersFile), "cpuset cpu io memory pids\n")
	writeFile(t, filepath.Join(root, cgroupV2CurrentFile), "not-a-number\n")
	writeFile(t, filepath.Join(root, cgroupV2MaxFile), "209715200\n")

	_, _, err := readCgroupMemory(root)
	assert.Error(t, err)
}

func TestIsCgroupV2(t *testing.T) {
	v2Root := t.TempDir()
	writeFile(t, filepath.Join(v2Root, cgroupV2ControllersFile), "memory\n")
	assert.True(t, isCgroupV2(v2Root))

	v1Root := t.TempDir()
	assert.False(t, isCgroupV2(v1Root))
}

func TestReadCgroupMemory_SentinelBoundary(t *testing.T) {
	// Sanity-check the >= comparison isn't accidentally excluding realistic
	// large-but-finite limits (e.g. a node with hundreds of GiB of RAM).
	root := t.TempDir()
	memDir := filepath.Join(root, cgroupV1MemoryDir)
	require.NoError(t, os.MkdirAll(memDir, 0o755))
	writeFile(t, filepath.Join(memDir, cgroupV1UsageFile), "1024\n")
	// 100Gi, comfortably below the 2^62 sentinel threshold.
	writeFile(t, filepath.Join(memDir, cgroupV1LimitFile), strconv.FormatUint(100*1024*1024*1024, 10))

	usage, ok, err := readCgroupMemory(root)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, uint64(100*1024*1024*1024), usage.LimitBytes)
}
