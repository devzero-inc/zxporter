package collector

import (
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeTestResource(key string) CollectedResource {
	return CollectedResource{
		ResourceType: Pod,
		EventType:    EventTypeAdd,
		Key:          key,
	}
}

func TestResourcesBatcher_EmitsOnSize(t *testing.T) {
	inputChan := make(chan CollectedResource, 200)
	outputChan := make(chan []CollectedResource, 100)
	batchSize := 5
	batchTime := 10 * time.Second // large so it won't fire

	batcher := NewResourcesBatcher(batchSize, batchTime, inputChan, outputChan, logr.Discard())
	batcher.start()

	// Send exactly batchSize items
	for i := 0; i < batchSize; i++ {
		inputChan <- makeTestResource("key")
	}

	// Should receive a batch quickly
	var received []CollectedResource
	select {
	case received = <-outputChan:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for batch from size trigger")
	}

	assert.Len(t, received, batchSize)

	// Stop the batcher
	close(inputChan)
	batcher.stop()
}

func TestResourcesBatcher_EmitsOnTimeout(t *testing.T) {
	inputChan := make(chan CollectedResource, 100)
	outputChan := make(chan []CollectedResource, 100)
	batchSize := 100                    // large so it won't fire
	batchTime := 100 * time.Millisecond // small so it fires quickly

	batcher := NewResourcesBatcher(batchSize, batchTime, inputChan, outputChan, logr.Discard())
	batcher.start()

	// Send fewer items than batchSize
	inputChan <- makeTestResource("key1")
	inputChan <- makeTestResource("key2")

	// Should receive a batch when the timer fires
	var received []CollectedResource
	select {
	case received = <-outputChan:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for batch from timeout trigger")
	}

	assert.Len(t, received, 2)

	close(inputChan)
	batcher.stop()
}

func TestResourcesBatcher_DrainOnStop(t *testing.T) {
	inputChan := make(chan CollectedResource, 100)
	outputChan := make(chan []CollectedResource, 100)
	batchSize := 100
	batchTime := 10 * time.Second

	batcher := NewResourcesBatcher(batchSize, batchTime, inputChan, outputChan, logr.Discard())
	batcher.start()

	// Add some items
	for i := 0; i < 3; i++ {
		inputChan <- makeTestResource("key")
	}

	// Close input channel to trigger drain
	close(inputChan)

	// stop() will wait for the goroutine to finish
	batcher.stop()

	// The output channel should be closed by the batcher
	var totalReceived int
	for batch := range outputChan {
		totalReceived += len(batch)
	}

	assert.Equal(t, 3, totalReceived, "all items should have been drained")
}

func TestResourcesBatcher_AdaptiveBatchSize_GrowsOnSizeHit(t *testing.T) {
	inputChan := make(chan CollectedResource, 200)
	outputChan := make(chan []CollectedResource, 100)
	initialBatchSize := 5
	batchTime := 10 * time.Second

	batcher := NewResourcesBatcher(initialBatchSize, batchTime, inputChan, outputChan, logr.Discard())
	batcher.start()

	// Fill exactly initialBatchSize to trigger size-based emit and growth
	for i := 0; i < initialBatchSize; i++ {
		inputChan <- makeTestResource("key")
	}

	// Wait for the batch to be emitted
	select {
	case <-outputChan:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for batch")
	}

	// Capture the current batch size before stopping
	grownBatchSize := batcher.batchSize

	// Close input then stop (must close input first so batcher drain can finish)
	close(inputChan)
	batcher.stop()

	// newBatchSize = int(min(1000, 1.5 * 5)) = int(7.5) = 7
	assert.Greater(t, grownBatchSize, initialBatchSize, "batch size should have grown after size trigger")
}

func TestResourcesBatcher_AdaptiveBatchSize_ShrinksOnTimeoutHit(t *testing.T) {
	inputChan := make(chan CollectedResource, 200)
	outputChan := make(chan []CollectedResource, 100)
	initialBatchSize := 100 // large so size won't trigger
	batchTime := 100 * time.Millisecond

	batcher := NewResourcesBatcher(initialBatchSize, batchTime, inputChan, outputChan, logr.Discard())
	batcher.start()

	// Add only a few items (less than batchSize), so only timeout fires
	inputChan <- makeTestResource("key1")
	inputChan <- makeTestResource("key2")

	// Wait for timeout-triggered batch
	select {
	case batch := <-outputChan:
		require.Len(t, batch, 2)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for batch")
	}

	// Batch size should have shrunk (min DefaultBatchSize = 50)
	// newBatchSize = int(max(50, 0.8 * 100)) = int(80)
	// 80 < 100, so shrunk
	shrunkBatchSize := batcher.batchSize

	close(inputChan)
	batcher.stop()

	assert.Less(t, shrunkBatchSize, initialBatchSize, "batch size should have shrunk after timeout trigger")
}
