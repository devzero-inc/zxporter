package networkmonitor

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"hash/maphash"

	"github.com/go-logr/logr"
	"google.golang.org/protobuf/types/known/timestamppb"
	"inet.af/netaddr"

	"github.com/devzero-inc/zxporter/internal/networkmonitor/dns"
	"github.com/devzero-inc/zxporter/internal/transport"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
)

// Config holds configuration for the monitor
type Config struct {
	ReadInterval    time.Duration
	CleanupInterval time.Duration
	FlushInterval   time.Duration
	NodeName        string
}

// NetworkFlow represents a single aggregated network flow
// This matches the conceptual "RawNetworkMetric" from egressd
type NetworkFlow struct {
	SrcIP     string `json:"src_ip"`
	DstIP     string `json:"dst_ip"`
	SrcDomain string `json:"src_domain,omitempty"`
	DstDomain string `json:"dst_domain,omitempty"`

	// Pod Metadata
	SrcPodName      string `json:"src_pod_name,omitempty"`
	SrcPodNamespace string `json:"src_pod_namespace,omitempty"`
	DstPodName      string `json:"dst_pod_name,omitempty"`
	DstPodNamespace string `json:"dst_pod_namespace,omitempty"`

	Protocol  uint8     `json:"protocol"`
	DstPort   uint16    `json:"dst_port"`
	TxBytes   uint64    `json:"tx_bytes"`
	RxBytes   uint64    `json:"rx_bytes"`
	TxPackets uint64    `json:"tx_packets"`
	RxPackets uint64    `json:"rx_packets"`
	Timestamp time.Time `json:"timestamp"`

	// internal lifetime tracking for cleanup
	lifetime time.Time `json:"-"`
}

type EnrichedDNSLookup struct {
	dns.DNSLookup
	SrcPodName      string `json:"src_pod_name,omitempty"`
	SrcPodNamespace string `json:"src_pod_namespace,omitempty"`
}

type MetricsResponse struct {
	NodeName   string              `json:"node_name"`
	Items      []*NetworkFlow      `json:"items"`
	DNSLookups []EnrichedDNSLookup `json:"dns_lookups"`
	Ip2Domain  map[string]string   `json:"ip2domain,omitempty"`
}

// Monitor collects network flows using conntrack and aggregates them
type Monitor struct {
	cfg        Config
	log        logr.Logger
	ct         Client
	dns        dns.DNSCollector
	podCache   *PodCache
	dakrClient transport.DakrClient

	mu sync.RWMutex

	// Cache of previous conntrack entries for delta calculation
	// Key: conntrackEntryKey
	entriesCache map[uint64]*Entry

	// Aggregated metrics by (SrcIP, DstIP, Proto)
	// Key: entryGroupKey
	podMetrics map[uint64]*NetworkFlow
}

// NewMonitor creates a new Monitor instance
func NewMonitor(cfg Config, log logr.Logger, podCache *PodCache, ct Client, dnsCollector dns.DNSCollector, dakrClient transport.DakrClient) (*Monitor, error) {
	return &Monitor{
		cfg:          cfg,
		log:          log,
		ct:           ct,
		dns:          dnsCollector,
		podCache:     podCache,
		dakrClient:   dakrClient,
		entriesCache: make(map[uint64]*Entry),
		podMetrics:   make(map[uint64]*NetworkFlow),
	}, nil
}

