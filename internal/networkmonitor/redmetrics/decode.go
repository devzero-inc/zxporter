package redmetrics

import (
	"time"
)

// decodeState is what the decoder is still willing to do with one connection.
//
// A connection is read until something stops it being readable, and none of the
// reading states moves back: HTTP/1.1 does not become HTTP/2, and opaque and
// ignored are final. The one exception is stateJoined, which is not a reading
// state at all -- it is a connection waiting for a boundary to start reading from,
// and it exists only until it finds one.
type decodeState uint8

const (
	// stateNew is a connection whose protocol has not been established yet, which
	// is every connection until its first bytes arrive. It is the zero value
	// because it is what a connection is until something says otherwise.
	stateNew decodeState = iota

	// stateHTTP1 and stateHTTP2 are connections being read as one protocol or the
	// other. The choice is made once, from the first bytes, and is never revisited:
	// a connection does not change protocol mid-stream, and a decoder willing to
	// believe it did would resynchronise onto noise.
	stateHTTP1
	stateHTTP2

	// stateOpaque is a connection that is no longer being decoded, and has already
	// contributed its conn_count. Nothing moves out of it.
	stateOpaque

	// stateIgnored is a connection nothing is measured about: one this node
	// originated, or one whose container could not be established. It is tracked
	// only so the decision is not taken again on every later syscall.
	stateIgnored

	// stateJoined is an inbound connection the agent started watching part-way
	// through, recognised as inbound because it is served on a port this node has
	// been seen receiving on. Its bytes cannot be read from where the agent came in
	// -- the first thing seen was a response, and the request it answers went past
	// before anyone was looking -- so it waits for the boundary that a response
	// ending and a new request arriving makes, and becomes stateNew there.
	stateJoined
)

// maxBufferBytes caps one held-over buffer. It is the default HTTP/2 maximum
// frame size, which is what bounds the largest single unit the decoder ever has
// to see whole; a peer that negotiated something larger, or a header block spread
// over more CONTINUATIONs than that, takes its connection opaque rather than
// growing the agent.
const maxBufferBytes = 16384

// halfStream is one direction of a connection's byte stream, reduced to what the
// decoder has to remember between socket events.
//
// Inside a body nothing is parsed, only counted, which is what makes a truncated
// copy harmless there: the probe reports the call's true length even when it copied
// a 2KB prefix of it, so the position advances by the length that crossed the socket
// rather than by the length that reached userspace.
type halfStream struct {
	// skip is the number of bytes still to consume before the next boundary: an
	// HTTP/1.1 body, or an HTTP/2 frame payload the decoder does not read. Zero
	// means the next byte on this half starts something the decoder reads.
	skip uint64

	// carry holds bytes from earlier events that did not add up to a whole unit,
	// so the next event resumes rather than losing them. Only HTTP/2 fills it --
	// its frames declare their length, so a split one can be waited out. HTTP/1.1
	// has no such marker and goes opaque on a split header block instead.
	carry []byte
}

// pending is one outstanding request: observed starting, not yet observed
// completing.
//
// Its fields divide by protocol, because the two protocols pair a response with a
// request in entirely different ways. method is HTTP/1.1's, where the pairing is
// positional. streamID, status and grpcError are HTTP/2's, where the pairing is
// explicit and the outcome arrives in two pieces.
type pending struct {
	// start is when the request's first byte crossed the socket, stamped by the
	// agent. The duration is measured against the completion's stamp.
	start time.Time

	// method is the HTTP/1.1 request method, kept because the response's framing
	// depends on it: a HEAD response declares a content length and then sends no
	// body.
	method string

	// streamID is the HTTP/2 stream this request arrived on, and is how its
	// response finds it again on a connection carrying many at once.
	streamID uint32

	// status is the :status of the HTTP/2 response so far, zero until its headers
	// arrive. It is read at completion and then discarded.
	status int

	// grpcError records that the response carried a non-zero grpc-status. It is
	// separate from status because a failed gRPC call still answers 200, so the two
	// have to be able to disagree.
	grpcError bool
}

// role says which half of the connection an event's bytes crossed on.
type role uint8

const (
	roleRequest role = iota
	roleResponse
)

