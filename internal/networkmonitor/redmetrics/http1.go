package redmetrics

import (
	"bufio"
	"bytes"
	"net/http"
	"sync"
)

// headerTerminator ends an HTTP/1.1 header block. Finding it is what tells the
// decoder that the whole block was copied: the standard library's readers would
// otherwise block on a truncated block, and a decoder that guessed at the missing
// bytes would emit plausible wrong numbers rather than fail.
var headerTerminator = []byte("\r\n\r\n")

// bodyLenUnknown is the body length of a message whose framing does not declare
// one: a chunked body, or a response delimited by the connection closing. The
// decoder cannot find the next message boundary past such a body, so the connection
// becomes opaque once the message it belongs to has been accounted for.
const bodyLenUnknown int64 = -1

// minStatusCode and maxStatusCode bound the status codes treated as HTTP. The
// standard library accepts any three digits and rejects only a negative one, so
// both a 900 and a 000 parse; a decoder that accepted either would be reading
// something that is not HTTP and reporting it as if it were.
//
// The bottom of the range matters as much as the top, and less obviously: a
// sub-100 code is not merely admitted, it is admitted as INFORMATIONAL, so those
// bytes advance the stream and answer nothing, and the request they were mistaken
// for stays outstanding until the sweep files it as unanswered.
const (
	minStatusCode = 100
	maxStatusCode = 600
)

// messageStart is what one message boundary told the decoder. Only one of method
// and status is set, depending on which half of the connection the bytes crossed.
type messageStart struct {
	// headerLen is how many bytes the header block occupied, terminator included. It
	// is the offset of the body from the start of the message.
	headerLen int

	// bodyLen is the declared body length, or bodyLenUnknown.
	bodyLen int64

	// method is the request method. It is carried on the in-flight request because
	// the response's framing depends on it: a HEAD response declares a length and
	// then sends no body at all.
	method string

	// status is the response status code.
	status int
}

// http1Reader is the scratch space one parse needs: the bufio.Reader the standard
// library's message readers insist on, and the bytes.Reader it wraps.
//
// It is pooled because it was not free. A fresh bufio.NewReader is a 4KB allocation,
// and the decoder made one per request and another per response -- 243MB, 22% of
// everything this agent allocated, on a node doing nothing unusual. None of it
// outlives the parse: the header block is decoded into values that are copied out,
// and neither http.ReadRequest nor http.ReadResponse hands back anything still
// pointing at the reader that we go on to read.
type http1Reader struct {
	block bytes.Reader
	buf   *bufio.Reader
}

var http1ReaderPool = sync.Pool{
	New: func() any {
		r := &http1Reader{}
		r.buf = bufio.NewReader(&r.block)
		return r
	},
}

// acquireHTTP1Reader returns a reader positioned at the start of block.
func acquireHTTP1Reader(block []byte) *http1Reader {
	r, ok := http1ReaderPool.Get().(*http1Reader)
	if !ok {
		r = &http1Reader{}
		r.buf = bufio.NewReader(&r.block)
	}
	r.block.Reset(block)
	// Reset the bufio too: its buffer still holds the tail of whatever it read last,
	// and Reset is what discards that rather than serving it to the next caller.
	r.buf.Reset(&r.block)
	return r
}

// releaseHTTP1Reader returns a reader to the pool.
//
// It clears the block first. The block is a window into the aggregator's copy of a
// connection, and a reader idling in the pool holding a reference to one would keep
// that copy alive for as long as it sat there -- turning a pool meant to save memory
// into one that pins it.
func releaseHTTP1Reader(r *http1Reader) {
	r.block.Reset(nil)
	r.buf.Reset(&r.block)
	http1ReaderPool.Put(r)
}

// parseRequestStart reads a request message boundary from the start of b.
//
// The parsing is the standard library's. A bespoke request-line reader would have
// to be right about absolute-form URIs, obsolete line folding and every framing
// rule that decides where the body ends, and being wrong about any of those
// desynchronises the stream silently rather than failing.
func parseRequestStart(b []byte) (messageStart, bool) {
	block, ok := headerBlock(b)
	if !ok {
		return messageStart{}, false
	}

	r := acquireHTTP1Reader(block)
	defer releaseHTTP1Reader(r)

	req, err := http.ReadRequest(r.buf)
	if err != nil {
		return messageStart{}, false
	}
	// An HTTP/2 connection preface parses cleanly as a request called PRI, so the
	// version is checked rather than assumed. The sniff sends h2c to its own decoder
	// before this is ever reached (see sniffLocked), so the check is a second line
	// rather than the only one -- but it is what stops a preface arriving by some
	// path the sniff did not see from being counted as one very strange HTTP/1.1
	// request.
	if req.ProtoMajor != 1 {
		return messageStart{}, false
	}

	return messageStart{
		headerLen: len(block),
		bodyLen:   req.ContentLength,
		method:    req.Method,
	}, true
}

// parseResponseStart reads a response message boundary from the start of b.
//
// method is the method of the request this answers, empty when the decoder does not
// know it. It is passed through to the standard library because the response's
// framing depends on it, and getting that wrong walks the stream position into the
// middle of the next message.
func parseResponseStart(b []byte, method string) (messageStart, bool) {
	block, ok := headerBlock(b)
	if !ok {
		return messageStart{}, false
	}

	var req *http.Request
	if method != "" {
		req = &http.Request{Method: method}
	}
	r := acquireHTTP1Reader(block)
	defer releaseHTTP1Reader(r)

	resp, err := http.ReadResponse(r.buf, req)
	if err != nil {
		return messageStart{}, false
	}
	if resp.ProtoMajor != 1 {
		return messageStart{}, false
	}
	if resp.StatusCode < minStatusCode || resp.StatusCode >= maxStatusCode {
		return messageStart{}, false
	}

	return messageStart{
		headerLen: len(block),
		bodyLen:   responseBodyLen(resp),
		status:    resp.StatusCode,
	}, true
}

// responseBodyLen is how many body bytes follow the header block, or
// bodyLenUnknown.
//
// It is not resp.ContentLength. On a HEAD response the standard library reports the
// declared length even though no body follows, so that a caller can see what the GET
// would have returned; a decoder that skipped those bytes would walk the stream
// position into the middle of the next response. The body reader carries the framing
// decision instead, and it is the same reader the standard library would have used
// to find the next message itself.
func responseBodyLen(resp *http.Response) int64 {
	if resp.Body == http.NoBody {
		return 0
	}
	return resp.ContentLength
}

// headerBlock is the complete header block at the start of b, terminator included.
// It reports false when b does not contain a whole block, which is what the probe's
// copy limit looks like from here.
func headerBlock(b []byte) ([]byte, bool) {
	end := bytes.Index(b, headerTerminator)
	if end < 0 {
		return nil, false
	}
	return b[:end+len(headerTerminator)], true
}

// informational reports whether a status code is a 1XX. An informational response
// is not a completion -- a 100 Continue is followed by the real response on the same
// request -- so it advances the stream and nothing else.
//
// It is only ever asked about a status parseResponseStart already accepted, which is
// what makes the single comparison enough: nothing below minStatusCode gets here.
func informational(status int) bool { return status < 200 }

// isServerError reports whether a status counts towards error_count. Only 5XX does.
//
// A 4XX is deliberately not an error: it is a statement about the caller, and
// counting it would make the error rate track client behaviour rather than the
// health of the container being measured -- which is the thing these numbers exist
// to put beside a resource recommendation. The status code is read and then
// discarded; the table has no status dimension, and this is the only decision made
// from it.
func isServerError(status int) bool { return status/100 == 5 }
