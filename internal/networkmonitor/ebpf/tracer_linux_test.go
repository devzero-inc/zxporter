//go:build linux

package ebpf

import (
	"bytes"
	"testing"

	"github.com/google/gopacket/layers"
)

// testQName is the name carried by dnsResponsePayload, in both its question and its
// answer. Named because both tests below assert on it from either section, and a
// payload edited without the assertions following would otherwise pass by accident.
const testQName = "google.lt"

// dnsResponsePayload is a captured IPv4/UDP DNS response: one question and one answer,
// google.lt A 172.217.21.163, from 192.168.65.7:53 to 172.17.0.3:57381. It is the shape
// the cgroup programs put on the perf ring -- an IP header first, no ethernet.
func dnsResponsePayload() []byte {
	return []byte{
		69,
		0,
		0,
		80,
		106,
		126,
		64,
		0,
		62,
		17,
		36,
		91,
		192,
		168,
		65,
		7,
		172,
		17,
		0,
		3,
		0,
		53,
		224,
		37,
		0,
		60,
		174,
		17,
		191,
		43,
		129,
		128,
		0,
		1,
		0,
		1,
		0,
		0,
		0,
		0,
		6,
		103,
		111,
		111,
		103,
		108,
		101,
		2,
		108,
		116,
		0,
		0,
		1,
		0,
		1,
		6,
		103,
		111,
		111,
		103,
		108,
		101,
		2,
		108,
		116,
		0,
		0,
		1,
		0,
		1,
		0,
		0,
		1,
		121,
		0,
		4,
		172,
		217,
		21,
		163,
	}
}

func TestDNSDecoderDecodesResponse(t *testing.T) {
	d := newDNSDecoder()
	if err := d.decode(dnsResponsePayload()); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !d.hasAnswers() {
		t.Fatal("hasAnswers = false, want true for a response carrying an A record")
	}

	ev := d.event()

	if got, want := ev.SrcIP.String(), "192.168.65.7"; got != want {
		t.Errorf("SrcIP = %q, want %q", got, want)
	}
	if got, want := ev.DstIP.String(), "172.17.0.3"; got != want {
		t.Errorf("DstIP = %q, want %q", got, want)
	}
	if got, want := len(ev.Questions), 1; got != want {
		t.Fatalf("len(Questions) = %d, want %d", got, want)
	}
	if got, want := ev.Questions[0].Name, testQName; got != want {
		t.Errorf("Questions[0].Name = %q, want %q", got, want)
	}
	if got, want := len(ev.Answers), 1; got != want {
		t.Fatalf("len(Answers) = %d, want %d", got, want)
	}
	if got, want := ev.Answers[0].Name, testQName; got != want {
		t.Errorf("Answers[0].Name = %q, want %q", got, want)
	}
	if got, want := ev.Answers[0].Type, layers.DNSTypeA; got != want {
		t.Errorf("Answers[0].Type = %v, want %v", got, want)
	}
	if got, want := ev.Answers[0].IP.String(), "172.217.21.163"; got != want {
		t.Errorf("Answers[0].IP = %q, want %q", got, want)
	}
}

// TestDNSDecoderEventSurvivesTheNextPacket is the test the decoder's design turns on.
// It decodes into layer structs it reuses, so the only thing keeping an event that has
// been handed to another goroutine from being rewritten under it is that event() copies
// everything out. If that copy is ever weakened -- a []byte kept instead of a string, a
// net.IP aliased instead of cloned -- this fails.
func TestDNSDecoderEventSurvivesTheNextPacket(t *testing.T) {
	second := dnsResponsePayload()
	// The first occurrence of the name is the question, so this rewrites the question
	// only: google.lt becomes moogle.lt, same length, so nothing downstream shifts.
	second[bytes.Index(second, []byte("google"))] = 'm'
	// The last four bytes are the A record's address.
	second[len(second)-1] = 99

	d := newDNSDecoder()
	if err := d.decode(dnsResponsePayload()); err != nil {
		t.Fatalf("decode first: %v", err)
	}
	first := d.event()

	if err := d.decode(second); err != nil {
		t.Fatalf("decode second: %v", err)
	}
	next := d.event()

	if got, want := next.Questions[0].Name, "moogle.lt"; got != want {
		t.Fatalf("second event Questions[0].Name = %q, want %q -- the payloads are not distinct", got, want)
	}
	if got, want := next.Answers[0].IP.String(), "172.217.21.99"; got != want {
		t.Fatalf("second event Answers[0].IP = %q, want %q -- the payloads are not distinct", got, want)
	}

	if got, want := first.Questions[0].Name, testQName; got != want {
		t.Errorf("first event Questions[0].Name = %q, want %q: clobbered by the next packet", got, want)
	}
	if got, want := first.Answers[0].Name, testQName; got != want {
		t.Errorf("first event Answers[0].Name = %q, want %q: clobbered by the next packet", got, want)
	}
	if got, want := first.Answers[0].IP.String(), "172.217.21.163"; got != want {
		t.Errorf("first event Answers[0].IP = %q, want %q: clobbered by the next packet", got, want)
	}
}

func TestDNSDecoderRejectsNonDNS(t *testing.T) {
	d := newDNSDecoder()

	if err := d.decode(nil); err == nil {
		t.Error("decode(nil) = nil, want an error")
	}
	if err := d.decode([]byte{69, 0, 0, 4}); err == nil {
		t.Error("decode(truncated IPv4) = nil, want an error")
	}

	// A well-formed packet whose UDP ports are not 53: no DNS layer to find.
	notDNS := dnsResponsePayload()
	notDNS[20], notDNS[21] = 0x1f, 0x90 // source port 8080
	notDNS[22], notDNS[23] = 0x1f, 0x91 // destination port 8081
	if err := d.decode(notDNS); err == nil {
		t.Error("decode(non-DNS UDP) = nil, want an error")
	}
}

// TestDNSDecoderDoesNotAllocatePerPacket guards the reason the decoder exists. The
// decode itself is the hot path -- most packets on a Kubernetes node answer nothing and
// are dropped without ever reaching event() -- so it is decode, not the copy, that has
// to stay free of allocation.
func TestDNSDecoderDoesNotAllocatePerPacket(t *testing.T) {
	d := newDNSDecoder()
	payload := dnsResponsePayload()

	// Warm the decoder's internal buffers: the first decode legitimately allocates them.
	if err := d.decode(payload); err != nil {
		t.Fatalf("decode: %v", err)
	}

	got := testing.AllocsPerRun(100, func() {
		_ = d.decode(payload)
	})
	if got != 0 {
		t.Errorf("decode allocated %v objects per packet, want 0", got)
	}
}
