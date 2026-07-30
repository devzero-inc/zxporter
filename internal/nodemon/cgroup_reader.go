package nodemon

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
)

const (
	// defaultCgroupRoot is the conventional mount point of the cgroup filesystem.
	defaultCgroupRoot = "/sys/fs/cgroup"
	// maxCgroupFileBytes caps reads of cgroup pseudo-files. They are tiny (a few
	// lines); the cap only guards against a pathological/proc-like blowup.
	maxCgroupFileBytes = 64 << 10
)

// bareCgroupDirRe matches a cgroup directory whose base name is a bare 64-char
// container ID. Some kubelet/runtime layouts place the container cgroup as
// `.../pod<uid>.slice/<container-id>` with no `cri-*`/`crio-` scope wrapper, so
// the scope regexes in proc_walk.go (which require a `.scope` suffix) miss it.
var bareCgroupDirRe = regexp.MustCompile(`^[a-f0-9]{64}$`)

// CgroupSignals holds the runtime-agnostic cgroup counters for a single
// container. All are cumulative kernel counters (a window rate is last-first),
// matching how the metrics pipeline treats counter columns.
type CgroupSignals struct {
	CfsPeriods             int64
	CfsThrottledPeriods    int64
	CfsThrottledUsec       int64
	MemoryEventsMax        int64
	CPUPressureSomeUsec    int64
	MemoryPressureSomeUsec int64
	MemoryPressureFullUsec int64
}

// CgroupReader reads cgroup counters directly from the cgroup filesystem
// (mounted read-only into the nodemon pod) and resolves each container cgroup to
// its {namespace,pod,container} identity via the shared PodContainerIndex.
//
// The identity join does NOT require hostPID: the index is populated from
// pod.Status.ContainerStatuses via a node-scoped Pod informer, not from /proc.
//
// A nil reader, or one with a nil index, yields nil from Collect — callers treat
// that as "no cgroup signals this cycle" and emit zeros.
type CgroupReader struct {
	root  string
	index *PodContainerIndex
	log   logr.Logger
}

// NewCgroupReader creates a CgroupReader rooted at the conventional cgroup mount
// point. index resolves container IDs to pod identity and must be started by the
// caller.
func NewCgroupReader(index *PodContainerIndex, log logr.Logger) *CgroupReader {
	return &CgroupReader{
		root:  defaultCgroupRoot,
		index: index,
		log:   log.WithName("cgroup-reader"),
	}
}

// Collect walks the cgroup filesystem and returns per-container signals keyed by
// "namespace/pod/container" — the same key nodemon's cAdvisor/GPU indexes use, so
// the exporter can merge them by lookup. Returns nil when the reader or its index
// is unavailable, or the cgroup root is absent (non-Linux hosts).
func (r *CgroupReader) Collect() map[string]CgroupSignals {
	if r == nil || r.index == nil {
		return nil
	}
	if _, err := os.Stat(r.root); err != nil {
		return nil
	}
	if r.isV2() {
		return r.collectV2()
	}
	return r.collectV1()
}

// isV2 reports whether the root is a cgroup v2 unified hierarchy. The
// cgroup.controllers file exists only on v2.
func (r *CgroupReader) isV2() bool {
	_, err := os.Stat(filepath.Join(r.root, "cgroup.controllers"))
	return err == nil
}

// keyFor resolves a hex container ID to the "namespace/pod/container" merge key.
func (r *CgroupReader) keyFor(containerID string) (string, bool) {
	info, ok := r.index.Lookup(containerID)
	if !ok {
		return "", false
	}
	return info.Namespace + "/" + info.Pod + "/" + info.Container, true
}

// collectV2 walks the unified hierarchy. Each source file lives in the container's
// own cgroup directory, so a single walk covers all seven signals.
func (r *CgroupReader) collectV2() map[string]CgroupSignals {
	out := make(map[string]CgroupSignals)

	// Anchor at kubepods.slice when present to skip host/system cgroups; fall
	// back to the whole root for non-systemd layouts.
	walkRoot := r.root
	if kp := filepath.Join(r.root, "kubepods.slice"); dirExists(kp) {
		walkRoot = kp
	}

	_ = filepath.WalkDir(walkRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d == nil || !d.IsDir() {
			return nil
		}
		id, ok := parseCgroupDirContainerID(d.Name())
		if !ok {
			return nil
		}
		// A container cgroup has no nested containers — stop descending whether or
		// not we could resolve it (unresolved = pause/sandbox or a just-started pod).
		if key, ok := r.keyFor(id); ok {
			out[key] = r.readV2Signals(path)
		}
		return filepath.SkipDir
	})

	return out
}

// readV2Signals reads and parses the seven counters from one container's v2
// cgroup directory. Missing files (older kernels lacking PSI) parse to zero.
func (r *CgroupReader) readV2Signals(dir string) CgroupSignals {
	var s CgroupSignals
	s.CfsPeriods, s.CfsThrottledPeriods, s.CfsThrottledUsec =
		parseCPUStat(readFileCapped(filepath.Join(dir, "cpu.stat"), maxCgroupFileBytes))
	s.MemoryEventsMax =
		parseMemoryEventsMax(readFileCapped(filepath.Join(dir, "memory.events"), maxCgroupFileBytes))
	s.CPUPressureSomeUsec, _ =
		parsePSITotals(readFileCapped(filepath.Join(dir, "cpu.pressure"), maxCgroupFileBytes))
	s.MemoryPressureSomeUsec, s.MemoryPressureFullUsec =
		parsePSITotals(readFileCapped(filepath.Join(dir, "memory.pressure"), maxCgroupFileBytes))
	return s
}

