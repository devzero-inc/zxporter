package nodemon

import (
	"debug/buildinfo"
	"debug/elf"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// This file implements detection + passive version resolution for runtimes beyond
// JVM and Node.js: .NET, Go, GraalVM native-image, Python, Ruby, Deno, and Bun.
// Like Node.js detection, this is deliberately scoped to existence + version — no
// live heap/GC stats (none of these expose an hsperfdata-equivalent counters
// file), and nothing is ever executed: resolution only reads /proc pseudo-files
// and, where needed, a bounded prefix of the on-disk binary.
//
// Two detection strategies exist because not every runtime is identifiable from
// comm/cmdline:
//
//   - table detectors (extraRuntimeDetectors): interpreters/launchers with a
//     stable process name — dotnet, python, ruby, deno, bun. Matched by
//     classifyRuntimeProcess during the /proc walk, same as java/node.
//   - probes (probeRuntimeProcess): compiled binaries named after the app — Go
//     binaries (detected via the embedded build info every Go binary carries)
//     and GraalVM native-image binaries (detected via the SubstrateVM image-heap
//     ELF section). Probing opens the process's executable, so it only runs for
//     processes that already resolved to a Kubernetes container cgroup and were
//     not classified by comm/cmdline.

// runtime names reported in RuntimeProcessMetric.Runtime.
const (
	runtimeNameNodeJS  = "nodejs"
	runtimeNameDotnet  = "dotnet"
	runtimeNameGo      = "go"
	runtimeNameGraalVM = "graalvm-native-image"
	runtimeNamePython  = "python"
	runtimeNameRuby    = "ruby"
	runtimeNameDeno    = "deno"
	runtimeNameBun     = "bun"
)

// version sources reported in RuntimeProcessMetric.VersionSource.
const (
	runtimeVersionSourceEnv        = "env"
	runtimeVersionSourceMaps       = "maps"
	runtimeVersionSourceExePath    = "exe-path"
	runtimeVersionSourceComm       = "comm"
	runtimeVersionSourceBuildInfo  = "buildinfo"
	runtimeVersionSourceBinaryScan = "binary-scan"
)

// maxMapsReadBytes caps reads of /proc/<pid>/maps for version resolution. maps
// grows with the process's mapping count; 2MiB comfortably covers even very
// library-heavy processes, and the runtime shared objects we grep for (coreclr,
// libruby) load early so they sit near the front anyway.
const maxMapsReadBytes = 2 << 20 // 2MiB

var (
	// dotnetMapsVersionRe extracts the shared-framework version from a mapped
	// libcoreclr path, e.g. /usr/share/dotnet/shared/Microsoft.NETCore.App/8.0.11/libcoreclr.so
	dotnetMapsVersionRe = regexp.MustCompile(`/Microsoft\.NETCore\.App/(\d+\.\d+\.\d+[^/ ]*)/`)
	// rubyMapsVersionRe extracts the version from a mapped libruby, e.g. libruby.so.3.2.4
	rubyMapsVersionRe = regexp.MustCompile(`/libruby\.so\.(\d+\.\d+(?:\.\d+)?)`)
	// pythonExeVersionRe extracts the version baked into CPython's binary/link
	// name, e.g. python3.11 (comm truncates to 15 chars, which still fits).
	pythonExeVersionRe = regexp.MustCompile(`^python(\d+(?:\.\d+)?)$`)
	// denoUserAgentRe / bunUserAgentRe match the HTTP user-agent string each
	// runtime embeds in its own binary, e.g. "Deno/1.44.0", "Bun/1.1.8". Like the
	// Node.js release-URL scan, the extracted value is display-only telemetry —
	// see nodeReleaseURLRe for why these deliberately have no ^ anchor (the string
	// legitimately appears mid-binary).
	denoUserAgentRe = regexp.MustCompile(`Deno/(\d+\.\d+\.\d+)`)
	bunUserAgentRe  = regexp.MustCompile(`Bun/(\d+\.\d+\.\d+)`)
	// graalVMVersionRe best-effort matches the GraalVM release string embedded in
	// native-image binaries, e.g. "GraalVM CE 22.3.1" or "Oracle GraalVM 21+35.1".
	graalVMVersionRe = regexp.MustCompile(`GraalVM (?:[A-Z]{2} )?(\d+(?:\.\d+)*(?:\+[\d.]+)?)`)
)

// runtimeDetector describes a runtime identifiable from comm/cmdline (an
// interpreter or launcher with a stable process name) plus its passive version
// resolver.
type runtimeDetector struct {
	name           string
	kind           processKind
	match          func(comm, cmdline string) bool
	resolveVersion func(pidDir string) (version, source string)
}

// extraRuntimeDetectors is the table of comm/cmdline-classifiable runtimes beyond
// java and node (which keep their dedicated pipelines).
var extraRuntimeDetectors = []runtimeDetector{
	{
		name:           runtimeNameNodeJS,
		kind:           processKindNode,
		match:          isNodeProcess,
		resolveVersion: resolveNodeVersion,
	},
	{
		name:           runtimeNameDotnet,
		kind:           processKindDotnet,
		match:          commOrArgv0Matcher(func(s string) bool { return s == "dotnet" }),
		resolveVersion: resolveDotnetVersion,
	},
	{
		name:           runtimeNamePython,
		kind:           processKindPython,
		match:          commOrArgv0Matcher(isPythonName),
		resolveVersion: resolvePythonVersion,
	},
	{
		name:           runtimeNameRuby,
		kind:           processKindRuby,
		match:          commOrArgv0Matcher(func(s string) bool { return s == "ruby" }),
		resolveVersion: resolveRubyVersion,
	},
	{
		name:           runtimeNameDeno,
		kind:           processKindDeno,
		match:          commOrArgv0Matcher(func(s string) bool { return s == "deno" }),
		resolveVersion: resolveDenoVersion,
	},
	{
		name:           runtimeNameBun,
		kind:           processKindBun,
		match:          commOrArgv0Matcher(func(s string) bool { return s == "bun" }),
		resolveVersion: resolveBunVersion,
	},
}

// runtimeNameForKind maps a processKind to the runtime name reported to the
// control plane. Only covers the generic runtimes (java/node have their own
// metric types).
func runtimeNameForKind(kind processKind) string {
	switch kind {
	case processKindNode:
		return runtimeNameNodeJS
	case processKindDotnet:
		return runtimeNameDotnet
	case processKindGo:
		return runtimeNameGo
	case processKindGraalVM:
		return runtimeNameGraalVM
	case processKindPython:
		return runtimeNamePython
	case processKindRuby:
		return runtimeNameRuby
	case processKindDeno:
		return runtimeNameDeno
	case processKindBun:
		return runtimeNameBun
	default:
		return ""
	}
}

// resolveRuntimeVersion dispatches to the per-runtime passive version resolver.
func resolveRuntimeVersion(kind processKind, pidDir string) (version, source string) {
	switch kind {
	case processKindGo:
		return resolveGoVersion(pidDir)
	case processKindGraalVM:
		return resolveGraalVMVersion(pidDir)
	default:
		for _, d := range extraRuntimeDetectors {
			if d.kind == kind {
				return d.resolveVersion(pidDir)
			}
		}
	}
	return "", ""
}

// commOrArgv0Matcher builds a match func that applies nameMatch to the process
// comm and to the basename of the first cmdline argument, the same shape
// isJavaProcess/isNodeProcess use.
func commOrArgv0Matcher(nameMatch func(string) bool) func(comm, cmdline string) bool {
	return func(comm, cmdline string) bool {
		if nameMatch(comm) {
			return true
		}
		parts := strings.Fields(cmdline)
		if len(parts) == 0 {
			return false
		}
		return nameMatch(filepath.Base(parts[0]))
	}
}

// classifyExtraRuntime classifies a process against the table of generic runtime
// detectors. Returns processKindUnknown if none match.
func classifyExtraRuntime(comm, cmdline string) processKind {
	for _, d := range extraRuntimeDetectors {
		if d.match(comm, cmdline) {
			return d.kind
		}
	}
	return processKindUnknown
}

// isPythonName matches python, python3, python3.11, ...
func isPythonName(s string) bool {
	return s == "python" || pythonExeVersionRe.MatchString(s)
}

// probeCacheEntry is the memoized result of probing a (container, executable)
// pair, with the same bounded-retry semantics as version caching: an unknown
// result is re-probed a few times (a .NET apphost may not have mapped
// libcoreclr yet in its first observed cycle) and then pinned.
type probeCacheEntry struct {
	Kind     processKind
	Attempts int
}

// newMemoizedProbe wraps inner with a per-cycle memo seeded from prev, so a
// long-lived unclassifiable process (nginx, postgres, any static binary) pays
// the executable inspection once, not on every scrape cycle. The cache key is
// containerID + the exe link target: same-container processes with different
// executables are probed independently, and the rebuilt map returned alongside
// the probe drops entries for containers/processes that are gone.
func newMemoizedProbe(prev map[string]probeCacheEntry, inner func(e procEntry) processKind) (func(e procEntry) processKind, map[string]probeCacheEntry) {
	next := make(map[string]probeCacheEntry)
	probe := func(e procEntry) processKind {
		exeTarget, err := os.Readlink(filepath.Join(e.PidDir, "exe"))
		if err != nil || exeTarget == "" {
			// Unreadable exe is transient (process exiting, permissions) — probe
			// without caching so it isn't pinned as unknown.
			return inner(e)
		}
		key := e.ContainerID + "|" + exeTarget

		entry, cached := next[key]
		if !cached {
			entry, cached = prev[key]
		}
		if !cached || (entry.Kind == processKindUnknown && entry.Attempts < maxVersionResolveAttempts) {
			entry = probeCacheEntry{Kind: inner(e), Attempts: entry.Attempts + 1}
		}
		next[key] = entry
		return entry.Kind
	}
	return probe, next
}

// probeRuntimeProcess identifies runtimes that can't be classified from
// comm/cmdline because the binary is named after the application: Go binaries
// and GraalVM native images. It only reads the executable's headers/sections via
// the /proc/<pid>/exe magic link — nothing is executed. Called by the /proc walk
// only for processes that already resolved to a container cgroup, so host
// daemons are never probed.
func probeRuntimeProcess(e procEntry) processKind {
	exePath := filepath.Join(e.PidDir, "exe")

	// Interpreters often run under a script-named comm (gunicorn, celery, puma,
	// rails, npm...) — comm/cmdline show the script, but readlink(exe) still
	// points at the real interpreter binary.
	switch base := exeBasename(e.PidDir); {
	case isPythonName(base):
		return processKindPython
	case base == "ruby":
		return processKindRuby
	case base == nodeBinName:
		return processKindNode
	}

	// Every Go binary embeds build info (module + toolchain version) in a
	// dedicated section; buildinfo.ReadFile reads only the file headers and that
	// section, so this is cheap even for large binaries.
	if _, err := buildinfo.ReadFile(exePath); err == nil {
		return processKindGo
	}

	// GraalVM native-image binaries carry the SubstrateVM image heap in a
	// dedicated ELF section. These are Java workloads that the hsperfdata-based
	// JVM detector cannot see (no HotSpot, no perf counters file).
	if hasSVMSection(exePath) {
		return processKindGraalVM
	}

	// .NET's default deployment shape is an apphost: a small native launcher
	// named after the app (comm "myapp", no "dotnet" anywhere in cmdline) that
	// loads libcoreclr. The mapped coreclr shared object is the reliable marker.
	maps := readFileCapped(filepath.Join(e.PidDir, "maps"), maxMapsReadBytes)
	if strings.Contains(maps, "/libcoreclr.so") {
		return processKindDotnet
	}

	return processKindUnknown
}

// hasSVMSection reports whether the ELF binary at path contains a SubstrateVM
// section (".svm_heap" et al.), the marker of a GraalVM native-image binary.
func hasSVMSection(path string) bool {
	f, err := elf.Open(path)
	if err != nil {
		return false
	}
	defer func() {
		// close errors on a read-only probe are not actionable.
		_ = f.Close()
	}()
	for _, s := range f.Sections {
		if strings.Contains(s.Name, "svm_heap") {
			return true
		}
	}
	return false
}

// resolveDotnetVersion resolves the .NET runtime version: DOTNET_VERSION env var
// (set by official mcr.microsoft.com/dotnet images), else the shared-framework
// version in the mapped libcoreclr path from /proc/<pid>/maps (covers
// framework-dependent apps; self-contained single-file apps may not resolve).
func resolveDotnetVersion(pidDir string) (version, source string) {
	if v := envVersion(pidDir, "DOTNET_VERSION"); v != "" {
		return v, runtimeVersionSourceEnv
	}
	maps := readFileCapped(filepath.Join(pidDir, "maps"), maxMapsReadBytes)
	if m := dotnetMapsVersionRe.FindStringSubmatch(maps); len(m) == 2 {
		return m[1], runtimeVersionSourceMaps
	}
	return "", ""
}

// resolveGoVersion reads the toolchain version embedded in every Go binary.
func resolveGoVersion(pidDir string) (version, source string) {
	bi, err := buildinfo.ReadFile(filepath.Join(pidDir, "exe"))
	if err != nil || bi.GoVersion == "" {
		return "", ""
	}
	// bi.GoVersion is e.g. "go1.22.4"; strip the prefix for a bare version.
	return strings.TrimPrefix(bi.GoVersion, "go"), runtimeVersionSourceBuildInfo
}

// resolveGraalVMVersion best-effort extracts the GraalVM release string embedded
// in a native-image binary. Detection (the SVM section) is the reliable signal;
// version is a bonus and may legitimately stay empty.
func resolveGraalVMVersion(pidDir string) (version, source string) {
	return scanBinaryVersion(pidDir, graalVMVersionRe)
}

// resolvePythonVersion resolves the CPython version: PYTHON_VERSION env var
// (official docker library images carry the full x.y.z), else the major.minor
// baked into the executable name (readlink /proc/<pid>/exe → .../python3.11),
// else the same pattern on comm.
func resolvePythonVersion(pidDir string) (version, source string) {
	if v := envVersion(pidDir, "PYTHON_VERSION"); v != "" {
		return v, runtimeVersionSourceEnv
	}
	if base := exeBasename(pidDir); base != "" {
		if m := pythonExeVersionRe.FindStringSubmatch(base); len(m) == 2 {
			return m[1], runtimeVersionSourceExePath
		}
	}
	comm := strings.TrimSpace(readProcFile(filepath.Join(pidDir, "comm")))
	if m := pythonExeVersionRe.FindStringSubmatch(comm); len(m) == 2 {
		return m[1], runtimeVersionSourceComm
	}
	return "", ""
}

// resolveRubyVersion resolves the Ruby version: RUBY_VERSION env var (official
// docker library images), else the version suffix of the mapped libruby shared
// object (MRI dynamically links libruby.so.<x.y> in essentially all distro and
// docker-library builds).
func resolveRubyVersion(pidDir string) (version, source string) {
	if v := envVersion(pidDir, "RUBY_VERSION"); v != "" {
		return v, runtimeVersionSourceEnv
	}
	maps := readFileCapped(filepath.Join(pidDir, "maps"), maxMapsReadBytes)
	if m := rubyMapsVersionRe.FindStringSubmatch(maps); len(m) == 2 {
		return m[1], runtimeVersionSourceMaps
	}
	return "", ""
}

// resolveDenoVersion resolves the Deno version: DENO_VERSION env var, else the
// user-agent string ("Deno/x.y.z") Deno embeds in its own binary.
func resolveDenoVersion(pidDir string) (version, source string) {
	if v := envVersion(pidDir, "DENO_VERSION"); v != "" {
		return v, runtimeVersionSourceEnv
	}
	return scanBinaryVersion(pidDir, denoUserAgentRe)
}

// resolveBunVersion resolves the Bun version: BUN_VERSION env var, else the
// user-agent string ("Bun/x.y.z") Bun embeds in its own binary.
func resolveBunVersion(pidDir string) (version, source string) {
	if v := envVersion(pidDir, "BUN_VERSION"); v != "" {
		return v, runtimeVersionSourceEnv
	}
	return scanBinaryVersion(pidDir, bunUserAgentRe)
}

// envVersion reads a single version-bearing env var from /proc/<pid>/environ.
func envVersion(pidDir, key string) string {
	env := readEnvVars(filepath.Join(pidDir, "environ"), key)
	if env == nil {
		return ""
	}
	return strings.TrimSpace(env[key])
}

// exeBasename returns the basename of the readlink target of /proc/<pid>/exe,
// or "" if unreadable.
func exeBasename(pidDir string) string {
	target := resolveExePath(pidDir)
	if target == "" {
		return ""
	}
	return filepath.Base(target)
}

// scanBinaryVersion streams a bounded prefix of the process's on-disk executable
// (via /proc/<pid>/root, same as the Node.js binary scan) through re and extracts
// its first capture group. Read-only; nothing is executed; memory is bounded by
// the scan's chunk buffer, not the prefix size.
func scanBinaryVersion(pidDir string, re *regexp.Regexp) (version, source string) {
	exePath := resolveExePath(pidDir)
	if exePath == "" {
		return "", ""
	}
	if v := scanFileSubmatch(exePath, maxNodeBinaryScanBytes, re); v != "" {
		return v, runtimeVersionSourceBinaryScan
	}
	return "", ""
}
