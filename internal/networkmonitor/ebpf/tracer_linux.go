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
	if err := loadBpfObjects(&objs, &ebpf.CollectionOptions{
		Maps: ebpf.MapOptions{},
		Programs: ebpf.ProgramOptions{
			KernelTypes: customBTF,
		},
		MapReplacements: nil,
	}); err != nil {
		return fmt.Errorf("loading objects: %w", err)
	}
	defer objs.Close()

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

	l, err := link.AttachCgroup(link.CgroupOptions{
		Path:    cgroupPath,
		Attach:  ebpf.AttachCGroupInetIngress,
		Program: objs.CgroupIngress,
	})
	if err != nil {
		return fmt.Errorf("attaching cgroup: %w", err)
	}
	defer l.Close()

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
			t.log.Info("skipping too small event", "bytes", len(record.RawSample))
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
			t.log.Info("dropping event, queue is full")
			continue
		}
	}
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
