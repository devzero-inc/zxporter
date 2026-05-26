package nodemon

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
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
	nodeName        string
	k8sClient       kubernetes.Interface
	informerFactory informers.SharedInformerFactory
	procRoot        string
	log             logr.Logger

	mu           sync.RWMutex
	containerMap map[string]containerInfo // containerID (hex) -> pod metadata
	stopCh       chan struct{}
}

// NewJVMCollector creates a JVMCollector. procRoot defaults to "/proc".
func NewJVMCollector(nodeName string, k8sClient kubernetes.Interface, log logr.Logger) *JVMCollector {
	return &JVMCollector{
		nodeName:     nodeName,
		k8sClient:    k8sClient,
		procRoot:     "/proc",
		log:          log.WithName("jvm-collector"),
		containerMap: make(map[string]containerInfo),
		stopCh:       make(chan struct{}),
	}
}

// Start creates a node-scoped pod informer and waits for the cache to sync.
// Must be called exactly once before serving HTTP requests.
func (c *JVMCollector) Start() error {
	if c.informerFactory != nil {
		return fmt.Errorf("JVMCollector already started")
	}

	c.informerFactory = informers.NewSharedInformerFactoryWithOptions(
		c.k8sClient,
		0,
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			if c.nodeName != "" {
				opts.FieldSelector = "spec.nodeName=" + c.nodeName
			}
		}),
	)

	podInformer := c.informerFactory.Core().V1().Pods().Informer()

	_, err := podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				return
			}
			c.updateContainerMap(pod)
		},
		UpdateFunc: func(_, newObj interface{}) {
			pod, ok := newObj.(*corev1.Pod)
			if !ok {
				return
			}
			c.updateContainerMap(pod)
		},
		DeleteFunc: func(obj interface{}) {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					return
				}
				pod, ok = tombstone.Obj.(*corev1.Pod)
				if !ok {
					return
				}
			}
			c.removeFromContainerMap(pod)
		},
	})
	if err != nil {
		return fmt.Errorf("adding pod event handler: %w", err)
	}

	c.informerFactory.Start(c.stopCh)

	c.log.Info("Waiting for pod informer cache to sync")
	if !cache.WaitForCacheSync(c.stopCh, podInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for pod informer cache to sync")
	}
	c.log.Info("Pod informer cache synced")

	return nil
}

// Stop shuts down the informer factory. Safe to call multiple times.
func (c *JVMCollector) Stop() {
	select {
	case <-c.stopCh:
		// already closed
	default:
		close(c.stopCh)
	}
}

func (c *JVMCollector) updateContainerMap(pod *corev1.Pod) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, cs := range pod.Status.ContainerStatuses {
		id := stripContainerIDScheme(cs.ContainerID)
		if id == "" {
			continue
		}
		c.containerMap[id] = containerInfo{
			Pod:       pod.Name,
			Namespace: pod.Namespace,
			Container: cs.Name,
		}
	}
}

func (c *JVMCollector) removeFromContainerMap(pod *corev1.Pod) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, cs := range pod.Status.ContainerStatuses {
		id := stripContainerIDScheme(cs.ContainerID)
		if id == "" {
			continue
		}
		delete(c.containerMap, id)
	}
}

// QueryJVMMetrics returns JVM metrics for all discovered Java containers on this node.
func (c *JVMCollector) QueryJVMMetrics(ctx context.Context) ([]JVMMetric, error) {
	c.mu.RLock()
	containerMap := make(map[string]containerInfo, len(c.containerMap))
	for k, v := range c.containerMap {
		containerMap[k] = v
	}
	c.mu.RUnlock()
	c.log.Info("Container map snapshot", "count", len(containerMap))

	start := time.Now()
	procs, err := discoverJavaProcesses(c.procRoot)
	if err != nil {
		return nil, fmt.Errorf("discovering java processes: %w", err)
	}
	c.log.Info("Discovered java processes", "count", len(procs), "took", time.Since(start).String())

	start = time.Now()
	metrics := make([]JVMMetric, 0, len(procs))
	for _, proc := range procs {
		select {
		case <-ctx.Done():
			c.log.Info("JVM metrics query cancelled", "collected", len(metrics), "remaining", len(procs)-len(metrics))
			return metrics, ctx.Err()
		default:
		}

		c.log.Info("Reading hsperfdata", "pid", proc.PidHost, "path", proc.HsperfDataPath)
		if st, err := os.Stat(proc.HsperfDataPath); err == nil {
			c.log.Info("hsperfdata stat", "pid", proc.PidHost, "sizeBytes", st.Size())
		} else {
			c.log.Error(err, "hsperfdata stat failed", "pid", proc.PidHost, "path", proc.HsperfDataPath)
		}
		readStart := time.Now()
		counters, err := readHsperfdata(proc.HsperfDataPath)
		if err != nil {
			c.log.Error(err, "Failed to read hsperfdata", "pid", proc.PidHost, "path", proc.HsperfDataPath)
			continue
		}
		c.log.Info("Read hsperfdata", "pid", proc.PidHost, "counters", len(counters), "took", time.Since(readStart).String())

		info := containerMap[proc.ContainerID]
		metrics = append(metrics, buildJVMMetric(counters, proc, info, c.nodeName))
	}
	c.log.Info("Built JVM metrics", "count", len(metrics), "took", time.Since(start).String())

	return metrics, nil
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
		// FlagsExtracted / FlagSources are filled after hsperf parsing.
		Timestamp: time.Now().UTC(),
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
	// HotSpot stores safepoint counters in milliseconds (not HRT ticks like GC collector time).
	// See: sun.rt.safepointTime, sun.rt.safepointSyncTime
	m.SafepointTimeSecondsTotal = float64(safeTicks) / 1000.0
	m.SafepointSyncTimeSecondsTotal = float64(syncTicks) / 1000.0

	flags, sources, effectiveCmd := ParseJVMFlagsWithSources(proc.CmdLine, proc.EnvJavaOpts)
	m.FlagsExtracted = flags
	m.FlagSources = sources
	m.RawCmdline = effectiveCmd

	return m
}
