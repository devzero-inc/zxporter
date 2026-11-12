package redmetrics

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/net/http2/hpack"
)

// The tests in this file drive CONNECTIONS carrying several streams at once, which
// is the thing HTTP/1.1 cannot do and therefore the thing none of the HTTP/1.1 tests
// can catch. A decoder that tracked one outstanding request per connection, or that
// paired responses positionally the way HTTP/1.1 requires, passes every sequential
// test and reports nonsense the moment a real gRPC client opens its second stream.

// h2encoder is one direction's HPACK encoder. There are two per test connection for
// the same reason there are two decoders in the aggregator: an HPACK dynamic table
// is per-direction state, and an encoder shared between the halves would produce
// blocks that no correct decoder could read.
type h2encoder struct {
	block bytes.Buffer
	enc   *hpack.Encoder
}

func newH2Encoder() *h2encoder {
	e := &h2encoder{}
	e.enc = hpack.NewEncoder(&e.block)
	return e
}

// encode returns one header block. The bytes are copied out because the encoder
// reuses its buffer, and because the caller holds them until it has framed them.
func (e *h2encoder) encode(t *testing.T, fields ...hpack.HeaderField) []byte {
	t.Helper()
	e.block.Reset()
	for _, f := range fields {
		if err := e.enc.WriteField(f); err != nil {
			t.Fatalf("encoding %s: %v", f.Name, err)
		}
	}
	return bytes.Clone(e.block.Bytes())
}

func hf(name, value string) hpack.HeaderField {
	return hpack.HeaderField{Name: name, Value: value}
}

// frame builds one HTTP/2 frame: the 9-byte header the decoder parses by hand,
// followed by the payload.
func frame(kind, flags uint8, streamID uint32, payload []byte) []byte {
	b := make([]byte, frameHeaderLen, frameHeaderLen+len(payload))
	b[0] = byte(len(payload) >> 16)
	b[1] = byte(len(payload) >> 8)
	b[2] = byte(len(payload))
	b[3] = kind
	b[4] = flags
	binary.BigEndian.PutUint32(b[5:9], streamID)
	return append(b, payload...)
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// preface is the 24 bytes that identify an h2c connection, and the only thing that
// does. Every test here starts with it because a connection without it is one the
// decoder refuses to read -- which is itself pinned, in decode_test.go.
func preface() []byte {
	return bytes.Clone(http2Preface)
}

// grpcRequest is the client half of a gRPC call: headers, then the message. The
// request keeps its stream open, which is what a real client does -- END_STREAM
// arrives on the last DATA frame, and the decoder must not be waiting for it before
// it counts the request.
func grpcRequest(t *testing.T, enc *h2encoder, stream uint32, method string) []byte {
	t.Helper()
	block := enc.encode(t,
		hf(":method", "POST"),
		hf(":scheme", "http"),
		hf(":path", method),
		hf(":authority", "shop.svc"),
		hf("content-type", "application/grpc"),
	)
	return concat(
		frame(frameHeaders, flagEndHeaders, stream, block),
		frame(frameData, flagEndStream, stream, []byte("\x00\x00\x00\x00\x03abc")),
	)
}

// grpcResponse is the server half: a 200, a message, and the trailer that carries
// the call's actual outcome.
func grpcResponse(t *testing.T, enc *h2encoder, stream uint32, grpcStatus string) []byte {
	t.Helper()
	head := enc.encode(t,
		hf(":status", "200"),
		hf("content-type", "application/grpc"),
	)
	trailers := enc.encode(t, hf("grpc-status", grpcStatus))
	return concat(
		frame(frameHeaders, flagEndHeaders, stream, head),
		frame(frameData, 0, stream, []byte("\x00\x00\x00\x00\x02ok")),
		frame(frameHeaders, flagEndHeaders|flagEndStream, stream, trailers),
	)
}

// TestHTTP2MeasuresConcurrentStreamsOnOneConnection is the central test of this
// slice, and the one HTTP/1.1 has no equivalent of.
//
// Three streams are open at once on a single connection and they finish in an order
// that has nothing to do with the order they started in. The durations are far
// apart on purpose: a decoder pairing responses positionally -- correct on HTTP/1.1,
// catastrophic here -- would produce three durations that still sum to the same
// total, so the sum cannot catch it. The maximum and the ladder can.
func TestHTTP2MeasuresConcurrentStreamsOnOneConnection(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())
	client, server := newH2Encoder(), newH2Encoder()

	// All three streams are opened before any of them is answered.
	agg.Observe(reqAt(1, at(t, 0), concat(preface(), grpcRequest(t, client, 1, "/Shop/Slow"))))
	agg.Observe(reqAt(1, at(t, time.Millisecond), grpcRequest(t, client, 3, "/Shop/Fast")))
	agg.Observe(reqAt(1, at(t, 2*time.Millisecond), grpcRequest(t, client, 5, "/Shop/Middling")))

	// Answered inside out: stream 3 took 2ms, stream 5 took 40ms, stream 1 took 300ms.
	agg.Observe(respAt(1, at(t, 3*time.Millisecond), grpcResponse(t, server, 3, "0")))
	agg.Observe(respAt(1, at(t, 42*time.Millisecond), grpcResponse(t, server, 5, "0")))
	agg.Observe(respAt(1, at(t, 300*time.Millisecond), grpcResponse(t, server, 1, "0")))

	item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))

	if item.RequestCount != 3 {
		t.Fatalf("request_count = %d, want 3", item.RequestCount)
	}
	if item.ErrorCount != 0 {
		t.Fatalf("error_count = %d, want 0: every call answered grpc-status 0", item.ErrorCount)
	}
	if want := uint64((300 * time.Millisecond).Microseconds()); item.DurationMaxUs != want {
		t.Fatalf("duration_max_us = %d, want %d: the slowest stream is the one opened first", item.DurationMaxUs, want)
	}
	if want := uint64((342 * time.Millisecond).Microseconds()); item.DurationSumUs != want {
		t.Fatalf("duration_sum_us = %d, want %d", item.DurationSumUs, want)
	}
	// 2ms at bound 2, 40ms at bound 6 (50ms), 300ms at bound 9 (500ms).
	wantBuckets(t, item.DurationBuckets, []uint64{0, 0, 1, 1, 1, 1, 2, 2, 2, 3, 3, 3, 3, 3})
	if item.ConnCount != 0 {
		t.Fatalf("conn_count = %d, want 0: the connection decoded cleanly", item.ConnCount)
	}
}

