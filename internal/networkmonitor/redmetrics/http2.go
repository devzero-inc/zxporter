package redmetrics

import (
	"encoding/binary"
	"strconv"
	"time"

	"golang.org/x/net/http2/hpack"
)

// http2Preface is the 24 bytes an h2c client sends before its first frame. It is
// the only unambiguous marker HTTP/2 has: everything after it is binary framing
// that looks like nothing in particular, so a connection the agent joined after
// the preface cannot be told from noise and is not decoded at all.
var http2Preface = []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")

// frameHeaderLen is the fixed size of an HTTP/2 frame header: a 24-bit length, a
// type, a set of flags, and a 31-bit stream id behind a reserved bit.
const frameHeaderLen = 9

// Frame types. Only the ones whose presence changes what is measured are named;
// everything else is stepped over by its declared length, which is the whole
// reason HTTP/2 is cheaper to walk than HTTP/1.1 -- every frame says how long it
// is, so there is no body whose end has to be inferred.
const (
	frameData         uint8 = 0x0
	frameHeaders      uint8 = 0x1
	frameRSTStream    uint8 = 0x3
	frameSettings     uint8 = 0x4
	framePushPromise  uint8 = 0x5
	frameContinuation uint8 = 0x9
)

// Frame flags. END_STREAM and END_HEADERS share their bit positions with flags
// that mean something else on other frame types -- 0x1 is ACK on SETTINGS and on
// PING -- so each is only ever read off a frame type it is defined for.
const (
	flagEndStream  uint8 = 0x1
	flagEndHeaders uint8 = 0x4
	flagPadded     uint8 = 0x8
	flagPriority   uint8 = 0x20
)

// settingHeaderTableSize is the SETTINGS parameter that raises the HPACK dynamic
// table ceiling. It is read because the decoder rejects a table size update above
// what it was told to allow, and a peer that negotiated a larger table will send
// one.
const settingHeaderTableSize uint16 = 0x1

// defaultHeaderTableSize is the HPACK dynamic table size every connection starts
// at, from RFC 7541. A peer may raise it in SETTINGS.
const defaultHeaderTableSize = 4096

// http2Half is the HPACK and header-block state of one direction.
//
// There is one per direction because HPACK is stateful and the two directions
// have SEPARATE dynamic tables: a request header block and a response header
// block are encoded against different histories, and feeding both to one decoder
// desynchronises it into decoding subsequent blocks as garbage -- or, worse, as
// plausible wrong headers.
type http2Half struct {
	// dec holds this direction's dynamic table. Every header block that crosses
	// this direction must be fed to it, in order: a block that is skipped is a
	// block whose table updates were skipped, and everything after it decodes
	// against a table the peer is not using.
	dec *hpack.Decoder

	// block accumulates a header block that HTTP/2 split across a HEADERS frame
	// and its CONTINUATIONs. HPACK cannot decode a fragment, so nothing is read
	// until END_HEADERS arrives.
	block []byte

	// blockOpen says a header block is still accumulating. While it is set, the
	// only legal frame on this direction is a CONTINUATION on the same stream.
	blockOpen bool

	// blockStream and blockEndStream carry the HEADERS frame's stream and its
	// END_STREAM flag forward across the CONTINUATIONs, which have neither: a
	// CONTINUATION cannot say the stream ended, so the frame that opened the block
	// is the only place that is known.
	blockStream    uint32
	blockEndStream bool
}

// http2Conn is the per-connection HTTP/2 state, allocated when a connection is
// recognised as HTTP/2 and never for an HTTP/1.1 one.
type http2Conn struct {
	request, response http2Half

	// maxStarted is the highest stream id a request-half header block has opened.
	// HTTP/2 requires every new client-initiated stream to use an id greater than
	// all the client has opened before (RFC 9113 5.1.1), so this is what tells a
	// new request apart from a second block on a stream already accounted for.
	maxStarted uint32
}

func newHTTP2Conn() *http2Conn {
	return &http2Conn{
		request:  http2Half{dec: hpack.NewDecoder(defaultHeaderTableSize, nil)},
		response: http2Half{dec: hpack.NewDecoder(defaultHeaderTableSize, nil)},
	}
}