// Start begins the collection loop
func (m *Monitor) Start(ctx context.Context) {
	readTicker := time.NewTicker(m.cfg.ReadInterval)
	cleanupTicker := time.NewTicker(m.cfg.CleanupInterval)
	flushTicker := time.NewTicker(m.cfg.FlushInterval)
	defer readTicker.Stop()
	defer cleanupTicker.Stop()
	defer flushTicker.Stop()

	// Start DNS collector if available
	if m.dns != nil {
		go func() {
			if err := m.dns.Start(ctx); err != nil {
				m.log.Error(err, "DNS collector failed")
			}
		}()
	}

	for {
		select {
		case <-ctx.Done():
			m.ct.Close()
			return
		case <-readTicker.C:
			if err := m.collect(); err != nil {
				m.log.Error(err, "Failed to collect conntrack entries")
			}
		case <-cleanupTicker.C:
			m.cleanup()
		case <-flushTicker.C:
			if err := m.flush(ctx); err != nil {
				m.log.Error(err, "Failed to flush metrics to control plane")
			}
		case <-flushTicker.C:
			// Run flush in goroutine to avoid blocking collection (unless we need strict ordering, but collection is fast)
			// Actually, flushing needs read lock, collect needs write lock.
			// We should probably just call it synchronously to ensure we don't pile up goroutines.
			if err := m.flush(ctx); err != nil {
				m.log.Error(err, "Failed to flush metrics to control plane")
			}
		}
	}
}

// collect fetches entries from conntrack and updates internal state
func (m *Monitor) collect() error {
	// 1. Get local pods to filter by Source IP
	localIPs := m.podCache.GetLocalPodIPs()

	// 2. Dump filtered conntrack entries
	entries, err := m.ct.ListEntries(FilterBySrcIP(localIPs))
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, entry := range entries {
		connKey := conntrackEntryKey(entry)

		// Calculate Deltas
		txBytes := entry.TxBytes
		txPackets := entry.TxPackets
		rxBytes := entry.RxBytes
		rxPackets := entry.RxPackets

		if cachedEntry, found := m.entriesCache[connKey]; found {
			// Calculate delta, handling resets (if new < old, assume reset and take new value)
			if txBytes >= cachedEntry.TxBytes {
				txBytes -= cachedEntry.TxBytes
			}
			if rxBytes >= cachedEntry.RxBytes {
				rxBytes -= cachedEntry.RxBytes
			}
			if txPackets >= cachedEntry.TxPackets {
				txPackets -= cachedEntry.TxPackets
			}
			if rxPackets >= cachedEntry.RxPackets {
				rxPackets -= cachedEntry.RxPackets
			}
		}

		// Update cache
		m.entriesCache[connKey] = entry

		// Aggregate into Pod Metrics
		groupKey := entryGroupKey(entry)
		if pm, found := m.podMetrics[groupKey]; found {
			pm.TxBytes += txBytes
			pm.TxPackets += txPackets
			pm.RxBytes += rxBytes
			pm.RxPackets += rxPackets
			if entry.Lifetime.After(pm.lifetime) {
				pm.lifetime = entry.Lifetime
			}
			pm.Timestamp = time.Now()
		} else {
			m.podMetrics[groupKey] = &NetworkFlow{
				SrcIP:     entry.Src.IP().String(),
				DstIP:     entry.Dst.IP().String(),
				Protocol:  entry.Proto,
				DstPort:   entry.Dst.Port(),
				TxBytes:   txBytes,
				TxPackets: txPackets,
				RxBytes:   rxBytes,
				RxPackets: rxPackets,
				Timestamp: time.Now(),
				lifetime:  entry.Lifetime,
			}
		}
	}

	return nil
}

func (m *Monitor) cleanup() {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now().UTC()

	// Cleanup stale entries
	for key, e := range m.entriesCache {
		if now.After(e.Lifetime) {
			delete(m.entriesCache, key)
		}
	}

	// Cleanup stale aggregated metrics
	for key, flow := range m.podMetrics {
		if now.After(flow.lifetime) {
			delete(m.podMetrics, key)
		}
	}
}

