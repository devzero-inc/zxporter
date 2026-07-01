package nodemon

// classifyRuntimeProcess classifies a process as Java, Node.js, or neither. Used
// for the combined /container/runtime-metrics path so a single /proc walk can
// feed every process-introspection collector instead of each running its own.
func classifyRuntimeProcess(comm, cmdline string) processKind {
	if isJavaProcess(comm, cmdline) {
		return processKindJava
	}
	if isNodeProcess(comm, cmdline) {
		return processKindNode
	}
	return processKindUnknown
}

// discoverRuntimeProcesses performs a single /proc walk and buckets matches into
// Java and Node.js processes, avoiding the double walk that running
// discoverJavaProcesses and discoverNodeProcesses separately would incur.
// Returns nil, nil, nil if procRoot does not exist (non-Linux hosts).
func discoverRuntimeProcesses(procRoot string) (javaProcs []JavaProcess, nodeProcs []NodeJSProcess, err error) {
	entries, err := walkProcEntries(procRoot, classifyRuntimeProcess)
	if err != nil {
		return nil, nil, err
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
			// walkProcEntries already filters these out; unreachable.
		}
	}

	return javaProcs, nodeProcs, nil
}
