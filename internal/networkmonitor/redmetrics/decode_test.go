package redmetrics

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
)

// The tests in this file drive CONNECTIONS, not single messages. A decoder that
// resynchronises incorrectly after the first response produces plausible counts and
// plausible durations rather than an error, so a test that only ever sends one
// request per connection cannot tell a working decoder from a broken one.

// dataAt builds an observation carrying bytes on one half of the test connection.
func dataAt(cookie uint64, path SyscallPath, at time.Time, b []byte) SocketEvent {
	ev := recvAt(cookie, at)
	ev.Path = path
	ev.TrueLen = uint32(len(b))
	ev.Payload = b
	return ev
}

// reqAt is bytes arriving from the client.
func reqAt(cookie uint64, at time.Time, b []byte) SocketEvent {
	return dataAt(cookie, SyscallRecv, at, b)
}

// respAt is bytes leaving towards the client.
func respAt(cookie uint64, at time.Time, b []byte) SocketEvent {
	return dataAt(cookie, SyscallSend, at, b)
}

// truncate models the probe's copy limit: the syscall carried every byte, and only
// a prefix of them reached userspace.
func truncate(ev SocketEvent, copied int) SocketEvent {
	ev.Payload = ev.Payload[:copied]
	return ev
}

// request builds an HTTP/1.1 request with a Content-Length body.
func request(method, path, body string) []byte {
	return []byte(fmt.Sprintf(
		"%s %s HTTP/1.1\r\nHost: shop.svc\r\nContent-Length: %d\r\n\r\n%s",
		method, path, len(body), body))
}

// response builds an HTTP/1.1 response with a Content-Length body.
func response(status int, body string) []byte {
	return []byte(fmt.Sprintf(
		"HTTP/1.1 %d %s\r\nContent-Length: %d\r\n\r\n%s",
		status, http.StatusText(status), len(body), body))
}

// headers builds a message from raw header lines and no body bytes.
func headers(lines ...string) []byte {
	return []byte(strings.Join(lines, "\r\n") + "\r\n\r\n")
}

// testBase is the minute every test in this file starts from. Bucketing is a
// statement about where a wall clock stood, so the tests need a fixed one.
const testBase = "2026-08-25T10:00:00Z"

// at is a timestamp inside the test's minute, offset by a duration.
func at(t *testing.T, offset time.Duration) time.Time {
	return minuteAt(t, testBase).Add(offset)
}

// flushOne runs a flush and insists it produced exactly one item.
func flushOne(t *testing.T, agg *Aggregator, clock *fakeClock, now time.Time) *gen.ContainerRedMetricsItem {
	t.Helper()
	clock.set(now)
	items := agg.Flush()
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	return items[0]
}

// wantBuckets asserts the cumulative ladder holds exactly one request at each named
// bound index and nothing before the first of them.
func wantBuckets(t *testing.T, got []uint64, want []uint64) {
	t.Helper()
	if len(got) != DurationBucketCount {
		t.Fatalf("ladder length = %d, want %d", len(got), DurationBucketCount)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("bucket %d = %d, want %d (full ladder %v)", i, got[i], want[i], got)
		}
	}
}