// h2half is the HTTP/2 state of the half an event's bytes crossed on.
func (c *connection) h2half(which role) *http2Half {
	if which == roleRequest {
		return &c.h2.request
	}
	return &c.h2.response
}

// oppositeHalf is the other direction. A SETTINGS frame describes what its SENDER
// will accept, so it constrains the decoder reading the other direction.
func oppositeHalf(which role) role {
	if which == roleRequest {
		return roleResponse
	}
	return roleRequest
}

// sniffLocked decides, from the first bytes to arrive on a connection, which
// decoder reads the rest of it.
//
// The h2c preface is the only thing that has to be recognised, because everything
// that is not it is either HTTP/1.1 -- which announces itself with a request line
// the HTTP/1.1 walk already parses -- or something this agent does not read. A
// connection the agent joined midstream lands here too: mid-connection HTTP/2
// bytes are frames, and a frame is not a preface, so such a connection becomes
// opaque rather than being decoded from an unknown position.
func (a *Aggregator) sniffLocked(c *connection, ev SocketEvent, payload []byte) {
	if ev.TrueLen == 0 {
		// A zero-length read is evidence of nothing. There are no first bytes to
		// fail to recognise, so the connection is still new and its real first
		// message is still to come.
		return
	}

	hs := c.stream(roleRequest)
	buf, trueLen := joinCarry(hs, payload, uint64(ev.TrueLen))

	switch {
	case uint64(len(buf)) >= uint64(len(http2Preface)) &&
		string(buf[:len(http2Preface)]) == string(http2Preface):
		a.releaseLocked(&hs.carry)
		c.state = stateHTTP2
		c.h2 = newHTTP2Conn()
		// The preface is not a frame and is consumed here; the walk starts at the
		// first frame header after it.
		a.walkHTTP2Locked(c, roleRequest, ev, buf[len(http2Preface):], trueLen-uint64(len(http2Preface)))

	case trueLen < uint64(len(http2Preface)) &&
		uint64(len(buf)) == trueLen &&
		string(http2Preface[:len(buf)]) == string(buf):
		// Every byte so far matches the preface and the preface is not finished. The
		// bytes are held rather than decided on: a client that wrote its preface in
		// two pieces is an h2c client, and calling it HTTP/1.1 here would make the
		// next read parse frame bytes as a request line.
		a.releaseLocked(&hs.carry)
		if !a.bufferLocked(&hs.carry, buf) {
			a.goOpaqueLocked(c, ev.Timestamp)
		}

	default:
		a.releaseLocked(&hs.carry)
		c.state = stateHTTP1
		a.walkHTTP1Locked(c, roleRequest, ev, buf, trueLen)
	}
}

// decodeHTTP2Locked walks one event as HTTP/2 frames.
//
// Like the HTTP/1.1 walk it advances over the event's TRUE length rather than its
// copied prefix, and for the same reason: a DATA frame far larger than the copy
// limit costs nothing, because its length is declared and its contents are never
// read. What HTTP/2 adds is that a unit the decoder DOES have to read -- a frame
// header, a header block -- can be split across syscalls without anything being
// lost, so those bytes are held over rather than surrendering the connection.
func (a *Aggregator) decodeHTTP2Locked(c *connection, which role, ev SocketEvent, payload []byte) {
	hs := c.stream(which)
	buf, trueLen := joinCarry(hs, payload, uint64(ev.TrueLen))
	a.walkHTTP2Locked(c, which, ev, buf, trueLen)
}

