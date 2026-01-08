package networkmonitor

import (
	"fmt"
	"net"
	"os"
	"time"

	ct "github.com/florianl/go-conntrack"
	"github.com/go-logr/logr"
	"inet.af/netaddr"
)

// Entry represents a single conntrack entry
type Entry struct {
	Src       netaddr.IPPort
	Dst       netaddr.IPPort
	TxBytes   uint64
	TxPackets uint64
	RxBytes   uint64
	RxPackets uint64
	Lifetime  time.Time
	Proto     uint8
}

type EntriesFilter func(e *Entry) bool

func FilterBySrcIP(ips map[netaddr.IP]struct{}) EntriesFilter {
	return func(e *Entry) bool {
		_, found := ips[e.Src.IP()]
		return found
	}
}

// Client defines the interface for conntrack clients (Netfilter or Cilium)
type Client interface {
	ListEntries(filter EntriesFilter) ([]*Entry, error)
	Close() error
}

// NetfilterClient uses the kernel's conntrack table via Netlink
type NetfilterClient struct {
	log  logr.Logger
	nfct *ct.Nfct
}

func NewNetfilterClient(log logr.Logger) (Client, error) {
	// Ensure accounting is enabled
	_ = os.WriteFile("/proc/sys/net/netfilter/nf_conntrack_acct", []byte{'1'}, 0600)

	nfct, err := ct.Open(&ct.Config{})
	if err != nil {
		return nil, fmt.Errorf("opening nfct: %w", err)
	}
	return &NetfilterClient{
		log:  log,
		nfct: nfct,
	}, nil
}

func (c *NetfilterClient) Close() error {
	return c.nfct.Close()
}

func (c *NetfilterClient) ListEntries(filter EntriesFilter) ([]*Entry, error) {
	sessions, err := c.nfct.Dump(ct.Conntrack, ct.IPv4)
	if err != nil {
		return nil, fmt.Errorf("dumping nfct sessions: %w", err)
	}

	return processSessions(sessions, filter), nil
}

func processSessions(sessions []ct.Con, filter EntriesFilter) []*Entry {
	res := make([]*Entry, 0)
	now := time.Now().UTC()

	for _, sess := range sessions {
		if sess.Origin == nil || sess.Origin.Src == nil || sess.Origin.Dst == nil || sess.Origin.Proto == nil ||
			sess.Reply == nil || sess.Reply.Dst == nil || sess.Reply.Proto == nil ||
			sess.CounterOrigin == nil || sess.CounterReply == nil {
			continue
		}

		if sess.Origin.Proto.SrcPort == nil || sess.Origin.Proto.DstPort == nil ||
			sess.Reply.Proto.SrcPort == nil {
			continue
		}

		origin := sess.Origin
		originCounter := sess.CounterOrigin
		reply := sess.Reply
		replyCounter := sess.CounterReply

		if originCounter.Bytes == nil || originCounter.Packets == nil ||
			replyCounter.Bytes == nil || replyCounter.Packets == nil {
			continue
		}

		entry := &Entry{
			Src:       netaddr.IPPortFrom(ipFromStdIP(*origin.Src), *origin.Proto.SrcPort),
			Dst:       netaddr.IPPortFrom(ipFromStdIP(*origin.Dst), *origin.Proto.DstPort),
			TxBytes:   *originCounter.Bytes,
			TxPackets: *originCounter.Packets,
			RxBytes:   *replyCounter.Bytes,
			RxPackets: *replyCounter.Packets,
			Proto:     *origin.Proto.Number,
		}

		if sess.Timeout != nil {
			entry.Lifetime = now.Add(time.Second * time.Duration(*sess.Timeout))
		}

		replySrc := netaddr.IPPortFrom(ipFromStdIP(*reply.Src), *reply.Proto.SrcPort)
		if entry.Dst != replySrc {
			entry.Dst = replySrc
		}

		if filter == nil || filter(entry) {
			res = append(res, entry)
		}
	}
	return res
}

func ipFromStdIP(ip net.IP) netaddr.IP {
	res, _ := netaddr.FromStdIP(ip)
	return res
}
