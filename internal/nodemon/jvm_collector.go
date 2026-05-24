package nodemon

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
)

type containerInfo struct {
	Pod       string
	Namespace string
	Container string
}

// JVMCollector collects JVM metrics from hsperfdata files via /proc.
// Requires the pod to run with hostPID: true and as UID 0 to read
// /proc/<pid>/root/tmp/hsperfdata_*/<nsPid> for other containers.
type JVMCollector struct {
	nodeName  string
	dynClient dynamic.Interface
	procRoot  string
	log       logr.Logger
}

// NewJVMCollector creates a JVMCollector. procRoot defaults to "/proc".
func NewJVMCollector(nodeName string, dynClient dynamic.Interface, log logr.Logger) *JVMCollector {
	return &JVMCollector{
		nodeName:  nodeName,
		dynClient: dynClient,
		procRoot:  "/proc",
		log:       log.WithName("jvm-collector"),
	}
}

// QueryJVMMetrics returns JVM metrics for all discovered Java containers on this node.
func (c *JVMCollector) QueryJVMMetrics(ctx context.Context) ([]JVMMetric, error) {
	containerMap, err := c.buildContainerMap(ctx)
	if err != nil {
		// Non-fatal: continue with empty map; pod/namespace/container fields will be blank.
		c.log.Error(err, "Failed to build container map; pod metadata will be missing")
		containerMap = map[string]containerInfo{}
	}

	procs, err := discoverJavaProcesses(c.procRoot)
	if err != nil {
		return nil, fmt.Errorf("discovering java processes: %w", err)
	}

	metrics := make([]JVMMetric, 0, len(procs))
	for _, proc := range procs {
		counters, err := readHsperfdata(proc.HsperfDataPath)
		if err != nil {
			c.log.Error(err, "Failed to read hsperfdata", "pid", proc.PidHost, "path", proc.HsperfDataPath)
			continue
		}
		info := containerMap[proc.ContainerID]
		metrics = append(metrics, buildJVMMetric(counters, proc, info, c.nodeName))
	}

	return metrics, nil
}

// buildContainerMap lists running pods on this node and returns a map of
// containerID (hex, no scheme prefix) → containerInfo.
func (c *JVMCollector) buildContainerMap(ctx context.Context) (map[string]containerInfo, error) {
	fieldSelector := "status.phase=Running"
	if c.nodeName != "" {
		fieldSelector = fmt.Sprintf("%s,spec.nodeName=%s", fieldSelector, c.nodeName)
	}

	podList, err := c.dynClient.Resource(podGVR).
		Namespace("").
		List(ctx, metav1.ListOptions{FieldSelector: fieldSelector})
	if err != nil {
		return nil, fmt.Errorf("listing pods: %w", err)
	}

	result := make(map[string]containerInfo, len(podList.Items))
	for _, item := range podList.Items {
		var pod corev1.Pod
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, &pod); err != nil {
			continue
		}
		for _, cs := range pod.Status.ContainerStatuses {
			id := stripContainerIDScheme(cs.ContainerID)
			if id == "" {
				continue
			}
			result[id] = containerInfo{
				Pod:       pod.Name,
				Namespace: pod.Namespace,
				Container: cs.Name,
			}
		}
	}
	return result, nil
}

// stripContainerIDScheme strips the URL scheme (e.g., "containerd://") from a container ID.
func stripContainerIDScheme(raw string) string {
	if i := strings.LastIndex(raw, "://"); i >= 0 {
		return raw[i+3:]
	}
	return raw
}

// buildJVMMetric assembles a JVMMetric from parsed hsperfdata counters and process metadata.
func buildJVMMetric(counters map[string]any, proc JavaProcess, info containerInfo, nodeName string) JVMMetric {
	m := JVMMetric{
		NodeName:    nodeName,
		Pod:         info.Pod,
		Namespace:   info.Namespace,
		Container:   info.Container,
		ContainerID: proc.ContainerID,
		PidHost:     proc.PidHost,
		PidNS:       proc.PidNS,
		RawCmdline:  proc.CmdLine,
		Timestamp:   time.Now().UTC(),
	}

	m.JavaCommand, _ = hsStr(counters, "sun.rt.javaCommand")
	m.JavaVersion, _ = hsStr(counters, "java.property.java.version")

	// Heap capacity: try aggregate heap counters first, then sum generation counters.
	if cap, ok := hsInt(counters, "sun.gc.heap.capacity"); ok {
		m.HeapSizeBytes = cap
	} else {
		gen0, _ := hsInt(counters, "sun.gc.generation.0.capacity")
		gen1, _ := hsInt(counters, "sun.gc.generation.1.capacity")
		m.HeapSizeBytes = gen0 + gen1
	}

	if used, ok := hsInt(counters, "sun.gc.heap.used"); ok {
		m.HeapUsedBytes = used
	} else {
		gen0, _ := hsInt(counters, "sun.gc.generation.0.used")
		gen1, _ := hsInt(counters, "sun.gc.generation.1.used")
		m.HeapUsedBytes = gen0 + gen1
	}

	if maxCap, ok := hsInt(counters, "sun.gc.heap.maxCapacity"); ok {
		m.HeapMaxSizeBytes = maxCap
	} else {
		gen0, _ := hsInt(counters, "sun.gc.generation.0.maxCapacity")
		gen1, _ := hsInt(counters, "sun.gc.generation.1.maxCapacity")
		m.HeapMaxSizeBytes = gen0 + gen1
	}

	// Convert GC time ticks to seconds using the JVM's high-resolution timer frequency.
	freq, _ := hsInt(counters, "sun.os.hrt.frequency")
	if freq <= 0 {
		freq = 1_000_000_000 // nanosecond fallback
	}

	m.GCTimeSecondsTotal = make(map[string]float64)
	for i := 0; i < 8; i++ {
		name, ok := hsStr(counters, fmt.Sprintf("sun.gc.collector.%d.name", i))
		if !ok || name == "" {
			break
		}
		ticks, _ := hsInt(counters, fmt.Sprintf("sun.gc.collector.%d.time", i))
		m.GCTimeSecondsTotal[name] = float64(ticks) / float64(freq)
	}

	safeTicks, _ := hsInt(counters, "sun.rt.safepointTime")
	syncTicks, _ := hsInt(counters, "sun.rt.safepointSyncTime")
	m.SafepointTimeSecondsTotal = float64(safeTicks) / float64(freq)
	m.SafepointSyncTimeSecondsTotal = float64(syncTicks) / float64(freq)

	m.FlagsExtracted = ParseJVMFlags(proc.CmdLine)

	return m
}