// walkHTTP2Locked is the frame loop, over a buffer that may already include bytes
// held over from earlier events. trueLen is how far the stream advances over that
// same buffer, which is longer than the buffer itself exactly when the probe's
// copy limit dropped bytes.
func (a *Aggregator) walkHTTP2Locked(c *connection, which role, ev SocketEvent, buf []byte, trueLen uint64) {
	hs := c.stream(which)

	for pos := uint64(0); pos < trueLen; {
		if hs.skip > 0 {
			pos = hs.consumeSkip(pos, trueLen)
			continue
		}

		if pos+frameHeaderLen > trueLen {
			a.holdLocked(c, hs, ev, buf, pos, trueLen)
			return
		}
		if pos+frameHeaderLen > uint64(len(buf)) {
			// The frame header is inside bytes the probe did not copy. Everything
			// after it would be read at an offset chosen at random.
			a.goOpaqueLocked(c, ev.Timestamp)
			return
		}

		h := parseFrameHeader(buf[pos:])
		end := pos + frameHeaderLen + uint64(h.length)

		read := framePayloadIsRead(h.kind)
		if read {
			if end > trueLen {
				a.holdLocked(c, hs, ev, buf, pos, trueLen)
				return
			}
			if end > uint64(len(buf)) {
				a.goOpaqueLocked(c, ev.Timestamp)
				return
			}
		}

		a.applyFrameLocked(c, which, ev, h, framePayload(buf, pos, end))
		if c.state == stateOpaque {
			return
		}

		if read {
			pos = end
			continue
		}

		// The payload was never copied, so the walk cannot step over it in this
		// buffer. It is owed to the skip counter, which spends it against this read
		// and the ones after it, exactly as an HTTP/1.1 body is stepped over.
		hs.skip = uint64(h.length)
		pos += frameHeaderLen
	}

	a.releaseLocked(&hs.carry)
}

// framePayload is the frame's payload bytes, empty when the walk is stepping over
// them rather than reading them.
func framePayload(buf []byte, pos, end uint64) []byte {
	start := pos + frameHeaderLen
	if end <= start || end > uint64(len(buf)) {
		return nil
	}
	return buf[start:end]
}

// framePayloadIsRead reports whether a frame type's payload has to be READ rather
// than counted.
//
// Only three types qualify. HEADERS and CONTINUATION carry the header block, which
// is the whole point of decoding at all; SETTINGS carries the HPACK table ceiling,
// and a decoder that missed a raise would reject the very header blocks it exists
// to read. Everything else -- DATA above all -- is stepped over.
func framePayloadIsRead(kind uint8) bool {
	return kind == frameHeaders || kind == frameContinuation || kind == frameSettings
}

// applyFrameLocked turns one frame into aggregate state.
func (a *Aggregator) applyFrameLocked(c *connection, which role, ev SocketEvent, h frameHeader, payload []byte) {
	h2 := c.h2half(which)

	// A header block admits nothing between its HEADERS and its CONTINUATIONs, on
	// any stream. A frame arriving here means the decoder has lost the framing or
	// the peer is not speaking HTTP/2, and continuing would feed HPACK a block it
	// cannot make sense of.
	if h2.blockOpen && (h.kind != frameContinuation || h.streamID != h2.blockStream) {
		a.goOpaqueLocked(c, ev.Timestamp)
		return
	}

	// The mirror of the same rule: a CONTINUATION continues a block or it is not a
	// CONTINUATION. Accumulating one with no block open would take the stream and
	// the END_STREAM flag from a frame that carries neither, so the request it
	// eventually filed would be attributed by guesswork.
	if h.kind == frameContinuation && !h2.blockOpen {
		a.goOpaqueLocked(c, ev.Timestamp)
		return
	}

	switch h.kind {
	case frameHeaders, frameContinuation:
		a.accumulateHeadersLocked(c, which, ev, h, payload)

	case frameData:
		if which == roleResponse && h.flags&flagEndStream != 0 {
			a.completeStreamLocked(c, ev.Timestamp, h.streamID)
		}

	case frameRSTStream:
		// A cancelled stream is a request that happened and was never answered. It
		// is deliberately not an error: a client hanging up, or a deadline the
		// caller set, says nothing about the health of the container serving it --
		// the same reasoning that keeps 4XX out of error_count.
		a.abandonStreamLocked(c, ev.Timestamp, h.streamID)

	case frameSettings:
		a.applySettingsLocked(c, which, payload)

	case framePushPromise:
		// A pushed stream carries a header block that HPACK must see to stay in
		// step, and creates a response with no request to pair it with. Server push
		// is effectively extinct -- browsers removed it and gRPC never used it -- so
		// the connection is surrendered rather than carrying code for it.
		a.goOpaqueLocked(c, ev.Timestamp)
	}
}

