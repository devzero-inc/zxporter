package nodemon

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var (
	// containerIDRe matches 64-char container IDs in cgroup paths for containerd and docker.
	containerIDRe = regexp.MustCompile(`(?:cri-containerd|docker|containerd)-([a-f0-9]{64})\.scope`)
	// crioRe matches CRI-O container IDs.
	crioRe = regexp.MustCompile(`crio-([a-f0-9]{64})\.scope`)
	// bareCgroupIDRe matches cgroupv2 paths where the last segment is a bare 64-char container ID.
	// Example: .../kubepods-burstable-pod<uid>.slice/<container-id>
	bareCgroupIDRe = regexp.MustCompile(`/([a-f0-9]{64})$`)
)

// procEntry is a process discovered under procRoot that has a resolvable cgroup
// container ID and namespace PID. It is the shared output of walkProcEntries,
// consumed by runtime-specific discovery (JVM, Node.js, ...) to avoid each
// reimplementing the same PID-readdir + cgroup/NSpid/cmdline-read skeleton.
type procEntry struct {
	PidHost     int
	PidDir      string
	Comm        string
	CmdLine     string
	ContainerID string
	PidNS       int
	Kind        processKind
}

// processKind identifies which runtime a discovered process belongs to, so a
// single /proc walk can serve every process-introspection collector (JVM,
// Node.js, ...) instead of each running its own separate walk.
type processKind int

const (
	processKindUnknown processKind = iota
	processKindJava
	processKindNode
	processKindDotnet
	processKindGo
	processKindGraalVM
	processKindPython
	processKindRuby
	processKindDeno
	processKindBun
)

// walkProcEntries scans procRoot (usually "/proc") and returns entries for
// processes whose (comm, cmdline) classify is not processKindUnknown, and which
// resolve to a Kubernetes container cgroup and namespace PID. Returns nil, nil if
// procRoot does not exist (non-Linux hosts).
func walkProcEntries(procRoot string, classify func(comm, cmdline string) processKind) ([]procEntry, error) {
	return walkProcEntriesProbed(procRoot, classify, nil)
}

// walkProcEntriesProbed is walkProcEntries plus an optional probe stage for
// runtimes that cannot be identified from comm/cmdline (Go binaries, GraalVM
// native images — both named after the application). When classify returns
// processKindUnknown and probe is non-nil, the entry's container cgroup and
// NSpid are still resolved first, and probe runs only if both succeed — so the
// (comparatively expensive) executable inspection never runs for host daemons
// or other non-containerized processes. Entries that remain unknown after the
// probe are dropped.
func walkProcEntriesProbed(procRoot string, classify func(comm, cmdline string) processKind, probe func(e procEntry) processKind) ([]procEntry, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", procRoot, err)
	}

	procs := make([]procEntry, 0, 64)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}

		pidDir := filepath.Join(procRoot, e.Name())

		comm := strings.TrimSpace(readProcFile(filepath.Join(pidDir, "comm")))

		// Read null-separated cmdline and convert to space-separated for display / parsing.
		rawCmdline := readProcFile(filepath.Join(pidDir, "cmdline"))
		cmdline := string(bytes.ReplaceAll([]byte(rawCmdline), []byte{0}, []byte{' '}))

		kind := classify(comm, cmdline)
		if kind == processKindUnknown && probe == nil {
			continue
		}

		cgroupContent := readProcFile(filepath.Join(pidDir, "cgroup"))
		containerID, ok := parseCgroupContainerID(cgroupContent)
		if !ok {
			continue
		}

		statusContent := readProcFile(filepath.Join(pidDir, "status"))
		nsPid, ok := parseNSpid(statusContent)
		if !ok {
			continue
		}

		entry := procEntry{
			PidHost:     pid,
			PidDir:      pidDir,
			Comm:        comm,
			CmdLine:     strings.TrimSpace(cmdline),
			ContainerID: containerID,
			Kind:        kind,
			PidNS:       nsPid,
		}

		// Probe stage: only for containerized processes comm/cmdline couldn't
		// classify (see walkProcEntriesProbed doc).
		if entry.Kind == processKindUnknown {
			entry.Kind = probe(entry)
			if entry.Kind == processKindUnknown {
				continue
			}
		}

		procs = append(procs, entry)
	}

	return procs, nil
}

