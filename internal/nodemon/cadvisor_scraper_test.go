package nodemon_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devzero-inc/zxporter/internal/nodemon"
)

// sample1 is the first scrape — sets the counter baseline.
const cadvisorSample1 = `# HELP container_cpu_cfs_throttled_periods_total Total throttled CPU periods.
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
# HELP container_network_receive_packets_dropped_total Total network receive packet drops.
# TYPE container_network_receive_packets_dropped_total counter
container_network_receive_packets_dropped_total{namespace="default",pod="web-abc"} 2
# HELP container_network_transmit_packets_dropped_total Total network transmit packet drops.
# TYPE container_network_transmit_packets_dropped_total counter
container_network_transmit_packets_dropped_total{namespace="default",pod="web-abc"} 1
`

// sample2 is the second scrape — 30 seconds later with incremented counters.
// throttle fraction: (330-300)/(1100-1000) = 30/100 = 0.30
// disk read bytes/sec: (532480-409600)/30 = 122880/30 = 4096.0
// disk write bytes/sec: (61440-0)/30 = 2048.0
// disk read ops/sec: (130-100)/30 ≈ 1.0
// disk write ops/sec: (80-50)/30 = 1.0
// network rx packets/sec: (1300-1000)/30 = 10.0
// network tx packets/sec: (1100-800)/30 ≈ 10.0
const cadvisorSample2 = `# HELP container_cpu_cfs_throttled_periods_total Total throttled CPU periods.
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
# HELP container_network_receive_packets_dropped_total Total network receive packet drops.
# TYPE container_network_receive_packets_dropped_total counter
container_network_receive_packets_dropped_total{namespace="default",pod="web-abc"} 2
# HELP container_network_transmit_packets_dropped_total Total network transmit packet drops.
# TYPE container_network_transmit_packets_dropped_total counter
container_network_transmit_packets_dropped_total{namespace="default",pod="web-abc"} 1
`

func newCAdvisorTestLogger() logr.Logger {
	zapLog, _ := zap.NewDevelopment()
	return zapr.NewLogger(zapLog)
}

// TestCAdvisorScraper_FirstScrapeReturnsEmpty verifies that the first scrape
// returns an empty slice because there is no baseline for rate computation.
func TestCAdvisorScraper_FirstScrapeReturnsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(cadvisorSample1))
	}))
	defer srv.Close()

	log := newCAdvisorTestLogger()
	scraper := nodemon.NewCAdvisorScraper(srv.URL, srv.Client(), log)

	now := time.Now()
	results, err := scraper.Scrape(context.Background(), now)

	require.NoError(t, err)
	assert.Empty(t, results, "first scrape should return empty slice (no baseline for rates)")
}

// TestCAdvisorScraper_ComputesRatesAfterTwoScrapes verifies that the second
// scrape returns correct per-second rates computed from the counter deltas.
func TestCAdvisorScraper_ComputesRatesAfterTwoScrapes(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		callCount++
		if callCount == 1 {
			_, _ = w.Write([]byte(cadvisorSample1))
		} else {
			_, _ = w.Write([]byte(cadvisorSample2))
		}
	}))
	defer srv.Close()

	log := newCAdvisorTestLogger()
	scraper := nodemon.NewCAdvisorScraper(srv.URL, srv.Client(), log)

	base := time.Now()
	// First scrape — seeds the baseline counters.
	_, err := scraper.Scrape(context.Background(), base)
	require.NoError(t, err)

	// Second scrape — 30 seconds later with incremented counters.
	results, err := scraper.Scrape(context.Background(), base.Add(30*time.Second))
	require.NoError(t, err)
	require.Len(t, results, 1, "should return one container entry")

	m := results[0]
	assert.Equal(t, "default", m.Namespace)
	assert.Equal(t, "web-abc", m.Pod)
	assert.Equal(t, "nginx", m.Container)

	// CPU throttle: (330-300)/(1100-1000) = 30/100 = 0.30
	assert.InDelta(t, 0.30, m.CPUThrottleFraction, 0.0001, "CPU throttle fraction")

	// Disk read bytes/sec: (532480-409600)/30 = 4096.0
	assert.InDelta(t, 4096.0, m.DiskReadBytesPerSec, 0.01, "disk read bytes/sec")

	// Disk write bytes/sec: (61440-0)/30 = 2048.0
	assert.InDelta(t, 2048.0, m.DiskWriteBytesPerSec, 0.01, "disk write bytes/sec")

	// Disk read ops/sec: (130-100)/30 = 1.0
	assert.InDelta(t, 1.0, m.DiskReadOpsPerSec, 0.001, "disk read ops/sec")

	// Disk write ops/sec: (80-50)/30 = 1.0
	assert.InDelta(t, 1.0, m.DiskWriteOpsPerSec, 0.001, "disk write ops/sec")

	// Network rx packets/sec: (1300-1000)/30 = 10.0
	assert.InDelta(t, 10.0, m.NetworkRxPacketsPerSec, 0.001, "network rx packets/sec")

	// Network tx packets/sec: (1100-800)/30 = 10.0
	assert.InDelta(t, 10.0, m.NetworkTxPacketsPerSec, 0.001, "network tx packets/sec")

	// Network errors unchanged → rate = 0
	assert.InDelta(t, 0.0, m.NetworkRxErrorsPerSec, 0.001, "network rx errors/sec (no change)")
	assert.InDelta(t, 0.0, m.NetworkTxErrorsPerSec, 0.001, "network tx errors/sec (no change)")

	// Network drops unchanged → rate = 0
	assert.InDelta(t, 0.0, m.NetworkRxDropsPerSec, 0.001, "network rx drops/sec (no change)")
	assert.InDelta(t, 0.0, m.NetworkTxDropsPerSec, 0.001, "network tx drops/sec (no change)")
}

// TestCAdvisorScraper_HandlesHTTPError verifies that an HTTP error from the
// cAdvisor endpoint is propagated as an error (not silently ignored).
func TestCAdvisorScraper_HandlesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log := newCAdvisorTestLogger()
	scraper := nodemon.NewCAdvisorScraper(srv.URL, srv.Client(), log)

	_, err := scraper.Scrape(context.Background(), time.Now())
	require.Error(t, err, "HTTP 500 should result in an error")
}
