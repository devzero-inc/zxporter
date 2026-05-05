package nodemon_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/devzero-inc/zxporter/internal/nodemon"
)

// integrationStatsSummaryJSON is the stats/summary payload used throughout the
// integration test. It contains one pod ("web-abc" in "default") with one
// container ("nginx"), a network interface, and a PVC-backed volume.
const integrationStatsSummaryJSON = `{
  "node": {
    "nodeName": "integration-node",
    "network": {
      "interfaces": [
        {"name": "eth0", "rxBytes": 5000, "txBytes": 6000}
      ]
    }
  },
  "pods": [
    {
      "podRef": {
        "name": "web-abc",
        "namespace": "default"
      },
      "containers": [
        {
          "name": "nginx",
          "cpu": {
            "time": "2024-01-01T00:00:00Z",
            "usageNanoCores": 50000000,
            "usageCoreNanoSeconds": 9000000000
          },
          "memory": {
            "time": "2024-01-01T00:00:00Z",
            "usageBytes": 209715200,
            "workingSetBytes": 104857600,
            "rssBytes": 52428800,
            "availableBytes": 1073741824,
            "pageFaults": 5,
            "majorPageFaults": 0
          }
        }
      ],
      "network": {
        "interfaces": [
          {"name": "eth0", "rxBytes": 1024, "txBytes": 2048}
        ]
      },
      "volume": [
        {
          "name": "data-pvc",
          "pvcRef": {
            "name": "data-claim",
            "namespace": "default"
          },
          "usedBytes": 536870912,
          "capacityBytes": 5368709120,
          "availableBytes": 4831838208
        }
      ]
    }
  ]
}`

// integrationCAdvisorBaseline is the first cAdvisor scrape — it seeds counter
// baselines without producing any rate output.
const integrationCAdvisorBaseline = `# HELP container_cpu_cfs_throttled_periods_total Total throttled CPU periods.
# TYPE container_cpu_cfs_throttled_periods_total counter
container_cpu_cfs_throttled_periods_total{container="nginx",namespace="default",pod="web-abc"} 300
# HELP container_cpu_cfs_periods_total Total CPU CFS periods.
# TYPE container_cpu_cfs_periods_total counter
container_cpu_cfs_periods_total{container="nginx",namespace="default",pod="web-abc"} 1000
# HELP container_fs_reads_bytes_total Total bytes read from filesystem.
# TYPE container_fs_reads_bytes_total counter
container_fs_reads_bytes_total{container="nginx",namespace="default",pod="web-abc"} 409600
# HELP container_fs_writes_bytes_total Total bytes written to filesystem.
# TYPE container_fs_writes_bytes_total counter
container_fs_writes_bytes_total{container="nginx",namespace="default",pod="web-abc"} 0
# HELP container_fs_reads_total Total filesystem read operations.
# TYPE container_fs_reads_total counter
container_fs_reads_total{container="nginx",namespace="default",pod="web-abc"} 100
# HELP container_fs_writes_total Total filesystem write operations.
# TYPE container_fs_writes_total counter
container_fs_writes_total{container="nginx",namespace="default",pod="web-abc"} 50
# HELP container_network_receive_packets_total Total network packets received.
# TYPE container_network_receive_packets_total counter
container_network_receive_packets_total{namespace="default",pod="web-abc"} 1000
# HELP container_network_transmit_packets_total Total network packets transmitted.
# TYPE container_network_transmit_packets_total counter
container_network_transmit_packets_total{namespace="default",pod="web-abc"} 800
# HELP container_network_receive_errors_total Total network receive errors.
# TYPE container_network_receive_errors_total counter
container_network_receive_errors_total{namespace="default",pod="web-abc"} 10
# HELP container_network_transmit_errors_total Total network transmit errors.
# TYPE container_network_transmit_errors_total counter
container_network_transmit_errors_total{namespace="default",pod="web-abc"} 5
# HELP container_network_receive_packets_dropped_total Total network receive drops.
# TYPE container_network_receive_packets_dropped_total counter
container_network_receive_packets_dropped_total{namespace="default",pod="web-abc"} 2
# HELP container_network_transmit_packets_dropped_total Total network transmit drops.
# TYPE container_network_transmit_packets_dropped_total counter
container_network_transmit_packets_dropped_total{namespace="default",pod="web-abc"} 1
`