// accumulateHeadersLocked appends one header-block fragment and decodes the block
// once it is complete.
func (a *Aggregator) accumulateHeadersLocked(c *connection, which role, ev SocketEvent, h frameHeader, payload []byte) {
	h2 := c.h2half(which)

	fragment, ok := headerFragment(h, payload)
	if !ok {
		a.goOpaqueLocked(c, ev.Timestamp)
		return
	}

	if !h2.blockOpen {
		h2.blockOpen = true
		h2.blockStream = h.streamID
		h2.blockEndStream = h.flags&flagEndStream != 0
	}
	if !a.bufferLocked(&h2.block, fragment) {
		a.goOpaqueLocked(c, ev.Timestamp)
		return
	}

	if h.flags&flagEndHeaders == 0 {
		return
	}

	fields, err := h2.dec.DecodeFull(h2.block)
	if err != nil {
		// The dynamic table is no longer the peer's. Nothing later on this direction
		// can be trusted, and a status read out of a mis-decoded block would be a
		// number with no relation to what the server said.
		a.goOpaqueLocked(c, ev.Timestamp)
		return
	}

	stream, endStream := h2.blockStream, h2.blockEndStream
	h2.blockOpen = false
	h2.blockEndStream = false
	a.releaseLocked(&h2.block)

	a.applyHeadersLocked(c, which, ev.Timestamp, stream, endStream, fields)
}

// applyHeadersLocked is where a decoded header block becomes a measurement.
//
// On the request half a block for an unseen stream starts a request. On the
// response half it carries the outcome: :status on the first block, and grpc-status
// on the trailers that a gRPC server sends after the body. Both are read into the
// outstanding request and neither is stored -- the table has no status dimension,
// and the only thing either decides is whether this request counted as an error.
func (a *Aggregator) applyHeadersLocked(c *connection, which role, at time.Time, stream uint32, endStream bool, fields []hpack.HeaderField) {
	if which == roleRequest {
		// Request trailers and the END_STREAM that ends a request body say only that
		// the caller finished talking, which is not the completion being measured.
		// Nothing outstanding for the stream is therefore not enough to call this a
		// new request: on a stream whose response has already been filed it would
		// start a pending that never completes, is swept as unanswered, and inflates
		// request_count with no error anywhere to show for it. The id is what
		// separates the two cases.
		if stream > c.h2.maxStarted {
			c.h2.maxStarted = stream
			a.startStreamLocked(c, at, stream)
		}
		return
	}

	i := a.pendingIndexForStream(c, stream)
	if i < 0 {
		// A response for a stream with nothing outstanding: the agent joined between
		// the request and its answer, or the request was already swept. Inventing a
		// start would put a duration in the histogram that no request produced.
		return
	}

	for _, f := range fields {
		switch f.Name {
		case ":status":
			if status, err := strconv.Atoi(f.Value); err == nil {
				c.inflight[i].status = status
			}
		case "grpc-status":
			// Every gRPC call answers 200 and reports its real outcome here, so a
			// gRPC server with no reachable dependency looks perfectly healthy to
			// anything reading :status alone.
			if code, err := strconv.Atoi(f.Value); err == nil && code != 0 {
				c.inflight[i].grpcError = true
			}
		}
	}

	if endStream {
		a.completeStreamLocked(c, at, stream)
	}
}

// applySettingsLocked raises this connection's HPACK ceiling when a peer asks for
// one.
//
// A SETTINGS frame states what its SENDER is willing to receive, so a raise from
// the client governs the encoder the SERVER uses -- which is the decoder reading
// the response half. Applying it to the wrong direction is invisible until a peer
// actually uses the larger table, at which point every header block on that
// direction fails to decode.
func (a *Aggregator) applySettingsLocked(c *connection, which role, payload []byte) {
	size, ok := headerTableSize(payload)
	if !ok {
		return
	}
	c.h2half(oppositeHalf(which)).dec.SetAllowedMaxDynamicTableSize(size)
}

// startStreamLocked begins tracking one HTTP/2 request.
//
// The cap behaves exactly as it does on HTTP/1.1, and for a reason that survives
// the change of protocol: a request that could not be tracked would leave its
// response with nothing to pair against, and on a multiplexed connection that
// response is not merely unmatched -- it is a stream id the decoder has no record
// of, which is indistinguishable from a stream it has already finished.
func (a *Aggregator) startStreamLocked(c *connection, at time.Time, stream uint32) {
	if a.inFlight >= a.cfg.MaxInFlightRequests {
		a.goOpaqueLocked(c, at)
		return
	}
	c.inflight = append(c.inflight, pending{start: at, streamID: stream})
	a.inFlight++
}

