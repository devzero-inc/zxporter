package nodemon

// classifyRuntimeProcess classifies a process from comm/cmdline as Java,
// Node.js, one of the generic table-detected runtimes (.NET, Python, Ruby,
// Deno, Bun), or unknown. Used for the combined /container/runtime-metrics path
// so a single /proc walk can feed every process-introspection collector instead
// of each running its own. Go and GraalVM native-image can't be identified from
// comm/cmdline — probeRuntimeProcess handles those in the walk's probe stage.
func classifyRuntimeProcess(comm, cmdline string) processKind {
	if isJavaProcess(comm, cmdline) {
		return processKindJava
	}
	if isNodeProcess(comm, cmdline) {
		return processKindNode
	}
	return classifyExtraRuntime(comm, cmdline)
}

// discoverRuntimeProcesses performs a single /proc walk and buckets matches into
// Java, Node.js, and generic-runtime processes, avoiding the multiple walks
// that per-runtime discovery would incur.
// Returns nil slices and nil error if procRoot does not exist (non-Linux hosts).
func discoverRuntimeProcesses(procRoot string) (javaProcs []JavaProcess, nodeProcs []NodeJSProcess, runtimeProcs []RuntimeProcess, err error) {
	entries, err := walkProcEntriesProbed(procRoot, classifyRuntimeProcess, probeRuntimeProcess)
	if err != nil {
		return nil, nil, nil, err
	}

	for _, e := range entries {
		switch e.Kind {
		case processKindJava:
			if jp, ok := javaProcessFromEntry(e); ok {
				javaProcs = append(javaProcs, jp)
			}
		case processKindNode:
			nodeProcs = append(nodeProcs, nodeJSProcessFromEntry(e))
		case processKindUnknown:
			// walkProcEntriesProbed already filters these out; unreachable.
		default:
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
	}

	return javaProcs, nodeProcs, runtimeProcs, nil
}