// integrationCAdvisorSecond is the second cAdvisor scrape — counters are
// incremented 30 seconds after the baseline. Expected rates:
//
//	CPU throttle fraction : (330-300)/(1100-1000) = 30/100 = 0.30
//	disk read bytes/sec   : (532480-409600)/30    = 4096.0
//	disk write bytes/sec  : (61440-0)/30          = 2048.0
//	disk read ops/sec     : (130-100)/30          ≈ 1.0
//	disk write ops/sec    : (80-50)/30            = 1.0
//	net rx packets/sec    : (1300-1000)/30        = 10.0
//	net tx packets/sec    : (1100-800)/30         = 10.0
const integrationCAdvisorSecond = `# HELP container_cpu_cfs_throttled_periods_total Total throttled CPU periods.
# TYPE container_cpu_cfs_throttled_periods_total counter
container_cpu_cfs_throttled_periods_total{container="nginx",namespace="default",pod="web-abc"} 330
# HELP container_cpu_cfs_periods_total Total CPU CFS periods.
# TYPE container_cpu_cfs_periods_total counter
container_cpu_cfs_periods_total{container="nginx",namespace="default",pod="web-abc"} 1100
# HELP container_fs_reads_bytes_total Total bytes read from filesystem.
# TYPE container_fs_reads_bytes_total counter
container_fs_reads_bytes_total{container="nginx",namespace="default",pod="web-abc"} 532480
# HELP container_fs_writes_bytes_total Total bytes written to filesystem.
# TYPE container_fs_writes_bytes_total counter
container_fs_writes_bytes_total{container="nginx",namespace="default",pod="web-abc"} 61440
# HELP container_fs_reads_total Total filesystem read operations.
# TYPE container_fs_reads_total counter
container_fs_reads_total{container="nginx",namespace="default",pod="web-abc"} 130
# HELP container_fs_writes_total Total filesystem write operations.
# TYPE container_fs_writes_total counter
container_fs_writes_total{container="nginx",namespace="default",pod="web-abc"} 80
# HELP container_network_receive_packets_total Total network packets received.
# TYPE container_network_receive_packets_total counter
container_network_receive_packets_total{namespace="default",pod="web-abc"} 1300
# HELP container_network_transmit_packets_total Total network packets transmitted.
# TYPE container_network_transmit_packets_total counter
container_network_transmit_packets_total{namespace="default",pod="web-abc"} 1100
# HELP container_network_receive_errors_total Total network receive errors.
# TYPE container_network_receive_errors_total counter
container_network_receive_errors_total{namespace="default",pod="web-abc"} 10
# HELP container_network_transmit_errors_total Total network transmit errors.
# TYPE container_network_transmit_errors_total counter
container_network_transmit_errors_total{namespace="default",pod="web-abc"} 5
# HELP container_network_receive_packets_dropped_total Total network receive drops.
# TYPE container_network_receive_packets_dropped_total counter
container_network_receive_packets_dropped_total{namespace="default",pod="web-abc"} 2
# HELP container_network_transmit_packets_dropped_total Total network transmit drops.
# TYPE container_network_transmit_packets_dropped_total counter
container_network_transmit_packets_dropped_total{namespace="default",pod="web-abc"} 1
`