// TestHTTP2CountsANonZeroGrpcStatusAsAnError pins the reason this decoder reads
// trailers at all.
//
// Every gRPC call answers :status 200, including the ones that failed. A decoder
// reading only the status line reports a service with a dead dependency as perfectly
// healthy -- error_count zero, latency fine -- which is worse than reporting nothing,
// because it looks like evidence.
func TestHTTP2CountsANonZeroGrpcStatusAsAnError(t *testing.T) {
	cases := []struct {
		name      string
		status    string
		wantError uint64
	}{
		{"ok", "0", 0},
		{"internal", "13", 1},
		{"unavailable", "14", 1},
		// NOT FOUND is an answer, not a failure of the server -- but gRPC does not
		// separate the two the way HTTP does, so it counts. The alternative is a
		// list of codes this decoder decides are acceptable, which is a policy and
		// does not belong in a probe.
		{"not found", "5", 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := &fakeClock{now: at(t, 0)}
			agg := newTestAggregator(clock, testPods())
			client, server := newH2Encoder(), newH2Encoder()

			agg.Observe(reqAt(1, at(t, 0), concat(preface(), grpcRequest(t, client, 1, "/Shop/Get"))))
			agg.Observe(respAt(1, at(t, 5*time.Millisecond), grpcResponse(t, server, 1, tc.status)))

			item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))

			if item.RequestCount != 1 {
				t.Fatalf("request_count = %d, want 1", item.RequestCount)
			}
			if item.ErrorCount != tc.wantError {
				t.Fatalf("error_count = %d, want %d for grpc-status %s", item.ErrorCount, tc.wantError, tc.status)
			}
			// Whatever the outcome, the call took 5ms and belongs on the ladder. A
			// failed request is still a measured one.
			if want := uint64((5 * time.Millisecond).Microseconds()); item.DurationSumUs != want {
				t.Fatalf("duration_sum_us = %d, want %d", item.DurationSumUs, want)
			}
		})
	}
}

// TestHTTP2ReadsAGrpcStatusSentWithoutAMessage pins the Trailers-Only response: a
// gRPC server rejecting a call before it runs sends ONE frame carrying both the
// :status and the grpc-status, ends the stream on it, and sends no body at all.
//
// It is the shape a failing service produces most often -- an unauthenticated call,
// an unknown method, a dead upstream -- so a decoder that only looked for
// grpc-status in a second header block would miss errors precisely when they matter.
func TestHTTP2ReadsAGrpcStatusSentWithoutAMessage(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())
	client, server := newH2Encoder(), newH2Encoder()

	agg.Observe(reqAt(1, at(t, 0), concat(preface(), grpcRequest(t, client, 1, "/Shop/Get"))))

	trailersOnly := server.encode(t,
		hf(":status", "200"),
		hf("content-type", "application/grpc"),
		hf("grpc-status", "16"),
		hf("grpc-message", "unauthenticated"),
	)
	agg.Observe(respAt(1, at(t, time.Millisecond),
		frame(frameHeaders, flagEndHeaders|flagEndStream, 1, trailersOnly)))

	item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))

	if item.RequestCount != 1 {
		t.Fatalf("request_count = %d, want 1", item.RequestCount)
	}
	if item.ErrorCount != 1 {
		t.Fatalf("error_count = %d, want 1: a 200 carrying grpc-status 16 is a failed call", item.ErrorCount)
	}
}

