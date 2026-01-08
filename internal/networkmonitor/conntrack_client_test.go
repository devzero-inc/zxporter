package networkmonitor

import (
	"net"
	"testing"

	ct "github.com/florianl/go-conntrack"
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