// TestIntegration_FullMetricsFlow exercises the complete pipeline end-to-end:
//
//  1. Stats/summary poller parses kubelet JSON and returns structured pod data.
//  2. CAdvisor scraper seeds baselines on the first call (no rates yet).
//  3. CAdvisor scraper computes per-second rates on the second call.
//  4. The expected CPU/memory/network fields from stats/summary are correct.
//  5. The expected throttle, disk-I/O, and network-packet rates from cAdvisor
//     are computed with the right formulas.
func TestIntegration_FullMetricsFlow(t *testing.T) {
	// --- stats/summary mock server ------------------------------------------
	statsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/stats/summary" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(integrationStatsSummaryJSON))
	}))
	defer statsSrv.Close()

	// --- cAdvisor mock server ------------------------------------------------
	cadvisorCallCount := 0
	cadvisorSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		w.WriteHeader(http.StatusOK)
		if cadvisorCallCount == 0 {
			_, _ = w.Write([]byte(integrationCAdvisorBaseline))
		} else {
			_, _ = w.Write([]byte(integrationCAdvisorSecond))
		}
		cadvisorCallCount++
	}))
	defer cadvisorSrv.Close()

	// --- real components (not mocks) ----------------------------------------
	log := logr.Discard()
	poller := nodemon.NewStatsPoller(statsSrv.URL, &http.Client{}, log)
	scraper := nodemon.NewCAdvisorScraper(cadvisorSrv.URL, &http.Client{}, log)

	ctx := context.Background()

	// --- first scrape: seeds stats and cAdvisor baseline --------------------
	stats, err := poller.Poll(ctx)
	require.NoError(t, err, "first stats/summary poll must succeed")

	t0 := time.Now()
	cadvisorResults1, err := scraper.Scrape(ctx, t0)
	require.NoError(t, err, "first cAdvisor scrape must not error")
	assert.Empty(t, cadvisorResults1, "first cAdvisor scrape should return no rates (baseline only)")

	// --- second scrape: 30 seconds later, rates now available ---------------
	stats2, err := poller.Poll(ctx)
	require.NoError(t, err, "second stats/summary poll must succeed")

	t1 := t0.Add(30 * time.Second)
	cadvisorResults2, err := scraper.Scrape(ctx, t1)
	require.NoError(t, err, "second cAdvisor scrape must not error")
	require.NotEmpty(t, cadvisorResults2, "second cAdvisor scrape should return rate results")

	// =========================================================================
	// Verify stats/summary fields (both polls return the same data)
	// =========================================================================

	for _, s := range []*nodemon.StatsSummary{stats, stats2} {
		require.Len(t, s.Pods, 1, "should have exactly one pod")
		pod := s.Pods[0]

		// Pod identity
		assert.Equal(t, "web-abc", pod.PodRef.Name)
		assert.Equal(t, "default", pod.PodRef.Namespace)

		// Container CPU (from stats/summary)
		require.Len(t, pod.Containers, 1)
		c := pod.Containers[0]
		assert.Equal(t, "nginx", c.Name)
		require.NotNil(t, c.CPU.UsageNanoCores, "usageNanoCores must be present")
		assert.Equal(t, uint64(50000000), *c.CPU.UsageNanoCores, "usageNanoCores")
		require.NotNil(t, c.CPU.UsageCoreNanoSeconds)
		assert.Equal(t, uint64(9000000000), *c.CPU.UsageCoreNanoSeconds)

		// Container memory (from stats/summary)
		require.NotNil(t, c.Memory.WorkingSetBytes, "workingSetBytes must be present")
		assert.Equal(t, uint64(104857600), *c.Memory.WorkingSetBytes, "workingSetBytes")
		require.NotNil(t, c.Memory.UsageBytes)
		assert.Equal(t, uint64(209715200), *c.Memory.UsageBytes, "usageBytes")

		// Pod network bytes (from stats/summary)
		require.Len(t, pod.Network.Interfaces, 1)
		iface := pod.Network.Interfaces[0]
		require.NotNil(t, iface.RxBytes)
		assert.Equal(t, uint64(1024), *iface.RxBytes, "network rxBytes")
		require.NotNil(t, iface.TxBytes)
		assert.Equal(t, uint64(2048), *iface.TxBytes, "network txBytes")

		// PVC volume (from stats/summary)
		require.Len(t, pod.VolumeStats, 1)
		vol := pod.VolumeStats[0]
		assert.Equal(t, "data-pvc", vol.Name)
		require.NotNil(t, vol.PVCRef, "pvcRef must be present")
		assert.Equal(t, "data-claim", vol.PVCRef.Name)
		assert.Equal(t, "default", vol.PVCRef.Namespace)
		require.NotNil(t, vol.UsedBytes)
		assert.Equal(t, uint64(536870912), *vol.UsedBytes, "volume usedBytes")
		require.NotNil(t, vol.CapacityBytes)
		assert.Equal(t, uint64(5368709120), *vol.CapacityBytes, "volume capacityBytes")
	}

	// =========================================================================
	// Verify cAdvisor rates (second scrape only)
	// =========================================================================

	require.Len(t, cadvisorResults2, 1, "exactly one container entry expected from cAdvisor")
	cm := cadvisorResults2[0]

	assert.Equal(t, "default", cm.Namespace)
	assert.Equal(t, "web-abc", cm.Pod)
	assert.Equal(t, "nginx", cm.Container)

	// CPU throttle fraction: (330-300)/(1100-1000) = 30/100 = 0.30
	assert.InDelta(t, 0.30, cm.CPUThrottleFraction, 0.01, "CPU throttle fraction")

	// Disk I/O rates
	// read bytes/sec:  (532480 - 409600) / 30 = 4096.0
	assert.InDelta(t, 4096.0, cm.DiskReadBytesPerSec, 1.0, "disk read bytes/sec")
	// write bytes/sec: (61440 - 0) / 30 = 2048.0
	assert.InDelta(t, 2048.0, cm.DiskWriteBytesPerSec, 1.0, "disk write bytes/sec")
	// read ops/sec:    (130 - 100) / 30 ≈ 1.0
	assert.InDelta(t, 1.0, cm.DiskReadOpsPerSec, 0.01, "disk read ops/sec")
	// write ops/sec:   (80 - 50) / 30 = 1.0
	assert.InDelta(t, 1.0, cm.DiskWriteOpsPerSec, 0.01, "disk write ops/sec")

	// Network packet rates
	// rx packets/sec:  (1300 - 1000) / 30 = 10.0
	assert.InDelta(t, 10.0, cm.NetworkRxPacketsPerSec, 0.01, "network rx packets/sec")
	// tx packets/sec:  (1100 - 800) / 30 = 10.0
	assert.InDelta(t, 10.0, cm.NetworkTxPacketsPerSec, 0.01, "network tx packets/sec")

	// Error and drop counters are unchanged between scrapes → rates must be 0
	assert.InDelta(t, 0.0, cm.NetworkRxErrorsPerSec, 0.001, "network rx errors/sec (unchanged)")
	assert.InDelta(t, 0.0, cm.NetworkTxErrorsPerSec, 0.001, "network tx errors/sec (unchanged)")
	assert.InDelta(t, 0.0, cm.NetworkRxDropsPerSec, 0.001, "network rx drops/sec (unchanged)")
	assert.InDelta(t, 0.0, cm.NetworkTxDropsPerSec, 0.001, "network tx drops/sec (unchanged)")
}
