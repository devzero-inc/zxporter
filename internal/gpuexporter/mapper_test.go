package gpuexporter_test

import (
	"context"
	"testing"

	"github.com/go-logr/zapr"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devzero-inc/zxporter/internal/gpuexporter"
)

func newGauge(value float64) *dto.Gauge {
	return &dto.Gauge{
		Value: &value,
	}
}

func newLabelPair(name, value string) *dto.LabelPair {
	return &dto.LabelPair{
		Name:  &name,
		Value: &value,
	}
}

func TestMetricMapper_MapToGPUMetrics(t *testing.T) {
	zapLog, _ := zap.NewDevelopment()
	log := zapr.NewLogger(zapLog)
	ctx := context.Background()

	t.Run("empty input yields empty slice", func(t *testing.T) {
		mapper := gpuexporter.NewMapper("test-node", nil, log)

		got := mapper.MapToGPUMetrics(ctx, []gpuexporter.MetricFamilyMap{})

		r := require.New(t)
		r.Empty(got)
	})

	t.Run("non-enabled metric is skipped", func(t *testing.T) {
		mapper := gpuexporter.NewMapper("test-node", nil, log)

		metricFamilyMaps := []gpuexporter.MetricFamilyMap{
			{
				"some_unknown_metric": {
					Type: dto.MetricType_GAUGE.Enum(),
					Metric: []*dto.Metric{
						{
							Label: []*dto.LabelPair{
								newLabelPair("label1", "value1"),
							},
							Gauge: newGauge(1.0),
						},
					},
				},
			},
		}

		got := mapper.MapToGPUMetrics(ctx, metricFamilyMaps)

		r := require.New(t)
		r.Empty(got)
	})

	t.Run("enabled metric is mapped with correct value", func(t *testing.T) {
		mapper := gpuexporter.NewMapper("test-node", nil, log)

		metricFamilyMaps := []gpuexporter.MetricFamilyMap{
			{
				gpuexporter.MetricGPUUtilization: {
					Type: dto.MetricType_GAUGE.Enum(),
					Metric: []*dto.Metric{
						{
							Label: []*dto.LabelPair{
								newLabelPair("device", "nvidia0"),
								newLabelPair("gpu", "0"),
							},
							Gauge: newGauge(85.5),
						},
					},
				},
			},
		}

		got := mapper.MapToGPUMetrics(ctx, metricFamilyMaps)

		r := require.New(t)
		r.Len(got, 1)
		r.Equal("test-node", got[0].NodeName)
		r.Equal("nvidia0", got[0].Device)
		r.Equal(85.5, got[0].GPUUtilization)
	})

	t.Run("multiple metrics for same GPU are aggregated", func(t *testing.T) {
		mapper := gpuexporter.NewMapper("test-node", nil, log)

		metricFamilyMaps := []gpuexporter.MetricFamilyMap{
			{
				gpuexporter.MetricGPUUtilization: {
					Type: dto.MetricType_GAUGE.Enum(),
					Metric: []*dto.Metric{
						{
							Label: []*dto.LabelPair{
								newLabelPair("device", "nvidia0"),
								newLabelPair("pod", "train-pod"),
								newLabelPair("namespace", "ml"),
							},
							Gauge: newGauge(85.0),
						},
					},
				},
				gpuexporter.MetricGPUTemperature: {
					Type: dto.MetricType_GAUGE.Enum(),
					Metric: []*dto.Metric{
						{
							Label: []*dto.LabelPair{
								newLabelPair("device", "nvidia0"),
								newLabelPair("pod", "train-pod"),
								newLabelPair("namespace", "ml"),
							},
							Gauge: newGauge(72.0),
						},
					},
				},
			},
		}

		got := mapper.MapToGPUMetrics(ctx, metricFamilyMaps)

		r := require.New(t)
		r.Len(got, 1)
		r.Equal(85.0, got[0].GPUUtilization)
		r.Equal(72.0, got[0].Temperature)
		r.Equal("train-pod", got[0].Pod)
	})
}
