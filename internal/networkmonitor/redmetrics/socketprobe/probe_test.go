package socketprobe

import (
	"encoding/binary"
	"net/netip"
	"testing"
	"time"

	"github.com/devzero-inc/zxporter/internal/networkmonitor/redmetrics"
)

// record builds one ring buffer record by hand, the way the probe writes it.
type record struct {
	cookie      uint64
	timestampNs uint64
	src         []byte
	dst         []byte
	family      uint16
	lport       uint16
	dport       uint16
	direction   uint8
	trueLen     uint32
	copyLen     uint32
	payload     []byte
}

func (r record) bytes() []byte {
	raw := make([]byte, recordSize)
	binary.LittleEndian.PutUint64(raw[offSocketCookie:], r.cookie)
	binary.LittleEndian.PutUint64(raw[offTimestampNs:], r.timestampNs)
	copy(raw[offSrcAddr:offSrcAddr+16], r.src)
	copy(raw[offDstAddr:offDstAddr+16], r.dst)
	binary.LittleEndian.PutUint16(raw[offFamily:], r.family)
	binary.LittleEndian.PutUint16(raw[offLocalPort:], r.lport)
	binary.LittleEndian.PutUint16(raw[offRemotePort:], r.dport)
	raw[offDirection] = r.direction
	binary.LittleEndian.PutUint32(raw[offTrueLen:], r.trueLen)
	binary.LittleEndian.PutUint32(raw[offCopyLen:], r.copyLen)
	copy(raw[offPayload:], r.payload)
	return raw
}

func v4(a, b, c, d byte) []byte { return []byte{a, b, c, d} }

var testOffset = clockOffset{
	wall:   time.Date(2026, 8, 21, 10, 0, 30, 0, time.UTC),
	baseNs: 1_000_000_000,
}

// The record layout is a contract with c/socketprobe.c: the C side writes struct
// sr_event and this side reads it by offset, so a silent drift mis-decodes every
// field after the one that moved. Both sides assert against the same literals
// rather than against each other -- the C file carries a _Static_assert per field,
// so a field that moves there fails that compile, and one that moves here fails
// this test.
func TestRecordLayoutMatchesTheProgram(t *testing.T) {
	fields := []struct {
		name string
		got  int
		want int
	}{
		{"socket_cookie", offSocketCookie, 0},
		{"timestamp_ns", offTimestampNs, 8},
		{"saddr", offSrcAddr, 16},
		{"daddr", offDstAddr, 32},
		{"family", offFamily, 48},
		{"lport", offLocalPort, 50},
		{"dport", offRemotePort, 52},
		{"direction", offDirection, 54},
		{"true_len", offTrueLen, 56},
		{"copy_len", offCopyLen, 60},
		{"payload", offPayload, 64},
	}
	for _, f := range fields {
		if f.got != f.want {
			t.Errorf("offset of sr_event.%s = %d, want %d", f.name, f.got, f.want)
		}
	}

	if recordHeaderSize != 64 {
		t.Fatalf("header size = %d, want 64 (struct sr_event up to payload)", recordHeaderSize)
	}
	if redmetrics.PayloadCap != 2048 {
		t.Fatalf("payload cap = %d, want 2048 (SR_MAX_PAYLOAD)", redmetrics.PayloadCap)
	}
	if recordSize != 2112 {
		t.Fatalf("record size = %d, want 2112 (sizeof(struct sr_event))", recordSize)
	}
}