// TestHTTP2CountsAServerErrorStatus pins that HTTP/2 has not stopped being HTTP. A
// plain h2c service answering 503 is an error for the same reason it is on
// HTTP/1.1, and nothing about the gRPC trailer rule may displace that.
func TestHTTP2CountsAServerErrorStatus(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())
	client, server := newH2Encoder(), newH2Encoder()

	agg.Observe(reqAt(1, at(t, 0), concat(preface(), grpcRequest(t, client, 1, "/health"))))
	agg.Observe(respAt(1, at(t, time.Millisecond),
		frame(frameHeaders, flagEndHeaders|flagEndStream, 1, server.encode(t, hf(":status", "503")))))

	item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))

	if item.RequestCount != 1 || item.ErrorCount != 1 {
		t.Fatalf("request_count = %d, error_count = %d, want 1 and 1", item.RequestCount, item.ErrorCount)
	}
}

// TestHTTP2DecodesAHeaderBlockSplitAcrossReads pins the carry buffer.
//
// A syscall boundary is not a protocol boundary, and where the kernel cuts one read
// from the next is not something the decoder gets to choose. The cuts here are
// deliberately hostile: one lands inside the connection preface, the next inside a
// frame HEADER -- three bytes of a length nobody can act on -- and the last inside
// the HPACK block itself.
func TestHTTP2DecodesAHeaderBlockSplitAcrossReads(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())
	client, server := newH2Encoder(), newH2Encoder()

	stream := concat(preface(), grpcRequest(t, client, 1, "/Shop/Get"))
	cuts := []int{10, 26, 31}

	prev := 0
	for i, cut := range append(cuts, len(stream)) {
		if cut > len(stream) {
			t.Fatalf("cut %d is past the end of a %d-byte stream", cut, len(stream))
		}
		agg.Observe(reqAt(1, at(t, time.Duration(i)*time.Microsecond), stream[prev:cut]))
		prev = cut
	}

	agg.Observe(respAt(1, at(t, 8*time.Millisecond), grpcResponse(t, server, 1, "0")))

	item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))

	if item.ConnCount != 0 {
		t.Fatalf("conn_count = %d, want 0: a split read is not a decode failure", item.ConnCount)
	}
	if item.RequestCount != 1 {
		t.Fatalf("request_count = %d, want 1", item.RequestCount)
	}
	// The request starts at the read that COMPLETED its header block -- the fourth,
	// at 3us -- not at the first byte to arrive. The decoder cannot know a request
	// exists until it has read the headers that say so, and dating it from the read
	// that first held bytes would credit the service with time it had not yet been
	// asked to do anything in.
	if want := uint64((8*time.Millisecond - 3*time.Microsecond).Microseconds()); item.DurationSumUs != want {
		t.Fatalf("duration_sum_us = %d, want %d", item.DurationSumUs, want)
	}
}

// TestHTTP2DecodesAHeaderBlockSplitAcrossContinuationFrames pins the other way a
// header block arrives in pieces: not cut by the kernel, but deliberately framed
// that way by the sender because the block exceeded the peer's maximum frame size.
//
// CONTINUATION is where an HPACK decoder is easiest to get wrong, because the pieces
// are only decodable joined: each is a fragment of one compressed stream, so
// decoding them separately does not produce partial headers, it produces an error
// and a dynamic table that has diverged from the sender's for the rest of the
// connection.
func TestHTTP2DecodesAHeaderBlockSplitAcrossContinuationFrames(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())
	client, server := newH2Encoder(), newH2Encoder()

	block := client.encode(t,
		hf(":method", "POST"),
		hf(":scheme", "http"),
		hf(":path", "/Shop/Get"),
		hf(":authority", "shop.svc"),
		hf("content-type", "application/grpc"),
	)
	if len(block) < 6 {
		t.Fatalf("block is %d bytes, too short to split meaningfully", len(block))
	}
	a, b, c := block[:2], block[2:5], block[5:]

	agg.Observe(reqAt(1, at(t, 0), concat(
		preface(),
		frame(frameHeaders, 0, 1, a),
		frame(frameContinuation, 0, 1, b),
		frame(frameContinuation, flagEndHeaders, 1, c),
	)))
	agg.Observe(respAt(1, at(t, 3*time.Millisecond), grpcResponse(t, server, 1, "0")))

	item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))

	if item.ConnCount != 0 {
		t.Fatalf("conn_count = %d, want 0", item.ConnCount)
	}
	if item.RequestCount != 1 {
		t.Fatalf("request_count = %d, want 1", item.RequestCount)
	}
}

