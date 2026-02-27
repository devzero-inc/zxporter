package collector

import (
	"testing"
)

// --- helpers ---

func assertFloat(t *testing.T, m map[string]interface{}, key string, want float64) {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Errorf("key %q not found in metrics map", key)
		return
	}
	got, ok := v.(float64)
	if !ok {
		t.Errorf("key %q: expected float64, got %T", key, v)
		return
	}
	// Allow tiny floating point tolerance
	diff := got - want
	if diff < 0 {
		diff = -diff
	}
	if diff > 0.001 {
		t.Errorf("key %q: got %.4f, want %.4f", key, got, want)
	}
}

func assertInt64(t *testing.T, m map[string]interface{}, key string, want int64) {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Errorf("key %q not found in metrics map", key)
		return
	}
	got, ok := v.(int64)
	if !ok {
		t.Errorf("key %q: expected int64, got %T (%v)", key, v, v)
		return
	}
	if got != want {
		t.Errorf("key %q: got %d, want %d", key, got, want)
	}
}

func assertStringSliceLen(t *testing.T, m map[string]interface{}, key string, wantLen int) {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Errorf("key %q not found", key)
		return
	}
	sl, ok := v.([]string)
	if !ok {
		t.Errorf("key %q: expected []string, got %T", key, v)
		return
	}
	if len(sl) != wantLen {
		t.Errorf("key %q: got len %d, want %d (values: %v)", key, len(sl), wantLen, sl)
	}
}

// --- makeGPU is a builder for test GPU metrics ---

func makeGPU(
	node, model, device, uuid, pod, container, ns string,
	util, fbUsed, fbFree, power, temp, smClk, memClk float64,
) GPUExporterMetric {
	return GPUExporterMetric{
		NodeName:        node,
		ModelName:       model,
		Device:          device,
		DeviceUUID:      uuid,
		Pod:             pod,
		Container:       container,
		Namespace:       ns,
		GPUUtilization:  util,
		FramebufferUsed: fbUsed,
		FramebufferFree: fbFree,
		PowerUsage:      power,
		Temperature:     temp,
		SMClock:         smClk,
		MemClock:        memClk,
	}
}

func TestIndexByContainer_Empty(t *testing.T) {
	idx := IndexByContainer(nil)
	if len(idx) != 0 {
		t.Errorf("expected empty index, got %d entries", len(idx))
	}
}

func TestIndexByContainer_SingleGPU_SingleContainer(t *testing.T) {
	metrics := []GPUExporterMetric{
		makeGPU("node1", "T4", "nvidia0", "UUID-1", "pod-a", "vllm", "default",
			50, 1000, 14000, 30, 40, 1200, 800),
	}

	idx := IndexByContainer(metrics)

	key := gpuContainerKey{Pod: "pod-a", Container: "vllm", Namespace: "default"}
	gpus, ok := idx[key]
	if !ok {
		t.Fatal("expected key to exist in index")
	}
	if len(gpus) != 1 {
		t.Errorf("expected 1 GPU for container, got %d", len(gpus))
	}
}

func TestIndexByContainer_MultiGPU_SingleContainer(t *testing.T) {
	metrics := []GPUExporterMetric{
		makeGPU("node1", "A100", "nvidia0", "UUID-1", "pod-a", "trainer", "ml",
			80, 20000, 20000, 250, 65, 1400, 1200),
		makeGPU("node1", "A100", "nvidia1", "UUID-2", "pod-a", "trainer", "ml",
			90, 25000, 15000, 280, 70, 1500, 1200),
	}

	idx := IndexByContainer(metrics)

	key := gpuContainerKey{Pod: "pod-a", Container: "trainer", Namespace: "ml"}
	gpus := idx[key]
	if len(gpus) != 2 {
		t.Errorf("expected 2 GPUs grouped for same container, got %d", len(gpus))
	}
}

