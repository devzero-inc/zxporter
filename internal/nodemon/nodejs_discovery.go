package nodemon

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const nodeComm = "node"
const nodeBinName = "node"

const (
	nodeVersionSourceEnv        = "env"
	nodeVersionSourceBinaryScan = "binary-scan"
)

// nodeReleaseURLRe matches the release-URL string Node embeds in its own binary
// for process.release metadata, e.g.:
//
//	https://nodejs.org/download/release/v20.11.1/node-v20.11.1.tar.gz
//
// This is deliberately anchored on Node's own release metadata rather than a
// generic semver pattern — a bare `v\d+\.\d+\.\d+` regex would also match the
// V8/OpenSSL/ICU/uv version strings baked into the same binary. The literal
// `https://` prefix anchors the host boundary so this can't match a lookalike
// domain like `evilnodejs.org/download/release/...` appearing elsewhere in the
// scanned bytes.
//
// CodeQL still flags this as go/regex/missing-regexp-anchor since the query
// looks for a syntactic `^`/`\A`/`\b` anchor and doesn't reason about literal
// prefixes. A real `^`/`\A` anchor isn't applicable here — the string legitimately
// appears mid-file, not at a fixed offset — and there is no meaningful security
// impact to suppress: the extracted value is display-only telemetry (NodeVersion),
// never fetched or used for any trust/redirect/file-path decision. A crafted
// binary could at worst cause a wrong version number to be reported for that
// binary's own container — no escalation, no cross-tenant effect. Dismissed as a
// false positive on the code-scanning alert directly (this repo runs CodeQL via
// Default Setup, which doesn't honor inline `codeql[...]` suppression comments —
// that's an Advanced Setup-only feature).
var nodeReleaseURLRe = regexp.MustCompile(`https://nodejs\.org/download/release/v(\d+\.\d+\.\d+)/`)

// maxNodeBinaryScanBytes bounds the read-only scan of a discovered node binary.
//
// Measured empirically against the official node:{18,20,22,24}-slim Docker Hub
// images: the release-URL string sits at 30-45MB into an 91-122MB binary (it
// scales with binary size, not a fixed offset — it is NOT near the head or tail
// of the file). 64MiB covers all four with ~20MB of margin over the largest
// observed offset. Best-effort: as Node's binary keeps growing across releases,
// this may eventually need to grow too — the nodejs-metrics-kind CI workflow
// exercises this against a real image on every PR specifically to catch that
// regression before it reaches production.
const maxNodeBinaryScanBytes = 64 << 20 // 64MiB

// NodeJSProcess holds info about a discovered Node.js process running inside a
// Kubernetes container, prior to version resolution (which the collector layer
// caches per-container since it can involve a binary scan).
type NodeJSProcess struct {
	PidHost     int
	PidNS       int
	ContainerID string
	CmdLine     string

	// PidDir is /proc/<pid> on procRoot, retained so the collector can resolve
	// the Node version (env var / binary scan) without re-walking /proc.
	PidDir string
}

// discoverNodeProcesses scans procRoot (usually "/proc") for Node.js processes
// running inside Kubernetes container cgroups.
// Returns nil, nil if procRoot does not exist (non-Linux hosts).
func discoverNodeProcesses(procRoot string) ([]NodeJSProcess, error) {
	entries, err := walkProcEntries(procRoot, classifyNodeOnly)
	if err != nil {
		return nil, err
	}

	procs := make([]NodeJSProcess, 0, len(entries))
	for _, e := range entries {
		procs = append(procs, nodeJSProcessFromEntry(e))
	}

	return procs, nil
}

// classifyNodeOnly is a single-runtime classifier for callers that only care
// about Node.js processes (the legacy /container/nodejs-metrics path).
func classifyNodeOnly(comm, cmdline string) processKind {
	if isNodeProcess(comm, cmdline) {
		return processKindNode
	}
	return processKindUnknown
}

// nodeJSProcessFromEntry builds a NodeJSProcess from a procEntry already
// classified as processKindNode. Version resolution is deferred to the
// collector layer, which caches it per container.
func nodeJSProcessFromEntry(e procEntry) NodeJSProcess {
	return NodeJSProcess{
		PidHost:     e.PidHost,
		PidNS:       e.PidNS,
		ContainerID: e.ContainerID,
		CmdLine:     e.CmdLine,
		PidDir:      e.PidDir,
	}
}

// isNodeProcess returns true if the process comm is "node" or its first cmdline
// argument is a binary named "node".
func isNodeProcess(comm, cmdline string) bool {
	if comm == nodeComm {
		return true
	}
	parts := strings.Fields(cmdline)
	if len(parts) == 0 {
		return false
	}
	return filepath.Base(parts[0]) == nodeBinName
}

// resolveNodeVersion best-effort resolves the Node.js version for a discovered
// process at pidDir (/proc/<pid> on procRoot). It never executes the discovered
// binary — it only reads an already-set environment variable and, failing that,
// scans the on-disk binary content for a release-URL string Node embeds for its
// own process.release metadata. Returns ("", "") if neither resolves.
func resolveNodeVersion(pidDir string) (version, source string) {
	if env := readEnvVars(filepath.Join(pidDir, "environ"), "NODE_VERSION"); env != nil {
		if v := strings.TrimSpace(env["NODE_VERSION"]); v != "" {
			return v, nodeVersionSourceEnv
		}
	}

	exePath := resolveNodeExePath(pidDir)
	if exePath == "" {
		return "", ""
	}

	content := readFileCapped(exePath, maxNodeBinaryScanBytes)
	if content == "" {
		return "", ""
	}

	if m := nodeReleaseURLRe.FindStringSubmatch(content); len(m) == 2 {
		return m[1], nodeVersionSourceBinaryScan
	}

	return "", ""
}

// resolveNodeExePath resolves the on-disk path to a process's executable via the
// /proc/<pid>/root overlay, so it can be read from outside the process's own
// mount namespace. Returns "" if /proc/<pid>/exe cannot be read (process may have
// exited, or permissions are insufficient).
func resolveNodeExePath(pidDir string) string {
	target, err := os.Readlink(filepath.Join(pidDir, "exe"))
	if err != nil || target == "" {
		return ""
	}
	return filepath.Join(pidDir, "root", target)
}