// TestHTTP2CountsACancelledStreamAsUnanswered pins RST_STREAM.
//
// A cancelled stream is a request the service was asked to do and never finished --
// a client that hung up, a deadline that expired. It counts as a request because it
// happened, it carries no duration because it never completed, and it is NOT an
// error, for the same reason a 404 is not: the failure is the caller's, and folding
// it into error_count would make a service look unhealthy in proportion to how
// impatient its clients are.
func TestHTTP2CountsACancelledStreamAsUnanswered(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())
	client := newH2Encoder()

	agg.Observe(reqAt(1, at(t, 0), concat(preface(), grpcRequest(t, client, 1, "/Shop/Watch"))))
	agg.Observe(reqAt(1, at(t, 2*time.Millisecond),
		frame(frameRSTStream, 0, 1, []byte{0x00, 0x00, 0x00, 0x08})))

	item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))

	if item.RequestCount != 1 {
		t.Fatalf("request_count = %d, want 1: a cancelled request still happened", item.RequestCount)
	}
	if item.ErrorCount != 0 {
		t.Fatalf("error_count = %d, want 0: the client cancelled, the server did not fail", item.ErrorCount)
	}
	if item.DurationSumUs != 0 {
		t.Fatalf("duration_sum_us = %d, want 0: a cancelled stream has no completion to measure", item.DurationSumUs)
	}
	wantBuckets(t, item.DurationBuckets, make([]uint64, DurationBucketCount))
}

// TestHTTP2CountsANeverCompletingStreamAsUnanswered pins the server-streaming call
// that is still streaming when the agent stops waiting.
//
// It is the shape that would break a decoder measuring at the response HEADERS the
// way HTTP/1.1 does: the headers arrive immediately and the call runs for an hour.
// Stopping the clock there would report a watch that streamed for an hour as a
// sub-millisecond request, and the latency distribution of any service with a watch
// API would become a distribution of how fast it says hello.
func TestHTTP2CountsANeverCompletingStreamAsUnanswered(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())
	client, server := newH2Encoder(), newH2Encoder()

	agg.Observe(reqAt(1, at(t, 0), concat(preface(), grpcRequest(t, client, 1, "/Shop/Watch"))))

	// Headers, then messages, and the stream stays open.
	agg.Observe(respAt(1, at(t, time.Millisecond), concat(
		frame(frameHeaders, flagEndHeaders, 1, server.encode(t,
			hf(":status", "200"), hf("content-type", "application/grpc"))),
		frame(frameData, 0, 1, []byte("\x00\x00\x00\x00\x02up")),
		frame(frameData, 0, 1, []byte("\x00\x00\x00\x00\x02up")),
	)))

	// The default RequestTTL is a minute, so the agent gives up at 10:01:00 and files
	// the request in THAT minute -- the minute the measurement became final, not the
	// one the call started in. The flush has to be late enough for 10:01 to have
	// closed, which is why it is two minutes out rather than one.
	item := flushOne(t, agg, clock, at(t, 2*time.Minute+5*time.Second))

	if item.RequestCount != 1 {
		t.Fatalf("request_count = %d, want 1", item.RequestCount)
	}
	if item.ErrorCount != 0 {
		t.Fatalf("error_count = %d, want 0: a slow stream is not a failed one", item.ErrorCount)
	}
	if item.DurationSumUs != 0 {
		t.Fatalf("duration_sum_us = %d, want 0: nothing completed, so nothing was measured", item.DurationSumUs)
	}
}

// TestHTTP2StepsOverADataFrameLargerThanTheCopyLimit pins that a body the probe
// could not copy does not cost the connection.
//
// The frame declares its own length, so the decoder steps over it by count without
// ever looking at the bytes -- which is what lets a service uploading megabytes stay
// measurable on a probe that copies two kilobytes. The second request is the actual
// assertion: it can only be read if the position landed exactly on the next frame
// header.
func TestHTTP2StepsOverADataFrameLargerThanTheCopyLimit(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())
	client, server := newH2Encoder(), newH2Encoder()

	block := client.encode(t,
		hf(":method", "POST"), hf(":scheme", "http"),
		hf(":path", "/Shop/Upload"), hf(":authority", "shop.svc"),
	)
	big := concat(
		preface(),
		frame(frameHeaders, flagEndHeaders, 1, block),
		frame(frameData, flagEndStream, 1, bytes.Repeat([]byte("x"), 40000)),
	)
	agg.Observe(truncate(reqAt(1, at(t, 0), big), 2048))

	agg.Observe(respAt(1, at(t, 4*time.Millisecond), grpcResponse(t, server, 1, "0")))

	// A second stream, which only decodes if the first one's payload was stepped
	// over by exactly its declared length.
	agg.Observe(reqAt(1, at(t, 5*time.Millisecond), grpcRequest(t, client, 3, "/Shop/Get")))
	agg.Observe(respAt(1, at(t, 6*time.Millisecond), grpcResponse(t, server, 3, "0")))

	item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))

	if item.ConnCount != 0 {
		t.Fatalf("conn_count = %d, want 0: an uncopied payload is not a decode failure", item.ConnCount)
	}
	if item.RequestCount != 2 {
		t.Fatalf("request_count = %d, want 2", item.RequestCount)
	}
}

