package ebpf

import (
	"fmt"
	"net"
	"strings"

	"github.com/google/gopacket/layers"
)

// DNSQuestion and DNSAnswer are the parts of a DNS message this agent has a use for.
//
// They are deliberately not gopacket's layers.DNSQuestion and layers.DNSResourceRecord.
// Those carry their names, IPs and RDATA as []byte pointing into the packet buffer and
// into the decoder's scratch space, and the decoder reuses both -- so a record that
// crossed the events channel would be rewritten underneath its reader by whichever
// packet arrived next. Names are held as strings here, which are copies by
// construction, and the only mutable field left is the IP, which the decoder clones.
//
// The narrower shape is also honest about what is filled in. A layers.DNSResourceRecord
// has fifteen fields; copying the three that are read would leave the other twelve
// looking valid and silently wrong.
type DNSQuestion struct {
	Name  string
	Type  layers.DNSType
	Class layers.DNSClass
}

type DNSAnswer struct {
	Name  string
	Type  layers.DNSType
	Class layers.DNSClass
	IP    net.IP
	CNAME string
}

// DNSEvent is one decoded DNS message. It owns everything reachable from it.
type DNSEvent struct {
	SrcIP     net.IP
	DstIP     net.IP
	Questions []DNSQuestion
	Answers   []DNSAnswer
}

func (e DNSEvent) String() string {
	var str strings.Builder
	for _, v := range e.Questions {
		str.WriteString("Questions:\n")
		str.WriteString(fmt.Sprintf("%s %s %s \n", v.Class, v.Type, v.Name))
	}
	for _, v := range e.Answers {
		str.WriteString("Answers:\n")
		str.WriteString(
			fmt.Sprintf("%s %s %s [%s] [%s] \n", v.Class, v.Type, v.Name, v.IP, v.CNAME),
		)
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
