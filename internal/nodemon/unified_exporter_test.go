package nodemon

import (
	"math"
	"testing"
	"time"
)

func TestIndexGPUMetrics_MultiGPU(t *testing.T) {
	metrics := []GPUMetric{
		{Namespace: "ns", Pod: "pod1", Container: "train", Device: "0", GPUUtilization: 80, FramebufferUsed: 1000, FramebufferFree: 500, PowerUsage: 200, Temperature: 70},
		{Namespace: "ns", Pod: "pod1", Container: "train", Device: "1", GPUUtilization: 60, FramebufferUsed: 800, FramebufferFree: 700, PowerUsage: 180, Temperature: 65},
		{Namespace: "ns", Pod: "pod1", Container: "train", Device: "2", GPUUtilization: 90, FramebufferUsed: 1200, FramebufferFree: 300, PowerUsage: 250, Temperature: 75},
		{Namespace: "ns", Pod: "pod1", Container: "train", Device: "3", GPUUtilization: 70, FramebufferUsed: 900, FramebufferFree: 600, PowerUsage: 190, Temperature: 68},
		{Namespace: "ns", Pod: "pod1", Container: "sidecar", Device: "0", GPUUtilization: 10, FramebufferUsed: 100, FramebufferFree: 1400, PowerUsage: 50, Temperature: 40},
	}

	idx := indexGPUMetrics(metrics)

	if len(idx["ns/pod1/train"]) != 4 {
		t.Fatalf("expected 4 GPUs for train container, got %d", len(idx["ns/pod1/train"]))
	}
	if len(idx["ns/pod1/sidecar"]) != 1 {
		t.Fatalf("expected 1 GPU for sidecar container, got %d", len(idx["ns/pod1/sidecar"]))
	}
}

func TestIndexGPUMetrics_SingleGPU(t *testing.T) {
	metrics := []GPUMetric{
		{Namespace: "ns", Pod: "pod1", Container: "app", Device: "0", GPUUtilization: 50},
	}

	idx := indexGPUMetrics(metrics)
	if len(idx["ns/pod1/app"]) != 1 {
		t.Fatalf("expected 1 GPU, got %d", len(idx["ns/pod1/app"]))
	}
}

func TestIndexGPUMetrics_Empty(t *testing.T) {
	idx := indexGPUMetrics(nil)
	if len(idx) != 0 {
		t.Fatalf("expected empty index, got %d entries", len(idx))
	}
}

func almostEqual(a, b, epsilon float64) bool {
	return math.Abs(a-b) < epsilon
}

func TestBuildSingleContainerMetric_MultiGPUAggregation(t *testing.T) {
	u := &UnifiedExporter{nodeName: "node1"}

	pod := PodStats{}
	pod.PodRef.Name = "pod1"
	pod.PodRef.Namespace = "ns"

	container := ContainerStats{Name: "train"}

	gpuIndex := map[string][]*GPUMetric{
		"ns/pod1/train": {
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
	// Utilization: average of (80+60+90+70)/4 = 75
	if !almostEqual(resp.GPUUtilization, 75.0, 0.01) {
		t.Errorf("GPUUtilization: want 75.0, got %f", resp.GPUUtilization)
	}
	// Memory used: sum = 1000+800+1200+900 = 3900
	if !almostEqual(resp.GPUMemoryUsedMiB, 3900.0, 0.01) {
		t.Errorf("GPUMemoryUsedMiB: want 3900.0, got %f", resp.GPUMemoryUsedMiB)
	}
	// Memory free: sum = 500+700+300+600 = 2100
	if !almostEqual(resp.GPUMemoryFreeMiB, 2100.0, 0.01) {
		t.Errorf("GPUMemoryFreeMiB: want 2100.0, got %f", resp.GPUMemoryFreeMiB)
	}
	// Power: sum = 200+180+250+190 = 820
	if !almostEqual(resp.GPUPowerWatts, 820.0, 0.01) {
		t.Errorf("GPUPowerWatts: want 820.0, got %f", resp.GPUPowerWatts)
	}
	// Temperature: average of (70+65+75+68)/4 = 69.5
	if !almostEqual(resp.GPUTemperature, 69.5, 0.01) {
		t.Errorf("GPUTemperature: want 69.5, got %f", resp.GPUTemperature)
	}
}

func TestBuildSingleContainerMetric_SingleGPU(t *testing.T) {
	u := &UnifiedExporter{nodeName: "node1"}

	pod := PodStats{}
	pod.PodRef.Name = "pod1"
	pod.PodRef.Namespace = "ns"

	container := ContainerStats{Name: "app"}

	gpuIndex := map[string][]*GPUMetric{
		"ns/pod1/app": {
			{GPUUtilization: 50, FramebufferUsed: 2000, FramebufferFree: 1000, PowerUsage: 150, Temperature: 60},
		},
	}

	now := time.Now()
	resp := u.buildSingleContainerMetric(pod, container, nil, gpuIndex, 0, 0, now)

	if resp.GPUDeviceCount != 1 {
		t.Errorf("GPUDeviceCount: want 1, got %d", resp.GPUDeviceCount)
	}
	if !almostEqual(resp.GPUUtilization, 50.0, 0.01) {
		t.Errorf("GPUUtilization: want 50.0, got %f", resp.GPUUtilization)
	}
	if !almostEqual(resp.GPUMemoryUsedMiB, 2000.0, 0.01) {
		t.Errorf("GPUMemoryUsedMiB: want 2000.0, got %f", resp.GPUMemoryUsedMiB)
	}
	if !almostEqual(resp.GPUPowerWatts, 150.0, 0.01) {
		t.Errorf("GPUPowerWatts: want 150.0, got %f", resp.GPUPowerWatts)
	}
}

func TestBuildSingleContainerMetric_NoGPU(t *testing.T) {
	u := &UnifiedExporter{nodeName: "node1"}

	pod := PodStats{}
	pod.PodRef.Name = "pod1"
	pod.PodRef.Namespace = "ns"

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
