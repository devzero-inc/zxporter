package redmetrics

import (
	"strings"
	"testing"
)

// These tests are about FRAMING, not about parsing. Whether a header field was read
// correctly barely matters here; where the next message starts matters entirely,
// because a decoder that is one byte out reports plausible wrong latencies for the
// rest of the connection's life rather than failing.

func TestParseRequestStartFraming(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		ok      bool
		bodyLen int64
		method  string
	}{
		{
			name:    "content length declares the body",
			raw:     "POST /x HTTP/1.1\r\nHost: h\r\nContent-Length: 5\r\n\r\nhello",
			ok:      true,
			bodyLen: 5,
			method:  "POST",
		},
		{
			name:    "no body means the next message starts at the terminator",
			raw:     "GET /x HTTP/1.1\r\nHost: h\r\n\r\n",
			ok:      true,
			bodyLen: 0,
			method:  "GET",
		},
		{
			// Where the body ends can only be learned by reading it, which the decoder
			// does not do. The connection goes opaque once this message is accounted for.
			name:    "chunked has no declared length",
			raw:     "POST /x HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked\r\n\r\n",
			ok:      true,
			bodyLen: bodyLenUnknown,
			method:  "POST",
		},
		{
			// The connection preface of an h2c client. It parses, which is exactly why
			// the version has to be checked rather than assumed.
			name: "http/2 preface is not an http/1.1 request",
			raw:  "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n",
			ok:   false,
		},
		{
			name: "an incomplete header block is not a boundary",
			raw:  "GET /x HTTP/1.1\r\nHost: h\r\n",
			ok:   false,
		},
		{
			name: "bytes from the middle of somebody's body are not a request",
			raw:  "\x16\x03\x01\x02\x00\r\n\r\n",
			ok:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, ok := parseRequestStart([]byte(tc.raw))
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !tc.ok {
				return
			}
			if msg.bodyLen != tc.bodyLen {
				t.Fatalf("bodyLen = %d, want %d", msg.bodyLen, tc.bodyLen)
			}
			if msg.method != tc.method {
				t.Fatalf("method = %q, want %q", msg.method, tc.method)
			}
			// The header length is the offset of the body, so it counts the blank line
			// that ends the block. Reporting it one terminator short would leave the
			// decoder parsing "\r\n\r\n" as the next request line.
			if want := strings.Index(tc.raw, "\r\n\r\n") + 4; msg.headerLen != want {
				t.Fatalf("headerLen = %d, want %d", msg.headerLen, want)
			}
		})
	}
}

func TestParseResponseStartFraming(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		raw     string
		ok      bool
		bodyLen int64
		status  int
	}{
		{
			name:    "content length declares the body",
			raw:     "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nhi",
			ok:      true,
			bodyLen: 2,
			status:  200,
		},
		{
			// The declared length describes the body a GET would have returned. Skipping
			// it would land the decoder inside the next response.
			name:    "a head response declares a length and sends nothing",
			method:  "HEAD",
			raw:     "HTTP/1.1 200 OK\r\nContent-Length: 4096\r\n\r\n",
			ok:      true,
			bodyLen: 0,
			status:  200,
		},
		{
			name:    "204 has no body whatever it declares",
			raw:     "HTTP/1.1 204 No Content\r\n\r\n",
			ok:      true,
			bodyLen: 0,
			status:  204,
		},
		{
			name:    "304 has no body whatever it declares",
			raw:     "HTTP/1.1 304 Not Modified\r\nContent-Length: 100\r\n\r\n",
			ok:      true,
			bodyLen: 0,
			status:  304,
		},
		{
			name:    "chunked has no declared length",
			raw:     "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n",
			ok:      true,
			bodyLen: bodyLenUnknown,
			status:  200,
		},
		{
			// Delimited by the connection closing: there is no next message to find, so
			// the length is unknown in the same way chunked is.
			name:    "a response with no framing at all runs to close",
			raw:     "HTTP/1.1 200 OK\r\nServer: x\r\n\r\n",
			ok:      true,
			bodyLen: bodyLenUnknown,
			status:  200,
		},
		{
			name:    "informational responses parse and answer nothing",
			raw:     "HTTP/1.1 100 Continue\r\n\r\n",
			ok:      true,
			bodyLen: 0,
			status:  100,
		},
		{
			// The standard library accepts any three digits. A 900 is not HTTP being
			// unusual, it is bytes that are not HTTP parsing by coincidence.
			name: "a status above the real range is not a response",
			raw:  "HTTP/1.1 900 Nonsense\r\n\r\n",
			ok:   false,
		},
		{
			// The bottom of the range is the more dangerous half. The standard library
			// rejects only a negative code, so a 000 parses -- and a status below 100
			// reads as informational, which would advance the stream past these bytes
			// and leave the request they answered nothing of outstanding.
			name: "a status below the real range is not a response",
			raw:  "HTTP/1.1 000 Nonsense\r\n\r\n",
			ok:   false,
		},
		{
			name: "a two-digit status padded to three is not a response",
			raw:  "HTTP/1.1 099 Nonsense\r\n\r\n",
			ok:   false,
		},
		{
			name: "an incomplete header block is not a boundary",
			raw:  "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n",
			ok:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, ok := parseResponseStart([]byte(tc.raw), tc.method)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !tc.ok {
				return
			}
			if msg.bodyLen != tc.bodyLen {
				t.Fatalf("bodyLen = %d, want %d", msg.bodyLen, tc.bodyLen)
			}
			if msg.status != tc.status {
				t.Fatalf("status = %d, want %d", msg.status, tc.status)
			}
			if want := strings.Index(tc.raw, "\r\n\r\n") + 4; msg.headerLen != want {
				t.Fatalf("headerLen = %d, want %d", msg.headerLen, want)
			}
		})
	}
}

// TestParseResponseStartWithoutAKnownMethod pins what happens when the decoder is
// holding no request to attribute the response to. It still has to parse -- the
// alternative is treating every such response as a lost stream -- and it falls back
// to the framing a GET would have.
func TestParseResponseStartWithoutAKnownMethod(t *testing.T) {
	msg, ok := parseResponseStart([]byte("HTTP/1.1 200 OK\r\nContent-Length: 7\r\n\r\npayload"), "")
	if !ok {
		t.Fatal("a response with no known request should still parse")
	}
	if msg.bodyLen != 7 {
		t.Fatalf("bodyLen = %d, want 7", msg.bodyLen)
	}
}

// TestIsServerError pins the boundary of error_count. A 4XX is the caller being
// wrong, and counting it would make the error rate track client behaviour rather
// than the health of the container the number sits beside.
func TestIsServerError(t *testing.T) {
	for _, status := range []int{200, 204, 301, 400, 404, 429, 499} {
		if isServerError(status) {
			t.Fatalf("status %d counted as a server error", status)
		}
	}
	for _, status := range []int{500, 502, 503, 599} {
		if !isServerError(status) {
			t.Fatalf("status %d not counted as a server error", status)
		}
	}
}

// TestInformational pins where a response stops being provisional. 199 is still the
// server saying "not yet"; 200 is an answer.
func TestInformational(t *testing.T) {
	for _, status := range []int{100, 101, 103, 199} {
		if !informational(status) {
			t.Fatalf("status %d should be informational", status)
		}
	}
	for _, status := range []int{200, 404, 500} {
		if informational(status) {
			t.Fatalf("status %d should not be informational", status)
		}
	}
}
