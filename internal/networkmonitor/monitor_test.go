package networkmonitor

import (
	"sync"
	"testing"

	"inet.af/netaddr"
)

func TestConcurrentHashing(t *testing.T) {
	// Define a shared entry to be hashed consistently
	srcIP := netaddr.MustParseIP("192.168.1.1")
	dstIP := netaddr.MustParseIP("10.0.0.1")
	entry := &Entry{
		Src:   netaddr.IPPortFrom(srcIP, 12345),
		Dst:   netaddr.IPPortFrom(dstIP, 80),
		Proto: 6, // TCP
	}

	// Calculate expected hash once (single-threaded)
	expectedConntrackHash := conntrackEntryKey(entry)
	expectedGroupHash := entryGroupKey(entry)

	var wg sync.WaitGroup
	numGoroutines := 100

	// Launch multiple goroutines to hash the same entry concurrently
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			// Verify conntrackEntryKey
			h1 := conntrackEntryKey(entry)
			if h1 != expectedConntrackHash {
				t.Errorf("Hash mismatch for conntrackEntryKey: expected %d, got %d", expectedConntrackHash, h1)
			}

			// Verify entryGroupKey
			h2 := entryGroupKey(entry)
			if h2 != expectedGroupHash {
				t.Errorf("Hash mismatch for entryGroupKey: expected %d, got %d", expectedGroupHash, h2)
			}
		}()
	}

	wg.Wait()
}