// parseCgroupContainerID extracts a 64-char hex container ID from cgroup file content.
func parseCgroupContainerID(content string) (string, bool) {
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()
		if m := containerIDRe.FindStringSubmatch(line); len(m) == 2 {
			return m[1], true
		}
		if m := crioRe.FindStringSubmatch(line); len(m) == 2 {
			return m[1], true
		}
		if m := bareCgroupIDRe.FindStringSubmatch(line); len(m) == 2 {
			return m[1], true
		}
	}
	return "", false
}

// parseNSpid extracts the innermost (last) NSpid value from /proc/<pid>/status content.
// The last value is the PID as seen from inside the container's pid namespace.
func parseNSpid(content string) (int, bool) {
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "NSpid:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, false
		}
		v, err := strconv.Atoi(fields[len(fields)-1])
		if err != nil {
			return 0, false
		}
		return v, true
	}
	return 0, false
}

// stripContainerIDScheme strips the URL scheme (e.g., "containerd://") from a container ID.
func stripContainerIDScheme(raw string) string {
	if i := strings.LastIndex(raw, "://"); i >= 0 {
		return raw[i+3:]
	}
	return raw
}

// readProcFile reads a /proc pseudo-file with a hard cap, returning "" on any error.
// Errors are expected and normal (process may disappear between readdir and read).
func readProcFile(path string) string {
	const maxProcBytes = 64 << 10 // 64KiB safety cap
	return readFileCapped(path, maxProcBytes)
}

// readFileCapped reads up to maxBytes from path, returning "" on any error. Unlike
// readProcFile (sized for small /proc pseudo-files), this is for larger on-disk
// files (e.g. binaries) where only a bounded prefix needs to be scanned.
func readFileCapped(path string, maxBytes int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() {
		// close errors on a read-only scan are not actionable.
		_ = f.Close()
	}()

	b, err := io.ReadAll(io.LimitReader(f, maxBytes))
	if err != nil {
		return ""
	}
	return string(b)
}

// scanFileSubmatch streams up to maxBytes of path through re in fixed-size
// chunks and returns the first capture group of the first match, or "" if none.
// Unlike readFileCapped, memory use is bounded by the chunk buffer (a few MiB)
// regardless of maxBytes — required for the binary version scans, which read up
// to 64MiB of runtime executables: loading that into memory per scan OOM-kills
// nodemon under its own 256Mi limit when several containers scan in one cycle.
// An overlap window carried between chunks ensures a match spanning a chunk
// boundary is still found.
func scanFileSubmatch(path string, maxBytes int64, re *regexp.Regexp) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() {
		// close errors on a read-only scan are not actionable.
		_ = f.Close()
	}()

	const chunkSize = 4 << 20 // 4MiB reads
	const overlap = 4 << 10   // 4KiB carry-over: far longer than any version string we match
	buf := make([]byte, overlap+chunkSize)

	carry := 0
	var total int64
	for total < maxBytes {
		want := int64(chunkSize)
		if remaining := maxBytes - total; remaining < want {
			want = remaining
		}
		n, err := io.ReadFull(f, buf[carry:int64(carry)+want])
		total += int64(n)
		if n > 0 {
			valid := carry + n
			if m := re.FindSubmatch(buf[:valid]); len(m) == 2 {
				return string(m[1])
			}
			// Carry the tail forward so a boundary-spanning match is seen whole.
			carry = overlap
			if valid < carry {
				carry = valid
			}
			copy(buf, buf[valid-carry:valid])
		}
		if err != nil {
			break
		}
	}
	return ""
}

// readEnvVars reads /proc/<pid>/environ-style NUL-separated key=value content
// and returns the requested keys that are present and non-empty.
func readEnvVars(environPath string, keys ...string) map[string]string {
	raw := readProcFile(environPath)
	if raw == "" {
		return nil
	}

	keySet := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		keySet[k] = struct{}{}
	}

	parts := strings.Split(raw, "\x00")
	out := map[string]string{}
	for _, kv := range parts {
		if kv == "" {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if _, want := keySet[k]; want && strings.TrimSpace(v) != "" {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
