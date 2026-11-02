package networkmonitor

import (
	"net"
	"testing"

	ct "github.com/florianl/go-conntrack"
	"inet.af/netaddr"
)

func TestProcessSessions(t *testing.T) {
	// Helpers
	uint8Ptr := func(v uint8) *uint8 { return &v }
	uint16Ptr := func(v uint16) *uint16 { return &v }
	uint64Ptr := func(v uint64) *uint64 { return &v }
	ip := net.ParseIP("1.2.3.4")

	validSess := ct.Con{
		Origin: &ct.IPTuple{
			Src: &ip,
			Dst: &ip,
			Proto: &ct.ProtoTuple{
				Number:  uint8Ptr(6),
				SrcPort: uint16Ptr(1234),
				DstPort: uint16Ptr(80),
			},
		},
		Reply: &ct.IPTuple{
			Src: &ip,
			Dst: &ip,
			Proto: &ct.ProtoTuple{
				Number:  uint8Ptr(6),
				SrcPort: uint16Ptr(80),
				DstPort: uint16Ptr(1234),
			},
		},
		CounterOrigin: &ct.Counter{
			Bytes:   uint64Ptr(100),
			Packets: uint64Ptr(1),
		},
		CounterReply: &ct.Counter{
			Bytes:   uint64Ptr(200),
			Packets: uint64Ptr(2),
		},
	}

	// Test 1: Valid Session
	t.Run("Valid Session", func(t *testing.T) {
		res := processSessions([]ct.Con{validSess}, nil)
		if len(res) != 1 {
			t.Errorf("Expected 1 entry, got %d", len(res))
		}
	})

	// Test 2: Missing Origin.Dst (should be skipped)
	t.Run("Missing Origin.Dst", func(t *testing.T) {
		badSess := validSess
		// Deep copy logic simplified: struct copy works for pointers, but we need to modify the pointer in the copy
		// Since we are modifying a pointer, we need make sure we don't affect validSess if we reuse it directly?
		// Struct assignment copies the pointer values. Modifying the struct field `Origin` is safe IF we replace the struct it points to...
		// But here `badSess.Origin` points to same `IPTuple` as `validSess.Origin`.
		// So we construct a new IPTuple.
		badSess.Origin = &ct.IPTuple{
			Src: &ip,
			Dst: nil, // The specific nil case we want to test
			Proto: &ct.ProtoTuple{
				Number:  uint8Ptr(6),
				SrcPort: uint16Ptr(1234),
				DstPort: uint16Ptr(80),
			},
		}

		res := processSessions([]ct.Con{badSess}, nil)
		if len(res) != 0 {
			t.Errorf("Expected 0 entries (skipped), got %d", len(res))
		}
	})

	// Test 3: Nil Filter (should pass)
	t.Run("Nil Filter", func(t *testing.T) {
		res := processSessions([]ct.Con{validSess}, nil)
		if len(res) != 1 {
			t.Errorf("Expected 1 entry, got %d", len(res))
		}
	})

	// Test 4: Deny Filter (should skip)
	t.Run("Deny Filter", func(t *testing.T) {
		filter := func(e *Entry) bool { return false }
		res := processSessions([]ct.Con{validSess}, filter)
		if len(res) != 0 {
			t.Errorf("Expected 0 entries, got %d", len(res))
		}
	})
}