// TestHTTP2AppliesTheHeaderTableSizeFromSettings pins the one SETTINGS value this
// decoder cannot ignore.
//
// SETTINGS_HEADER_TABLE_SIZE announces what its SENDER will accept, so a client
// raising it is licensing the SERVER to compress harder -- and the decoder that has
// to keep up is the one reading the other direction. Miss it and the failure is
// total and delayed: the response header block that first exceeds the default is
// rejected, and every block after it on that direction is rejected too, because the
// dynamic table has diverged.
func TestHTTP2AppliesTheHeaderTableSizeFromSettings(t *testing.T) {
	const raised = 8192

	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())
	client, server := newH2Encoder(), newH2Encoder()

	settings := make([]byte, 6)
	binary.BigEndian.PutUint16(settings[:2], settingHeaderTableSize)
	binary.BigEndian.PutUint32(settings[2:], raised)

	agg.Observe(reqAt(1, at(t, 0), concat(
		preface(),
		frame(frameSettings, 0, 0, settings),
		grpcRequest(t, client, 1, "/Shop/Get"),
	)))

	// The server takes the client up on it, which puts a dynamic table size update
	// of 8192 at the front of its next header block.
	server.enc.SetMaxDynamicTableSizeLimit(raised)
	server.enc.SetMaxDynamicTableSize(raised)
	agg.Observe(respAt(1, at(t, 2*time.Millisecond), grpcResponse(t, server, 1, "0")))

	item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))

	if item.ConnCount != 0 {
		t.Fatalf("conn_count = %d, want 0: the client licensed the larger table", item.ConnCount)
	}
	if item.RequestCount != 1 {
		t.Fatalf("request_count = %d, want 1", item.RequestCount)
	}
}

// TestHTTP2GoesOpaqueOnUnreadableHpackState pins what happens when the compression
// context is lost.
//
// HPACK is not a per-message encoding: every block mutates a table both sides must
// agree on, so one block the decoder cannot read means it can never read another on
// that direction. The connection is surrendered rather than skipped past, because a
// decoder continuing here would emit statuses read from a table that no longer
// matches the sender's -- plausible values, arbitrarily wrong.
func TestHTTP2GoesOpaqueOnUnreadableHpackState(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())

	// 0x80 is an indexed field referring to index 0, which HPACK defines as illegal.
	agg.Observe(reqAt(1, at(t, 0), concat(
		preface(),
		frame(frameHeaders, flagEndHeaders, 1, []byte{0x80}),
	)))

	item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))

	if item.ConnCount != 1 {
		t.Fatalf("conn_count = %d, want 1", item.ConnCount)
	}
	if item.RequestCount != 0 {
		t.Fatalf("request_count = %d, want 0: nothing was read, so nothing may be claimed", item.RequestCount)
	}
}

// TestHTTP2GoesOpaqueOnServerPush pins the deliberate gap. PUSH_PROMISE carries a
// header block the HPACK decoder would have to consume to stay in step, and creates
// a response with no request for the pairing to find. Server push is disabled by
// default in every current client and removed from Chrome entirely, so the
// connection is surrendered rather than the code being carried indefinitely for it.
func TestHTTP2GoesOpaqueOnServerPush(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())
	client := newH2Encoder()

	agg.Observe(reqAt(1, at(t, 0), concat(preface(), grpcRequest(t, client, 1, "/index"))))
	agg.Observe(respAt(1, at(t, time.Millisecond),
		frame(framePushPromise, flagEndHeaders, 1, []byte{0x00, 0x00, 0x00, 0x02})))

	item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))

	if item.ConnCount != 1 {
		t.Fatalf("conn_count = %d, want 1", item.ConnCount)
	}
	// The outstanding request is dropped with the rest of the connection's state:
	// the decoder is giving up because it no longer trusts what it read.
	if item.RequestCount != 0 {
		t.Fatalf("request_count = %d, want 0", item.RequestCount)
	}
}

