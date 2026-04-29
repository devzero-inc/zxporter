package collector

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCollectNodeNetworkIOMetrics_TransformsPrometheusResponse(t *testing.T) {
	nodeName := "node-1"

	mock := &mockPrometheusAPI{
		queryResults: map[string]model.Value{
			`sum(rate(node_network_receive_bytes_total{node="node-1"}[5m]))`:    newSampleVector(1000.0),
			`sum(rate(node_network_transmit_bytes_total{node="node-1"}[5m]))`:   newSampleVector(2000.0),
			`sum(rate(node_network_receive_packets_total{node="node-1"}[5m]))`:  newSampleVector(300.0),
			`sum(rate(node_network_transmit_packets_total{node="node-1"}[5m]))`: newSampleVector(400.0),
			`sum(rate(node_network_receive_errs_total{node="node-1"}[5m]))`:     newSampleVector(5.0),
			`sum(rate(node_network_transmit_errs_total{node="node-1"}[5m]))`:    newSampleVector(6.0),
			`sum(rate(node_network_receive_drop_total{node="node-1"}[5m]))`:     newSampleVector(7.0),
			`sum(rate(node_network_transmit_drop_total{node="node-1"}[5m]))`:    newSampleVector(8.0),
			`sum(rate(node_disk_read_bytes_total{node="node-1"}[5m]))`:          newSampleVector(512.0),
			`sum(rate(node_disk_written_bytes_total{node="node-1"}[5m]))`:       newSampleVector(1024.0),
			`sum(rate(node_disk_reads_completed_total{node="node-1"}[5m]))`:     newSampleVector(10.0),
			`sum(rate(node_disk_writes_completed_total{node="node-1"}[5m]))`:    newSampleVector(20.0),
		},
	}

	nc := &NodeCollector{
		prometheusAPI: mock,
		logger:        logr.Discard(),
	}

	result, err := nc.collectNodeNetworkIOMetrics(context.Background(), nodeName)

	require.NoError(t, err)
	assert.Len(t, result, 12)

	assert.InDelta(t, 1000.0, result["NetworkReceiveBytes"], 0.01)
	assert.InDelta(t, 2000.0, result["NetworkTransmitBytes"], 0.01)
	assert.InDelta(t, 300.0, result["NetworkReceivePackets"], 0.01)
	assert.InDelta(t, 400.0, result["NetworkTransmitPackets"], 0.01)
	assert.InDelta(t, 5.0, result["NetworkReceiveErrors"], 0.01)
	assert.InDelta(t, 6.0, result["NetworkTransmitErrors"], 0.01)
	assert.InDelta(t, 7.0, result["NetworkReceiveDropped"], 0.01)
	assert.InDelta(t, 8.0, result["NetworkTransmitDropped"], 0.01)
	assert.InDelta(t, 512.0, result["FSReadBytes"], 0.01)
	assert.InDelta(t, 1024.0, result["FSWriteBytes"], 0.01)
	assert.InDelta(t, 10.0, result["FSReads"], 0.01)
	assert.InDelta(t, 20.0, result["FSWrites"], 0.01)
}