// connection is everything remembered about one tracked connection.
type connection struct {
	// lastSeen is the timestamp of its most recent syscall, and the only thing the
	// idle sweep looks at.
	lastSeen time.Time

	// container is the container serving it, resolved once at the first inbound
	// observation. Every measurement the connection produces is filed against it,
	// including a conn_count filed minutes later, so that a pod deleted mid-flight
	// cannot change where its own traffic lands.
	container ContainerKey

	state decodeState

	request, response halfStream

	// h2 is the HPACK and header-block state, allocated only for a connection
	// established as HTTP/2. An HTTP/1.1 connection never pays for two decoders.
	h2 *http2Conn

	// inflight holds requests in the order they were observed. On HTTP/1.1 that
	// order IS the pairing -- nothing on the wire ties a response to a request, so
	// the oldest outstanding one is the answer. On HTTP/2 the stream id does the
	// pairing and the order only serves the sweep, which needs oldest first.
	inflight []pending
}

// stream is the half an event's bytes crossed on.
// consumeSkip spends what the walk owes the skip counter against this buffer and
// reports the position after it. The debt outlives the read that announced it: a
// body or a frame payload may be larger than the buffer it was declared in, and
// the bytes making up the rest of it arrive over later reads.
func (h *halfStream) consumeSkip(pos, trueLen uint64) uint64 {
	consumed := min(h.skip, trueLen-pos)
	h.skip -= consumed
	return pos + consumed
}

func (c *connection) stream(which role) *halfStream {
	if which == roleRequest {
		return &c.request
	}
	return &c.response
}

// roleOf maps a syscall to a half. Only inbound connections are decoded, so bytes
// arriving are always a request and bytes leaving are always its answer.
func roleOf(p SyscallPath) role {
	if p == SyscallRecv {
		return roleRequest
	}
	return roleResponse
}

// oldestMethod is the method of the oldest outstanding request, empty when there is
// none. The response parser needs it, and on HTTP/1.1 the oldest outstanding request
// is the one being answered.
func (c *connection) oldestMethod() string {
	if len(c.inflight) == 0 {
		return ""
	}
	return c.inflight[0].method
}

// decodeLocked advances a connection's decode state by one socket event, filing
// every request that completed in it. The caller holds the lock.
func (a *Aggregator) decodeLocked(c *connection, ev SocketEvent) {
	// The probe may hand over more bytes than the syscall carried only if something
	// is wrong with the record; clamping keeps the walk's two lengths consistent
	// with each other rather than trusting either alone.
	payload := ev.Payload
	if uint32(len(payload)) > ev.TrueLen {
		payload = payload[:ev.TrueLen]
	}

	which := roleOf(ev.Path)

	switch c.state {
	case stateJoined:
		// Neither a response nor an empty read is a place to start reading from: the
		// first is the middle of a message this agent never saw the start of, and the
		// second is not a message at all. Both leave the connection waiting.
		if which == roleResponse || ev.TrueLen == 0 {
			return
		}
		// A request arriving after a response went out is a fresh message start on
		// HTTP/1.1, which is what makes this resynchronisation sound rather than
		// hopeful. On HTTP/2 it is not -- frames interleave, so there is no such
		// boundary -- and the sniff below will fail to find a preface and take the
		// connection opaque, which is the honest answer for a multiplexed stream
		// joined midway.
		c.state = stateNew
		a.sniffLocked(c, ev, payload)

	case stateNew:
		if which == roleResponse {
			// The server is talking on a connection whose first request was never
			// seen. Whatever protocol this is, the decoder joined it after the point
			// that would have identified it.
			a.goOpaqueLocked(c, ev.Timestamp)
			return
		}
		a.sniffLocked(c, ev, payload)
	case stateHTTP1:
		a.walkHTTP1Locked(c, which, ev, payload, uint64(ev.TrueLen))
	case stateHTTP2:
		a.decodeHTTP2Locked(c, which, ev, payload)
	}
}

// joinCarry puts an event's bytes after whatever was held over from earlier ones,
// and reports how far the stream advances across the pair.
//
// The returned buffer never aliases the carry, so the caller may release the carry
// before it has finished reading. When nothing was held over it is the event's own
// bytes, which is the common case and costs nothing.
func joinCarry(hs *halfStream, payload []byte, trueLen uint64) ([]byte, uint64) {
	if len(hs.carry) == 0 {
		return payload, trueLen
	}
	buf := make([]byte, 0, len(hs.carry)+len(payload))
	buf = append(buf, hs.carry...)
	buf = append(buf, payload...)
	return buf, uint64(len(hs.carry)) + trueLen
}