// TestDecodeMeasuresEveryRequestOnAKeepAliveConnection is the central test of this
// slice. Three requests with different latencies go down one connection, one after
// another, and every one of them has to be measured -- which can only happen if the
// decoder found the end of each body and put its stream position exactly on the next
// request line.
//
// The three durations are deliberately far apart. A decoder that paired the wrong
// response with the wrong request would produce three durations that still sum to
// roughly the same total, so the sum alone cannot catch it; the maximum and the
// ladder can.
func TestDecodeMeasuresEveryRequestOnAKeepAliveConnection(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())

	// 1ms, then 30ms, then 500ms.
	agg.Observe(reqAt(1, at(t, 0), request("POST", "/a", "hello")))
	agg.Observe(respAt(1, at(t, time.Millisecond), response(200, "ok")))

	agg.Observe(reqAt(1, at(t, 100*time.Millisecond), request("POST", "/b", "world!!")))
	agg.Observe(respAt(1, at(t, 130*time.Millisecond), response(200, "fine")))

	agg.Observe(reqAt(1, at(t, 200*time.Millisecond), request("GET", "/c", "")))
	agg.Observe(respAt(1, at(t, 700*time.Millisecond), response(200, "slow")))

	item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))

	if item.RequestCount != 3 {
		t.Fatalf("request_count = %d, want 3", item.RequestCount)
	}
	if item.ErrorCount != 0 {
		t.Fatalf("error_count = %d, want 0", item.ErrorCount)
	}
	if item.ConnCount != 0 {
		t.Fatalf("conn_count = %d, want 0: a connection decoded end to end is not an opaque connection", item.ConnCount)
	}
	if want := uint64((1 + 30 + 500) * time.Millisecond.Microseconds()); item.DurationSumUs != want {
		t.Fatalf("duration_sum_us = %d, want %d", item.DurationSumUs, want)
	}
	if want := uint64(500 * time.Millisecond.Microseconds()); item.DurationMaxUs != want {
		t.Fatalf("duration_max_us = %d, want %d", item.DurationMaxUs, want)
	}
	// 1ms at bound 1ms, 30ms at bound 50ms, 500ms at bound 500ms.
	wantBuckets(t, item.DurationBuckets, []uint64{0, 1, 1, 1, 1, 1, 2, 2, 2, 3, 3, 3, 3, 3})
}

// TestDecodePairsPipelinedResponsesInOrder pins the pairing rule. Two requests are
// outstanding at once and nothing on the wire says which response answers which, so
// the decoder pairs them in order -- which is what HTTP/1.1 requires of the server.
//
// The offsets are chosen so that a decoder pairing them the other way round would
// report the same duration SUM. Only the maximum and the ladder separate the two.
func TestDecodePairsPipelinedResponsesInOrder(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())

	agg.Observe(reqAt(1, at(t, 0), request("GET", "/first", "")))
	agg.Observe(reqAt(1, at(t, 10*time.Millisecond), request("GET", "/second", "")))

	// In order, the durations are 20ms and 190ms. Paired backwards they would be
	// 10ms and 200ms -- the same total, a different maximum.
	agg.Observe(respAt(1, at(t, 20*time.Millisecond), response(200, "1")))
	agg.Observe(respAt(1, at(t, 200*time.Millisecond), response(200, "2")))

	item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))

	if item.RequestCount != 2 {
		t.Fatalf("request_count = %d, want 2", item.RequestCount)
	}
	if want := uint64(190 * time.Millisecond.Microseconds()); item.DurationMaxUs != want {
		t.Fatalf("duration_max_us = %d, want %d: responses were paired out of order", item.DurationMaxUs, want)
	}
	// 20ms at bound 20ms, 190ms at bound 200ms. Backwards pairing would put one at
	// the 10ms bound instead.
	wantBuckets(t, item.DurationBuckets, []uint64{0, 0, 0, 0, 0, 1, 1, 1, 2, 2, 2, 2, 2, 2})
}

// TestDecodeStepsOverABodyLargerThanTheCopyLimit pins the reason the probe reports a
// true length at all. A body far larger than what reached userspace must not cost
// the connection its position, or every upload would look like an opaque connection.
func TestDecodeStepsOverABodyLargerThanTheCopyLimit(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())

	big := request("POST", "/upload", strings.Repeat("x", 64*1024))
	agg.Observe(truncate(reqAt(1, at(t, 0), big), PayloadCap))
	agg.Observe(respAt(1, at(t, 5*time.Millisecond), response(200, "stored")))

	// The second request only decodes if the first body was stepped over exactly.
	agg.Observe(reqAt(1, at(t, 10*time.Millisecond), request("GET", "/after", "")))
	agg.Observe(respAt(1, at(t, 12*time.Millisecond), response(200, "ok")))

	item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))

	if item.RequestCount != 2 {
		t.Fatalf("request_count = %d, want 2: the oversized body lost the stream position", item.RequestCount)
	}
	if item.ConnCount != 0 {
		t.Fatalf("conn_count = %d, want 0: a truncated BODY is not a truncated boundary", item.ConnCount)
	}
}

