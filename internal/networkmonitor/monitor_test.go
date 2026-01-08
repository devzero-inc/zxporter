package networkmonitor

import (
	"fmt"
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
	errChan := make(chan string, numGoroutines*2) // Buffer for potential errors
	// Launch multiple goroutines to hash the same entry concurrently
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Verify conntrackEntryKey
			h1 := conntrackEntryKey(entry)
			if h1 != expectedConntrackHash {
				errChan <- fmt.Sprintf("Hash mismatch for conntrackEntryKey: expected %d, got %d", expectedConntrackHash, h1)
			}
			// Verify entryGroupKey
			h2 := entryGroupKey(entry)
			if h2 != expectedGroupHash {
				errChan <- fmt.Sprintf("Hash mismatch for entryGroupKey: expected %d, got %d", expectedGroupHash, h2)
			}
		}()
	}
	wg.Wait()
	close(errChan)
	// Report all collected errors
	for err := range errChan {
		t.Error(err)
	}
}

func TestIPv6Hashing(t *testing.T) {
	// Verify that IPv6 addresses are hashed correctly and do not panic
	srcIP := netaddr.MustParseIP("2001:db8::1")
	dstIP := netaddr.MustParseIP("2001:db8::2")
	entry := &Entry{
		Src:   netaddr.IPPortFrom(srcIP, 12345),
		Dst:   netaddr.IPPortFrom(dstIP, 80),
		Proto: 6, // TCP
	}

	// Should not panic
	h1 := conntrackEntryKey(entry)
	h2 := entryGroupKey(entry)

	if h1 == 0 {
		t.Error("conntrackEntryKey returned 0 for IPv6")
	}
	if h2 == 0 {
		t.Error("entryGroupKey returned 0 for IPv6")
	}
}
