package nodemon

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/go-logr/logr"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// podNetworkSentinel is a placeholder container name used as the grouping key
// for network metrics that carry no container label (only namespace + pod).
const podNetworkSentinel = "__pod__"

// containerKey is the composite identity for grouping per-container counters.
type containerKey struct {
	namespace string
	pod       string
	container string
}

// rawCounters accumulates the latest scraped counter values for a single key.
type rawCounters struct {
	cpuThrottledPeriods float64
	cpuTotalPeriods     float64

	fsReadBytes  float64
	fsWriteBytes float64
	fsReadOps    float64
	fsWriteOps   float64

	netRxPackets float64
	netTxPackets float64
	netRxErrors  float64
	netTxErrors  float64
	netRxDrops   float64
	netTxDrops   float64
}

// CAdvisorScraper fetches the kubelet /metrics/cadvisor endpoint and computes
// per-second rates for CPU throttle, disk I/O, and network counters.
type CAdvisorScraper struct {
	url        string
	httpClient HTTPClient
	rates      *RateCalculator
	log        logr.Logger
}

// NewCAdvisorScraper constructs a CAdvisorScraper that will scrape
// baseURL + "/metrics/cadvisor".
func NewCAdvisorScraper(baseURL string, httpClient HTTPClient, log logr.Logger) *CAdvisorScraper {
	return &CAdvisorScraper{
		url:        baseURL + "/metrics/cadvisor",
		httpClient: httpClient,
		rates:      NewRateCalculator(),
		log:        log.WithName("cadvisor-scraper"),
	}
}

// Scrape fetches cAdvisor metrics, computes rates, and returns per-container
// results. The first call returns an empty slice because no baseline has been
// established for rate computation yet.
func (s *CAdvisorScraper) Scrape(ctx context.Context, now time.Time) ([]CAdvisorContainerMetrics, error) {
	families, err := s.fetchMetrics(ctx)
	if err != nil {
		return nil, err
	}

	counters := s.groupCounters(families)
	return s.computeRates(counters, now), nil
}

// fetchMetrics performs the HTTP request and parses the Prometheus text body.
func (s *CAdvisorScraper) fetchMetrics(ctx context.Context) (map[string]*dto.MetricFamily, error) {
	ctxWithTimeout, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctxWithTimeout, http.MethodGet, s.url, nil)
	if err != nil {
		return nil, fmt.Errorf("cadvisor: cannot create request: %w", err)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cadvisor: HTTP request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cadvisor: request failed with status %d", resp.StatusCode)
	}

	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("cadvisor: cannot parse metrics: %w", err)
	}

	return families, nil
}

// labelValue returns the value of a label by name from a metric, or "" if absent.
func labelValue(m *dto.Metric, name string) string {
	for _, lp := range m.GetLabel() {
		if lp.GetName() == name {
			return lp.GetValue()
		}
	}
	return ""
}

// counterValue returns the float64 value of a counter metric.
func counterValue(m *dto.Metric) float64 {
	if c := m.GetCounter(); c != nil {
		return c.GetValue()
	}
	return 0
}

// groupCounters walks the parsed metric families and accumulates raw counter
// values into a map keyed by (namespace, pod, container).
func (s *CAdvisorScraper) groupCounters(families map[string]*dto.MetricFamily) map[containerKey]*rawCounters {
	groups := make(map[containerKey]*rawCounters)

	// Helper that returns (or lazily creates) the rawCounters entry for a key.
	get := func(k containerKey) *rawCounters {
		if groups[k] == nil {
			groups[k] = &rawCounters{}
		}
		return groups[k]
	}

	// Per-container metrics (have a "container" label).
	applyContainerMetric := func(family *dto.MetricFamily, apply func(rc *rawCounters, v float64)) {
		if family == nil {
			return
		}
		for _, m := range family.GetMetric() {
			ns := labelValue(m, "namespace")
			pod := labelValue(m, "pod")
			container := labelValue(m, "container")
			if ns == "" || pod == "" || container == "" {
				continue
			}
			apply(get(containerKey{ns, pod, container}), counterValue(m))
		}
	}

	// Per-pod network metrics (no "container" label — use sentinel).
	applyNetworkMetric := func(family *dto.MetricFamily, apply func(rc *rawCounters, v float64)) {
		if family == nil {
			return
		}
		for _, m := range family.GetMetric() {
			ns := labelValue(m, "namespace")
			pod := labelValue(m, "pod")
			if ns == "" || pod == "" {
				continue
			}
			apply(get(containerKey{ns, pod, podNetworkSentinel}), counterValue(m))
		}
	}

	applyContainerMetric(families["container_cpu_cfs_throttled_periods_total"],
		func(rc *rawCounters, v float64) { rc.cpuThrottledPeriods = v })
	applyContainerMetric(families["container_cpu_cfs_periods_total"],
		func(rc *rawCounters, v float64) { rc.cpuTotalPeriods = v })

	applyContainerMetric(families["container_fs_reads_bytes_total"],
		func(rc *rawCounters, v float64) { rc.fsReadBytes = v })
	applyContainerMetric(families["container_fs_writes_bytes_total"],
		func(rc *rawCounters, v float64) { rc.fsWriteBytes = v })
	applyContainerMetric(families["container_fs_reads_total"],
		func(rc *rawCounters, v float64) { rc.fsReadOps = v })
	applyContainerMetric(families["container_fs_writes_total"],
		func(rc *rawCounters, v float64) { rc.fsWriteOps = v })

	applyNetworkMetric(families["container_network_receive_packets_total"],
		func(rc *rawCounters, v float64) { rc.netRxPackets = v })
	applyNetworkMetric(families["container_network_transmit_packets_total"],
		func(rc *rawCounters, v float64) { rc.netTxPackets = v })
	applyNetworkMetric(families["container_network_receive_errors_total"],
		func(rc *rawCounters, v float64) { rc.netRxErrors = v })
	applyNetworkMetric(families["container_network_transmit_errors_total"],
		func(rc *rawCounters, v float64) { rc.netTxErrors = v })
	applyNetworkMetric(families["container_network_receive_packets_dropped_total"],
		func(rc *rawCounters, v float64) { rc.netRxDrops = v })
	applyNetworkMetric(families["container_network_transmit_packets_dropped_total"],
		func(rc *rawCounters, v float64) { rc.netTxDrops = v })

	return groups
}