// TestProcessSessionsTranslatedDst covers the DNAT case. A connection to a
// ClusterIP is rewritten by kube-proxy to one backend pod, so the REPLY tuple's
// source is not the destination the client dialed. Both addresses have to
// survive: the dialed one names the Service, the reply one names the pod whose
// zone decides the price.
func TestProcessSessionsTranslatedDst(t *testing.T) {
	uint8Ptr := func(v uint8) *uint8 { return &v }
	uint16Ptr := func(v uint16) *uint16 { return &v }
	uint64Ptr := func(v uint64) *uint64 { return &v }

	clientIP := net.ParseIP("10.0.0.5")
	clusterIP := net.ParseIP("10.96.0.10")
	backendIP := net.ParseIP("10.0.1.50")

	dnatSess := ct.Con{
		Origin: &ct.IPTuple{
			Src: &clientIP,
			Dst: &clusterIP,
			Proto: &ct.ProtoTuple{
				Number:  uint8Ptr(6),
				SrcPort: uint16Ptr(40000),
				DstPort: uint16Ptr(80),
			},
		},
		Reply: &ct.IPTuple{
			// The backend answers from its own address and its own port.
			Src: &backendIP,
			Dst: &clientIP,
			Proto: &ct.ProtoTuple{
				Number:  uint8Ptr(6),
				SrcPort: uint16Ptr(8080),
				DstPort: uint16Ptr(40000),
			},
		},
		CounterOrigin: &ct.Counter{Bytes: uint64Ptr(100), Packets: uint64Ptr(1)},
		CounterReply:  &ct.Counter{Bytes: uint64Ptr(200), Packets: uint64Ptr(2)},
	}

	t.Run("a DNAT'd session keeps the dialed address and records the backend", func(t *testing.T) {
		res := processSessions([]ct.Con{dnatSess}, nil)
		if len(res) != 1 {
			t.Fatalf("expected 1 entry, got %d", len(res))
		}
		e := res[0]

		if got := e.Dst.IP().String(); got != "10.96.0.10" {
			t.Errorf("Dst should stay the dialed ClusterIP, got %s", got)
		}
		if got := e.Dst.Port(); got != 80 {
			t.Errorf("Dst port should stay the service port, got %d", got)
		}
		if !e.HasTranslatedDst() {
			t.Fatal("HasTranslatedDst should be true for a DNAT'd session")
		}
		if got := e.DstTranslated.IP().String(); got != "10.0.1.50" {
			t.Errorf("DstTranslated should be the backend pod, got %s", got)
		}
		if got := e.DstTranslated.Port(); got != 8080 {
			t.Errorf("DstTranslated port should be the backend port, got %d", got)
		}
	})

	t.Run("an untranslated session records no backend", func(t *testing.T) {
		// Reply source equals the dialed destination: no rewrite happened.
		plain := dnatSess
		plain.Reply = &ct.IPTuple{
			Src: &clusterIP,
			Dst: &clientIP,
			Proto: &ct.ProtoTuple{
				Number:  uint8Ptr(6),
				SrcPort: uint16Ptr(80),
				DstPort: uint16Ptr(40000),
			},
		}

		res := processSessions([]ct.Con{plain}, nil)
		if len(res) != 1 {
			t.Fatalf("expected 1 entry, got %d", len(res))
		}
		if res[0].HasTranslatedDst() {
			t.Errorf("no translation should leave DstTranslated zero, got %s", res[0].DstTranslated.String())
		}
	})
}

// TestEntryGroupKeyDistinguishesBackends pins the property that makes the
// translated address worth carrying: two connections to the same ClusterIP that
// landed on different backends are different flows. Folding them together would
// report one backend's zone for both, which is exactly the mis-pricing this is
// meant to fix.
func TestEntryGroupKeyDistinguishesBackends(t *testing.T) {
	mk := func(backend string, port uint16) *Entry {
		e := &Entry{
			Src:   netaddr.IPPortFrom(netaddr.IPv4(10, 0, 0, 5), 40000),
			Dst:   netaddr.IPPortFrom(netaddr.IPv4(10, 96, 0, 10), 80),
			Proto: 6,
		}
		if backend != "" {
			ip, err := netaddr.ParseIP(backend)
			if err != nil {
				t.Fatalf("bad test IP %q: %v", backend, err)
			}
			e.DstTranslated = netaddr.IPPortFrom(ip, port)
		}
		return e
	}

	backendA := entryGroupKey(mk("10.0.1.50", 8080))
	backendB := entryGroupKey(mk("10.0.2.51", 8080))
	noBackend := entryGroupKey(mk("", 0))

	if backendA == backendB {
		t.Error("two backends of the same service must not share a group key")
	}
	if backendA == noBackend {
		t.Error("a translated flow must not share a group key with an untranslated one")
	}
	if backendA != entryGroupKey(mk("10.0.1.50", 8080)) {
		t.Error("the same flow must hash to the same group key")
	}

	// The delta cache key carries the same distinction, so counters for one
	// backend are never subtracted from another's.
	if conntrackEntryKey(mk("10.0.1.50", 8080)) == conntrackEntryKey(mk("10.0.2.51", 8080)) {
		t.Error("two backends of the same service must not share a conntrack cache key")
	}
}