// holdLocked keeps the tail of a buffer for the next event, because it stopped
// part-way through something the decoder has to read whole.
//
// It can only do that when the event was copied in full. A buffer shorter than the
// length that crossed the socket means bytes the decoder needs never reached
// userspace, and resuming from what remains would silently skip them.
func (a *Aggregator) holdLocked(c *connection, hs *halfStream, ev SocketEvent, buf []byte, pos, trueLen uint64) {
	if trueLen != uint64(len(buf)) {
		a.goOpaqueLocked(c, ev.Timestamp)
		return
	}
	a.releaseLocked(&hs.carry)
	if !a.bufferLocked(&hs.carry, buf[pos:]) {
		a.goOpaqueLocked(c, ev.Timestamp)
	}
}

// bufferLocked appends bytes to one of a connection's held-over buffers, reporting
// false when doing so would break either bound.
//
// There are two bounds and they answer different questions. maxBufferBytes says no
// single unit is that big, so a connection asking for more is not one worth
// following. MaxBufferedBytes is the one that matters on a busy node: it is the
// agent's total, so a thousand connections each holding a legitimate half-frame
// cannot add up to memory pressure on a DaemonSet sharing a node with the
// workloads it measures.
func (a *Aggregator) bufferLocked(dst *[]byte, b []byte) bool {
	if len(*dst)+len(b) > maxBufferBytes {
		return false
	}
	if a.buffered+len(b) > a.cfg.MaxBufferedBytes {
		return false
	}
	a.buffered += len(b)
	*dst = append(*dst, b...)
	return true
}

// releaseLocked empties one held-over buffer and refunds it to the agent's total.
func (a *Aggregator) releaseLocked(dst *[]byte) {
	a.buffered -= len(*dst)
	*dst = nil
}

// releaseBuffersLocked gives back everything a connection was holding. It runs when
// the connection stops being decoded, whether that is because it went opaque or
// because the sweep dropped it.
func (a *Aggregator) releaseBuffersLocked(c *connection) {
	a.releaseLocked(&c.request.carry)
	a.releaseLocked(&c.response.carry)
	if c.h2 != nil {
		a.releaseLocked(&c.h2.request.block)
		a.releaseLocked(&c.h2.response.block)
		// Dropping the decoders drops two dynamic tables, which are the largest
		// thing an HTTP/2 connection holds.
		c.h2 = nil
	}
}

// walkHTTP1Locked walks one event as HTTP/1.1 messages.
//
// The walk is over the event's TRUE length rather than its copied prefix. Body bytes
// are consumed by count, so a body far larger than the copy limit costs nothing; a
// message boundary needs the bytes themselves, and when one lands past what the
// probe copied the connection goes opaque rather than guessing at what it could not
// see.
//
// Unlike the HTTP/2 walk it never holds bytes over. A split header block could in
// principle be waited out too, but HTTP/1.1 gives no length to wait for and no
// marker to resynchronise on, so the same code would be waiting on something that
// may never come while the connection's real traffic went unread.
func (a *Aggregator) walkHTTP1Locked(c *connection, which role, ev SocketEvent, buf []byte, trueLen uint64) {
	stream := c.stream(which)

	for pos := uint64(0); pos < trueLen; {
		if stream.skip > 0 {
			pos = stream.consumeSkip(pos, trueLen)
			continue
		}

		if pos >= uint64(len(buf)) {
			// The next message starts in bytes the probe did not copy. Reading the
			// stream from here would mean picking a boundary at random.
			a.goOpaqueLocked(c, ev.Timestamp)
			return
		}

		msg, ok := parseMessageStartLocked(c, which, buf[pos:])
		if !ok {
			a.goOpaqueLocked(c, ev.Timestamp)
			return
		}

		pos += uint64(msg.headerLen)
		if msg.bodyLen > 0 {
			stream.skip = uint64(msg.bodyLen)
		}

		a.recordMessageLocked(c, which, ev.Timestamp, msg)

		if msg.bodyLen == bodyLenUnknown {
			// A chunked or close-delimited body: this message was accounted for, but
			// where it ends is not knowable from a length, so neither is the next
			// boundary on this connection.
			a.goOpaqueLocked(c, ev.Timestamp)
			return
		}

		if c.state == stateOpaque {
			// recordMessageLocked gave up on the connection -- the in-flight cap.
			return
		}
	}
}