func TestDecodeEvent_SendRecord(t *testing.T) {
	payload := []byte("GET / HTTP/1.1\r\n")
	raw := record{
		cookie:      0xdeadbeef,
		timestampNs: 3_000_000_000, // two seconds past the captured base
		src:         v4(10, 0, 0, 7),
		dst:         v4(10, 0, 1, 9),
		family:      afInet,
		lport:       54321,
		dport:       8080,
		direction:   dirSend,
		trueLen:     uint32(len(payload)),
		copyLen:     uint32(len(payload)),
		payload:     payload,
	}.bytes()

	ev, ok := decodeEvent(raw, testOffset)
	if !ok {
		t.Fatal("decodeEvent did not decode a well formed record")
	}
	if ev.SocketCookie != 0xdeadbeef {
		t.Errorf("socket cookie = %#x, want 0xdeadbeef", ev.SocketCookie)
	}
	if ev.Path != redmetrics.SyscallSend {
		t.Errorf("path = %v, want send", ev.Path)
	}
	if string(ev.Payload) != string(payload) {
		t.Errorf("payload = %q, want %q", ev.Payload, payload)
	}
	if got, want := ev.LocalAddr.String(), "10.0.0.7"; got != want {
		t.Errorf("local address = %s, want %s", got, want)
	}
	if got, want := ev.RemoteAddr.String(), "10.0.1.9"; got != want {
		t.Errorf("remote address = %s, want %s", got, want)
	}
	if ev.LocalPort != 54321 || ev.RemotePort != 8080 {
		t.Errorf("ports = %d/%d, want 54321/8080", ev.LocalPort, ev.RemotePort)
	}
	if want := testOffset.wall.Add(2 * time.Second); !ev.Timestamp.Equal(want) {
		t.Errorf("timestamp = %s, want %s", ev.Timestamp, want)
	}
}

func TestDecodeEvent_ReturnRecordIsAReceive(t *testing.T) {
	raw := record{
		src:       v4(10, 0, 0, 7),
		dst:       v4(10, 0, 1, 9),
		family:    afInet,
		direction: dirRecv,
		trueLen:   16,
	}.bytes()

	ev, ok := decodeEvent(raw, testOffset)
	if !ok {
		t.Fatal("decodeEvent did not decode a well formed record")
	}
	if ev.Path != redmetrics.SyscallRecv {
		t.Errorf("path = %v, want recv", ev.Path)
	}
}

// A close record carries no bytes. It is still a record about a real connection, so
// its tuple has to decode exactly like a data record's -- that is the whole reason
// the close probe reads the socket the same way, and it is what lets the aggregator
// find the entry it is being told to release.
func TestDecodeEvent_CloseRecord(t *testing.T) {
	raw := record{
		src:       v4(10, 0, 0, 7),
		lport:     8080,
		dst:       v4(10, 0, 1, 9),
		dport:     54321,
		family:    afInet,
		direction: dirClose,
	}.bytes()

	ev, ok := decodeEvent(raw, testOffset)
	if !ok {
		t.Fatal("decodeEvent did not decode a well formed record")
	}
	if ev.Path != redmetrics.SyscallClose {
		t.Errorf("path = %v, want close", ev.Path)
	}
	if ev.TrueLen != 0 {
		t.Errorf("true length = %d, want 0: a close carries no bytes", ev.TrueLen)
	}
	if len(ev.Payload) != 0 {
		t.Errorf("payload = %d bytes, want none", len(ev.Payload))
	}
	if ev.LocalPort != 8080 || ev.RemotePort != 54321 {
		t.Errorf("ports = %d/%d, want 8080/54321", ev.LocalPort, ev.RemotePort)
	}
}

// The close probe reserves the 64-byte header alone rather than the whole 2112-byte
// record, because a close carries no payload and the ring buffer is the resource that
// decides whether a measurement is taken at all. That makes a truncated record the
// normal shape of a close, not a corruption, so the decoder has to read one correctly
// -- and it must not read the payload region that is genuinely not there.
//
// This is the Go half of that contract; the C half is the reservation size in
// sr_emit_close.
func TestDecodeEvent_CloseRecordIsHeaderOnly(t *testing.T) {
	full := record{
		src:       v4(10, 0, 0, 7),
		lport:     8080,
		dst:       v4(10, 0, 1, 9),
		dport:     54321,
		family:    afInet,
		direction: dirClose,
	}.bytes()

	raw := full[:recordHeaderSize]

	ev, ok := decodeEvent(raw, testOffset)
	if !ok {
		t.Fatalf("decodeEvent rejected a %d byte close record: the probe emits exactly this", len(raw))
	}
	if ev.Path != redmetrics.SyscallClose {
		t.Errorf("path = %v, want close", ev.Path)
	}
	if ev.Payload != nil {
		t.Errorf("payload = %d bytes, want nil: there is no payload region in the record", len(ev.Payload))
	}
	if ev.LocalPort != 8080 || ev.RemotePort != 54321 {
		t.Errorf("ports = %d/%d, want 8080/54321: the tuple must survive the truncation",
			ev.LocalPort, ev.RemotePort)
	}
	if ev.SocketCookie != 0 || ev.TrueLen != 0 {
		t.Errorf("cookie = %d, true length = %d, want 0/0", ev.SocketCookie, ev.TrueLen)
	}
}

