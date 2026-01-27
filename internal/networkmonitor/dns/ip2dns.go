package dns

import (
	"context"
	"sync"
	"time"

	cache "github.com/Code-Hex/go-generics-cache"
	"github.com/go-logr/logr"
	"github.com/google/gopacket/layers"
	"inet.af/netaddr"

	"github.com/devzero-inc/zxporter/internal/networkmonitor/ebpf"
)

type DNSLookup struct {
	ClientIP    string    `json:"client_ip"`
	Domain      string    `json:"domain"`
	ResolvedIPs []string  `json:"resolved_ips"`
	Timestamp   time.Time `json:"timestamp"`
}

type DNSCollector interface {
	Start(ctx context.Context) error
	Lookups() []DNSLookup
	PeekLookups() []DNSLookup
	Records() map[string]string // Keep for compatibility if needed, but we'll focus on Lookups
}

var _ DNSCollector = (*IP2DNS)(nil)

type tracer interface {
	Run(ctx context.Context) error
	Events() <-chan ebpf.DNSEvent
}

var defaultDNSTTL = 2 * time.Minute

type IP2DNS struct {
	Tracer tracer
	log    logr.Logger

	mu      sync.Mutex
	lookups []DNSLookup

	// Keep cache for enrichment if needed, though mostly redundant with Lookups stream
	ipToName *cache.Cache[string, string]
}

func NewIP2DNS(tracer tracer, log logr.Logger) *IP2DNS {
	return &IP2DNS{
		Tracer: tracer,
		log:    log,
	}
}

func (d *IP2DNS) Start(ctx context.Context) error {
	d.ipToName = cache.NewContext[string, string](ctx)

	// Tracer is already running (started in main.go)
	// We just consume events from its channel
	evCh := d.Tracer.Events()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-evCh:
			if !ok {
				return nil
			}

			// Process DNS Event
			var resolvedIPs []string
			var domain string

			for _, answer := range ev.Answers {
				name := string(answer.Name)
				if domain == "" {
					domain = name
				}

				switch answer.Type {
				case layers.DNSTypeA:
					ip, ok := netaddr.FromStdIP(answer.IP)
					if !ok {
						continue
					}
					resolvedIPs = append(resolvedIPs, ip.String())
					d.ipToName.Set(ip.String(), name, cache.WithExpiration(defaultDNSTTL))
				case layers.DNSTypeCNAME:
					// CNAME handling if needed
				}
			}

			if len(resolvedIPs) > 0 {
				var clientIP string
				// eBPF Tracer (CgroupIngress): SrcIP = Server, DstIP = Client (Pod)
				if ip, ok := netaddr.FromStdIP(ev.DstIP); ok {
					clientIP = ip.String()
				}

				d.mu.Lock()
				d.lookups = append(d.lookups, DNSLookup{
					ClientIP:    clientIP,
					Domain:      domain,
					ResolvedIPs: resolvedIPs,
					Timestamp:   time.Now(),
				})
				d.mu.Unlock()
			}
		}
	}
}

// Lookups returns the accumulated lookups and clears the buffer (Delta)
func (d *IP2DNS) Lookups() []DNSLookup {
	d.mu.Lock()
	defer d.mu.Unlock()

	res := d.lookups
	d.lookups = make([]DNSLookup, 0)
	return res
}

// PeekLookups returns the accumulated lookups WITHOUT clearing the buffer
func (d *IP2DNS) PeekLookups() []DNSLookup {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Return a copy to avoid races
	res := make([]DNSLookup, len(d.lookups))
	copy(res, d.lookups)
	return res
}

func (d *IP2DNS) Records() map[string]string {
	if d.ipToName == nil {
		return nil
	}
	res := make(map[string]string)
	for _, ip := range d.ipToName.Keys() {
		domain, _ := d.ipToName.Get(ip)
		res[ip] = domain
	}
	return res
}
