package health

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// defaultCgroupRoot is where the kernel mounts cgroupfs inside a container.
const defaultCgroupRoot = "/sys/fs/cgroup"

const (
	cgroupV2ControllersFile = "cgroup.controllers" // presence marks the unified (v2) hierarchy
	cgroupV2CurrentFile     = "memory.current"
	cgroupV2MaxFile         = "memory.max" // literal "max" means no limit configured

	cgroupV1MemoryDir = "memory"
	cgroupV1UsageFile = "memory.usage_in_bytes"
	cgroupV1LimitFile = "memory.limit_in_bytes"

	// cgroupV1UnlimitedSentinel is the value memory.limit_in_bytes reports when no
	// limit is set (typically 9223372036854771712 — math.MaxInt64 rounded down to
	// the kernel's page size). Treat anything at or above it as "unlimited" rather
	// than matching one exact constant, since the low bits vary by page size.
	cgroupV1UnlimitedSentinel = uint64(1) << 62
)

// cgroupMemoryUsage is a single usage/limit reading from the cgroup filesystem.
type cgroupMemoryUsage struct {
	UsageBytes uint64
	LimitBytes uint64
}

// readCgroupMemory reads current memory usage and the configured limit from the
// cgroup filesystem rooted at root, auto-detecting v1 vs v2. ok is false when no
// limit is configured (cgroup v1's huge sentinel value, cgroup v2's literal
// "max") — callers should skip the pressure check rather than compute a
// percentage against a meaningless denominator.
func readCgroupMemory(root string) (usage cgroupMemoryUsage, ok bool, err error) {
	if isCgroupV2(root) {
		return readCgroupV2Memory(root)
	}
	return readCgroupV1Memory(root)
}

// isCgroupV2 detects the unified hierarchy by the presence of cgroup.controllers,
// which only exists under cgroup v2.
func isCgroupV2(root string) bool {
	_, err := os.Stat(filepath.Join(root, cgroupV2ControllersFile))
	return err == nil
}

func readCgroupV2Memory(root string) (cgroupMemoryUsage, bool, error) {
	dir := resolveCgroupDir(root, false)

	usageBytes, err := readUintFile(filepath.Join(dir, cgroupV2CurrentFile))
	if err != nil {
		return cgroupMemoryUsage{}, false, err
	}

	rawLimit, err := readTrimmedFile(filepath.Join(dir, cgroupV2MaxFile))
	if err != nil {
		return cgroupMemoryUsage{}, false, err
	}
	if rawLimit == "max" {
		return cgroupMemoryUsage{}, false, nil
	}
	limitBytes, err := strconv.ParseUint(rawLimit, 10, 64)
	if err != nil {
		return cgroupMemoryUsage{}, false, fmt.Errorf("parsing %s: %w", cgroupV2MaxFile, err)
	}

	return cgroupMemoryUsage{UsageBytes: usageBytes, LimitBytes: limitBytes}, true, nil
}

func readCgroupV1Memory(root string) (cgroupMemoryUsage, bool, error) {
	dir := resolveCgroupDir(filepath.Join(root, cgroupV1MemoryDir), true)

	usageBytes, err := readUintFile(filepath.Join(dir, cgroupV1UsageFile))
	if err != nil {
		return cgroupMemoryUsage{}, false, err
	}
	limitBytes, err := readUintFile(filepath.Join(dir, cgroupV1LimitFile))
	if err != nil {
		return cgroupMemoryUsage{}, false, err
	}
	if limitBytes >= cgroupV1UnlimitedSentinel {
		return cgroupMemoryUsage{}, false, nil
	}

	return cgroupMemoryUsage{UsageBytes: usageBytes, LimitBytes: limitBytes}, true, nil
}

// resolveCgroupDir returns the directory to actually read cgroup files from.
// subsystemRoot is where the subsystem is mounted (e.g. "/sys/fs/cgroup" for
// v2, "/sys/fs/cgroup/memory" for v1).
//
// Kubernetes containers see one of two mount shapes depending on the
// container runtime and cgroup driver: either subsystemRoot is already
// bind-mounted to this container's own cgroup (cgroupns=private, the v2
// default since Kubernetes 1.22 and common for v1 too) — reading
// subsystemRoot directly is correct — or subsystemRoot exposes the full host
// hierarchy (cgroupns=host, still seen with older v1 setups), in which case
// the root-level files are the host's aggregate figures, not this
// container's: memory.limit_in_bytes at the true root reads as "unlimited",
// silently disabling the whole pressure check on exactly the clusters most
// likely to need it. /proc/self/cgroup records this process's own path
// within each hierarchy regardless of which shape applies, so join it onto
// subsystemRoot and prefer that when it resolves to a real path — falling
// back to subsystemRoot whenever the join doesn't exist (already-scoped
// mount, unreadable/unparseable /proc/self/cgroup, or a test fixture with no
// real cgroup filesystem at all).
func resolveCgroupDir(subsystemRoot string, v1MemorySubsystem bool) string {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return subsystemRoot
	}

	relPath := ""
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		hierarchyID, controllers, path := parts[0], parts[1], parts[2]
		if v1MemorySubsystem {
			for _, c := range strings.Split(controllers, ",") {
				if c == "memory" {
					relPath = path
				}
			}
		} else if hierarchyID == "0" && controllers == "" {
			relPath = path
		}
	}

	if relPath == "" || relPath == "/" {
		return subsystemRoot
	}
	candidate := filepath.Join(subsystemRoot, relPath)
	if _, err := os.Stat(candidate); err != nil {
		return subsystemRoot
	}
	return candidate
}

func readUintFile(path string) (uint64, error) {
	s, err := readTrimmedFile(path)
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing %s: %w", path, err)
	}
	return v, nil
}

func readTrimmedFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