func TestIndexByContainer_MultipleContainers(t *testing.T) {
	metrics := []GPUExporterMetric{
		makeGPU("node1", "T4", "nvidia0", "UUID-1", "pod-a", "vllm", "ns1",
			50, 1000, 14000, 30, 40, 1200, 800),
		makeGPU("node1", "T4", "nvidia0", "UUID-1", "pod-b", "inference", "ns2",
			20, 500, 14500, 15, 35, 1000, 800),
	}

	idx := IndexByContainer(metrics)

	if len(idx) != 2 {
		t.Errorf("expected 2 container groups, got %d", len(idx))
	}

	key1 := gpuContainerKey{Pod: "pod-a", Container: "vllm", Namespace: "ns1"}
	key2 := gpuContainerKey{Pod: "pod-b", Container: "inference", Namespace: "ns2"}

	if _, ok := idx[key1]; !ok {
		t.Error("missing key for pod-a/vllm")
	}
	if _, ok := idx[key2]; !ok {
		t.Error("missing key for pod-b/inference")
	}
}

func TestIndexByContainer_NoContainerLabel(t *testing.T) {
	// GPUs not assigned to any container (idle/unassigned)
	metrics := []GPUExporterMetric{
		makeGPU("node1", "T4", "nvidia0", "UUID-1", "", "", "",
			0, 0, 15000, 10, 25, 300, 405),
	}

	idx := IndexByContainer(metrics)

	key := gpuContainerKey{Pod: "", Container: "", Namespace: ""}
	gpus, ok := idx[key]
	if !ok {
		t.Fatal("expected empty-key entry for unassigned GPU")
	}
	if len(gpus) != 1 {
		t.Errorf("expected 1 GPU, got %d", len(gpus))
	}
}

func TestContainerGPUMetricsFromExporter_Empty(t *testing.T) {
	m := ContainerGPUMetricsFromExporter(nil, 0, 0)
	if len(m) != 0 {
		t.Errorf("expected empty map for nil input, got %d keys", len(m))
	}

	m = ContainerGPUMetricsFromExporter([]GPUExporterMetric{}, 1, 1)
	if len(m) != 0 {
		t.Errorf("expected empty map for empty slice, got %d keys", len(m))
	}
}

func TestContainerGPUMetricsFromExporter_SingleGPU(t *testing.T) {
	gpus := []GPUExporterMetric{
		makeGPU("node1", "Tesla T4", "nvidia0", "UUID-A", "pod-1", "vllm", "default",
			45.0, 3000.0, 12095.0, 35.5, 38.0, 1350.0, 810.0),
	}

	m := ContainerGPUMetricsFromExporter(gpus, 1, 1)

	// With 1 GPU, avg = the single value
	assertFloat(t, m, "GPUMetricsCount", 1.0)
	assertFloat(t, m, "GPUUtilizationPercentage", 45.0) // avg of 1 = 45
	assertFloat(t, m, "GPUMemoryUsedMb", 3000.0)        // sum = 3000
	assertFloat(t, m, "GPUMemoryFreeMb", 12095.0)       // sum = 12095
	assertFloat(t, m, "GPUTotalMemoryMb", 15095.0)      // 3000 + 12095
	assertFloat(t, m, "GPUPowerUsageWatts", 35.5)       // sum = 35.5
	assertFloat(t, m, "GPUTemperatureCelsius", 38.0)    // avg = 38
	assertFloat(t, m, "GPUSMClockMHz", 1350.0)          // avg = 1350
	assertFloat(t, m, "GPUMemClockMHz", 810.0)          // avg = 810
	assertFloat(t, m, "GPUUsage", 0.45)                 // (45 * 1) / 100

	assertInt64(t, m, "GPURequestCount", 1)
	assertInt64(t, m, "GPULimitCount", 1)
	assertStringSliceLen(t, m, "GPUUUIDs", 1)
	assertStringSliceLen(t, m, "GPUModels", 1)

	// Verify individual GPU data
	indiv := m["IndividualGPUs"].([]map[string]interface{})
	if len(indiv) != 1 {
		t.Fatalf("expected 1 individual GPU, got %d", len(indiv))
	}
	if indiv[0]["UUID"] != "UUID-A" {
		t.Errorf("expected UUID-A, got %v", indiv[0]["UUID"])
	}
}

