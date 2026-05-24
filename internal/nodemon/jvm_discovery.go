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
	// containerIDRe matches 64-char container IDs in cgroup paths for containerd, docker, and bare containerd.
	containerIDRe = regexp.MustCompile(`(?:cri-containerd|docker|containerd)-([a-f0-9]{64})\.scope`)
	// crioRe matches CRI-O container IDs.
	crioRe = regexp.MustCompile(`crio-([a-f0-9]{64})\.scope`)
)

// JavaProcess holds info about a discovered Java process running inside a Kubernetes container.
type JavaProcess struct {
	PidHost        int
	PidNS          int
	ContainerID    string
	CmdLine        string
	HsperfDataPath string
}

// discoverJavaProcesses scans procRoot (usually "/proc") for Java processes
// running inside Kubernetes container cgroups.
// Returns nil, nil if procRoot does not exist (non-Linux hosts).
func discoverJavaProcesses(procRoot string) ([]JavaProcess, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", procRoot, err)
	}

	var procs []JavaProcess
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
		// Fast path: most JVMs have comm == "java". Avoid reading cmdline for every PID.
		if comm != "java" {
			continue
		}

		// Read null-separated cmdline and convert to space-separated for parsing flags.
		rawCmdline := readProcFile(filepath.Join(pidDir, "cmdline"))
		cmdline := string(bytes.ReplaceAll([]byte(rawCmdline), []byte{0}, []byte{' '}))

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

		hsperfPath := findHsperfdata(pidDir, nsPid)
		if hsperfPath == "" {
			continue
		}

		procs = append(procs, JavaProcess{
			PidHost:        pid,
			PidNS:          nsPid,
			ContainerID:    containerID,
			CmdLine:        strings.TrimSpace(cmdline),
			HsperfDataPath: hsperfPath,
		})
	}

	return procs, nil
}

// isJavaProcess returns true if the process comm is "java" or its first cmdline
// argument is a binary named "java".
func isJavaProcess(comm, cmdline string) bool {
	if comm == "java" {
		return true
	}
	parts := strings.Fields(cmdline)
	if len(parts) == 0 {
		return false
	}
	return filepath.Base(parts[0]) == "java"
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

// readProcFile reads a /proc pseudo-file with a hard cap, returning "" on any error.
// Errors are expected and normal (process may disappear between readdir and read).
func readProcFile(path string) string {
	const maxProcBytes = 64 << 10 // 64KiB safety cap

	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	b, err := io.ReadAll(io.LimitReader(f, maxProcBytes))
	if err != nil {
		return ""
	}
	return string(b)
}

// ParseJVMFlags parses JVM memory and container-awareness flags from a process cmdline string.
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
