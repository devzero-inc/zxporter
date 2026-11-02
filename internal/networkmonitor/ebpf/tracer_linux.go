//go:build linux

package ebpf

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/btf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/perf"
	"github.com/cilium/ebpf/rlimit"
	"github.com/go-logr/logr"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
)

var ErrCgroup2NotMounted = errors.New("cgroup2 not mounted")

func (t *Tracer) Run(ctx context.Context) error {
	// Allow the current process to lock memory for eBPF resources.
	if err := rlimit.RemoveMemlock(); err != nil {
		return err
	}

	t.log.V(1).Info("running")
	defer t.log.V(1).Info("stopping")

	objs := bpfObjects{}
	var customBTF *btf.Spec
	if t.cfg.CustomBTFFilePath != "" {
		t.log.V(1).Info("loading custom btf", "path", t.cfg.CustomBTFFilePath)
		spec, err := btf.LoadSpec(t.cfg.CustomBTFFilePath)
		if err != nil {
			return err
		}
		customBTF = spec
	}

	// Load pre-compiled programs and maps into the kernel.
	// We load Specs first to access the InnerMap definition for ARRAY_OF_MAPS
	spec, err := loadBpf()
	if err != nil {
		return fmt.Errorf("loading bpf spec: %w", err)
	}

	if err := spec.LoadAndAssign(&objs, &ebpf.CollectionOptions{
		Maps: ebpf.MapOptions{},
		Programs: ebpf.ProgramOptions{
			KernelTypes: customBTF,
		},
		MapReplacements: nil,
	}); err != nil {
		return fmt.Errorf("loading objects: %w", err)
	}
	defer objs.Close()
	t.objs = &objs

	// Retrieve the inner map spec from the collection spec
	// The outer map is "network_traffic_buffer_map"
	outerMapSpec := spec.Maps["network_traffic_buffer_map"]
	if outerMapSpec == nil {
		return fmt.Errorf("missing network_traffic_buffer_map in spec")
	}
	if outerMapSpec.InnerMap == nil {
		return fmt.Errorf("network_traffic_buffer_map has no inner map spec")
	}
	innerMapSpec := outerMapSpec.InnerMap
	// Ensure MaxEntries is set (should be from C, but just in case)
	if innerMapSpec.MaxEntries == 0 {
		innerMapSpec.MaxEntries = 65535
	}

	// Initialize inner maps for double buffering using precise spec
	for i := uint32(0); i < 2; i++ {
		// Copy spec to set unique name
		currentSpec := innerMapSpec.Copy()
		currentSpec.Name = fmt.Sprintf("netflow_inner_%d", i)

		innerMap, err := ebpf.NewMap(currentSpec)
		if err != nil {
			return fmt.Errorf("creating inner map %d: %w", i, err)
		}
		// Map-in-Map holds reference, so we can close this FD when function exits
		// But usually we close it after Put.
		defer func() {
			_ = innerMap.Close()
		}()

		if err := objs.NetworkTrafficBufferMap.Put(i, innerMap); err != nil {
			return fmt.Errorf("populating outer map index %d: %w", i, err)
		}
	}

	// Initialize config map (index 0)
	zero := uint32(0)
	config := bpfNetflowConfigT{MapIndex: 0}
	if err := objs.NetflowConfigMap.Update(zero, config, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("init config map: %w", err)
	}

	// Get the first-mounted cgroupv2 path.
	cgroupPath, err := detectCgroupPath()
	if errors.Is(err, ErrCgroup2NotMounted) {
		if err := mountCgroup2(); err != nil {
			return fmt.Errorf("cgroup2 not mounted and failed to mount manually: %w", err)
		}
		cgroupPath, err = detectCgroupPath()
	}
	if err != nil {
		return err
	}

	t.log.V(1).Info("using cgroup2", "path", cgroupPath)

	links := []link.Link{}
	defer func() {
		for _, l := range links {
			_ = l.Close()
		}
	}()

	// Attach ingress
	l, err := link.AttachCgroup(link.CgroupOptions{
		Path:    cgroupPath,
		Attach:  ebpf.AttachCGroupInetIngress,
		Program: objs.CgroupIngress,
	})
	if err != nil {
		return fmt.Errorf("attaching cgroup ingress: %w", err)
	}
	links = append(links, l)

	// Attach egress
	l, err = link.AttachCgroup(link.CgroupOptions{
		Path:    cgroupPath,
		Attach:  ebpf.AttachCGroupInetEgress,
		Program: objs.CgroupEgress,
	})
	if err != nil {
		return fmt.Errorf("attaching cgroup egress: %w", err)
	}
	links = append(links, l)

	// Keep running until context is cancelled
	// We don't read events loop here anymore, or we can keep it if we want events (DNS)
	// The original code had DNS event reading. We should KEEP it as 'monitor.go' might rely on it for DNS?
	// The user Objective is "collect network flow metric", but existing code had DNS.
	// We should preserve existing functionality.

	// Open perf reader for events
	reader, err := perf.NewReader(objs.Events, 1024)
	if err != nil {
		return err
	}
	defer reader.Close()

	return t.readDNS(ctx, reader)
}