// TestHTTP2GoesOpaqueWhenAFrameIsInterleavedIntoAHeaderBlock pins the rule that
// makes CONTINUATION safe to reassemble: between a HEADERS frame and its END_HEADERS
// nothing else may appear on the connection, not even on another stream.
//
// Without the check the fragments of two different blocks could be concatenated into
// one buffer and handed to HPACK, which would decode SOMETHING from the mixture --
// header fields belonging to neither request.
func TestHTTP2GoesOpaqueWhenAFrameIsInterleavedIntoAHeaderBlock(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())
	client := newH2Encoder()

	block := client.encode(t, hf(":method", "POST"), hf(":path", "/Shop/Get"))

	agg.Observe(reqAt(1, at(t, 0), concat(
		preface(),
		frame(frameHeaders, 0, 1, block[:2]),
		frame(frameData, 0, 3, []byte("interloper")),
		frame(frameContinuation, flagEndHeaders, 1, block[2:]),
	)))

	item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))

	if item.ConnCount != 1 {
		t.Fatalf("conn_count = %d, want 1", item.ConnCount)
	}
	if item.RequestCount != 0 {
		t.Fatalf("request_count = %d, want 0", item.RequestCount)
	}
}

// TestHTTP2DropsAResponseForAStreamItNeverSaw pins the same rule the HTTP/1.1
// decoder has, arrived at differently. Without a start there is no duration, and
// counting the response as a request would put a number in request_count that no
// observed request produced. It happens whenever the agent starts mid-connection on
// a stream that was already open.
func TestHTTP2DropsAResponseForAStreamItNeverSaw(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())
	client, server := newH2Encoder(), newH2Encoder()

	agg.Observe(reqAt(1, at(t, 0), concat(preface(), grpcRequest(t, client, 1, "/Shop/Get"))))
	// Stream 7 was never opened in front of this decoder.
	agg.Observe(respAt(1, at(t, time.Millisecond), grpcResponse(t, server, 7, "13")))
	agg.Observe(respAt(1, at(t, 2*time.Millisecond), grpcResponse(t, server, 1, "0")))

	item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))

	if item.RequestCount != 1 {
		t.Fatalf("request_count = %d, want 1: only stream 1 was observed starting", item.RequestCount)
	}
	if item.ErrorCount != 0 {
		t.Fatalf("error_count = %d, want 0: the failed call was never paired with a request", item.ErrorCount)
	}
	if item.ConnCount != 0 {
		t.Fatalf("conn_count = %d, want 0: an unknown stream is not a lost stream position", item.ConnCount)
	}
}

// TestHTTP2KeepsConnectionsIndependent pins that HPACK state is per connection. Two
// connections encoding their own header blocks share nothing; a decoder holding one
// table for the process would read the second connection's blocks against the
// first's table and produce header fields from the wrong request.
func TestHTTP2KeepsConnectionsIndependent(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())

	for cookie := uint64(1); cookie <= 2; cookie++ {
		client, server := newH2Encoder(), newH2Encoder()
		agg.Observe(reqAt(cookie, at(t, 0), concat(preface(), grpcRequest(t, client, 1, "/Shop/Get"))))
		agg.Observe(respAt(cookie, at(t, time.Duration(cookie)*time.Millisecond),
			grpcResponse(t, server, 1, "0")))
	}

	item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))

	if item.RequestCount != 2 {
		t.Fatalf("request_count = %d, want 2", item.RequestCount)
	}
	if item.ConnCount != 0 {
		t.Fatalf("conn_count = %d, want 0", item.ConnCount)
	}
	if want := uint64((3 * time.Millisecond).Microseconds()); item.DurationSumUs != want {
		t.Fatalf("duration_sum_us = %d, want %d", item.DurationSumUs, want)
	}
}

// TestHTTP2ReleasesHeldBytesWhenAConnectionIsSwept pins that the carry buffer is
// given back. It is accounted globally, so a connection that goes idle holding a
// half-frame -- which a keep-alive connection between requests routinely does --
// would otherwise consume a slice of the agent's budget permanently, and enough of
// them would stop every other connection on the node from ever holding anything.
func TestHTTP2ReleasesHeldBytesWhenAConnectionIsSwept(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())
	client := newH2Encoder()

	stream := concat(preface(), grpcRequest(t, client, 1, "/Shop/Get"))
	// Stop three bytes into a frame header, which the decoder has to hold.
	agg.Observe(reqAt(1, at(t, 0), stream[:len(http2Preface)+3]))

	agg.mu.Lock()
	held := agg.buffered
	agg.mu.Unlock()
	if held == 0 {
		t.Fatal("nothing was held over, so this test is not exercising the buffer")
	}

	// Past ConnTTL, so the sweep drops the connection. The connection is left to the
	// TTL rather than closed, because the buffer has to come back on both paths and
	// the sweep is the one that used to leak it.
	clock.set(at(t, 15*time.Minute))
	agg.Flush()

	agg.mu.Lock()
	defer agg.mu.Unlock()
	if agg.buffered != 0 {
		t.Fatalf("buffered = %d after the connection was swept, want 0", agg.buffered)
	}
	if len(agg.conns) != 0 {
		t.Fatalf("%d connections still tracked, want 0", len(agg.conns))
	}
}