// TestDecodeHandlesAResponseSplitAcrossReads pins that a message may arrive in
// several socket calls. A server that writes its headers and then its body does not
// owe the agent one syscall per message.
func TestDecodeHandlesAResponseSplitAcrossReads(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())

	agg.Observe(reqAt(1, at(t, 0), request("GET", "/split", "")))
	agg.Observe(respAt(1, at(t, 2*time.Millisecond),
		headers("HTTP/1.1 200 OK", "Content-Length: 11")))
	agg.Observe(respAt(1, at(t, 3*time.Millisecond), []byte("hello world")))

	// Proof the position survived the split: the next exchange decodes too.
	agg.Observe(reqAt(1, at(t, 4*time.Millisecond), request("GET", "/after", "")))
	agg.Observe(respAt(1, at(t, 6*time.Millisecond), response(200, "ok")))

	item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))

	if item.RequestCount != 2 {
		t.Fatalf("request_count = %d, want 2", item.RequestCount)
	}
	if item.ConnCount != 0 {
		t.Fatalf("conn_count = %d, want 0", item.ConnCount)
	}
	// The completion is stamped by the HEADERS, which is when the response became
	// an answer; the body that follows is not part of the latency. The reasoning --
	// why time-to-first-byte is the right thing to put beside a resource
	// recommendation -- is in docs/features/red-metrics/INVARIANTS.md.
	if want := uint64(2 * time.Millisecond.Microseconds()); item.DurationMaxUs != want {
		t.Fatalf("duration_max_us = %d, want %d", item.DurationMaxUs, want)
	}
}

// TestDecodeGoesOpaqueWhenAMessageBOUNDARYWasTruncated is the other half of the
// truncation rule. A header block the probe could not copy whole is not something to
// guess at: the connection is given up on, and the conn_count says so.
func TestDecodeGoesOpaqueWhenAMessageBoundaryWasTruncated(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())

	agg.Observe(reqAt(1, at(t, 0), request("GET", "/fine", "")))
	agg.Observe(respAt(1, at(t, time.Millisecond), response(200, "ok")))

	// A request whose headers ran past the copy limit: no terminator arrived, so
	// where the body starts is unknowable.
	huge := request("GET", "/"+strings.Repeat("q", 8*1024), "")
	agg.Observe(truncate(reqAt(1, at(t, 2*time.Millisecond), huge), 512))

	// Everything after it is unreadable, and must not be counted as if it were.
	agg.Observe(respAt(1, at(t, 3*time.Millisecond), response(200, "ignored")))
	agg.Observe(reqAt(1, at(t, 4*time.Millisecond), request("GET", "/ignored", "")))

	item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))

	if item.RequestCount != 1 {
		t.Fatalf("request_count = %d, want 1: only the exchange before the truncation is knowable", item.RequestCount)
	}
	if item.ConnCount != 1 {
		t.Fatalf("conn_count = %d, want 1", item.ConnCount)
	}
}

// TestDecodeGoesOpaqueOnAChunkedBody pins the framing limit. A chunked response is
// accounted for -- it answered its request -- but the decoder cannot find where the
// body ends without reading it, so the connection stops being decoded there.
func TestDecodeGoesOpaqueOnAChunkedBody(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())

	agg.Observe(reqAt(1, at(t, 0), request("GET", "/stream", "")))
	agg.Observe(respAt(1, at(t, 5*time.Millisecond),
		headers("HTTP/1.1 200 OK", "Transfer-Encoding: chunked")))
	agg.Observe(reqAt(1, at(t, 50*time.Millisecond), request("GET", "/after", "")))

	item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))

	if item.RequestCount != 1 {
		t.Fatalf("request_count = %d, want 1", item.RequestCount)
	}
	if want := uint64(5 * time.Millisecond.Microseconds()); item.DurationSumUs != want {
		t.Fatalf("duration_sum_us = %d, want %d: the chunked response still answered its request", item.DurationSumUs, want)
	}
	if item.ConnCount != 1 {
		t.Fatalf("conn_count = %d, want 1", item.ConnCount)
	}
}

