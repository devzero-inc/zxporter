package collector

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func newTestPod(namespace, name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
}

func newSampleVector(value float64) model.Vector {
	return model.Vector{
		&model.Sample{
			Value:     model.SampleValue(value),
			Timestamp: model.TimeFromUnix(time.Now().Unix()),
		},
	}
}

func TestCollectPodNetworkMetrics_TransformsPrometheusResponse(t *testing.T) {
	pod := newTestPod("default", "web-abc")

	mock := &mockPrometheusAPI{
		queryResults: map[string]model.Value{
			`sum(rate(container_network_receive_bytes_total{namespace="default", pod="web-abc"}[5m]))`:          newSampleVector(1024.5),
			`sum(rate(container_network_transmit_bytes_total{namespace="default", pod="web-abc"}[5m]))`:         newSampleVector(2048.0),
			`sum(rate(container_network_receive_packets_total{namespace="default", pod="web-abc"}[5m]))`:        newSampleVector(300.0),
			`sum(rate(container_network_transmit_packets_total{namespace="default", pod="web-abc"}[5m]))`:       newSampleVector(400.0),
			`sum(rate(container_network_receive_errors_total{namespace="default", pod="web-abc"}[5m]))`:         newSampleVector(5.0),
			`sum(rate(container_network_transmit_errors_total{namespace="default", pod="web-abc"}[5m]))`:        newSampleVector(6.0),
			`sum(rate(container_network_receive_packets_dropped_total{namespace="default", pod="web-abc"}[5m]))`:  newSampleVector(7.0),
			`sum(rate(container_network_transmit_packets_dropped_total{namespace="default", pod="web-abc"}[5m]))`: newSampleVector(8.0),
		},
	}

	c := &ContainerResourceCollector{
		prometheusAPI: mock,
		logger:        logr.Discard(),
	}

	result := c.collectPodNetworkMetrics(context.Background(), pod)

	assert.InDelta(t, 1024.5, result["NetworkReceiveBytes"], 0.01)
	assert.InDelta(t, 2048.0, result["NetworkTransmitBytes"], 0.01)
	assert.InDelta(t, 300.0, result["NetworkReceivePackets"], 0.01)
	assert.InDelta(t, 400.0, result["NetworkTransmitPackets"], 0.01)
	assert.InDelta(t, 5.0, result["NetworkReceiveErrors"], 0.01)
	assert.InDelta(t, 6.0, result["NetworkTransmitErrors"], 0.01)
	assert.InDelta(t, 7.0, result["NetworkReceiveDropped"], 0.01)
	assert.InDelta(t, 8.0, result["NetworkTransmitDropped"], 0.01)
}

func TestCollectContainerIOMetrics_TransformsPrometheusResponse(t *testing.T) {
	pod := newTestPod("default", "web-abc")
	containerName := "nginx"

	mock := &mockPrometheusAPI{
		queryResults: map[string]model.Value{
			`sum(rate(container_fs_reads_bytes_total{namespace="default", pod="web-abc", container="nginx"}[5m]))`:  newSampleVector(512.0),
			`sum(rate(container_fs_writes_bytes_total{namespace="default", pod="web-abc", container="nginx"}[5m]))`: newSampleVector(1024.0),
			`sum(rate(container_fs_reads_total{namespace="default", pod="web-abc", container="nginx"}[5m]))`:        newSampleVector(10.0),
			`sum(rate(container_fs_writes_total{namespace="default", pod="web-abc", container="nginx"}[5m]))`:       newSampleVector(20.0),
		},
	}

	c := &ContainerResourceCollector{
		prometheusAPI: mock,
		logger:        logr.Discard(),
	}

	result := c.collectContainerIOMetrics(context.Background(), pod, containerName)

	assert.InDelta(t, 512.0, result["FSReadBytes"], 0.01)
	assert.InDelta(t, 1024.0, result["FSWriteBytes"], 0.01)
	assert.InDelta(t, 10.0, result["FSReads"], 0.01)
	assert.InDelta(t, 20.0, result["FSWrites"], 0.01)
}

func TestCollectContainerCPUThrottleMetrics_TransformsPrometheusResponse(t *testing.T) {
	pod := newTestPod("default", "web-abc")
	containerName := "nginx"

	mock := &mockPrometheusAPI{
		queryResults: map[string]model.Value{
			`sum(rate(container_cpu_cfs_throttled_periods_total{namespace="default", pod="web-abc", container="nginx"}[5m]))`: newSampleVector(30.0),
			`sum(rate(container_cpu_cfs_periods_total{namespace="default", pod="web-abc", container="nginx"}[5m]))`:           newSampleVector(100.0),
		},
	}

	c := &ContainerResourceCollector{
		prometheusAPI: mock,
		logger:        logr.Discard(),
	}

	fraction, err := c.collectContainerCPUThrottleMetrics(context.Background(), pod, containerName)

	assert.NoError(t, err)
	assert.InDelta(t, 0.30, fraction, 0.01)
}

func TestCollectContainerCPUThrottleMetrics_ZeroPeriods(t *testing.T) {
	pod := newTestPod("default", "web-abc")
	containerName := "nginx"

	// No matching query results → mock returns scalar 0 for both queries
	mock := &mockPrometheusAPI{
		queryResults: map[string]model.Value{},
	}

	c := &ContainerResourceCollector{
		prometheusAPI: mock,
		logger:        logr.Discard(),
	}

	fraction, err := c.collectContainerCPUThrottleMetrics(context.Background(), pod, containerName)

	assert.NoError(t, err)
	assert.InDelta(t, 0.0, fraction, 0.01)
}

func TestCollectContainerCPUThrottleMetrics_ClampedToOne(t *testing.T) {
	pod := newTestPod("default", "web-abc")
	containerName := "nginx"

	mock := &mockPrometheusAPI{
		queryResults: map[string]model.Value{
			`sum(rate(container_cpu_cfs_throttled_periods_total{namespace="default", pod="web-abc", container="nginx"}[5m]))`: newSampleVector(150.0),
			`sum(rate(container_cpu_cfs_periods_total{namespace="default", pod="web-abc", container="nginx"}[5m]))`:           newSampleVector(100.0),
		},
	}

	c := &ContainerResourceCollector{
		prometheusAPI: mock,
		logger:        logr.Discard(),
	}

	fraction, err := c.collectContainerCPUThrottleMetrics(context.Background(), pod, containerName)

	assert.NoError(t, err)
	assert.InDelta(t, 1.0, fraction, 0.01)
}