// parseMessageStartLocked reads a message boundary from the start of b, as a request
// or a response depending on which half it crossed. It carries the suffix because it
// reads the connection's in-flight list, which only the aggregator's lock protects.
func parseMessageStartLocked(c *connection, which role, b []byte) (messageStart, bool) {
	if which == roleResponse {
		return parseResponseStart(b, c.oldestMethod())
	}
	return parseRequestStart(b)
}

// recordMessageLocked turns a message boundary into aggregate state: a request start
// goes on the in-flight list, a response completes the oldest one.
func (a *Aggregator) recordMessageLocked(c *connection, which role, at time.Time, msg messageStart) {
	if which == roleRequest {
		a.startRequestLocked(c, at, msg.method)
		return
	}
	if informational(msg.status) {
		// A 100 Continue answers nothing; the real response follows on the same
		// request, and treating this as the completion would report a latency that
		// measured the server's willingness to read a body.
		return
	}
	a.completeRequestLocked(c, at, msg.status)
}

// startRequestLocked begins tracking one HTTP/1.1 request.
//
// The cap is the memory bound on in-flight state. A request arriving with no room
// takes the connection opaque, rather than being dropped on its own: the response it
// will never be matched against would otherwise be paired with a LATER request on
// the same connection, reporting a duration made of two different requests.
func (a *Aggregator) startRequestLocked(c *connection, at time.Time, method string) {
	if a.inFlight >= a.cfg.MaxInFlightRequests {
		a.goOpaqueLocked(c, at)
		return
	}
	c.inflight = append(c.inflight, pending{start: at, method: method})
	a.inFlight++
}

// completeRequestLocked files the oldest outstanding request as completed by this
// response.
//
// A response with nothing outstanding is dropped. Without a start there is no
// duration, and inventing one would put a number in the histogram that no request
// produced; counting it as a request without a duration would be worse still, since
// request_count would then disagree with the top bucket for reasons that have
// nothing to do with the service. It happens when the agent joined the connection
// between a request and its response, or after that request was swept.
func (a *Aggregator) completeRequestLocked(c *connection, at time.Time, status int) {
	if len(c.inflight) == 0 {
		return
	}

	// Index zero because on HTTP/1.1 the oldest outstanding request is the one this
	// response answers; HTTP/2 reaches the same function through a stream lookup.
	req := a.popPendingAtLocked(c, 0)

	durationUs := at.Sub(req.start).Microseconds()
	if durationUs < 0 {
		// The two timestamps come from two different socket calls and can straddle a
		// clock adjustment. A negative duration is not a measurement.
		durationUs = 0
	}

	a.fileAnsweredLocked(c, at, durationUs, isServerError(status))
}

// popPendingAtLocked takes one outstanding request off the in-flight list.
//
// The remaining entries keep their relative order, which both callers depend on for
// different reasons: HTTP/1.1 needs the next response to find the next-oldest
// request, and the sweep needs the list sorted by start so it can stop at the first
// entry that has not expired.
func (a *Aggregator) popPendingAtLocked(c *connection, i int) pending {
	req := c.inflight[i]
	// Shifted rather than resliced: a keep-alive connection completes one request
	// after another, and resliced capacity would make every one of them allocate.
	copy(c.inflight[i:], c.inflight[i+1:])
	c.inflight = c.inflight[:len(c.inflight)-1]
	a.inFlight--
	return req
}

// goOpaqueLocked stops decoding a connection for good, and files the conn_count that
// says so.
//
// A decoder that lost the stream cannot regain it: neither protocol offers a
// resynchronisation point a reader can find from the middle, and a decoder guessing
// at boundaries reports plausible wrong latencies rather than failing. The
// connection is counted HERE rather than when it was first seen, which is what makes
// conn_count mean "traffic this agent could not read" instead of "traffic". A
// connection that decodes cleanly for its whole life contributes no conn_count at
// all.
//
// Requests still outstanding are dropped without being counted. They were observed,
// so counting them would be defensible -- but the decoder is giving up precisely
// because it no longer trusts what it read, and a request_count that included them
// would be asserting they were real requests on exactly the evidence just declared
// untrustworthy. The gap is what conn_count is for.
func (a *Aggregator) goOpaqueLocked(c *connection, at time.Time) {
	if c.state == stateOpaque {
		return
	}
	c.state = stateOpaque

	a.inFlight -= len(c.inflight)
	c.inflight = nil
	a.releaseBuffersLocked(c)

	if bucket := a.countersLocked(at, c.container); bucket != nil {
		bucket.connCount++
	}
}
