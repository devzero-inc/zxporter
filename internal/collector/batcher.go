package collector

import (
	"math"
	"sync"
	"time"

	"github.com/go-logr/logr"
)

const (
	// DefaultBatchSize is the default maximum number of resources in a batch.
	DefaultBatchSize = 50

	// DefaultMaxBatchSize is the maximum number the batch size will grow to.
	DefaultMaxBatchSize = 1000

	// DefaultMaxBatchTime is the default maximum time duration before sending a batch.
	DefaultMaxBatchTime = 5 * time.Second
)

// ResourcesBatcher handles batching of CollectedResource items.
type ResourcesBatcher struct {
	batchSize    int
	maxBatchTime time.Duration
	inputChan    <-chan CollectedResource
	outBatchChan chan<- []CollectedResource
	stopCh       chan struct{}
	wg           sync.WaitGroup
	logger       logr.Logger
}

// NewResourcesBatcher creates a new ResourcesBatcher.
func NewResourcesBatcher(
	batchSize int,
	maxBatchTime time.Duration,
	inputChan <-chan CollectedResource,
	outBatchChan chan<- []CollectedResource,
	logger logr.Logger,
) *ResourcesBatcher {
	if batchSize <= 0 {
		batchSize = DefaultBatchSize
	}
	if maxBatchTime <= 0 {
		maxBatchTime = DefaultMaxBatchTime
	}
	return &ResourcesBatcher{
		batchSize:    batchSize,
		maxBatchTime: maxBatchTime,
		inputChan:    inputChan,
		outBatchChan: outBatchChan,
		stopCh:       make(chan struct{}),
		logger:       logger.WithName("resources-batcher"),
	}
}

// start begins the batching process in a separate goroutine.
func (b *ResourcesBatcher) start() {
	b.wg.Add(1)
	go func() {
		// Ensure WaitGroup is decremented and output channel is closed on exit
		defer func() {
			close(b.outBatchChan) // Close the output channel as we are done producing batches
			b.logger.Info("Closed batcher output channel")
			b.wg.Done()
		}()

		batch := make([]CollectedResource, 0, b.batchSize)
		ticker := time.NewTicker(b.maxBatchTime)
		defer ticker.Stop() // Ensure ticker is stopped eventually

	loop:
		for {
			select {
			case <-b.stopCh:
				b.logger.Info("Stop signal received, entering drain phase")
				break loop // Exit the main select loop to start draining

			case resource, ok := <-b.inputChan:
				if !ok { // Input channel closed
					b.logger.Info("Input channel closed, preparing final batch")
					b.inputChan = nil // Set to nil to disable this case in select
					break loop        // Exit the main select loop to send final batch
				}
				batch = append(batch, resource)
				if len(batch) >= b.batchSize {
					b.logger.Info("Sending batch due to size limit", "batchSize", len(batch))
					b.outBatchChan <- batch

					// increase the batch size cuz last batch size limit was hit, but always cap at DefaultMaxBatchSize
					newBatchSize := int(math.Min(DefaultMaxBatchSize, 1.5*float64(b.batchSize)))
					b.logger.Info("Batch size resizing due to size limit being hit", "old", b.batchSize, "new", newBatchSize)
					b.batchSize = newBatchSize

					batch = make([]CollectedResource, 0, b.batchSize) // Reset batch
					// Reset the timer only when a batch is sent due to size
					// to ensure the time limit applies correctly to the *new* batch.
					ticker.Reset(b.maxBatchTime)
				}

			case <-ticker.C:
				if len(batch) > 0 {
					b.logger.Info("Sending batch due to time limit", "batchSize", len(batch))
					b.outBatchChan <- batch

					// decrease the batch size cuz last batch time limit was hit, but always cap at DefaultBatchSize
					newBatchSize := int(math.Max(DefaultBatchSize, 0.8*float64(b.batchSize)))
					b.logger.Info("Batch size resizing due to time limit being hit", "old", b.batchSize, "new", newBatchSize)
					b.batchSize = newBatchSize

					batch = make([]CollectedResource, 0, b.batchSize) // Reset batch
				}
				// Timer resets automatically after read, no need for explicit Reset here.
			}
		}

		// --- Draining Phase ---
		// This section is reached when stopCh is closed or inputChan is closed.

		// Drain any remaining items from inputChan *if* it wasn't the reason we exited the loop.
		// If b.inputChan is nil, it means it was detected as closed in the select loop.
		if b.inputChan != nil {
			b.logger.Info("Draining remaining items from input channel...")
			// Stop the timer explicitly if we exited due to stopCh, as we no longer need it.
			ticker.Stop()
			for resource := range b.inputChan { // Reads until channel is closed and empty
				batch = append(batch, resource)
				if len(batch) >= b.batchSize {
					b.logger.Info("Sending batch due to size limit during drain", "batchSize", len(batch))
					b.outBatchChan <- batch
					batch = make([]CollectedResource, 0, b.batchSize) // Reset batch
				}
			}
			b.logger.Info("Input channel drained")
		}

		// Send any final partial batch that might remain
		if len(batch) > 0 {
			b.logger.Info("Sending final batch", "batchSize", len(batch))
			b.outBatchChan <- batch
		}

		b.logger.Info("Batching goroutine finished")
	}()
}

// stop signals the batching goroutine to stop and waits for it to finish.
func (b *ResourcesBatcher) stop() {
	b.logger.Info("Stopping resources batcher")
	// Ensure stopCh is closed only once.
	select {
	case <-b.stopCh:
		// Already closed.
		b.logger.Info("Stop channel was already closed")
	default:
		// Close the channel to signal the goroutine.
		close(b.stopCh)
		b.logger.Info("Closed stop channel")
	}
	b.wg.Wait() // Wait for the goroutine to complete its draining and exit.
	b.logger.Info("Resources batcher stopped")
}
