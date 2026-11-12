package nodemon

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/go-logr/logr"
)

const (
	javaComm    = "java"
	javaBinName = "java"
)

// JavaProcess holds info about a discovered Java process running inside a Kubernetes container.
type JavaProcess struct {
	PidHost     int
	PidNS       int
	ContainerID string

	CmdLine string
	// EnvJavaOpts includes any env-injected Java options found in /proc/<pid>/environ.
	// Keys are env var names (JAVA_TOOL_OPTIONS, JDK_JAVA_OPTIONS, JAVA_OPTS).
	EnvJavaOpts map[string]string

	HsperfDataPath string
}

// discoverJavaProcesses scans procRoot (usually "/proc") for Java processes
// running inside Kubernetes container cgroups.
// Returns nil, nil if procRoot does not exist (non-Linux hosts). denied, if
// non-nil, is told about JVMs whose hsperfdata could not be read because access
// to the container's /tmp was refused.
func discoverJavaProcesses(procRoot string, denied *hsperfDeniedReporter) ([]JavaProcess, error) {
	entries, err := walkProcEntries(procRoot, classifyJavaOnly)
	if err != nil {
		return nil, err
	}

	procs := make([]JavaProcess, 0, len(entries))
	for _, e := range entries {
		if jp, ok := javaProcessFromEntry(e, denied); ok {
			procs = append(procs, jp)
		}
	}

	return procs, nil
}

// classifyJavaOnly is a single-runtime classifier for callers that only care
// about Java processes (the legacy /container/jvm-metrics path).
func classifyJavaOnly(comm, cmdline string) processKind {
	if isJavaProcess(comm, cmdline) {
		return processKindJava
	}
	return processKindUnknown
}

// javaProcessFromEntry builds a JavaProcess from a procEntry already classified
// as processKindJava, reading the JVM-specific bits (hsperfdata path, env-injected
// options) that walkProcEntries doesn't itself resolve. Returns ok=false if no
// hsperfdata file is found (e.g. -XX:-UsePerfData, or the process just exited).
// A permission denial on the container's /tmp is also ok=false, and is passed
// to denied (which may be nil) so it does not pass silently as "no hsperfdata".
func javaProcessFromEntry(e procEntry, denied *hsperfDeniedReporter) (JavaProcess, bool) {
	hsperfPath, accessErr := findHsperfdata(e.PidDir, e.PidNS)
	if hsperfPath == "" {
		if accessErr != nil {
			denied.report(e, accessErr)
		}
		return JavaProcess{}, false
	}

	// Also capture env-injected JVM options (common in k8s via JAVA_TOOL_OPTIONS,
	// JDK_JAVA_OPTIONS, JAVA_OPTS). These do NOT appear in /proc/<pid>/cmdline.
	envJavaOpts := readEnvVars(filepath.Join(e.PidDir, "environ"), "JAVA_TOOL_OPTIONS", "JDK_JAVA_OPTIONS", "JAVA_OPTS")

	return JavaProcess{
		PidHost:        e.PidHost,
		PidNS:          e.PidNS,
		ContainerID:    e.ContainerID,
		CmdLine:        e.CmdLine,
		EnvJavaOpts:    envJavaOpts,
		HsperfDataPath: hsperfPath,
	}, true
}

// isJavaProcess returns true if the process comm is "java" or its first cmdline
// argument is a binary named "java".
func isJavaProcess(comm, cmdline string) bool {
	if comm == javaComm {
		return true
	}
	parts := strings.Fields(cmdline)
	if len(parts) == 0 {
		return false
	}
	return filepath.Base(parts[0]) == javaBinName
}

// findHsperfdata returns the hsperfdata path for a Java process via its /proc/<pid>/root
// overlay: /proc/<pid>/root/tmp/hsperfdata_*/<nsPid>
// Returns "" if no file is found. The error is non-nil only when the lookup
// was refused (EACCES/EPERM) rather than finding nothing: filepath.Glob drops
// I/O errors, so without this check a denial (for example SELinux MCS
// isolation between containers) is indistinguishable from a JVM that writes
// no hsperfdata. The path result is "" in both cases.
func findHsperfdata(pidDir string, nsPid int) (string, error) {
	tmpDir := filepath.Join(pidDir, "root", "tmp")
	pid := strconv.Itoa(nsPid)
	if matches, err := filepath.Glob(filepath.Join(tmpDir, "hsperfdata_*", pid)); err == nil && len(matches) > 0 {
		return matches[0], nil
	}

	// No match. Re-walk the two directory levels Glob read, keeping only
	// permission errors; anything else (missing /tmp, exited process) is the
	// ordinary "not found".
	f, err := os.Open(tmpDir)
	if err != nil {
		return "", permissionError(err)
	}
	_, err = f.Readdirnames(1)
	_ = f.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return "", permissionError(err)
	}
	userDirs, _ := filepath.Glob(filepath.Join(tmpDir, "hsperfdata_*"))
	for _, dir := range userDirs {
		if _, err := os.Lstat(filepath.Join(dir, pid)); err != nil && errors.Is(err, fs.ErrPermission) {
			return "", err
		}
	}
	return "", nil
}

