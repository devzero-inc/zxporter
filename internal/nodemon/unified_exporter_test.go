package nodemon

import (
	"math"
	"testing"
	"time"
)

const (
	testNamespace     = "ns"
	testPodName       = "pod1"
	testNodeName      = "node1"
	testContainerName = "train"
)

func TestIndexGPUMetrics_MultiGPU(t *testing.T) {
	metrics := []GPUMetric{
		{Namespace: testNamespace, Pod: testPodName, Container: testContainerName, Device: "0", GPUUtilization: 80, FramebufferUsed: 1000, FramebufferFree: 500, PowerUsage: 200, Temperature: 70},
		{Namespace: testNamespace, Pod: testPodName, Container: testContainerName, Device: "1", GPUUtilization: 60, FramebufferUsed: 800, FramebufferFree: 700, PowerUsage: 180, Temperature: 65},
		{Namespace: testNamespace, Pod: testPodName, Container: testContainerName, Device: "2", GPUUtilization: 90, FramebufferUsed: 1200, FramebufferFree: 300, PowerUsage: 250, Temperature: 75},
		{Namespace: testNamespace, Pod: testPodName, Container: testContainerName, Device: "3", GPUUtilization: 70, FramebufferUsed: 900, FramebufferFree: 600, PowerUsage: 190, Temperature: 68},
		{Namespace: testNamespace, Pod: testPodName, Container: "sidecar", Device: "0", GPUUtilization: 10, FramebufferUsed: 100, FramebufferFree: 1400, PowerUsage: 50, Temperature: 40},
	}

	idx := indexGPUMetrics(metrics)

	trainKey := testNamespace + "/" + testPodName + "/" + testContainerName
	if len(idx[trainKey]) != 4 {
		t.Fatalf("expected 4 GPUs for train container, got %d", len(idx[trainKey]))
	}
	sidecarKey := testNamespace + "/" + testPodName + "/sidecar"
	if len(idx[sidecarKey]) != 1 {
		t.Fatalf("expected 1 GPU for sidecar container, got %d", len(idx[sidecarKey]))
	}
}

func TestIndexGPUMetrics_SingleGPU(t *testing.T) {
	metrics := []GPUMetric{
		{Namespace: testNamespace, Pod: testPodName, Container: "app", Device: "0", GPUUtilization: 50},
	}

	idx := indexGPUMetrics(metrics)
	key := testNamespace + "/" + testPodName + "/app"
	if len(idx[key]) != 1 {
		t.Fatalf("expected 1 GPU, got %d", len(idx[key]))
	}
}

func TestIndexGPUMetrics_Empty(t *testing.T) {
	idx := indexGPUMetrics(nil)
	if len(idx) != 0 {
		t.Fatalf("expected empty index, got %d entries", len(idx))
	}
}

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < 0.01
}

func TestBuildSingleContainerMetric_MultiGPUAggregation(t *testing.T) {
	u := &UnifiedExporter{nodeName: testNodeName}

	pod := PodStats{}
	pod.PodRef.Name = testPodName
	pod.PodRef.Namespace = testNamespace

	container := ContainerStats{Name: testContainerName}

	gpuKey := testNamespace + "/" + testPodName + "/" + testContainerName
	gpuIndex := map[string][]*GPUMetric{
		gpuKey: {
			{GPUUtilization: 80, FramebufferUsed: 1000, FramebufferFree: 500, PowerUsage: 200, Temperature: 70},
			{GPUUtilization: 60, FramebufferUsed: 800, FramebufferFree: 700, PowerUsage: 180, Temperature: 65},
			{GPUUtilization: 90, FramebufferUsed: 1200, FramebufferFree: 300, PowerUsage: 250, Temperature: 75},
			{GPUUtilization: 70, FramebufferUsed: 900, FramebufferFree: 600, PowerUsage: 190, Temperature: 68},
		},
	}

	now := time.Now()
	resp := u.buildSingleContainerMetric(pod, container, nil, gpuIndex, 0, 0, now)

	if resp.GPUDeviceCount != 4 {
		t.Errorf("GPUDeviceCount: want 4, got %d", resp.GPUDeviceCount)
	}
	if !almostEqual(resp.GPUUtilization, 75.0) {
		t.Errorf("GPUUtilization: want 75.0, got %f", resp.GPUUtilization)
	}
	if !almostEqual(resp.GPUMemoryUsedMiB, 3900.0) {
		t.Errorf("GPUMemoryUsedMiB: want 3900.0, got %f", resp.GPUMemoryUsedMiB)
	}
	if !almostEqual(resp.GPUMemoryFreeMiB, 2100.0) {
		t.Errorf("GPUMemoryFreeMiB: want 2100.0, got %f", resp.GPUMemoryFreeMiB)
	}
	if !almostEqual(resp.GPUPowerWatts, 820.0) {
		t.Errorf("GPUPowerWatts: want 820.0, got %f", resp.GPUPowerWatts)
	}
	if !almostEqual(resp.GPUTemperature, 69.5) {
		t.Errorf("GPUTemperature: want 69.5, got %f", resp.GPUTemperature)
	}
}

func TestBuildSingleContainerMetric_SingleGPU(t *testing.T) {
	u := &UnifiedExporter{nodeName: testNodeName}

	pod := PodStats{}
	pod.PodRef.Name = testPodName
	pod.PodRef.Namespace = testNamespace

	container := ContainerStats{Name: "app"}

	gpuKey := testNamespace + "/" + testPodName + "/app"
	gpuIndex := map[string][]*GPUMetric{
		gpuKey: {
			{GPUUtilization: 50, FramebufferUsed: 2000, FramebufferFree: 1000, PowerUsage: 150, Temperature: 60},
		},
	}

	now := time.Now()
	resp := u.buildSingleContainerMetric(pod, container, nil, gpuIndex, 0, 0, now)

	if resp.GPUDeviceCount != 1 {
		t.Errorf("GPUDeviceCount: want 1, got %d", resp.GPUDeviceCount)
	}
	if !almostEqual(resp.GPUUtilization, 50.0) {
		t.Errorf("GPUUtilization: want 50.0, got %f", resp.GPUUtilization)
	}
	if !almostEqual(resp.GPUMemoryUsedMiB, 2000.0) {
		t.Errorf("GPUMemoryUsedMiB: want 2000.0, got %f", resp.GPUMemoryUsedMiB)
	}
	if !almostEqual(resp.GPUPowerWatts, 150.0) {
		t.Errorf("GPUPowerWatts: want 150.0, got %f", resp.GPUPowerWatts)
	}
}

func TestBuildSingleContainerMetric_NoGPU(t *testing.T) {
	u := &UnifiedExporter{nodeName: testNodeName}

	pod := PodStats{}
	pod.PodRef.Name = testPodName
	pod.PodRef.Namespace = testNamespace

	container := ContainerStats{Name: "web"}

	gpuIndex := map[string][]*GPUMetric{}

	now := time.Now()
	resp := u.buildSingleContainerMetric(pod, container, nil, gpuIndex, 0, 0, now)

	if resp.GPUDeviceCount != 0 {
		t.Errorf("GPUDeviceCount: want 0, got %d", resp.GPUDeviceCount)
	}
	if resp.GPUUtilization != 0 {
		t.Errorf("GPUUtilization: want 0, got %f", resp.GPUUtilization)
	}
}