// computeRates turns raw counter snapshots into per-second rate metrics.
// It merges per-container CPU/disk entries with their corresponding pod-level
// network entries so that the final result has one record per real container.
func (s *CAdvisorScraper) computeRates(groups map[containerKey]*rawCounters, now time.Time) []CAdvisorContainerMetrics {
	// Compute rates for every key (including network sentinels).
	type rateRecord struct {
		m        CAdvisorContainerMetrics
		nonZero  bool
		isNetKey bool
	}

	// First pass — process all non-network keys (real containers).
	// Second pass — attach network rates from sentinel keys.

	// Build a pod→network-rates lookup from sentinel entries.
	type netRates struct {
		rxPkts float64
		txPkts float64
		rxErr  float64
		txErr  float64
		rxDrop float64
		txDrop float64
	}
	podNet := make(map[[2]string]netRates) // key: [namespace, pod]

	for k, rc := range groups {
		if k.container != podNetworkSentinel {
			continue
		}
		entity := "cadvisor/" + k.namespace + "/" + k.pod + "/" + k.container
		nr := netRates{
			rxPkts: s.rates.Rate(entity, "net_rx_packets", rc.netRxPackets, now),
			txPkts: s.rates.Rate(entity, "net_tx_packets", rc.netTxPackets, now),
			rxErr:  s.rates.Rate(entity, "net_rx_errors", rc.netRxErrors, now),
			txErr:  s.rates.Rate(entity, "net_tx_errors", rc.netTxErrors, now),
			rxDrop: s.rates.Rate(entity, "net_rx_drops", rc.netRxDrops, now),
			txDrop: s.rates.Rate(entity, "net_tx_drops", rc.netTxDrops, now),
		}
		podNet[[2]string{k.namespace, k.pod}] = nr
	}

	// Second pass — real container entries.
	var results []CAdvisorContainerMetrics

	for k, rc := range groups {
		if k.container == podNetworkSentinel {
			continue
		}

		entity := "cadvisor/" + k.namespace + "/" + k.pod + "/" + k.container

		throttledRate := s.rates.Rate(entity, "cpu_throttled_periods", rc.cpuThrottledPeriods, now)
		totalRate := s.rates.Rate(entity, "cpu_total_periods", rc.cpuTotalPeriods, now)
		diskRB := s.rates.Rate(entity, "fs_read_bytes", rc.fsReadBytes, now)
		diskWB := s.rates.Rate(entity, "fs_write_bytes", rc.fsWriteBytes, now)
		diskRO := s.rates.Rate(entity, "fs_read_ops", rc.fsReadOps, now)
		diskWO := s.rates.Rate(entity, "fs_write_ops", rc.fsWriteOps, now)

		// All counter rates are 0 on first scrape — skip this entry.
		if throttledRate == 0 && totalRate == 0 &&
			diskRB == 0 && diskWB == 0 && diskRO == 0 && diskWO == 0 {
			// Check if there are any network rates too.
			nr := podNet[[2]string{k.namespace, k.pod}]
			if nr.rxPkts == 0 && nr.txPkts == 0 && nr.rxErr == 0 &&
				nr.txErr == 0 && nr.rxDrop == 0 && nr.txDrop == 0 {
				continue
			}
		}

		var throttleFraction float64
		if totalRate > 0 {
			throttleFraction = throttledRate / totalRate
			if throttleFraction > 1 {
				throttleFraction = 1
			}
		}

		nr := podNet[[2]string{k.namespace, k.pod}]

		results = append(results, CAdvisorContainerMetrics{
			Namespace: k.namespace,
			Pod:       k.pod,
			Container: k.container,

			NetworkRxPacketsPerSec: nr.rxPkts,
			NetworkTxPacketsPerSec: nr.txPkts,
			NetworkRxErrorsPerSec:  nr.rxErr,
			NetworkTxErrorsPerSec:  nr.txErr,
			NetworkRxDropsPerSec:   nr.rxDrop,
			NetworkTxDropsPerSec:   nr.txDrop,

			DiskReadBytesPerSec:  diskRB,
			DiskWriteBytesPerSec: diskWB,
			DiskReadOpsPerSec:    diskRO,
			DiskWriteOpsPerSec:   diskWO,

			CPUThrottleFraction: throttleFraction,
		})
	}

	return results
}