// TestDecodeCountsAnUnansweredRequestWithoutALatency pins the asymmetry that lets
// request_count and the last latency bucket disagree. A request nobody answered
// happened, so it counts; it has no duration, so it enters no bucket; and it is not
// an error, because no status ever said so.
func TestDecodeCountsAnUnansweredRequestWithoutALatency(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())

	agg.Observe(reqAt(1, at(t, 50*time.Second), request("GET", "/hangs", "")))

	// DefaultConfig gives the request fifteen seconds, so it is filed at 10:01:05 --
	// in the minute it was given up on, not the one it started in. The request starts
	// late in its minute deliberately: with a short deadline, a request that starts
	// early times out in the same minute it began, and the test would pass without
	// distinguishing the two rules.
	item := flushOne(t, agg, clock, at(t, 2*time.Minute+5*time.Second))

	if got := item.Timestamp.AsTime(); !got.Equal(minuteAt(t, "2026-08-25T10:01:00Z")) {
		t.Fatalf("timestamp = %s, want the minute the request timed out in", got)
	}
	if item.RequestCount != 1 {
		t.Fatalf("request_count = %d, want 1", item.RequestCount)
	}
	if item.ErrorCount != 0 {
		t.Fatalf("error_count = %d, want 0: nothing answered, so nothing failed", item.ErrorCount)
	}
	if item.DurationSumUs != 0 || item.DurationMaxUs != 0 {
		t.Fatalf("duration sum/max = %d/%d, want 0/0", item.DurationSumUs, item.DurationMaxUs)
	}
	for i, v := range item.DurationBuckets {
		if v != 0 {
			t.Fatalf("bucket %d = %d, want 0: an unanswered request has no latency", i, v)
		}
	}
}

// TestDecodeSettlesAnInFlightRequestWhenTheConnectionCloses pins where an
// interrupted request lands. A client that hangs up mid-response, or a server that
// dies with the request on the floor, produced a request that happened and was never
// answered -- so it counts, on the same terms as one that timed out.
//
// The instant matters as much as the count. The request below would have been filed
// at 10:01:05 by the deadline alone, in the wrong minute; the close is what ended it,
// and that is when the measurement became final.
func TestDecodeSettlesAnInFlightRequestWhenTheConnectionCloses(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())

	agg.Observe(reqAt(1, at(t, 50*time.Second), request("GET", "/hangs", "")))
	agg.Observe(closeAt(1, at(t, 52*time.Second)))

	item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))

	if got := item.Timestamp.AsTime(); !got.Equal(minuteAt(t, "2026-08-25T10:00:00Z")) {
		t.Fatalf("timestamp = %s, want the minute the connection closed in", got)
	}
	if item.RequestCount != 1 {
		t.Fatalf("request_count = %d, want 1", item.RequestCount)
	}
	if item.ErrorCount != 0 {
		t.Fatalf("error_count = %d, want 0: nothing answered, so nothing failed", item.ErrorCount)
	}
	if item.DurationSumUs != 0 || item.DurationMaxUs != 0 {
		t.Fatalf("duration sum/max = %d/%d, want 0/0", item.DurationSumUs, item.DurationMaxUs)
	}
}

// TestDecodeFilesAnAbandonedRequestAtItsDeadlineNotTheClose pins the other half of
// the close path. A connection can sit open long after the agent gave up waiting for
// a response, and closing it does not change when that happened: the request expired
// at its deadline, so it belongs to the minute the deadline fell in. Filing it at the
// close would pile every abandoned request onto whichever minute the socket happened
// to be torn down in, which for a pooled client is arbitrary.
func TestDecodeFilesAnAbandonedRequestAtItsDeadlineNotTheClose(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())

	// Deadline is 10:01:05, two minutes before the close.
	agg.Observe(reqAt(1, at(t, 50*time.Second), request("GET", "/hangs", "")))
	agg.Observe(closeAt(1, at(t, 3*time.Minute)))

	item := flushOne(t, agg, clock, at(t, 4*time.Minute))

	if got := item.Timestamp.AsTime(); !got.Equal(minuteAt(t, "2026-08-25T10:01:00Z")) {
		t.Fatalf("timestamp = %s, want the minute the request was given up on", got)
	}
	if item.RequestCount != 1 {
		t.Fatalf("request_count = %d, want 1", item.RequestCount)
	}
}