// dnsFlushInterval bounds how long a partly-filled batch waits. It only has to be
// short relative to how often the consumer's output is read, which is on the order of
// seconds.
const dnsFlushInterval = 10 * time.Millisecond

// readDNS drains the perf ring, decodes the DNS packets on it and hands them to the
// consumer in batches.
//
// Batching is the point. Sending one event per packet meant a channel operation and a
// consumer wakeup for every DNS packet crossing the node, and on a busy node that was
// 9% of this agent's CPU in the send itself plus the larger part of another 13% spent
// in the scheduler waking the reader back up. Nothing downstream needs an event the
// instant it is decoded -- the consumer's output is polled on a much slower cycle --
// so the wakeups were pure overhead.
func (t *Tracer) readDNS(ctx context.Context, reader *perf.Reader) error {
	dec := newDNSDecoder()
	batch := make([]DNSEvent, 0, dnsBatchSize)

	flush := func() {
		if len(batch) == 0 {
			return
		}
		select {
		case t.events <- batch:
			// The consumer owns that slice now, so the next batch needs its own
			// backing array. One allocation per dnsBatchSize packets.
			batch = make([]DNSEvent, 0, dnsBatchSize)
		default:
			t.log.V(1).Info("dropping events, queue is full", "count", len(batch))
			batch = batch[:0]
		}
	}

	for {
		select {
		case <-ctx.Done():
			// A batch filled during the last iteration is still pending here, and
			// this is the last chance to hand it over. flush is non-blocking, so a
			// consumer that has already stopped reading costs nothing.
			flush()
			return ctx.Err()
		default:
		}

		// The deadline is armed only when something is waiting to be sent. An idle
		// node should sit blocked in Read, not wake a hundred times a second to
		// discover it has nothing to flush.
		if len(batch) > 0 {
			reader.SetDeadline(time.Now().Add(dnsFlushInterval))
		} else {
			reader.SetDeadline(time.Time{})
		}

		record, err := reader.Read()
		if err != nil {
			switch {
			case errors.Is(err, os.ErrDeadlineExceeded):
				flush()
				continue
			case errors.Is(err, perf.ErrClosed):
				flush()
				return nil
			default:
				return err
			}
		}

		if len(record.RawSample) < 1 {
			t.log.V(1).Info("skipping empty event")
			continue
		}

		if err := dec.decode(record.RawSample); err != nil {
			t.log.V(1).Info("skipping non-DNS event", "err", err)
			continue
		}

		// Queries, NXDOMAIN and anything else that resolved nothing are dropped here
		// rather than sent and ignored. Kubernetes resolution is amplified by the
		// ndots:5 search path -- a single hostname fans out into several queries, most
		// of which answer nothing -- so the packets that carry an answer are a small
		// fraction of the packets that arrive, and the rest cost a decode into reused
		// memory and nothing else. This is not a narrowing of what the consumer sees:
		// an event with no answers resolves no IPs, and an event that resolves no IPs
		// is discarded on arrival (see dns.IP2DNS.Start).
		if !dec.hasAnswers() {
			continue
		}

		batch = append(batch, dec.event())
		if len(batch) >= dnsBatchSize {
			flush()
		}
	}
}