// TestHTTP2ReleasesHeldBytesWhenAConnectionCloses is the same accounting on the
// other release path. Every connection now ends at its close rather than at an idle
// timeout, so this is the path that actually runs; the sweep only sees the closes
// that went missing.
func TestHTTP2ReleasesHeldBytesWhenAConnectionCloses(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())
	client := newH2Encoder()

	stream := concat(preface(), grpcRequest(t, client, 1, "/Shop/Get"))
	// Stop three bytes into a frame header, which the decoder has to hold.
	agg.Observe(reqAt(1, at(t, 0), stream[:len(http2Preface)+3]))

	agg.mu.Lock()
	held := agg.buffered
	agg.mu.Unlock()
	if held == 0 {
		t.Fatal("nothing was held over, so this test is not exercising the buffer")
	}

	agg.Observe(closeAt(1, at(t, time.Second)))

	agg.mu.Lock()
	defer agg.mu.Unlock()
	if agg.buffered != 0 {
		t.Fatalf("buffered = %d after the connection closed, want 0", agg.buffered)
	}
	if agg.inFlight != 0 {
		t.Fatalf("in-flight = %d after the connection closed, want 0", agg.inFlight)
	}
	if len(agg.conns) != 0 {
		t.Fatalf("%d connections still tracked, want 0", len(agg.conns))
	}
}

// TestHTTP2DoesNotCountRequestTrailersAsASecondRequest pins the one thing a stream
// id is for on the request half. A client may keep talking on a stream after the
// server has answered it -- request trailers, or a body it was still sending -- and
// a header block arriving then has nothing outstanding to pair with, exactly like
// the first block of a genuinely new request. Treating the two alike invents a
// request that is never answered, so it is swept as unanswered and lands in
// request_count with no error and no duration: the count is simply wrong, and
// nothing in the output says so.
func TestHTTP2DoesNotCountRequestTrailersAsASecondRequest(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())
	client, server := newH2Encoder(), newH2Encoder()

	agg.Observe(reqAt(1, at(t, 0), concat(preface(), grpcRequest(t, client, 1, "/Shop/Get"))))
	agg.Observe(respAt(1, at(t, 4*time.Millisecond), grpcResponse(t, server, 1, "0")))

	// The stream is finished and filed. The client now sends trailers on it.
	agg.Observe(reqAt(1, at(t, 5*time.Millisecond),
		frame(frameHeaders, flagEndHeaders|flagEndStream, 1,
			client.encode(t, hf("x-client-trace", "done")))))

	// Two minutes, not one. A phantom request started by the trailers would not be
	// swept until its TTL expired a minute later, landing it in the minute AFTER the
	// one the real request was filed under -- so a flush that closed only the first
	// minute would report the correct number and hide the bug in a bucket it had not
	// looked at yet. Waiting for both minutes to close means the phantom shows up
	// either as a second item or not at all.
	item := flushOne(t, agg, clock, at(t, 2*time.Minute+5*time.Second))

	if item.RequestCount != 1 {
		t.Fatalf("request_count = %d, want 1 -- the trailers started a phantom request", item.RequestCount)
	}
	if item.ErrorCount != 0 {
		t.Fatalf("error_count = %d, want 0", item.ErrorCount)
	}
	if item.DurationSumUs != 4000 {
		t.Fatalf("duration_sum_us = %d, want 4000", item.DurationSumUs)
	}
}

// TestHTTP2TrimsPaddingAndPriorityFromAHeaderBlock covers the two optional fields a
// HEADERS frame may carry ahead of and behind its fragment. They are rare enough on
// real traffic to go unnoticed and fatal when mishandled: a fragment trimmed by the
// wrong number of bytes does not fail here, it feeds HPACK garbage and desynchronises
// the dynamic table for every block after it on this direction.
func TestHTTP2TrimsPaddingAndPriorityFromAHeaderBlock(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())
	client, server := newH2Encoder(), newH2Encoder()

	// PADDED and PRIORITY together: one pad-length byte, five priority bytes, the
	// fragment, then the padding itself.
	block := client.encode(t,
		hf(":method", "POST"), hf(":scheme", "http"), hf(":path", "/Shop/Get"),
		hf(":authority", "shop.svc"), hf("content-type", "application/grpc"))
	padded := concat(
		[]byte{4},                          // pad length
		[]byte{0x00, 0x00, 0x00, 0x01, 15}, // stream dependency and weight
		block,
		[]byte{0, 0, 0, 0}) // the padding

	agg.Observe(reqAt(1, at(t, 0), concat(preface(),
		frame(frameHeaders, flagEndHeaders|flagPadded|flagPriority, 1, padded),
		frame(frameData, flagEndStream, 1, []byte("\x00\x00\x00\x00\x03abc")))))
	agg.Observe(respAt(1, at(t, 6*time.Millisecond), grpcResponse(t, server, 1, "0")))

	// A second request proves the dynamic table survived the trim: its block is
	// encoded against the table the first one left behind, so a fragment read at the
	// wrong offset would take this one down with it.
	agg.Observe(reqAt(1, at(t, 7*time.Millisecond), grpcRequest(t, client, 3, "/Shop/List")))
	agg.Observe(respAt(1, at(t, 9*time.Millisecond), grpcResponse(t, server, 3, "0")))

	item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))

	if item.RequestCount != 2 || item.ConnCount != 0 {
		t.Fatalf("request_count = %d, conn_count = %d, want 2 and 0", item.RequestCount, item.ConnCount)
	}
	if item.DurationSumUs != 8000 {
		t.Fatalf("duration_sum_us = %d, want 8000", item.DurationSumUs)
	}
}

