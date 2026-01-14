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
		defer innerMap.Close()

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
			l.Close()
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

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		record, err := reader.Read()
		if err != nil {
			if errors.Is(err, perf.ErrClosed) {
				return nil
			}
			return err
		}

		if len(record.RawSample) < 4 {
			t.log.V(1).Info("skipping too small event", "bytes", len(record.RawSample))
			continue
		}

		// First 4 bytes now reserved for payload size. See net_event_context in types.h for full structure.
		event, err := parseEvent(record.RawSample[4:])
		if err != nil {
			t.log.Error(err, "parsing event")
			continue
		}

		select {
		case t.events <- event:
		default:
			t.log.V(1).Info("dropping event, queue is full")
			continue
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
	if err := t.objs.NetworkTrafficBufferMap.Lookup(int32(activeIdx), &innerMapID); err != nil {
		return nil, fmt.Errorf("lookup outer map index %d: %w", activeIdx, err)
	}

	innerMap, err := ebpf.NewMapFromID(innerMapID)
	if err != nil {
		return nil, fmt.Errorf("new map from id %d: %w", innerMapID, err)
	}
	defer innerMap.Close()

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

func (t *Tracer) Events() <-chan DNSEvent {
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

func parseEvent(data []byte) (DNSEvent, error) {
	packet := gopacket.NewPacket(
		data,
		layers.LayerTypeIPv4,
		gopacket.Default,
	)

	var res DNSEvent
	if packet == nil {
		return res, errors.New("parsing packet")
	}

	ipLayer := packet.NetworkLayer()
	if ipLayer == nil {
		return res, errors.New("layer L3 is missing")
	}

	appLayer := packet.ApplicationLayer()
	if appLayer == nil {
		return res, errors.New("layer L7 is missing")
	}

	dns, ok := appLayer.(*layers.DNS)
	if !ok {
		return res, fmt.Errorf("expected dns layer, actual type %T", appLayer)
	}

	srcIP, dstIP := getIPs(ipLayer)

	return DNSEvent{
		SrcIP:     srcIP,
		DstIP:     dstIP,
		Questions: dns.Questions,
		Answers:   dns.Answers,
	}, nil
}

func getIPs(l gopacket.NetworkLayer) (net.IP, net.IP) {
	if ipv4, ok := l.(*layers.IPv4); ok {
		return ipv4.SrcIP, ipv4.DstIP
	}
	if ipv6, ok := l.(*layers.IPv6); ok {
		return ipv6.SrcIP, ipv6.DstIP
	}
	return nil, nil
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