func (t *Tracer) CollectNetworkSummary() (map[NetworkTrafficKey]TrafficSummary, error) {
	if t.objs == nil {
		return nil, fmt.Errorf("ebpf objects not loaded")
	}

	result := make(map[NetworkTrafficKey]TrafficSummary)

	// Read current config to find active index
	var config bpfNetflowConfigT
	zero := uint32(0)
	if err := t.objs.NetflowConfigMap.Lookup(zero, &config); err != nil {
		return nil, fmt.Errorf("lookup config map: %w", err)
	}

	activeIdx := config.MapIndex
	nextIdx := 1 - activeIdx

	// Swap double buffer
	config.MapIndex = nextIdx
	if err := t.objs.NetflowConfigMap.Update(zero, config, ebpf.UpdateAny); err != nil {
		return nil, fmt.Errorf("update config map: %w", err)
	}

	// Read from the PREVIOUS active index (now inactive)
	// The map at 'activeIdx' contains the data collected during the last interval.

	// network_traffic_buffer_map is ARRAY_OF_MAPS. Value is ID of inner map.
	var innerMapID ebpf.MapID
	if err := t.objs.NetworkTrafficBufferMap.Lookup(activeIdx, &innerMapID); err != nil {
		return nil, fmt.Errorf("lookup outer map index %d: %w", activeIdx, err)
	}

	innerMap, err := ebpf.NewMapFromID(innerMapID)
	if err != nil {
		return nil, fmt.Errorf("new map from id %d: %w", innerMapID, err)
	}
	defer func() {
		_ = innerMap.Close()
	}()

	// Iterate and collect
	var key bpfIpKey
	var val bpfTrafficSummary
	iter := innerMap.Iterate()
	for iter.Next(&key, &val) {
		outKey := NetworkTrafficKey{
			SrcPort:  key.Tuple.Sport,
			DstPort:  key.Tuple.Dport,
			Protocol: key.Proto,
			Family:   key.Tuple.Family,
		}

		if key.Tuple.Family == 2 { // AF_INET
			outKey.SrcIP = net.IP(key.Tuple.Saddr.Raw[:4]).String()
			outKey.DstIP = net.IP(key.Tuple.Daddr.Raw[:4]).String()
		} else {
			outKey.SrcIP = net.IP(key.Tuple.Saddr.Raw[:]).String()
			outKey.DstIP = net.IP(key.Tuple.Daddr.Raw[:]).String()
		}

		outVal := TrafficSummary{
			RxPackets: val.RxPackets,
			RxBytes:   val.RxBytes,
			TxPackets: val.TxPackets,
			TxBytes:   val.TxBytes,
		}

		result[outKey] = outVal

		// Clear entry after read to reset for next cycle (when this buffer becomes active again)
		// Deleting while iterating is safe in HASH maps usually, but BatchDelete is better if available.
		// For now simple delete.
		if err := innerMap.Delete(&key); err != nil {
			// Best effort
			t.log.V(1).Error(err, "failed to delete flow entry")
		}
	}

	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("map iteration: %w", err)
	}

	return result, nil
}

func (t *Tracer) Events() <-chan []DNSEvent {
	return t.events
}

func IsKernelBTFAvailable() bool {
	_, err := os.Stat("/sys/kernel/btf/vmlinux")
	return err == nil
}

func InitCgroupv2(log logr.Logger) error {
	_, err := detectCgroupPath()
	if errors.Is(err, ErrCgroup2NotMounted) {
		log.Info("mounting cgroup v2")
		if err := mountCgroup2(); err != nil {
			return fmt.Errorf("cgroup2 not mounted and failed to mount manually: %w", err)
		}
	}
	return nil
}

// dnsDecoder decodes DNS packets off the perf ring without allocating per packet.
//
// What it replaced was gopacket.NewPacket with gopacket.Default, which copies the
// payload and eagerly decodes every layer into freshly allocated structures. That one
// call was the most expensive thing this agent did: 16% of its CPU and 54% of every
// byte it allocated, on a node carrying nothing but ordinary Kubernetes traffic. The
// shape of a DNS packet is known in advance -- IP, UDP, DNS, and the kernel side
// already filters to UDP/53 -- so there is nothing for a general packet decoder to
// discover.
//
// A DecodingLayerParser over preallocated layer structs is gopacket's answer to that.
// layers.DNS is written for it: it truncates its record slices rather than
// reallocating them and decodes names into a scratch buffer it keeps, so a steady
// stream of packets settles into allocating nothing at all.
//
// The price is that everything reachable from the decoded layers is invalidated by the
// next call to decode. A decoder therefore belongs to exactly one goroutine, and
// anything that outlives the call is copied out by event().
type dnsDecoder struct {
	ip4 layers.IPv4
	ip6 layers.IPv6
	udp layers.UDP
	dns layers.DNS

	v4      *gopacket.DecodingLayerParser
	v6      *gopacket.DecodingLayerParser
	decoded []gopacket.LayerType

	srcIP net.IP
	dstIP net.IP
}

