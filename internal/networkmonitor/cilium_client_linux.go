//go:build linux

package networkmonitor

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/cilium/cilium/pkg/bpf"
	"github.com/cilium/cilium/pkg/defaults"
	"github.com/cilium/cilium/pkg/maps/ctmap"
	"github.com/cilium/cilium/pkg/maps/nat"
	"inet.af/netaddr"
)

// https://raw.githubusercontent.com/cilium/cilium/main/pkg/defaults/defaults.go
const (
	bpfMapRoot = defaults.BPFFSRoot
	bpfMaps    = defaults.TCGlobalsPath
)

func bpfMapsExist() bool {
	path := filepath.Join(bpfMapRoot, bpfMaps)
	file, err := os.Stat(path)
	return err == nil && file != nil
}

func listRecords(
	maps []interface{},
	clockSource ClockSource,
	filter EntriesFilter,
) ([]*Entry, error) {
	entries := make([]*Entry, 0)

	now := time.Now().UTC()

	timeDiff, err := kernelTimeDiffSecondsFunc(clockSource)
	if err != nil {
		return nil, fmt.Errorf("getting kernel time diff func: %w", err)
	}

	for _, m := range maps {
		m := m.(ctmap.CtMap)
		_, err := m.Path()
		if err == nil {
			err = m.Open()
		}
		if err != nil {
			if os.IsNotExist(err) {
				continue
				// return nil, fmt.Errorf("unable to open map %s: %w", path, err)
			}
		}

		defer m.Close()
		cb := func(key bpf.MapKey, v bpf.MapValue) {
			k := key.(ctmap.CtKey).ToHost().(*ctmap.CtKey4Global)
			if k.NextHeader == 0 {
				return
			}

			srcIP := k.DestAddr.IP() // Addresses are swapped due to cilium issue #21346.
			dstIP := k.SourceAddr.IP()
			val := v.(*ctmap.CtEntry)
			// No DstTranslated on this path. A Cilium CT entry's key holds one
			// tuple and no reference to the backend a service connection was
			// load-balanced to -- that mapping lives in the separate lb4 backend
			// and SNAT maps this collector does not read. DNAT'd flows collected
			// here therefore report only the address the client dialed, and the
			// control plane cannot attribute them to the pod that answered.
			// See NetworkTrafficItem.dst_ip_translated.
			expireSeconds := timeDiff(int64(val.Lifetime))
			record := &Entry{
				Src: netaddr.IPPortFrom(
					netaddr.IPv4(srcIP[0], srcIP[1], srcIP[2], srcIP[3]),
					k.SourcePort,
				),
				Dst: netaddr.IPPortFrom(
					netaddr.IPv4(dstIP[0], dstIP[1], dstIP[2], dstIP[3]),
					k.DestPort,
				),
				TxBytes:   val.Bytes,
				TxPackets: val.Packets,
				RxBytes:   0,
				RxPackets: 0,
				Lifetime:  now.Add(time.Duration(expireSeconds) * time.Second),
				Proto:     uint8(k.NextHeader),
			}
			if filter(record) {
				entries = append(entries, record)
			}
		}
		if err = m.DumpWithCallback(cb); err != nil {
			return nil, fmt.Errorf("error while collecting BPF map entries: %w", err)
		}
	}
	return entries, nil
}

func initMaps() []interface{} {
	// We might need to handle failure here gracefully or ensure we have permissions
	// IPv4-only global CT maps, paired with the IPv4 global SNAT map. Cilium
	// 1.19 dropped InitMapInfo(v4, v6, nodeport) in favour of passing the NAT
	// maps in, and renamed GlobalMaps to Maps; this is the same configuration
	// the 1.17 InitMapInfo(true, false, true) call built internally.
	nat4, _ := nat.GlobalMaps(nil, true, false)
	ctmap.InitMapInfo(nat4, nil)
	maps := ctmap.Maps(true, false)
	ctMaps := make([]interface{}, len(maps))
	for i, m := range maps {
		ctMaps[i] = m
	}
	return ctMaps
}
