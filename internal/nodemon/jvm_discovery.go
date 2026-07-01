package nodemon

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

const javaComm = "java"
const javaBinName = "java"

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
// Returns nil, nil if procRoot does not exist (non-Linux hosts).
func discoverJavaProcesses(procRoot string) ([]JavaProcess, error) {
	entries, err := walkProcEntries(procRoot, classifyJavaOnly)
	if err != nil {
		return nil, err
	}

	procs := make([]JavaProcess, 0, len(entries))
	for _, e := range entries {
		if jp, ok := javaProcessFromEntry(e); ok {
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
func javaProcessFromEntry(e procEntry) (JavaProcess, bool) {
	hsperfPath := findHsperfdata(e.PidDir, e.PidNS)
	if hsperfPath == "" {
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
// Returns "" if no file is found.
func findHsperfdata(pidDir string, nsPid int) string {
	pattern := filepath.Join(pidDir, "root", "tmp", "hsperfdata_*", strconv.Itoa(nsPid))
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return ""
	}
	return matches[0]
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
//     JAVA_TOOL_OPTIONS, JDK_JAVA_OPTIONS, JAVA_OPTS
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

	effective := strings.TrimSpace(cmdline)
	for _, k := range []string{"JAVA_TOOL_OPTIONS", "JDK_JAVA_OPTIONS", "JAVA_OPTS"} {
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
	if s == "" {
		return 0, fmt.Errorf("empty size string")
	}
	lower := strings.ToLower(strings.TrimSpace(s))
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