func newDNSDecoder() *dnsDecoder {
	d := &dnsDecoder{decoded: make([]gopacket.LayerType, 0, 4)}

	// Two parsers, because a DecodingLayerParser is fixed at construction to the layer
	// type it starts from and the version is only known once the packet is in hand.
	// They share the layer structs, so the second one costs nothing per packet.
	d.v4 = gopacket.NewDecodingLayerParser(layers.LayerTypeIPv4, &d.ip4, &d.udp, &d.dns)
	d.v6 = gopacket.NewDecodingLayerParser(layers.LayerTypeIPv6, &d.ip6, &d.udp, &d.dns)

	// A packet whose next layer has no decoder registered -- a fragment, an unexpected
	// protocol -- stops the parse rather than failing it. It is then caught below by
	// the absence of a DNS layer, which is the condition actually being tested.
	d.v4.IgnoreUnsupported = true
	d.v6.IgnoreUnsupported = true

	return d
}

// decode parses one raw packet into the decoder's layer structs. The decoded values
// are valid until the next call to decode and no longer.
func (d *dnsDecoder) decode(data []byte) error {
	d.srcIP, d.dstIP = nil, nil

	if len(data) == 0 {
		return errors.New("empty data")
	}

	parser := d.v4
	if data[0]>>4 == 6 {
		parser = d.v6
	}

	d.decoded = d.decoded[:0]
	if err := parser.DecodeLayers(data, &d.decoded); err != nil {
		return fmt.Errorf("decoding packet: %w", err)
	}

	var haveDNS bool
	for _, layerType := range d.decoded {
		switch layerType {
		case layers.LayerTypeIPv4:
			d.srcIP, d.dstIP = d.ip4.SrcIP, d.ip4.DstIP
		case layers.LayerTypeIPv6:
			d.srcIP, d.dstIP = d.ip6.SrcIP, d.ip6.DstIP
		case layers.LayerTypeDNS:
			haveDNS = true
		}
	}

	if d.srcIP == nil {
		return errors.New("layer L3 is missing")
	}
	if !haveDNS {
		return errors.New("layer L7 is missing")
	}
	return nil
}

// hasAnswers reports whether the last decoded packet resolved anything.
func (d *dnsDecoder) hasAnswers() bool { return len(d.dns.Answers) > 0 }

// event copies the last decoded packet into a DNSEvent that owns its own memory and is
// therefore safe to send to another goroutine.
func (d *dnsDecoder) event() DNSEvent {
	ev := DNSEvent{
		SrcIP:     cloneIP(d.srcIP),
		DstIP:     cloneIP(d.dstIP),
		Questions: make([]DNSQuestion, 0, len(d.dns.Questions)),
		Answers:   make([]DNSAnswer, 0, len(d.dns.Answers)),
	}
	for i := range d.dns.Questions {
		q := &d.dns.Questions[i]
		ev.Questions = append(ev.Questions, DNSQuestion{
			Name:  string(q.Name),
			Type:  q.Type,
			Class: q.Class,
		})
	}
	for i := range d.dns.Answers {
		a := &d.dns.Answers[i]
		ev.Answers = append(ev.Answers, DNSAnswer{
			Name:  string(a.Name),
			Type:  a.Type,
			Class: a.Class,
			IP:    cloneIP(a.IP),
			CNAME: string(a.CNAME),
		})
	}
	return ev
}

func cloneIP(ip net.IP) net.IP {
	if ip == nil {
		return nil
	}
	return append(net.IP(nil), ip...)
}

func detectCgroupPath() (string, error) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), " ")
		if len(fields) >= 3 && fields[2] == "cgroup2" {
			return fields[1], nil
		}
	}

	return "", ErrCgroup2NotMounted
}
