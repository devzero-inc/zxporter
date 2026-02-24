package gpuexporter

import (
	"context"

	"github.com/go-logr/logr"
	dto "github.com/prometheus/client_model/go"
)

const (
	nodeNameLabel  = "Hostname"
	modelNameLabel = "modelName"
	deviceLabel    = "device"
	podLabel       = "pod"
	containerLabel = "container"
	namespaceLabel = "namespace"
	gpuIDLabel     = "gpu"
	gpuUUIDLabel   = "UUID"
	gpuMIGProfile  = "GPU_I_PROFILE"
	gpuInstanceID  = "GPU_I_ID"
)

// MetricMapper maps scraped DCGM metric families into structured GPU metrics.
type MetricMapper interface {
	MapToGPUMetrics(ctx context.Context, metrics []MetricFamilyMap) []GPUMetric
}

// WorkloadResolver resolves the top-level owning workload for a pod.
type WorkloadResolver interface {
	FindWorkloadForPod(ctx context.Context, name, namespace string) (kind, workloadName string, err error)
}

type metricMapper struct {
	nodeName         string
	workloadResolver WorkloadResolver
	log              logr.Logger
}

type gpuMetricKey struct {
	device        string
	pod           string
	namespace     string
	container     string
	deviceID      string
	deviceUUID    string
	MIGProfile    string
	MIGInstanceID string
}

// NewMapper creates a new MetricMapper.
func NewMapper(nodeName string, resolver WorkloadResolver, log logr.Logger) MetricMapper {
	return &metricMapper{
		nodeName:         nodeName,
		workloadResolver: resolver,
		log:              log.WithName("gpu-mapper"),
	}
}

func getLabelValue(labels []*dto.LabelPair, name string) string {
	for _, lp := range labels {
		if lp.GetName() == name {
			return lp.GetValue()
		}
	}
	return ""
}

// MapToGPUMetrics maps scraped DCGM metric families into a flat GPUMetric slice.
func (p *metricMapper) MapToGPUMetrics(ctx context.Context, metricFamilyMaps []MetricFamilyMap) []GPUMetric {
	gpuMetrics := make(map[gpuMetricKey]*GPUMetric)

	for _, familyMap := range metricFamilyMaps {
		for name, family := range familyMap {
			if _, found := EnabledMetrics[name]; !found {
				continue
			}

			for _, m := range family.Metric {
				key := gpuMetricKey{
					device:        getLabelValue(m.Label, deviceLabel),
					pod:           getLabelValue(m.Label, podLabel),
					namespace:     getLabelValue(m.Label, namespaceLabel),
					container:     getLabelValue(m.Label, containerLabel),
					deviceID:      getLabelValue(m.Label, gpuIDLabel),
					deviceUUID:    getLabelValue(m.Label, gpuUUIDLabel),
					MIGProfile:    getLabelValue(m.Label, gpuMIGProfile),
					MIGInstanceID: getLabelValue(m.Label, gpuInstanceID),
				}

				gm, exists := gpuMetrics[key]
				if !exists {
					nodeName := getLabelValue(m.Label, nodeNameLabel)
					if p.nodeName != "" {
						nodeName = p.nodeName
					}

					gm = &GPUMetric{
						NodeName:      nodeName,
						ModelName:     getLabelValue(m.Label, modelNameLabel),
						Device:        key.device,
						DeviceID:      key.deviceID,
						DeviceUUID:    key.deviceUUID,
						MIGProfile:    key.MIGProfile,
						MIGInstanceID: key.MIGInstanceID,
						Pod:           key.pod,
						Container:     key.container,
						Namespace:     key.namespace,
					}

					if key.pod != "" && p.workloadResolver != nil {
						kind, wName, err := p.workloadResolver.FindWorkloadForPod(ctx, key.pod, key.namespace)
						if err != nil {
							p.log.Error(err, "Failed to resolve workload",
								"pod", key.pod, "namespace", key.namespace)
						} else {
							gm.WorkloadName = wName
							gm.WorkloadKind = kind
						}
					}

					gpuMetrics[key] = gm
				}

				var value float64
				switch family.Type.String() {
				case "COUNTER":
					value = *m.GetCounter().Value
				case "GAUGE":
					value = *m.GetGauge().Value
				}

				switch name {
				case MetricStreamingMultiProcessorActive:
					gm.SMActive = value
				case MetricStreamingMultiProcessorOccupancy:
					gm.SMOccupancy = value
				case MetricStreamingMultiProcessorTensor:
					gm.TensorActive = value
				case MetricDRAMActive:
					gm.DRAMActive = value
				case MetricPCIeTXBytes:
					gm.PCIeTXBytes = value
				case MetricPCIeRXBytes:
					gm.PCIeRXBytes = value
				case MetricNVLinkTXBytes:
					gm.NVLinkTXBytes = value
				case MetricNVLinkRXBytes:
					gm.NVLinkRXBytes = value
				case MetricGraphicsEngineActive:
					gm.GraphicsEngineActive = value
				case MetricFrameBufferTotal:
					gm.FramebufferTotal = value
				case MetricFrameBufferFree:
					gm.FramebufferFree = value
				case MetricFrameBufferUsed:
					gm.FramebufferUsed = value
				case MetricPCIeLinkGen:
					gm.PCIeLinkGen = value
				case MetricPCIeLinkWidth:
					gm.PCIeLinkWidth = value
				case MetricGPUTemperature:
					gm.Temperature = value
				case MetricMemoryTemperature:
					gm.MemoryTemperature = value
				case MetricPowerUsage:
					gm.PowerUsage = value
				case MetricGPUUtilization:
					gm.GPUUtilization = value
				case MetricIntPipeActive:
					gm.IntPipeActive = value
				case MetricFloat16PipeActive:
					gm.FP16PipeActive = value
				case MetricFloat32PipeActive:
					gm.FP32PipeActive = value
				case MetricFloat64PipeActive:
					gm.FP64PipeActive = value
				case MetricClocksEventReasons:
					gm.ClocksEventReasons = value
				case MetricXIDErrors:
					gm.XIDErrors = value
				case MetricPowerViolation:
					gm.PowerViolation = value
				case MetricThermalViolation:
					gm.ThermalViolation = value
				case MetricSMClock:
					gm.SMClock = value
				case MetricMemClock:
					gm.MemClock = value
				}
			}
		}
	}

	metrics := make([]GPUMetric, 0, len(gpuMetrics))
	for _, gm := range gpuMetrics {
		metrics = append(metrics, *gm)
	}

	return metrics
}
