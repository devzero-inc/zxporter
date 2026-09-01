package redmetrics

import (
	"net/netip"
	"time"
)

// PayloadCap is the most bytes a single socket observation carries into
// userspace. The probe copies at most this much per call and always reports the
// call's true length separately, so a decoder can advance its stream position
// through a body it never saw.
//
// It is the reason a truncated message boundary costs a connection its
// decodability while a truncated body does not: a body is walked past by count,
// but a header block has to be read, and 2KB is the budget for reading one.
const PayloadCap = 2048

// SyscallPath says which socket call the event came from. Direction falls out of
// the two that carry bytes: the first bytes seen on a connection arriving on the
// receive path mean this node is answering a request, and on the send path mean it
// is making one.
type SyscallPath uint8

// The socket calls the probe attaches to.
const (
	// SyscallSend is tcp_sendmsg: bytes leaving this node.
	SyscallSend SyscallPath = iota
	// SyscallRecv is tcp_recvmsg: bytes arriving at this node.
	SyscallRecv
	// SyscallClose is tcp_close: the socket is going away. It carries no bytes --
	// TrueLen is zero and Payload is nil -- and says only that this connection will
	// never be seen again, which is what lets its state be released at the moment it
	// stops being worth anything instead of when an idle timer runs out.
	SyscallClose
)

// String renders the syscall path for logs.
func (p SyscallPath) String() string {
	switch p {
	case SyscallSend:
		return "send"
	case SyscallRecv:
		return "recv"
	case SyscallClose:
		return "close"
	default:
		return "unknown"
	}
}

// SocketEvent is one observation of bytes crossing a TCP socket, read off the
// socket structure rather than parsed out of packet headers. Reading the socket
// rather than the wire is what removes both header parsing and NAT ambiguity:
// the addresses are the ones the kernel itself associates with the connection.
type SocketEvent struct {
	// SocketCookie identifies the socket the bytes crossed. It is opaque and only
	// unique among sockets alive at the same instant: the kernel recycles the
	// structure it derives from, so it must be paired with the connection's
	// four-tuple before two observations can be called the same connection.
	SocketCookie uint64

	// Path is the syscall the bytes crossed on.
	Path SyscallPath

	// LocalAddr and LocalPort are this node's side of the connection. For an
	// inbound service request these are the served pod and the port it listens on,
	// which is what attribution keys off.
	LocalAddr netip.Addr
	LocalPort uint16

	// RemoteAddr and RemotePort are the peer's side of the connection. They are
	// recorded but never aggregated by: keying on the caller is exactly what would
	// make the in-memory map grow with caller cardinality.
	RemoteAddr netip.Addr
	RemotePort uint16

	// TrueLen is the length the syscall actually carried, whatever prefix of it the
	// probe copied. The decoder walks by this rather than by len(Payload), which is
	// what lets a body larger than the copy limit be stepped over without losing the
	// stream position.
	TrueLen uint32

	// Payload is the prefix of those bytes the probe copied, at most PayloadCap of
	// them and often fewer. It is a copy rather than a view of the ring buffer
	// sample, which the kernel reuses the moment the record is released.
	//
	// It may be shorter than TrueLen and may be empty. A decoder must treat a
	// boundary that falls beyond it as unreadable rather than as absent.
	Payload []byte

	// Timestamp is when the kernel saw the call, already mapped onto wall clock.
	Timestamp time.Time
}