// TestDecodeReleasesAnOpaqueConnectionOnClose pins that giving up on decoding a
// connection does not exempt it from being reclaimed. An opaque connection holds no
// requests and no buffers, but it holds a table entry and the admission slot that
// goes with it -- and on a node running TLS, which is most of them, that is the state
// the table is mostly made of.
func TestDecodeReleasesAnOpaqueConnectionOnClose(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())

	agg.Observe(reqAt(1, at(t, 0), []byte{0x16, 0x03, 0x01, 0x02, 0x00, 0x01, 0x00}))
	if got := agg.connCount(); got != 1 {
		t.Fatalf("tracked connections = %d, want the opaque connection tracked", got)
	}

	agg.Observe(closeAt(1, at(t, time.Second)))
	if got := agg.connCount(); got != 0 {
		t.Fatalf("tracked connections = %d, want it released", got)
	}

	// It was counted once when it went opaque, and the close does not count it again.
	item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))
	if item.ConnCount != 1 {
		t.Fatalf("conn_count = %d, want 1", item.ConnCount)
	}
	if item.RequestCount != 0 {
		t.Fatalf("request_count = %d, want 0", item.RequestCount)
	}
}

// TestDecodeFilesARequestUnderTheMinuteItCompleted pins the bucketing rule against
// the obvious alternative. A request that began at 10:00:59 and was answered at
// 10:01:02 belongs to 10:01, because that is when its measurement became final --
// filing it under 10:00 would mean reopening a minute that may already have shipped.
func TestDecodeFilesARequestUnderTheMinuteItCompleted(t *testing.T) {
	clock := &fakeClock{now: minuteAt(t, "2026-08-25T10:00:59Z")}
	agg := newTestAggregator(clock, testPods())

	agg.Observe(reqAt(1, minuteAt(t, "2026-08-25T10:00:59Z"), request("GET", "/late", "")))
	agg.Observe(respAt(1, minuteAt(t, "2026-08-25T10:01:02Z"), response(200, "ok")))

	item := flushOne(t, agg, clock, minuteAt(t, "2026-08-25T10:02:05Z"))

	if got := item.Timestamp.AsTime(); !got.Equal(minuteAt(t, "2026-08-25T10:01:00Z")) {
		t.Fatalf("timestamp = %s, want 10:01:00", got)
	}
	if want := uint64(3 * time.Second.Microseconds()); item.DurationSumUs != want {
		t.Fatalf("duration_sum_us = %d, want %d", item.DurationSumUs, want)
	}
}

// TestDecodeCountsServerErrorsOnly pins what error_count means. A 4XX is the caller
// being wrong; counting it would make the error rate track client behaviour rather
// than the health of the container the number sits beside.
func TestDecodeCountsServerErrorsOnly(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())

	statuses := []int{200, 404, 400, 500, 503, 301}
	for i, status := range statuses {
		offset := time.Duration(i) * 10 * time.Millisecond
		agg.Observe(reqAt(1, at(t, offset), request("GET", "/x", "")))
		agg.Observe(respAt(1, at(t, offset+time.Millisecond), response(status, "body")))
	}

	item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))

	if item.RequestCount != uint64(len(statuses)) {
		t.Fatalf("request_count = %d, want %d", item.RequestCount, len(statuses))
	}
	if item.ErrorCount != 2 {
		t.Fatalf("error_count = %d, want 2 (the 500 and the 503 only)", item.ErrorCount)
	}
}

// TestDecodeIgnoresInformationalResponses pins that a 100 Continue is not an answer.
// Treating it as one would report a latency that measured the server's willingness
// to read a body, and would leave the real response with nothing outstanding.
func TestDecodeIgnoresInformationalResponses(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())

	agg.Observe(reqAt(1, at(t, 0), request("POST", "/expect", "payload")))
	agg.Observe(respAt(1, at(t, time.Millisecond), headers("HTTP/1.1 100 Continue")))
	agg.Observe(respAt(1, at(t, 40*time.Millisecond), response(200, "done")))

	item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))

	if item.RequestCount != 1 {
		t.Fatalf("request_count = %d, want 1", item.RequestCount)
	}
	if want := uint64(40 * time.Millisecond.Microseconds()); item.DurationSumUs != want {
		t.Fatalf("duration_sum_us = %d, want %d: the latency is measured to the real response", item.DurationSumUs, want)
	}
	if item.ConnCount != 0 {
		t.Fatalf("conn_count = %d, want 0", item.ConnCount)
	}
}