func TestContainerGPUMetricsFromExporter_MultiGPU(t *testing.T) {
	gpus := []GPUExporterMetric{
		makeGPU("node1", "A100", "nvidia0", "UUID-1", "pod-1", "trainer", "ml",
			80.0, 20000.0, 20000.0, 250.0, 65.0, 1400.0, 1200.0),
		makeGPU("node1", "A100", "nvidia1", "UUID-2", "pod-1", "trainer", "ml",
			60.0, 25000.0, 15000.0, 200.0, 70.0, 1300.0, 1100.0),
		makeGPU("node1", "A100", "nvidia2", "UUID-3", "pod-1", "trainer", "ml",
			90.0, 30000.0, 10000.0, 300.0, 75.0, 1500.0, 1300.0),
	}

	m := ContainerGPUMetricsFromExporter(gpus, 3, 3)

	assertFloat(t, m, "GPUMetricsCount", 3.0)

	// Averages: (80+60+90)/3 = 76.667
	assertFloat(t, m, "GPUUtilizationPercentage", 230.0/3.0)

	// Sums: 20k+25k+30k = 75000
	assertFloat(t, m, "GPUMemoryUsedMb", 75000.0)
	assertFloat(t, m, "GPUMemoryFreeMb", 45000.0)   // 20k+15k+10k
	assertFloat(t, m, "GPUTotalMemoryMb", 120000.0) // 75k+45k

	// Power sum: 250+200+300 = 750
	assertFloat(t, m, "GPUPowerUsageWatts", 750.0)

	// Temp avg: (65+70+75)/3 = 70
	assertFloat(t, m, "GPUTemperatureCelsius", 70.0)

	// SM Clock avg: (1400+1300+1500)/3 = 1400
	assertFloat(t, m, "GPUSMClockMHz", 4200.0/3.0)

	// GPUUsage: (avgUtil * count)/100 = (76.667 * 3)/100 = 2.3
	assertFloat(t, m, "GPUUsage", 230.0/100.0)

	assertStringSliceLen(t, m, "GPUUUIDs", 3)
	assertStringSliceLen(t, m, "GPUModels", 1) // all same model → 1 entry "3x A100"

	assertInt64(t, m, "GPURequestCount", 3)
	assertInt64(t, m, "GPULimitCount", 3)
}

func TestContainerGPUMetricsFromExporter_MixedModels(t *testing.T) {
	gpus := []GPUExporterMetric{
		makeGPU("node1", "A100", "nvidia0", "UUID-1", "pod-1", "c1", "ns",
			50, 10000, 30000, 200, 60, 1400, 1200),
		makeGPU("node1", "V100", "nvidia1", "UUID-2", "pod-1", "c1", "ns",
			40, 8000, 8000, 150, 55, 1200, 900),
	}

	m := ContainerGPUMetricsFromExporter(gpus, 2, 2)

	// 2 different models → 2 model summary entries
	assertStringSliceLen(t, m, "GPUModels", 2)
	assertStringSliceLen(t, m, "GPUUUIDs", 2)
	assertFloat(t, m, "GPUMetricsCount", 2.0)
}

func TestContainerGPUMetricsFromExporter_ZeroUtilization(t *testing.T) {
	gpus := []GPUExporterMetric{
		makeGPU("node1", "T4", "nvidia0", "UUID-1", "pod-1", "idle", "ns",
			0, 0, 15095, 14, 25, 300, 405),
	}

	m := ContainerGPUMetricsFromExporter(gpus, 1, 1)

	assertFloat(t, m, "GPUUtilizationPercentage", 0.0)
	assertFloat(t, m, "GPUMemoryUsedMb", 0.0)
	assertFloat(t, m, "GPUMemoryFreeMb", 15095.0)
	assertFloat(t, m, "GPUUsage", 0.0)
}

func TestNodeGPUMetricsFromExporter_Empty(t *testing.T) {
	m := NodeGPUMetricsFromExporter(nil)
	if len(m) != 0 {
		t.Errorf("expected empty map for nil input, got %d keys", len(m))
	}

	m = NodeGPUMetricsFromExporter([]GPUExporterMetric{})
	if len(m) != 0 {
		t.Errorf("expected empty map for empty slice, got %d keys", len(m))
	}
}

