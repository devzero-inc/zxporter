package nodemon_test

import (
	"context"
	"testing"

	"github.com/go-logr/zapr"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakedynamic "k8s.io/client-go/dynamic/fake"

	"github.com/devzero-inc/zxporter/internal/nodemon"
)

// mockScraper implements nodemon.Scraper for testing.
type mockScraper struct {
	result []nodemon.MetricFamilyMap
	err    error
	urls   []string
}

func (m *mockScraper) Scrape(
	_ context.Context,
	urls []string,
) ([]nodemon.MetricFamilyMap, error) {
	m.urls = urls
	return m.result, m.err
}

// mockMapper implements nodemon.MetricMapper for testing.
type mockMapper struct {
	result []nodemon.GPUMetric
	input  []nodemon.MetricFamilyMap
}

func (m *mockMapper) MapToGPUMetrics(
	_ context.Context,
	metrics []nodemon.MetricFamilyMap,
) []nodemon.GPUMetric {
	m.input = metrics
	return m.result
}

func TestExporter_QueryMetrics(t *testing.T) {
	zapLog, _ := zap.NewDevelopment()
	log := zapr.NewLogger(zapLog)

	t.Run("discovers pods and returns mapped metrics", func(t *testing.T) {
		ctx := context.Background()
		r := require.New(t)

		scheme := runtime.NewScheme()
		_ = corev1.AddToScheme(scheme)
		dynClient := fakedynamic.NewSimpleDynamicClient(scheme, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "dcgm-exporter",
				Namespace: "default",
				Labels:    map[string]string{"app": "dcgm-exporter"},
			},
			Status: corev1.PodStatus{
				PodIP: "192.168.1.1",
				Phase: corev1.PodRunning,
			},
		})

		metricFamilies := []nodemon.MetricFamilyMap{
			{
				nodemon.MetricGPUUtilization: {
					Type: dto.MetricType_GAUGE.Enum(),
					Metric: []*dto.Metric{
						{
							Label: []*dto.LabelPair{
								newLabelPair("pod", "train-pod"),
							},
							Gauge: newGauge(85.0),
						},
					},
				},
			},
		}

		expectedGPUMetrics := []nodemon.GPUMetric{
			{Pod: "train-pod", GPUUtilization: 85.0},
		}

		scraper := &mockScraper{result: metricFamilies}
		mapper := &mockMapper{result: expectedGPUMetrics}

		ex := nodemon.NewExporter(nodemon.ExporterConfig{
			DCGMPort:            9400,
			DCGMMetricsEndpoint: "/metrics",
			DCGMLabels:          "app=dcgm-exporter",
		}, dynClient, scraper, mapper, log)

		result, err := ex.QueryMetrics(ctx)
		r.NoError(err)
		r.Len(result, 1)
		r.Equal("train-pod", result[0].Pod)
		r.Equal(85.0, result[0].GPUUtilization)
	})

	t.Run("single host mode uses configured host", func(t *testing.T) {
		ctx := context.Background()
		r := require.New(t)

		scheme := runtime.NewScheme()
		_ = corev1.AddToScheme(scheme)
		dynClient := fakedynamic.NewSimpleDynamicClient(scheme)

		metricFamilies := []nodemon.MetricFamilyMap{
			{
				nodemon.MetricGPUTemperature: {
					Type: dto.MetricType_GAUGE.Enum(),
					Metric: []*dto.Metric{
						{
							Label: []*dto.LabelPair{},
							Gauge: newGauge(72.0),
						},
					},
				},
			},
		}

		expectedGPUMetrics := []nodemon.GPUMetric{
			{Temperature: 72.0},
		}

		scraper := &mockScraper{result: metricFamilies}
		mapper := &mockMapper{result: expectedGPUMetrics}

		ex := nodemon.NewExporter(nodemon.ExporterConfig{
			DCGMPort:            9400,
			DCGMMetricsEndpoint: "/metrics",
			DCGMHost:            "localhost",
		}, dynClient, scraper, mapper, log)

		result, err := ex.QueryMetrics(ctx)
		r.NoError(err)
		r.Len(result, 1)
		r.Equal(72.0, result[0].Temperature)
		r.Equal([]string{"http://localhost:9400/metrics"}, scraper.urls)
	})

	t.Run("no DCGM URLs returns nil", func(t *testing.T) {
		ctx := context.Background()
		r := require.New(t)

		scheme := runtime.NewScheme()
		_ = corev1.AddToScheme(scheme)
		dynClient := fakedynamic.NewSimpleDynamicClient(scheme)

		scraper := &mockScraper{}
		mapper := &mockMapper{}

		ex := nodemon.NewExporter(nodemon.ExporterConfig{
			DCGMPort:            9400,
			DCGMMetricsEndpoint: "/metrics",
			DCGMLabels:          "app=dcgm-exporter",
		}, dynClient, scraper, mapper, log)

		result, err := ex.QueryMetrics(ctx)
		r.NoError(err)
		r.Nil(result)
	})

	t.Run("empty scrape result returns nil", func(t *testing.T) {
		ctx := context.Background()
		r := require.New(t)

		scheme := runtime.NewScheme()
		_ = corev1.AddToScheme(scheme)
		dynClient := fakedynamic.NewSimpleDynamicClient(scheme)

		scraper := &mockScraper{result: []nodemon.MetricFamilyMap{}}
		mapper := &mockMapper{}

		ex := nodemon.NewExporter(nodemon.ExporterConfig{
			DCGMPort:            9400,
			DCGMMetricsEndpoint: "/metrics",
			DCGMHost:            "localhost",
		}, dynClient, scraper, mapper, log)

		result, err := ex.QueryMetrics(ctx)
		r.NoError(err)
		r.Nil(result)
	})
}