// permissionError returns err if it is a permission denial, else nil.
func permissionError(err error) error {
	if errors.Is(err, fs.ErrPermission) {
		return err
	}
	return nil
}

// hsperfDeniedReporterMaxSeen bounds the dedupe set. When it fills, the set is
// cleared, so a long-lived nodemon logs a still-denied container again at
// most once per that many distinct containers.
const hsperfDeniedReporterMaxSeen = 1024

// hsperfDeniedReporter logs, once per container, that a JVM's hsperfdata could
// not be read because access to the container's /tmp was refused. Discovery
// runs every collection tick, so logging every occurrence would repeat the same
// line for each JVM every cycle. A nil *hsperfDeniedReporter discards reports.
type hsperfDeniedReporter struct {
	log logr.Logger

	mu   sync.Mutex
	seen map[string]struct{}
}

func newHsperfDeniedReporter(log logr.Logger) *hsperfDeniedReporter {
	return &hsperfDeniedReporter{log: log, seen: make(map[string]struct{})}
}

// report logs err for e unless this container (or, without a container ID,
// this host PID) was already reported. It returns whether it logged.
func (r *hsperfDeniedReporter) report(e procEntry, err error) bool {
	if r == nil {
		return false
	}
	key := e.ContainerID
	if key == "" {
		key = "pid:" + strconv.Itoa(e.PidHost)
	}

	r.mu.Lock()
	if _, ok := r.seen[key]; ok {
		r.mu.Unlock()
		return false
	}
	if len(r.seen) >= hsperfDeniedReporterMaxSeen {
		clear(r.seen)
	}
	r.seen[key] = struct{}{}
	r.mu.Unlock()

	r.log.Info("Permission denied reading a JVM's /tmp through /proc/<pid>/root; no heap metrics for it. "+
		"On SELinux-enforcing nodes nodemon needs seLinuxOptions.level s0:c0.c1023",
		"pid", e.PidHost, "containerID", e.ContainerID, "error", err.Error())
	return true
}

// ParseJVMFlags parses JVM memory and container-awareness flags from a process cmdline string.
//
// NOTE: This is *best-effort* parsing for the few flags we care about for sizing.
func ParseJVMFlags(cmdline string) JVMFlagsExtracted {
	var flags JVMFlagsExtracted
	for _, token := range strings.Fields(cmdline) {
		switch {
		case strings.HasPrefix(token, "-Xms"):
			if v, err := parseMemSize(token[4:]); err == nil {
				flags.XmsBytes = &v
			}
		case strings.HasPrefix(token, "-Xmx"):
			if v, err := parseMemSize(token[4:]); err == nil {
				flags.XmxBytes = &v
			}
		case strings.HasPrefix(token, "-XX:MaxRAMPercentage="):
			s := token[len("-XX:MaxRAMPercentage="):]
			if v, err := strconv.ParseFloat(s, 64); err == nil {
				flags.MaxRamPercentage = &v
			}
		case token == "-XX:+UseContainerSupport":
			t := true
			flags.UseContainerSupport = &t
		case token == "-XX:-UseContainerSupport":
			f := false
			flags.UseContainerSupport = &f
		}
	}
	return flags
}

