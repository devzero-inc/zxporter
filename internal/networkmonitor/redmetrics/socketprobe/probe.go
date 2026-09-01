// Package socketprobe attaches the socket probes that feed container RED metric
// aggregation and turns their ring buffer records into redmetrics.SocketEvent
// values.
//
// The probes are four eBPF programs across three attach points: tcp_sendmsg,
// tcp_recvmsg on both entry and return, and tcp_close. They read addresses and
// ports off the socket structure, so no packet header is parsed, and they add no
// capability, host mount or kernel requirement beyond what the DaemonSet already
// has.
//
// Only the loader is Linux-only. The record layout and its decoder live here so
// they can be tested anywhere.
package socketprobe

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"time"

	"github.com/devzero-inc/zxporter/internal/networkmonitor/redmetrics"
)

// ErrUnsupported is returned when the probes cannot run on this platform.
var ErrUnsupported = errors.New("socketprobe: socket probes require Linux")

// Record layout, matching struct sr_event in c/socketprobe.c. The decoder reads
// by offset, so a change on either side has to be made on both.
const (
	offSocketCookie = 0
	offTimestampNs  = 8
	offSrcAddr      = 16
	offDstAddr      = 32
	offFamily       = 48
	offLocalPort    = 50
	offRemotePort   = 52
	offDirection    = 54
	offTrueLen      = 56
	offCopyLen      = 60
	offPayload      = 64

	// recordHeaderSize is everything before the payload.
	recordHeaderSize = offPayload
	// recordSize is one whole ring buffer record.
	recordSize = recordHeaderSize + redmetrics.PayloadCap
)

// Address families as the kernel reports them on the socket.
const (
	afInet  = 2
	afInet6 = 10
)

// Direction values written by the probe, matching SR_DIR_SEND, SR_DIR_RECV and
// SR_DIR_CLOSE.
const (
	dirSend  = 0
	dirRecv  = 1
	dirClose = 2
)

// clockOffset converts the kernel monotonic clock the probe stamps events with
// into wall clock. It is captured once, when the probe starts.
type clockOffset struct {
	wall   time.Time
	baseNs uint64
}

// toWall maps a kernel monotonic timestamp onto the wall clock.
func (o clockOffset) toWall(ns uint64) time.Time {
	if o.wall.IsZero() {
		return time.Time{}
	}
	if ns >= o.baseNs {
		return o.wall.Add(time.Duration(ns - o.baseNs))
	}
	return o.wall.Add(-time.Duration(o.baseNs - ns))
}

// decodeEvent turns one ring buffer record into a socket event. It reports false
// for a record that is too short or carries an address family the probe does not
// handle, which is the same thing the probe already filters for.
func decodeEvent(raw []byte, off clockOffset) (redmetrics.SocketEvent, bool) {
	if len(raw) < recordHeaderSize {
		return redmetrics.SocketEvent{}, false
	}

	family := binary.LittleEndian.Uint16(raw[offFamily:])
	local, ok := decodeAddr(raw[offSrcAddr:offSrcAddr+16], family)
	if !ok {
		return redmetrics.SocketEvent{}, false
	}
	remote, ok := decodeAddr(raw[offDstAddr:offDstAddr+16], family)
	if !ok {
		return redmetrics.SocketEvent{}, false
	}

	// Mapped exhaustively rather than by defaulting the unrecognised value to a
	// send. A direction this build does not know is drift against the compiled
	// program, and calling it a send is the worst available guess: an unrecognised
	// value on a connection's first event would classify it outbound, which is a
	// verdict the aggregator never revisits.
	var path redmetrics.SyscallPath
	switch raw[offDirection] {
	case dirSend:
		path = redmetrics.SyscallSend
	case dirRecv:
		path = redmetrics.SyscallRecv
	case dirClose:
		path = redmetrics.SyscallClose
	default:
		return redmetrics.SocketEvent{}, false
	}

	return redmetrics.SocketEvent{
		SocketCookie: binary.LittleEndian.Uint64(raw[offSocketCookie:]),
		Path:         path,
		LocalAddr:    local,
		LocalPort:    binary.LittleEndian.Uint16(raw[offLocalPort:]),
		RemoteAddr:   remote,
		RemotePort:   binary.LittleEndian.Uint16(raw[offRemotePort:]),
		TrueLen:      binary.LittleEndian.Uint32(raw[offTrueLen:]),
		Payload:      decodePayload(raw),
		Timestamp:    off.toWall(binary.LittleEndian.Uint64(raw[offTimestampNs:])),
	}, true
}

// decodePayload copies the prefix of the message the probe captured.
//
// The copy is not an oversight to be optimised away later. The ring buffer hands
// out a view of memory it reuses as soon as the record is released, so a decoder
// holding a sub-slice of it across events -- which is exactly what a partially
// consumed stream does -- would read whatever syscall came next. The cost is bounded
// by PayloadCap and is the price of reading message boundaries at all.
//
// copy_len is what the probe managed to read, which is at most true_len and at most
// the cap, but it arrives from the kernel and is clamped here rather than trusted: a
// bad value would otherwise index past the end of the record.
func decodePayload(raw []byte) []byte {
	copyLen := int(binary.LittleEndian.Uint32(raw[offCopyLen:]))
	if copyLen > redmetrics.PayloadCap {
		copyLen = redmetrics.PayloadCap
	}
	if copyLen > len(raw)-offPayload {
		copyLen = len(raw) - offPayload
	}
	if copyLen <= 0 {
		return nil
	}
	payload := make([]byte, copyLen)
	copy(payload, raw[offPayload:offPayload+copyLen])
	return payload
}

// decodeAddr reads an address out of the fixed sixteen byte field. A v4 mapped
// v6 address is unmapped, so a dual stack socket and a v4 socket talking to the
// same pod produce the same string.
func decodeAddr(b []byte, family uint16) (netip.Addr, bool) {
	switch family {
	case afInet:
		return netip.AddrFrom4([4]byte(b[:4])), true
	case afInet6:
		addr := netip.AddrFrom16([16]byte(b[:16]))
		return addr.Unmap(), true
	default:
		return netip.Addr{}, false
	}
}