// GetMetricsHandler serves the collected metrics
// NOTE: This currently resets the counters after read (Delta mode), mimicking egressd's behavior
func (m *Monitor) GetMetricsHandler(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()

	flows := make([]*NetworkFlow, 0, len(m.podMetrics))

	for _, f := range m.podMetrics {
		// Enrich with Pod Metadata
		if m.podCache != nil {
			if srcIP, err := netaddr.ParseIP(f.SrcIP); err == nil {
				if srcPod, ok := m.podCache.GetPodByIP(srcIP); ok {
					f.SrcPodName = srcPod.Name
					f.SrcPodNamespace = srcPod.Namespace
				}
			}

			if dstIP, err := netaddr.ParseIP(f.DstIP); err == nil {
				if dstPod, ok := m.podCache.GetPodByIP(dstIP); ok {
					f.DstPodName = dstPod.Name
					f.DstPodNamespace = dstPod.Namespace
				}
			}
		}

		// Create a copy to return (Peek behavior, no reset)
		flows = append(flows, &NetworkFlow{
			SrcIP:           f.SrcIP,
			DstIP:           f.DstIP,
			SrcPodName:      f.SrcPodName,
			SrcPodNamespace: f.SrcPodNamespace,
			DstPodName:      f.DstPodName,
			DstPodNamespace: f.DstPodNamespace,
			Protocol:        f.Protocol,
			DstPort:         f.DstPort,
			TxBytes:         f.TxBytes,
			RxBytes:         f.RxBytes,
			TxPackets:       f.TxPackets,
			RxPackets:       f.RxPackets,
			Timestamp:       f.Timestamp,
		})
	}

	var enrichedLookups []EnrichedDNSLookup
	var dnsRecords map[string]string

	if m.dns != nil {
		// 1. Get Log of lookups (Peek)
		rawLookups := m.dns.PeekLookups()
		enrichedLookups = make([]EnrichedDNSLookup, 0, len(rawLookups))
		for _, l := range rawLookups {
			el := EnrichedDNSLookup{DNSLookup: l}
			if m.podCache != nil {
				if clientIP, err := netaddr.ParseIP(l.ClientIP); err == nil {
					if pod, ok := m.podCache.GetPodByIP(clientIP); ok {
						el.SrcPodName = pod.Name
						el.SrcPodNamespace = pod.Namespace
					}
				}
			}
			enrichedLookups = append(enrichedLookups, el)
		}

		// 2. Get Snapshot of IP->Domain cache (State)
		dnsRecords = m.dns.Records()
	}

	resp := MetricsResponse{
		NodeName:   m.cfg.NodeName,
		Items:      flows,
		DNSLookups: enrichedLookups,
		Ip2Domain:  dnsRecords,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		m.log.Error(err, "Failed to encode metrics")
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

// Hashing helpers
var conntrackEntryHash maphash.Hash
var entryGroupHash maphash.Hash

func conntrackEntryKey(e *Entry) uint64 {
	conntrackEntryHash.Reset()
	srcIP := e.Src.IP().As4()
	conntrackEntryHash.Write(srcIP[:])
	var srcPort [2]byte
	binary.LittleEndian.PutUint16(srcPort[:], e.Src.Port())
	conntrackEntryHash.Write(srcPort[:])

	dstIP := e.Dst.IP().As4()
	conntrackEntryHash.Write(dstIP[:])
	var dstPort [2]byte
	binary.LittleEndian.PutUint16(dstPort[:], e.Dst.Port())
	conntrackEntryHash.Write(dstPort[:])

	conntrackEntryHash.WriteByte(e.Proto)
	return conntrackEntryHash.Sum64()
}

func entryGroupKey(e *Entry) uint64 {
	entryGroupHash.Reset()
	srcIP := e.Src.IP().As4()
	entryGroupHash.Write(srcIP[:])

	dstIP := e.Dst.IP().As4()
	entryGroupHash.Write(dstIP[:])

	var dstPort [2]byte
	binary.LittleEndian.PutUint16(dstPort[:], e.Dst.Port())
	entryGroupHash.Write(dstPort[:])

	entryGroupHash.WriteByte(e.Proto)
	return entryGroupHash.Sum64()
}

// flush sends aggregated metrics to the control plane and resets counters
func (m *Monitor) flush(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.dakrClient == nil {
		return nil
	}

	// 1. Prepare DNS Records (State)
	var dnsRecords map[string]string
	if m.dns != nil {
		dnsRecords = m.dns.Records()
	}

	// 2. Prepare DNS Lookups (Delta)
	var dnsLookupItems []*gen.DnsLookupItem
	if m.dns != nil {
		rawLookups := m.dns.Lookups()
		dnsLookupItems = make([]*gen.DnsLookupItem, 0, len(rawLookups))
		for _, l := range rawLookups {
			item := &gen.DnsLookupItem{
				ClientIp:  l.ClientIP,
				Domain:    l.Domain,
				Timestamp: timestamppb.New(l.Timestamp),
			}
			if len(l.ResolvedIPs) > 0 {
				item.ResolvedIps = l.ResolvedIPs
			}

			// Enrich with Pod Metadata
			if m.podCache != nil {
				if clientIP, err := netaddr.ParseIP(l.ClientIP); err == nil {
					if pod, ok := m.podCache.GetPodByIP(clientIP); ok {
						item.SrcPodName = pod.Name
						item.SrcPodNamespace = pod.Namespace
					}
				}
			}
			dnsLookupItems = append(dnsLookupItems, item)
		}
	}

	// 3. Prepare Network Flows (Delta)
	items := make([]*gen.NetworkTrafficItem, 0, len(m.podMetrics))
	for _, f := range m.podMetrics {
		// Only send flows with activity
		if f.TxBytes == 0 && f.RxBytes == 0 && f.TxPackets == 0 && f.RxPackets == 0 {
			continue
		}

		// Enrich with Pod Metadata if not already
		if m.podCache != nil {
			if srcIP, err := netaddr.ParseIP(f.SrcIP); err == nil {
				if srcPod, ok := m.podCache.GetPodByIP(srcIP); ok {
					f.SrcPodName = srcPod.Name
					f.SrcPodNamespace = srcPod.Namespace
				}
			}
			if dstIP, err := netaddr.ParseIP(f.DstIP); err == nil {
				if dstPod, ok := m.podCache.GetPodByIP(dstIP); ok {
					f.DstPodName = dstPod.Name
					f.DstPodNamespace = dstPod.Namespace
				}
			}
		}

		// Enrich with DNS domains
		if dnsRecords != nil {
			if domain, ok := dnsRecords[f.SrcIP]; ok {
				f.SrcDomain = domain
			}
			if domain, ok := dnsRecords[f.DstIP]; ok {
				f.DstDomain = domain
			}
		}

		// Create Proto Item
		items = append(items, &gen.NetworkTrafficItem{
			SrcIp:           f.SrcIP,
			DstIp:           f.DstIP,
			SrcPodName:      f.SrcPodName,
			SrcPodNamespace: f.SrcPodNamespace,
			Protocol:        int32(f.Protocol),
			DstPort:         int32(f.DstPort),
			TxBytes:         int64(f.TxBytes),
			RxBytes:         int64(f.RxBytes),
			TxPackets:       int64(f.TxPackets),
			RxPackets:       int64(f.RxPackets),
			Timestamp:       timestamppb.New(f.Timestamp),
		})

		// Reset counters (Delta behavior for Control Plane)
		f.TxBytes = 0
		f.RxBytes = 0
		f.TxPackets = 0
		f.RxPackets = 0

		// Extend lifetime if active
		if time.Now().Add(2 * time.Minute).After(f.lifetime) {
			f.lifetime = time.Now().Add(2 * time.Minute)
		}
	}

	if len(items) == 0 && len(dnsLookupItems) == 0 {
		return nil
	}

	// 4. Send Request
	req := &gen.SendNetworkTrafficMetricsRequest{
		NodeName:   m.cfg.NodeName,
		Items:      items,
		DnsLookups: dnsLookupItems,
		Ip2Domain:  dnsRecords,
	}

	_, err := m.dakrClient.SendNetworkTrafficMetrics(ctx, req)
	return err
}