func TestNodeGPUMetricsFromExporter_SingleGPU(t *testing.T) {
	gpus := []GPUExporterMetric{
		makeGPU("node1", "T4", "nvidia0", "UUID-1", "pod-a", "vllm", "default",
			30.0, 2000.0, 13095.0, 14.5, 38.0, 1200.0, 810.0),
	}

	m := NodeGPUMetricsFromExporter(gpus)

	assertFloat(t, m, "GPUCount", 1.0)
	assertFloat(t, m, "GPUUtilizationAvg", 30.0)
	assertFloat(t, m, "GPUUtilizationMax", 30.0) // single GPU: max = avg
	assertFloat(t, m, "GPUMemoryUsedTotal", 2000.0)
	assertFloat(t, m, "GPUMemoryFreeTotal", 13095.0)
	assertFloat(t, m, "GPUMemoryTotalMb", 15095.0)
	assertFloat(t, m, "GPUPowerUsageTotal", 14.5)
	assertFloat(t, m, "GPUTemperatureAvg", 38.0)
	assertFloat(t, m, "GPUTemperatureMax", 38.0) // single GPU: max = avg
	assertFloat(t, m, "GPUUsage", 30.0/100.0)

	assertStringSliceLen(t, m, "GPUUUIDs", 1)
	assertStringSliceLen(t, m, "GPUModels", 1)
}

func TestNodeGPUMetricsFromExporter_MultiGPU_AvgMaxSum(t *testing.T) {
	gpus := []GPUExporterMetric{
		makeGPU("node1", "A100", "nvidia0", "UUID-1", "", "", "",
			40.0, 10000.0, 30000.0, 200.0, 60.0, 1400.0, 1200.0),
		makeGPU("node1", "A100", "nvidia1", "UUID-2", "", "", "",
			80.0, 20000.0, 20000.0, 300.0, 75.0, 1500.0, 1200.0),
	}

	m := NodeGPUMetricsFromExporter(gpus)

	assertFloat(t, m, "GPUCount", 2.0)

	// Avg: (40+80)/2 = 60
	assertFloat(t, m, "GPUUtilizationAvg", 60.0)
	// Max: 80
	assertFloat(t, m, "GPUUtilizationMax", 80.0)

	// Sum memory: 10k+20k = 30k used, 30k+20k = 50k free
	assertFloat(t, m, "GPUMemoryUsedTotal", 30000.0)
	assertFloat(t, m, "GPUMemoryFreeTotal", 50000.0)
	assertFloat(t, m, "GPUMemoryTotalMb", 80000.0)

	// Sum power: 200+300 = 500
	assertFloat(t, m, "GPUPowerUsageTotal", 500.0)

	// Avg temp: (60+75)/2 = 67.5
	assertFloat(t, m, "GPUTemperatureAvg", 67.5)
	// Max temp: 75
	assertFloat(t, m, "GPUTemperatureMax", 75.0)

	// GPUUsage: totalUtil/100 = (40+80)/100 = 1.2
	assertFloat(t, m, "GPUUsage", 1.2)

	assertStringSliceLen(t, m, "GPUUUIDs", 2)
	assertStringSliceLen(t, m, "GPUModels", 1) // same model
}

func TestNodeGPUMetricsFromExporter_FourGPUs_MixedModels(t *testing.T) {
	gpus := []GPUExporterMetric{
		makeGPU("node1", "A100", "nvidia0", "UUID-1", "", "", "",
			100.0, 40000.0, 0.0, 400.0, 80.0, 1500.0, 1200.0),
		makeGPU("node1", "A100", "nvidia1", "UUID-2", "", "", "",
			10.0, 5000.0, 35000.0, 100.0, 45.0, 800.0, 1200.0),
		makeGPU("node1", "V100", "nvidia2", "UUID-3", "", "", "",
			50.0, 8000.0, 8000.0, 200.0, 60.0, 1200.0, 900.0),
		makeGPU("node1", "V100", "nvidia3", "UUID-4", "", "", "",
			70.0, 12000.0, 4000.0, 250.0, 70.0, 1300.0, 900.0),
	}

	m := NodeGPUMetricsFromExporter(gpus)

	assertFloat(t, m, "GPUCount", 4.0)

	// Avg util: (100+10+50+70)/4 = 57.5
	assertFloat(t, m, "GPUUtilizationAvg", 57.5)
	// Max util: 100
	assertFloat(t, m, "GPUUtilizationMax", 100.0)

	// Sum mem used: 40k+5k+8k+12k = 65k
	assertFloat(t, m, "GPUMemoryUsedTotal", 65000.0)
	// Sum mem free: 0+35k+8k+4k = 47k
	assertFloat(t, m, "GPUMemoryFreeTotal", 47000.0)

	// Sum power: 400+100+200+250 = 950
	assertFloat(t, m, "GPUPowerUsageTotal", 950.0)

	// Avg temp: (80+45+60+70)/4 = 63.75
	assertFloat(t, m, "GPUTemperatureAvg", 63.75)
	// Max temp: 80
	assertFloat(t, m, "GPUTemperatureMax", 80.0)

	// GPUUsage: (100+10+50+70)/100 = 2.3
	assertFloat(t, m, "GPUUsage", 2.3)

	assertStringSliceLen(t, m, "GPUUUIDs", 4)
	assertStringSliceLen(t, m, "GPUModels", 2) // A100 + V100
}