// TestDecodeHandlesHeadResponses pins the one framing rule that cannot be read off
// the response alone. A HEAD response declares the length the body WOULD have had
// and then sends nothing, so a decoder that skipped that many bytes would land in
// the middle of the next response and never recover.
func TestDecodeHandlesHeadResponses(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())

	agg.Observe(reqAt(1, at(t, 0), request("HEAD", "/thing", "")))
	agg.Observe(respAt(1, at(t, time.Millisecond),
		headers("HTTP/1.1 200 OK", "Content-Length: 4096")))

	agg.Observe(reqAt(1, at(t, 2*time.Millisecond), request("GET", "/thing", "")))
	agg.Observe(respAt(1, at(t, 3*time.Millisecond), response(200, "real body")))

	item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))

	if item.RequestCount != 2 {
		t.Fatalf("request_count = %d, want 2: the phantom HEAD body was skipped as if it were real", item.RequestCount)
	}
	if item.ConnCount != 0 {
		t.Fatalf("conn_count = %d, want 0", item.ConnCount)
	}
}

// TestDecodeDropsAResponseWithNothingOutstanding pins what happens when the wire
// says something the decoder has no start for. Counting it would put a request in
// the total that has no duration and no cause; inventing a duration would put a
// number in the ladder that no request produced.
func TestDecodeDropsAResponseWithNothingOutstanding(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())

	agg.Observe(reqAt(1, at(t, 0), request("GET", "/one", "")))
	agg.Observe(respAt(1, at(t, time.Millisecond), response(200, "ok")))
	agg.Observe(respAt(1, at(t, 2*time.Millisecond), response(500, "stray")))

	item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))

	if item.RequestCount != 1 {
		t.Fatalf("request_count = %d, want 1", item.RequestCount)
	}
	if item.ErrorCount != 0 {
		t.Fatalf("error_count = %d, want 0: a 5XX answering nothing is not a failed request", item.ErrorCount)
	}
}

// TestDecodeTreatsUnreadableFirstBytesAsAnOpaqueConnection pins the sniff. TLS, a
// protocol nobody taught this decoder, and a connection joined midstream all look
// the same from here: bytes with no request line, and no way to synchronise.
func TestDecodeTreatsUnreadableFirstBytesAsAnOpaqueConnection(t *testing.T) {

	cases := []struct {
		name  string
		first []byte
	}{
		{"tls client hello", []byte{0x16, 0x03, 0x01, 0x02, 0x00, 0x01, 0x00}},
		{"midstream body bytes", []byte("...continuation of somebody's body\r\n\r\n")},
		// An HTTP/2 connection the agent joined after its preface. The frame bytes
		// are perfectly valid HTTP/2 -- a HEADERS frame on stream 1 -- and are still
		// unreadable, because the preface is the only marker HTTP/2 offers and
		// without it there is nothing to say where a frame starts. Guessing would
		// mean reading a length out of the middle of somebody's payload.
		{"midstream http/2 frames", []byte{0x00, 0x00, 0x04, 0x01, 0x05, 0x00, 0x00, 0x00, 0x01, 0x82, 0x84, 0x86, 0x41}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := &fakeClock{now: at(t, 0)}
			agg := newTestAggregator(clock, testPods())

			agg.Observe(reqAt(1, at(t, 0), tc.first))
			agg.Observe(respAt(1, at(t, time.Millisecond), response(200, "ok")))

			item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))

			if item.ConnCount != 1 {
				t.Fatalf("conn_count = %d, want 1", item.ConnCount)
			}
			if item.RequestCount != 0 {
				t.Fatalf("request_count = %d, want 0", item.RequestCount)
			}
		})
	}
}

// TestDecodeSurvivesAnEmptyFirstRead pins that a syscall carrying no bytes decides
// nothing about a connection. A zero-length recv is what a poll loop or a peek looks
// like from the probe, and it is evidence of nothing: there are no first bytes to
// fail to recognise, so the connection is still new and its real first request is
// still to come.
//
// The distinction matters because the alternative is silent and expensive. Treating
// an empty read as unreadable would count a conn_count against a container that was
// serving perfectly readable HTTP, and would then decode none of the traffic that
// followed on that connection -- opacity being permanent by design.
func TestDecodeSurvivesAnEmptyFirstRead(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())

	agg.Observe(reqAt(1, at(t, 0), nil))
	agg.Observe(reqAt(1, at(t, time.Millisecond), request("GET", "/x", "")))
	agg.Observe(respAt(1, at(t, 3*time.Millisecond), response(200, "ok")))

	item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))

	if item.ConnCount != 0 {
		t.Fatalf("conn_count = %d, want 0: an empty read is not an unreadable connection", item.ConnCount)
	}
	if item.RequestCount != 1 {
		t.Fatalf("request_count = %d, want 1", item.RequestCount)
	}
	if want := uint64(2 * time.Millisecond.Microseconds()); item.DurationSumUs != want {
		t.Fatalf("duration_sum_us = %d, want %d", item.DurationSumUs, want)
	}
}