// TestHTTP2TakesTheLastHeaderTableSizeInASettingsFrame pins duplicate handling. A
// SETTINGS frame may carry the same parameter twice and the last value is the one in
// force (RFC 9113 6.5.3), so a decoder sized from the first is sized to a limit the
// peer has already replaced -- and a peer that then uses the larger table produces
// blocks it cannot read.
func TestHTTP2TakesTheLastHeaderTableSizeInASettingsFrame(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())
	client, server := newH2Encoder(), newH2Encoder()

	// 512 first, then 8192. Only the second is in force.
	settings := concat(
		[]byte{0x00, 0x01, 0x00, 0x00, 0x02, 0x00},
		[]byte{0x00, 0x01, 0x00, 0x00, 0x20, 0x00})
	agg.Observe(reqAt(1, at(t, 0), concat(preface(), frame(frameSettings, 0, 0, settings))))

	server.enc.SetMaxDynamicTableSizeLimit(8192)
	server.enc.SetMaxDynamicTableSize(8192)

	agg.Observe(reqAt(1, at(t, time.Millisecond), grpcRequest(t, client, 1, "/Shop/Get")))
	agg.Observe(respAt(1, at(t, 3*time.Millisecond), grpcResponse(t, server, 1, "0")))

	item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))

	if item.ConnCount != 0 {
		t.Fatalf("conn_count = %d, want 0 -- the decoder was sized from the superseded value", item.ConnCount)
	}
	if item.RequestCount != 1 {
		t.Fatalf("request_count = %d, want 1", item.RequestCount)
	}
}

// TestHTTP2GoesOpaqueOnAContinuationWithNoBlockOpen is the mirror of the rule that
// nothing may interleave into a header block. A CONTINUATION carries neither a
// stream that means anything on its own nor an END_STREAM flag, so accumulating one
// with no block open would attribute whatever it decoded to a stream taken from a
// frame that does not name one.
func TestHTTP2GoesOpaqueOnAContinuationWithNoBlockOpen(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())
	client := newH2Encoder()

	block := client.encode(t, hf(":method", "GET"), hf(":path", "/late"))
	agg.Observe(reqAt(1, at(t, 0), concat(preface(),
		frame(frameContinuation, flagEndHeaders, 1, block))))

	item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))

	if item.ConnCount != 1 {
		t.Fatalf("conn_count = %d, want 1", item.ConnCount)
	}
	if item.RequestCount != 0 {
		t.Fatalf("request_count = %d, want 0", item.RequestCount)
	}
}

// TestHTTP2GoesOpaqueWhenTheBufferBudgetIsExhausted covers the agent-wide bound.
// Held-over bytes are the one thing a peer can make the agent allocate, and the
// budget is what stops a slow trickle of them from being unbounded. Exhausting it
// must cost the connection its visibility, not the agent its memory.
func TestHTTP2GoesOpaqueWhenTheBufferBudgetIsExhausted(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := NewAggregator(Config{MaxBufferedBytes: 8}, clock, testPods(), logr.Discard())
	client := newH2Encoder()

	block := client.encode(t, hf(":method", "GET"), hf(":path", "/big"))
	full := frame(frameHeaders, flagEndHeaders, 1, block)

	// The preface lands whole; the frame arrives one byte short of complete, so the
	// bytes so far must be held -- and there is no budget to hold them in.
	agg.Observe(reqAt(1, at(t, 0), preface()))
	agg.Observe(reqAt(1, at(t, time.Millisecond), full[:len(full)-1]))

	item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))

	if item.ConnCount != 1 {
		t.Fatalf("conn_count = %d, want 1 -- the connection kept buffering past its budget", item.ConnCount)
	}
	if agg.buffered != 0 {
		t.Fatalf("buffered = %d, want 0 after the connection went opaque", agg.buffered)
	}
}
