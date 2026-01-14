package ebpf

import (
	"fmt"
	"net"
	"strings"

	"github.com/google/gopacket/layers"
)

type DNSEvent struct {
	SrcIP     net.IP
	DstIP     net.IP
	Questions []layers.DNSQuestion
	Answers   []layers.DNSResourceRecord
}

func (e DNSEvent) String() string {
	var str strings.Builder
	for _, v := range e.Questions {
		str.WriteString("Questions:\n")
		str.WriteString(fmt.Sprintf("%s %s %s \n", v.Class, v.Type, string(v.Name)))
	}
	for _, v := range e.Answers {
		str.WriteString("Answers:\n")
		str.WriteString(fmt.Sprintf("%s %s %s [%s] [%s] \n", v.Class, v.Type, string(v.Name), v.IP, v.CNAME))
	}
	return str.String()
}

type NetworkTrafficKey struct {
	SrcIP    string
	DstIP    string
	SrcPort  uint16
	DstPort  uint16
	Protocol uint8
	Family   uint16
}

type TrafficSummary struct {
	RxPackets uint64
	RxBytes   uint64
	TxPackets uint64
	TxBytes   uint64
}