// ParseJVMFlagsWithSources extracts sizing-related JVM flags and also returns
// where each value came from.
//
// We cannot observe the final JVM argument list directly (env-injected options
// don’t appear in /proc/<pid>/cmdline), so this is best-effort:
//   - cmdline is treated as highest precedence
//   - env vars are applied in the following precedence order:
//     JDK_JAVA_OPTIONS, JAVA_TOOL_OPTIONS, JAVA_OPTS (matching real JVM
//     last-occurrence-wins semantics — see the comment at the application loop)
//
// The returned effectiveCmdline is the cmdline plus the env options appended as
// tokens for observability.
func ParseJVMFlagsWithSources(cmdline string, envJavaOpts map[string]string) (JVMFlagsExtracted, JVMFlagSources, string) {
	flags := JVMFlagsExtracted{}
	src := JVMFlagSources{}

	applyTokens := func(tokens []string, source string) {
		for _, token := range tokens {
			switch {
			case strings.HasPrefix(token, "-Xms"):
				if flags.XmsBytes == nil {
					if v, err := parseMemSize(token[4:]); err == nil {
						flags.XmsBytes = &v
						src.XmsBytes = source
					}
				}
			case strings.HasPrefix(token, "-Xmx"):
				if flags.XmxBytes == nil {
					if v, err := parseMemSize(token[4:]); err == nil {
						flags.XmxBytes = &v
						src.XmxBytes = source
					}
				}
			case strings.HasPrefix(token, "-XX:MaxRAMPercentage="):
				if flags.MaxRamPercentage == nil {
					s := token[len("-XX:MaxRAMPercentage="):]
					if v, err := strconv.ParseFloat(s, 64); err == nil {
						flags.MaxRamPercentage = &v
						src.MaxRamPercentage = source
					}
				}
			case token == "-XX:+UseContainerSupport":
				if flags.UseContainerSupport == nil {
					t := true
					flags.UseContainerSupport = &t
					src.UseContainerSupport = source
				}
			case token == "-XX:-UseContainerSupport":
				if flags.UseContainerSupport == nil {
					f := false
					flags.UseContainerSupport = &f
					src.UseContainerSupport = source
				}
			}
		}
	}

	applyTokens(strings.Fields(cmdline), "cmdline")

	// Application order encodes precedence (applyTokens is first-wins):
	// cmdline > JDK_JAVA_OPTIONS > JAVA_TOOL_OPTIONS > JAVA_OPTS. This matches
	// the real JVM: the launcher prepends JDK_JAVA_OPTIONS to the command line,
	// while JAVA_TOOL_OPTIONS options come before everything — and for duplicate
	// flags the last occurrence wins, so JDK_JAVA_OPTIONS overrides
	// JAVA_TOOL_OPTIONS. (Validated against hsperfdata ground truth in CI: with
	// JAVA_TOOL_OPTIONS=-Xmx96m and JDK_JAVA_OPTIONS=-Xmx128m the JVM runs a
	// 128MiB heap.) JAVA_OPTS is not read by the JVM at all — it's a shell-script
	// convention — so it ranks last as a best-effort hint.
	effective := strings.TrimSpace(cmdline)
	for _, k := range []string{"JDK_JAVA_OPTIONS", "JAVA_TOOL_OPTIONS", "JAVA_OPTS"} {
		v := ""
		if envJavaOpts != nil {
			v = envJavaOpts[k]
		}
		if strings.TrimSpace(v) == "" {
			continue
		}
		toks := splitJavaOpts(v)
		if len(toks) > 0 {
			effective = strings.TrimSpace(effective + "  " + strings.Join(toks, " "))
		}
		applyTokens(toks, k)
	}

	return flags, src, effective
}

func splitJavaOpts(s string) []string {
	var out []string
	var cur strings.Builder
	inSingle := false
	inDouble := false
	escape := false

	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}

	for _, r := range s {
		if escape {
			cur.WriteRune(r)
			escape = false
			continue
		}
		if r == '\\' && !inSingle {
			escape = true
			continue
		}
		switch r {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
				continue
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
				continue
			}
		}
		if !inSingle && !inDouble {
			if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
				flush()
				continue
			}
		}
		cur.WriteRune(r)
	}
	flush()
	return out
}

// parseMemSize parses JVM memory size strings: "256m", "4g", "512k", or bare bytes.
func parseMemSize(s string) (int64, error) {
	lower := strings.ToLower(strings.TrimSpace(s))
	// Check AFTER trimming: a whitespace-only value (constructible via quoted
	// JVM env vars like JAVA_TOOL_OPTIONS=-Xmx" ", which are read from the
	// target container's environ and thus attacker-controlled) must not reach
	// the last-byte index below — that would panic on every scrape for as long
	// as the offending process lives.
	if lower == "" {
		return 0, fmt.Errorf("empty size string")
	}
	var multiplier int64 = 1
	switch lower[len(lower)-1] {
	case 'k':
		multiplier = 1024
		lower = lower[:len(lower)-1]
	case 'm':
		multiplier = 1024 * 1024
		lower = lower[:len(lower)-1]
	case 'g':
		multiplier = 1024 * 1024 * 1024
		lower = lower[:len(lower)-1]
	}
	v, err := strconv.ParseInt(lower, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid memory size %q: %w", s, err)
	}
	return v * multiplier, nil
}