// TestDecodeCountsAnOpaqueConnectionOnce pins that conn_count is a count of
// connections given up on, not of unreadable syscalls. A connection that goes opaque
// and then carries a thousand more reads is still one opaque connection.
func TestDecodeCountsAnOpaqueConnectionOnce(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())

	agg.Observe(reqAt(1, at(t, 0), []byte{0x16, 0x03, 0x01, 0x00, 0x05}))
	for i := 1; i < 200; i++ {
		agg.Observe(reqAt(1, at(t, time.Duration(i)*time.Millisecond), []byte("more ciphertext")))
	}

	item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))

	if item.ConnCount != 1 {
		t.Fatalf("conn_count = %d, want 1", item.ConnCount)
	}
}

// TestDecodeCountsBothWhenAConnectionDegradesMidStream pins the case the glossary's
// "an opaque connection contributes a connection count and nothing else" does not
// describe: a connection is not opaque or readable for its whole life, it is readable
// until the moment it is not. The requests read before that moment are real
// measurements and stay; the conn_count marks where the reading stopped.
func TestDecodeCountsBothWhenAConnectionDegradesMidStream(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())

	agg.Observe(reqAt(1, at(t, 0), request("GET", "/a", "")))
	agg.Observe(respAt(1, at(t, time.Millisecond), response(200, "ok")))
	agg.Observe(reqAt(1, at(t, 2*time.Millisecond), request("GET", "/b", "")))
	agg.Observe(respAt(1, at(t, 4*time.Millisecond), response(200, "ok")))

	// An upgrade, a proxied CONNECT tunnel, anything: from here the bytes are not
	// HTTP/1.1 messages.
	agg.Observe(reqAt(1, at(t, 6*time.Millisecond), []byte{0x00, 0x01, 0x02, 0x03}))

	item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))

	if item.RequestCount != 2 {
		t.Fatalf("request_count = %d, want 2", item.RequestCount)
	}
	if item.ConnCount != 1 {
		t.Fatalf("conn_count = %d, want 1", item.ConnCount)
	}
}

// TestDecodeDropsOutstandingRequestsWhenAConnectionGoesOpaque pins the deliberate
// gap. The decoder gives up because it no longer trusts what it read, so counting
// the requests it was still holding would assert they were real on exactly the
// evidence just declared untrustworthy.
func TestDecodeDropsOutstandingRequestsWhenAConnectionGoesOpaque(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())

	agg.Observe(reqAt(1, at(t, 0), request("GET", "/never-answered", "")))
	agg.Observe(reqAt(1, at(t, time.Millisecond), []byte{0xff, 0xfe, 0xfd}))

	// Long enough that the request sweep would have filed it, had it survived.
	item := flushOne(t, agg, clock, at(t, 5*time.Minute))

	if item.RequestCount != 0 {
		t.Fatalf("request_count = %d, want 0", item.RequestCount)
	}
	if item.ConnCount != 1 {
		t.Fatalf("conn_count = %d, want 1", item.ConnCount)
	}
}

// TestDecodeTakesTheConnectionOpaqueAtTheInFlightCap pins the memory bound's
// behaviour. Skipping the request that found no room would leave its response to be
// paired with a LATER request on the same connection, reporting a duration made of
// two different requests -- so the connection goes with it.
func TestDecodeTakesTheConnectionOpaqueAtTheInFlightCap(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := NewAggregator(Config{MaxInFlightRequests: 2}, clock, testPods(), logr.Discard())

	for i := 0; i < 3; i++ {
		agg.Observe(reqAt(1, at(t, time.Duration(i)*time.Millisecond), request("GET", "/x", "")))
	}
	agg.Observe(respAt(1, at(t, 10*time.Millisecond), response(200, "ok")))

	item := flushOne(t, agg, clock, at(t, 5*time.Minute))

	if item.ConnCount != 1 {
		t.Fatalf("conn_count = %d, want 1", item.ConnCount)
	}
	if item.RequestCount != 0 {
		t.Fatalf("request_count = %d, want 0: the two it was holding went with the connection", item.RequestCount)
	}
}