// The probe copies at most 2KB of any one call but always reports the call's true
// length, so a large request stays a large request in the aggregate.
func TestDecodeEvent_TruncatedCopyKeepsTheTrueLength(t *testing.T) {
	payload := make([]byte, redmetrics.PayloadCap)
	for i := range payload {
		payload[i] = byte(i)
	}
	raw := record{
		src:     v4(10, 0, 0, 7),
		dst:     v4(10, 0, 1, 9),
		family:  afInet,
		trueLen: 1 << 20,
		copyLen: redmetrics.PayloadCap,
		payload: payload,
	}.bytes()

	ev, ok := decodeEvent(raw, testOffset)
	if !ok {
		t.Fatal("decodeEvent did not decode a well formed record")
	}
	if ev.TrueLen != 1<<20 {
		t.Errorf("true length = %d, want %d", ev.TrueLen, 1<<20)
	}
	if len(ev.Payload) != redmetrics.PayloadCap {
		t.Errorf("payload length = %d, want the cap %d", len(ev.Payload), redmetrics.PayloadCap)
	}
}

// The ring buffer hands out a view of memory it reuses. A decoder keeps a partially
// consumed stream across events, so a payload that aliased the sample would be read
// again after the kernel had overwritten it with an unrelated syscall.
func TestDecodeEvent_PayloadDoesNotAliasTheSample(t *testing.T) {
	payload := []byte("GET /orders HTTP/1.1\r\n\r\n")
	raw := record{
		src:     v4(10, 0, 0, 7),
		dst:     v4(10, 0, 1, 9),
		family:  afInet,
		trueLen: uint32(len(payload)),
		copyLen: uint32(len(payload)),
		payload: payload,
	}.bytes()

	ev, ok := decodeEvent(raw, testOffset)
	if !ok {
		t.Fatal("decodeEvent did not decode a well formed record")
	}
	if string(ev.Payload) != string(payload) {
		t.Fatalf("payload = %q, want %q", ev.Payload, payload)
	}

	// Stand in for the kernel reusing the sample.
	for i := range raw {
		raw[i] = 0xff
	}
	if string(ev.Payload) != string(payload) {
		t.Fatalf("payload after the sample was reused = %q, want %q", ev.Payload, payload)
	}
}

// copy_len says how much of the call the probe managed to read. It arrives from the
// kernel, so a value past the end of the record has to be clamped rather than used
// to index: the alternative is a panic on the ring buffer's hot path.
func TestDecodeEvent_CopyLengthIsClampedToTheCap(t *testing.T) {
	tests := []struct {
		name    string
		copyLen uint32
		want    int
	}{
		{name: "no bytes copied", copyLen: 0, want: 0},
		{name: "the whole cap", copyLen: redmetrics.PayloadCap, want: redmetrics.PayloadCap},
		{name: "past the cap", copyLen: redmetrics.PayloadCap * 4, want: redmetrics.PayloadCap},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := record{
				src:     v4(10, 0, 0, 7),
				dst:     v4(10, 0, 1, 9),
				family:  afInet,
				trueLen: 1 << 20,
				copyLen: tc.copyLen,
				payload: make([]byte, redmetrics.PayloadCap),
			}.bytes()

			ev, ok := decodeEvent(raw, testOffset)
			if !ok {
				t.Fatal("decodeEvent did not decode a well formed record")
			}
			if len(ev.Payload) != tc.want {
				t.Fatalf("payload length = %d, want %d", len(ev.Payload), tc.want)
			}
		})
	}
}