func TestNodeGPUMetricsFromExporter_MemoryTemperature(t *testing.T) {
	gpus := []GPUExporterMetric{
		{
			NodeName: "node1", ModelName: "A100", Device: "nvidia0",
			DeviceUUID: "UUID-1", Temperature: 65, MemoryTemperature: 80,
			GPUUtilization: 50, FramebufferUsed: 10000, FramebufferFree: 30000,
			PowerUsage: 200, SMClock: 1400, MemClock: 1200,
		},
		{
			NodeName: "node1", ModelName: "A100", Device: "nvidia1",
			DeviceUUID: "UUID-2", Temperature: 70, MemoryTemperature: 90,
			GPUUtilization: 60, FramebufferUsed: 15000, FramebufferFree: 25000,
			PowerUsage: 250, SMClock: 1500, MemClock: 1200,
		},
	}

	m := NodeGPUMetricsFromExporter(gpus)

	// Memory temp avg: (80+90)/2 = 85
	assertFloat(t, m, "GPUMemoryTemperatureAvg", 85.0)
	// Memory temp max: 90
	assertFloat(t, m, "GPUMemoryTemperatureMax", 90.0)
}

func TestNodeGPUMetricsFromExporter_ProfilingMetrics(t *testing.T) {
	gpus := []GPUExporterMetric{
		{
			NodeName: "node1", ModelName: "A100", Device: "nvidia0",
			DeviceUUID: "UUID-1", GPUUtilization: 50,
			FramebufferUsed: 10000, FramebufferFree: 30000,
			PowerUsage: 200, Temperature: 65, SMClock: 1400, MemClock: 1200,
			TensorActive: 0.75, DRAMActive: 0.30,
			PCIeTXBytes: 1000000, PCIeRXBytes: 2000000,
			GraphicsEngineActive: 0.60,
		},
		{
			NodeName: "node1", ModelName: "A100", Device: "nvidia1",
			DeviceUUID: "UUID-2", GPUUtilization: 60,
			FramebufferUsed: 15000, FramebufferFree: 25000,
			PowerUsage: 250, Temperature: 70, SMClock: 1500, MemClock: 1200,
			TensorActive: 0.85, DRAMActive: 0.50,
			PCIeTXBytes: 3000000, PCIeRXBytes: 4000000,
			GraphicsEngineActive: 0.40,
		},
	}

	m := NodeGPUMetricsFromExporter(gpus)

	// TensorActive avg: (0.75+0.85)/2 = 0.80
	assertFloat(t, m, "GPUTensorUtilizationAvg", 0.80)
	// DRAMActive avg: (0.30+0.50)/2 = 0.40
	assertFloat(t, m, "GPUDramUtilizationAvg", 0.40)
	// PCIe sum: 1M+3M = 4M, 2M+4M = 6M
	assertFloat(t, m, "GPUPCIeTxBytesTotal", 4000000.0)
	assertFloat(t, m, "GPUPCIeRxBytesTotal", 6000000.0)
	// Graphics avg: (0.60+0.40)/2 = 0.50
	assertFloat(t, m, "GPUGraphicsUtilizationAvg", 0.50)
}