// completeStreamLocked files one stream as answered, at the moment its response
// ended.
//
// The completion is END_STREAM on the response half, NOT the response headers --
// which is where HTTP/1.1 stops the clock, and the difference is deliberate. A
// gRPC call reports its outcome in trailers that only arrive when the call is
// over, so headers-time on a gRPC stream would measure how quickly the server
// agreed to answer rather than how long the call took, and would read every failed
// call as a success. docs/features/red-metrics/INVARIANTS.md argues both halves.
func (a *Aggregator) completeStreamLocked(c *connection, at time.Time, stream uint32) {
	i := a.pendingIndexForStream(c, stream)
	if i < 0 {
		return
	}
	req := a.popPendingAtLocked(c, i)

	durationUs := at.Sub(req.start).Microseconds()
	if durationUs < 0 {
		// Two syscall timestamps can straddle a clock adjustment. A negative
		// duration is not a measurement.
		durationUs = 0
	}

	a.fileAnsweredLocked(c, at, durationUs, isServerError(req.status) || req.grpcError)
}

// abandonStreamLocked drops a reset stream and files it as unanswered.
func (a *Aggregator) abandonStreamLocked(c *connection, at time.Time, stream uint32) {
	i := a.pendingIndexForStream(c, stream)
	if i < 0 {
		return
	}
	a.popPendingAtLocked(c, i)
	a.fileUnansweredLocked(c, at)
}

// pendingIndexForStream finds the outstanding request on a stream, or -1.
//
// The scan is linear over the connection's outstanding requests, which is bounded
// by the peer's concurrent-stream limit and in practice is a handful. A map per
// connection would cost an allocation on every connection to save a walk over a
// list that is almost always shorter than one cache line's worth of entries.
func (a *Aggregator) pendingIndexForStream(c *connection, stream uint32) int {
	for i := range c.inflight {
		if c.inflight[i].streamID == stream {
			return i
		}
	}
	return -1
}

// frameHeader is a decoded 9-byte frame header.
type frameHeader struct {
	length   uint32
	kind     uint8
	flags    uint8
	streamID uint32
}

// parseFrameHeader reads a frame header from the start of b, which the caller has
// already established holds at least frameHeaderLen bytes.
func parseFrameHeader(b []byte) frameHeader {
	return frameHeader{
		length: uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2]),
		kind:   b[3],
		flags:  b[4],
		// The top bit is reserved and senders are not required to clear it, so it is
		// masked off rather than assumed zero.
		streamID: binary.BigEndian.Uint32(b[5:9]) & 0x7fffffff,
	}
}

// headerFragment is the HPACK bytes inside a HEADERS or CONTINUATION payload.
//
// A HEADERS frame may carry padding and a priority block, neither of which is
// HPACK; feeding them to the decoder corrupts the dynamic table for the rest of
// the connection. A CONTINUATION carries neither and is returned whole.
func headerFragment(h frameHeader, payload []byte) ([]byte, bool) {
	if h.kind == frameContinuation {
		return payload, true
	}

	if h.flags&flagPadded != 0 {
		if len(payload) < 1 {
			return nil, false
		}
		pad := int(payload[0])
		payload = payload[1:]
		if len(payload) < pad {
			return nil, false
		}
		payload = payload[:len(payload)-pad]
	}
	if h.flags&flagPriority != 0 {
		if len(payload) < 5 {
			return nil, false
		}
		payload = payload[5:]
	}
	return payload, true
}

// headerTableSize reads SETTINGS_HEADER_TABLE_SIZE out of a SETTINGS payload,
// reporting false when the frame does not carry one. A settings payload is a flat
// run of six-byte entries; a trailing partial entry is malformed and is ignored
// rather than read.
func headerTableSize(payload []byte) (uint32, bool) {
	// A SETTINGS frame may carry the same parameter more than once, and the final
	// value is the one that takes effect (RFC 9113 6.5.3). Returning the first
	// would leave the decoder sized to a limit the peer has already superseded.
	var (
		size  uint32
		found bool
	)
	for len(payload) >= 6 {
		if binary.BigEndian.Uint16(payload[:2]) == settingHeaderTableSize {
			size, found = binary.BigEndian.Uint32(payload[2:6]), true
		}
		payload = payload[6:]
	}
	return size, found
}