func TestDecodeEvent_IPv6AndMappedAddresses(t *testing.T) {
	six := netip.MustParseAddr("fd00::1").As16()
	mapped := netip.MustParseAddr("::ffff:10.0.0.7").As16()

	tests := []struct {
		name     string
		src, dst []byte
		wantSrc  string
		wantDst  string
	}{
		{
			name:    "native v6 is preserved",
			src:     six[:],
			dst:     six[:],
			wantSrc: "fd00::1",
			wantDst: "fd00::1",
		},
		{
			// A dual stack socket reports v4 traffic as v4 mapped v6. Unmapping keeps
			// it the same string dakr resolves for the v4 socket to the same pod.
			name:    "v4 mapped v6 is unmapped",
			src:     mapped[:],
			dst:     mapped[:],
			wantSrc: "10.0.0.7",
			wantDst: "10.0.0.7",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := record{src: tc.src, dst: tc.dst, family: afInet6, trueLen: 8}.bytes()
			ev, ok := decodeEvent(raw, testOffset)
			if !ok {
				t.Fatal("decodeEvent did not decode a well formed record")
			}
			if got := ev.LocalAddr.String(); got != tc.wantSrc {
				t.Errorf("local address = %s, want %s", got, tc.wantSrc)
			}
			if got := ev.RemoteAddr.String(); got != tc.wantDst {
				t.Errorf("remote address = %s, want %s", got, tc.wantDst)
			}
		})
	}
}

func TestDecodeEvent_RejectsRecordsItCannotRead(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
	}{
		{
			name: "short record",
			raw:  make([]byte, recordHeaderSize-1),
		},
		{
			// The program filters for AF_INET and AF_INET6; anything else means the
			// record is not what this decoder thinks it is.
			name: "unhandled address family",
			raw:  record{src: v4(10, 0, 0, 7), dst: v4(10, 0, 1, 9), family: 1, trueLen: 8}.bytes(),
		},
		{
			// A direction this build does not know means the loaded program is not the
			// one this decoder was compiled against. Rejecting is the only safe
			// reading: the discarded alternative was to treat an unrecognised value as
			// a send, and on a connection's first event that classifies it outbound --
			// a verdict the aggregator never revisits, so the connection is silently
			// never measured at all.
			name: "unknown direction",
			raw: record{
				src: v4(10, 0, 0, 7), dst: v4(10, 0, 1, 9),
				family: afInet, direction: dirClose + 1, trueLen: 8,
			}.bytes(),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := decodeEvent(tc.raw, testOffset); ok {
				t.Fatal("decodeEvent accepted a record it cannot read")
			}
		})
	}
}

// Events stamped by the kernel are monotonic nanoseconds; the offset is captured
// once at attach and every event is placed relative to it. An event stamped just
// before the capture must land just before it, not far in the future.
func TestClockOffset_ToWall(t *testing.T) {
	tests := []struct {
		name string
		ns   uint64
		want time.Time
	}{
		{name: "at the base", ns: testOffset.baseNs, want: testOffset.wall},
		{name: "after the base", ns: testOffset.baseNs + uint64(90*time.Second), want: testOffset.wall.Add(90 * time.Second)},
		{name: "before the base", ns: testOffset.baseNs - uint64(250*time.Millisecond), want: testOffset.wall.Add(-250 * time.Millisecond)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := testOffset.toWall(tc.ns); !got.Equal(tc.want) {
				t.Errorf("toWall(%d) = %s, want %s", tc.ns, got, tc.want)
			}
		})
	}
}

// Without an offset there is no way to place an event on the wall clock, and a
// zero time is the honest answer rather than a timestamp near the Unix epoch.
func TestClockOffset_ZeroOffsetYieldsZeroTime(t *testing.T) {
	if got := (clockOffset{}).toWall(12345); !got.IsZero() {
		t.Errorf("toWall on a zero offset = %s, want the zero time", got)
	}
}