// collectV1 is a best-effort fallback for legacy cgroup v1 hosts. CFS throttle
// counters come from the cpu controller and the "hit the ceiling" counter from
// memory.failcnt under the memory controller (the v1 equivalent of v2's
// memory.events:max). PSI does not exist on v1, so pressure stays zero.
func (r *CgroupReader) collectV1() map[string]CgroupSignals {
	out := make(map[string]CgroupSignals)

	if cpuBase := firstExistingDir(
		filepath.Join(r.root, "cpu"),
		filepath.Join(r.root, "cpu,cpuacct"),
	); cpuBase != "" {
		r.walkV1Containers(cpuBase, func(key, dir string) {
			s := out[key]
			s.CfsPeriods, s.CfsThrottledPeriods, s.CfsThrottledUsec =
				parseCPUStat(readFileCapped(filepath.Join(dir, "cpu.stat"), maxCgroupFileBytes))
			out[key] = s
		})
	}

	if memBase := filepath.Join(r.root, "memory"); dirExists(memBase) {
		r.walkV1Containers(memBase, func(key, dir string) {
			s := out[key]
			s.MemoryEventsMax =
				parseSingleInt(readFileCapped(filepath.Join(dir, "memory.failcnt"), maxCgroupFileBytes))
			out[key] = s
		})
	}

	return out
}

// walkV1Containers walks a single v1 controller hierarchy and invokes fn for each
// resolvable container cgroup directory.
func (r *CgroupReader) walkV1Containers(base string, fn func(key, dir string)) {
	_ = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d == nil || !d.IsDir() {
			return nil
		}
		id, ok := parseCgroupDirContainerID(d.Name())
		if !ok {
			return nil
		}
		if key, ok := r.keyFor(id); ok {
			fn(key, path)
		}
		return filepath.SkipDir
	})
}

// parseCgroupDirContainerID extracts a 64-char hex container ID from a cgroup
// directory base name, handling systemd scope names (cri-containerd/docker/crio)
// and bare-ID layouts.
func parseCgroupDirContainerID(base string) (string, bool) {
	if m := containerIDRe.FindStringSubmatch(base); len(m) == 2 {
		return m[1], true
	}
	if m := crioRe.FindStringSubmatch(base); len(m) == 2 {
		return m[1], true
	}
	if bareCgroupDirRe.MatchString(base) {
		return base, true
	}
	return "", false
}

// parseCPUStat extracts the CFS counters from cpu.stat content. Handles both v2
// (throttled_usec, already microseconds) and v1 (throttled_time, nanoseconds).
func parseCPUStat(content string) (periods, throttledPeriods, throttledUsec int64) {
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		key, val, ok := splitStatLine(sc.Text())
		if !ok {
			continue
		}
		switch key {
		case "nr_periods":
			periods = val
		case "nr_throttled":
			throttledPeriods = val
		case "throttled_usec": // cgroup v2: microseconds
			throttledUsec = val
		case "throttled_time": // cgroup v1: nanoseconds
			throttledUsec = val / 1000
		}
	}
	return periods, throttledPeriods, throttledUsec
}

// parseMemoryEventsMax reads the "max" counter from memory.events content — the
// number of times allocation was throttled against memory.max.
func parseMemoryEventsMax(content string) int64 {
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		if key, val, ok := splitStatLine(sc.Text()); ok && key == "max" {
			return val
		}
	}
	return 0
}

// parsePSITotals extracts the cumulative "some"/"full" stall totals (in
// microseconds) from a PSI file (cpu.pressure / memory.pressure). cpu.pressure
// may omit the "full" line on some kernels; fullUsec then stays zero.
func parsePSITotals(content string) (someUsec, fullUsec int64) {
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "some":
			someUsec = psiTotal(fields)
		case "full":
			fullUsec = psiTotal(fields)
		}
	}
	return someUsec, fullUsec
}

// psiTotal pulls the value of the "total=<usec>" field out of a PSI line's
// fields, returning 0 if absent or unparseable.
func psiTotal(fields []string) int64 {
	for _, f := range fields {
		if rest, ok := strings.CutPrefix(f, "total="); ok {
			if v, err := strconv.ParseInt(rest, 10, 64); err == nil {
				return v
			}
			return 0
		}
	}
	return 0
}

// splitStatLine parses a "key value" line into its key and int64 value.
func splitStatLine(line string) (string, int64, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", 0, false
	}
	v, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return fields[0], v, true
}

// parseSingleInt parses a cgroup file whose entire content is one integer
// (e.g. memory.failcnt), returning 0 on any error.
func parseSingleInt(content string) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(content), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// dirExists reports whether path exists and is a directory.
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// firstExistingDir returns the first path that exists and is a directory, or "".
func firstExistingDir(paths ...string) string {
	for _, p := range paths {
		if dirExists(p) {
			return p
		}
	}
	return ""
}