// TestDecodeReleasesInFlightRequestsBackToTheCap pins that the cap is a ceiling on
// concurrency rather than a lifetime budget. A completed request has to give its slot
// back, or a long-lived agent would eventually refuse everything.
func TestDecodeReleasesInFlightRequestsBackToTheCap(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := NewAggregator(Config{MaxInFlightRequests: 1}, clock, testPods(), logr.Discard())

	for i := 0; i < 5; i++ {
		offset := time.Duration(i) * 10 * time.Millisecond
		agg.Observe(reqAt(1, at(t, offset), request("GET", "/x", "")))
		agg.Observe(respAt(1, at(t, offset+time.Millisecond), response(200, "ok")))
	}

	item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))

	if item.RequestCount != 5 {
		t.Fatalf("request_count = %d, want 5", item.RequestCount)
	}
	if item.ConnCount != 0 {
		t.Fatalf("conn_count = %d, want 0: each request freed its slot", item.ConnCount)
	}
}

// TestDecodeGoesOpaqueWhenAPipelinedBoundaryWasTruncated is the truncation rule at
// the harder position. The read carried two whole requests and the probe copied only
// the first, so the second request's boundary sits in bytes that exist on the wire
// and not in memory. Walking only the copied prefix would end the walk quietly and
// leave the connection looking healthy while its position was already wrong.
func TestDecodeGoesOpaqueWhenAPipelinedBoundaryWasTruncated(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())

	first := request("GET", "/first", "")
	both := append(append([]byte{}, first...), request("GET", "/second", "")...)

	agg.Observe(truncate(reqAt(1, at(t, 0), both), len(first)))

	// Long enough that a request left in flight would have been swept and counted.
	item := flushOne(t, agg, clock, at(t, 5*time.Minute))

	if item.ConnCount != 1 {
		t.Fatalf("conn_count = %d, want 1: the second boundary was never copied", item.ConnCount)
	}
	if item.RequestCount != 0 {
		t.Fatalf("request_count = %d, want 0: the request in flight went with the connection", item.RequestCount)
	}
}

// TestDecodeKeepsConnectionsIndependent pins that decode state is per connection.
// A shared in-flight list would pair a response on one connection with a request on
// another -- a duration made of two different clients' requests -- and a shared
// stream position would desynchronise both the moment either one sent a body.
func TestDecodeKeepsConnectionsIndependent(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())

	// Two clients, interleaved, with very different latencies.
	agg.Observe(reqAt(1, at(t, 0), request("POST", "/slow", "body-on-one")))
	agg.Observe(reqAt(2, at(t, time.Millisecond), request("GET", "/fast", "")))
	agg.Observe(respAt(2, at(t, 2*time.Millisecond), response(200, "quick")))
	agg.Observe(respAt(1, at(t, 300*time.Millisecond), response(500, "eventually")))

	// Both must still be readable afterwards, which only holds if each kept its own
	// position through the other's traffic.
	agg.Observe(reqAt(2, at(t, 310*time.Millisecond), request("GET", "/again", "")))
	agg.Observe(respAt(2, at(t, 311*time.Millisecond), response(200, "ok")))

	item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))

	if item.RequestCount != 3 {
		t.Fatalf("request_count = %d, want 3", item.RequestCount)
	}
	if item.ErrorCount != 1 {
		t.Fatalf("error_count = %d, want 1", item.ErrorCount)
	}
	if item.ConnCount != 0 {
		t.Fatalf("conn_count = %d, want 0", item.ConnCount)
	}
	// 300ms on connection 1, 1ms and 1ms on connection 2. Crossed pairing would put
	// the maximum at 299ms or 310ms instead.
	if want := uint64(300 * time.Millisecond.Microseconds()); item.DurationMaxUs != want {
		t.Fatalf("duration_max_us = %d, want %d: responses crossed between connections", item.DurationMaxUs, want)
	}
	if want := uint64((300 + 1 + 1) * time.Millisecond.Microseconds()); item.DurationSumUs != want {
		t.Fatalf("duration_sum_us = %d, want %d", item.DurationSumUs, want)
	}
}
