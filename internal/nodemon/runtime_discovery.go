package nodemon

// classifyRuntimeProcess classifies a process from comm/cmdline as Java, one of
// the table-detected runtimes (Node.js, .NET, Python, Ruby, Deno, Bun), or
// unknown. Used for the combined /container/runtime-metrics path so a single
// /proc walk can feed every process-introspection collector instead of each
// running its own. Go and GraalVM native-image can't be identified from
// comm/cmdline — probeRuntimeProcess handles those in the walk's probe stage.
// Java keeps a dedicated branch (and downstream pipeline) because its payload
// is hsperfdata-backed heap metrics + flag extraction, not just
// existence + version.
func classifyRuntimeProcess(comm, cmdline string) processKind {
	if isJavaProcess(comm, cmdline) {
		return processKindJava
	}
	return classifyExtraRuntime(comm, cmdline)
}

// discoverRuntimeProcesses performs a single /proc walk and buckets matches into
// Java and generic-runtime processes, avoiding the multiple walks that
// per-runtime discovery would incur. probe classifies containerized processes
// that comm/cmdline couldn't (callers pass probeRuntimeProcess or a memoized
// wrapper around it).
// denied, if non-nil, is told about JVMs whose hsperfdata could not be read
// because access to the container's /tmp was refused.
// Returns nil slices and nil error if procRoot does not exist (non-Linux hosts).
func discoverRuntimeProcesses(procRoot string, probe func(e procEntry) processKind, denied *hsperfDeniedReporter) (javaProcs []JavaProcess, runtimeProcs []RuntimeProcess, err error) {
	entries, err := walkProcEntriesProbed(procRoot, classifyRuntimeProcess, probe)
	if err != nil {
		return nil, nil, err
	}

	for _, e := range entries {
		if e.Kind == processKindUnknown {
			// walkProcEntriesProbed already filters these out; unreachable.
			continue
		}

		if e.Kind == processKindJava {
			jp, hasHsperf := javaProcessFromEntry(e, denied)
			javaProcs = append(javaProcs, jp)
			if hasHsperf {
				continue
			}
			// No readable hsperfdata file. The JVM is running fine — HotSpot
			// silently falls back to non-shared perf memory when it cannot
			// create /tmp/hsperfdata_<user>/<pid> (`readOnlyRootFilesystem: true`
			// with no writable /tmp, -XX:-UsePerfData / -XX:+PerfDisableSharedMem),
			// and a nodemon without root cannot read another user's /tmp at all.
			// It still goes to the JVM bucket above for its flags and version,
			// and is also reported in the generic bucket: that "java" entry is
			// the signal the JVM CPU startup floor and the language filter have
			// always used for JVMs without heap metrics.
		}

		runtimeProcs = append(runtimeProcs, RuntimeProcess{
			Kind:        e.Kind,
			Runtime:     runtimeNameForKind(e.Kind),
			PidHost:     e.PidHost,
			PidNS:       e.PidNS,
			ContainerID: e.ContainerID,
			CmdLine:     e.CmdLine,
			PidDir:      e.PidDir,
		})
	}

	return javaProcs, runtimeProcs, nil
}
