// internal/transport/buffered_sender.go
package transport

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/devzero-inc/zxporter/internal/collector"
	"github.com/go-logr/logr"
)

// BufferedSender is a sender that buffers resources when the destination is unavailable
type BufferedSender struct {
	pulseClient       PulseClient
	logger            logr.Logger
	buffer            []BufferedItem
	mu                sync.Mutex
	stopCh            chan struct{}
	retryInterval     time.Duration
	maxBufferSize     int
	maxRetryAttempts  int
	baseRetryDelay    time.Duration
	maxRetryDelay     time.Duration
	processWorkerDone chan struct{}
	started           bool
}

// BufferedItem represents an item in the buffer
type BufferedItem struct {
	Resource  collector.CollectedResource
	Attempts  int
	NextRetry time.Time
	FirstTry  time.Time
}

// BufferedSenderOptions configures the buffered sender
type BufferedSenderOptions struct {
	MaxBufferSize    int           // Maximum number of items in buffer
	RetryInterval    time.Duration // How often to check for retries
	MaxRetryAttempts int           // Maximum number of retry attempts per item
	BaseRetryDelay   time.Duration // Initial retry delay
	MaxRetryDelay    time.Duration // Maximum retry delay with backoff
}

// DefaultBufferedSenderOptions returns sensible defaults
func DefaultBufferedSenderOptions() BufferedSenderOptions {
	// TODO: this might need to put on config and it should hold atleast 1 day old data
	return BufferedSenderOptions{
		MaxBufferSize:    100000,
		RetryInterval:    1 * time.Minute,
		MaxRetryAttempts: 12,
		BaseRetryDelay:   1 * time.Minute,
		MaxRetryDelay:    6 * time.Hour,
	}
}

// NewBufferedSender creates a new buffered sender with the provided options
func NewBufferedSender(pulseClient PulseClient, logger logr.Logger, options BufferedSenderOptions) Sender {
	return &BufferedSender{
		pulseClient:       pulseClient,
		logger:            logger.WithName("buffered-sender"),
		buffer:            make([]BufferedItem, 0, options.MaxBufferSize),
		stopCh:            make(chan struct{}),
		processWorkerDone: make(chan struct{}),
		retryInterval:     options.RetryInterval,
		maxBufferSize:     options.MaxBufferSize,
		maxRetryAttempts:  options.MaxRetryAttempts,
		baseRetryDelay:    options.BaseRetryDelay,
		maxRetryDelay:     options.MaxRetryDelay,
	}
}

// Start initializes the sender and starts the background processing
func (s *BufferedSender) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		return nil
	}

	s.logger.Info("Starting buffered sender",
		"maxBufferSize", s.maxBufferSize,
		"retryInterval", s.retryInterval,
		"maxRetryAttempts", s.maxRetryAttempts)

	// Start the background process to retry sending buffered items
	go s.processBuffer(ctx)

	s.started = true
	return nil
}

// Stop terminates the sender gracefully
func (s *BufferedSender) Stop() error {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	s.logger.Info("Stopping buffered sender", "bufferedItems", s.GetBufferSize())

	// Signal the processor to stop
	close(s.stopCh)

	// Wait for the processor to finish
	<-s.processWorkerDone

	s.mu.Lock()
	s.started = false
	s.mu.Unlock()

	return nil
}

// Send attempts to send a resource, buffering it if the send fails
func (s *BufferedSender) Send(ctx context.Context, resource collector.CollectedResource) error {
	// First try to send directly
	err := s.pulseClient.SendResource(ctx, resource)
	if err == nil {
		return nil
	}

	// If sending failed, add to buffer
	s.mu.Lock()
	defer s.mu.Unlock()

	// Check buffer capacity
	if len(s.buffer) >= s.maxBufferSize {
		// Buffer is full, log warning and discard oldest item
		s.logger.Info("Buffer full, discarding oldest item",
			"bufferSize", len(s.buffer),
			"maxBufferSize", s.maxBufferSize)

		// Make room by removing the oldest item
		s.buffer = s.buffer[1:]
	}

	// Add the new item to the buffer
	s.buffer = append(s.buffer, BufferedItem{
		Resource:  resource,
		Attempts:  1, // Count the initial attempt
		NextRetry: time.Now().Add(s.calculateBackoff(1)),
		FirstTry:  time.Now(),
	})

	s.logger.V(4).Info("Added resource to buffer",
		"resourceType", resource.ResourceType,
		"key", resource.Key,
		"bufferSize", len(s.buffer))

	return fmt.Errorf("failed to send immediately, added to retry buffer: %w", err)
}

// GetBufferSize returns the current number of items in the buffer
func (s *BufferedSender) GetBufferSize() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.buffer)
}

// Flush attempts to immediately send all buffered items
func (s *BufferedSender) Flush(ctx context.Context) error {
	s.mu.Lock()
	if len(s.buffer) == 0 {
		s.mu.Unlock()
		return nil
	}

	// Take a snapshot of the buffer to work with
	itemsToFlush := make([]BufferedItem, len(s.buffer))
	copy(itemsToFlush, s.buffer)
	s.mu.Unlock()

	s.logger.Info("Flushing buffered items", "count", len(itemsToFlush))

	var lastError error
	successCount := 0

	// Try to send each item
	for _, item := range itemsToFlush {
		if err := s.pulseClient.SendResource(ctx, item.Resource); err != nil {
			lastError = err
			s.logger.V(4).Info("Failed to flush item",
				"resourceType", item.Resource.ResourceType,
				"key", item.Resource.Key,
				"error", err)
		} else {
			successCount++

			// Remove from buffer if successful
			s.mu.Lock()
			for j, bufItem := range s.buffer {
				if bufferItemsEqual(bufItem.Resource, item.Resource) {
					// Remove this item from the buffer
					s.buffer = append(s.buffer[:j], s.buffer[j+1:]...)
					break
				}
			}
			s.mu.Unlock()
		}

		// Check if context is done after each item
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			// Continue processing
		}
	}

	s.logger.Info("Flush complete",
		"total", len(itemsToFlush),
		"successful", successCount,
		"failed", len(itemsToFlush)-successCount,
		"remainingInBuffer", s.GetBufferSize())

	if lastError != nil {
		return fmt.Errorf("some items failed to flush: %w", lastError)
	}
	return nil
}

// processBuffer is a background worker that processes the buffer periodically
func (s *BufferedSender) processBuffer(ctx context.Context) {
	ticker := time.NewTicker(s.retryInterval)
	defer func() {
		ticker.Stop()
		close(s.processWorkerDone)
	}()

	for {
		select {
		case <-s.stopCh:
			s.logger.Info("Buffer processor stopping, attempting final flush")
			// Try a final flush with a new context
			flushCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			s.Flush(flushCtx)
			cancel()
			return
		case <-ctx.Done():
			s.logger.Info("Context done, stopping buffer processor")
			return
		case <-ticker.C:
			s.processBufferedItems(ctx)
		}
	}
}

// processBufferedItems attempts to send items that are due for retry
func (s *BufferedSender) processBufferedItems(ctx context.Context) {
	s.mu.Lock()
	if len(s.buffer) == 0 {
		s.mu.Unlock()
		return
	}

	now := time.Now()
	var itemsToRetry []BufferedItem
	var itemsToRemove []int

	// Identify items due for retry
	for i, item := range s.buffer {
		if now.After(item.NextRetry) {
			itemsToRetry = append(itemsToRetry, item)
			itemsToRemove = append(itemsToRemove, i)
		}
	}

	// Remove items from buffer that we'll retry (we'll add back the ones that fail)
	for i := len(itemsToRemove) - 1; i >= 0; i-- {
		idx := itemsToRemove[i]
		s.buffer = append(s.buffer[:idx], s.buffer[idx+1:]...)
	}
	s.mu.Unlock()

	if len(itemsToRetry) == 0 {
		return
	}

	s.logger.V(4).Info("Processing buffered items", "count", len(itemsToRetry), "total", s.GetBufferSize())

	// Try to send each item
	for _, item := range itemsToRetry {
		// Check if max attempts reached
		if item.Attempts >= s.maxRetryAttempts {
			s.logger.Info("Discarding item after max retry attempts",
				"resourceType", item.Resource.ResourceType,
				"key", item.Resource.Key,
				"attempts", item.Attempts,
				"maxAttempts", s.maxRetryAttempts,
				"age", time.Since(item.FirstTry))
			continue
		}

		// Try to send
		err := s.pulseClient.SendResource(ctx, item.Resource)
		if err != nil {
			// Failed, increment attempts and calculate next retry time
			item.Attempts++
			item.NextRetry = time.Now().Add(s.calculateBackoff(item.Attempts))

			// Add back to buffer
			s.mu.Lock()
			if len(s.buffer) < s.maxBufferSize {
				s.buffer = append(s.buffer, item)
			} else {
				s.logger.Info("Buffer full during retry, discarding retry item",
					"resourceType", item.Resource.ResourceType,
					"key", item.Resource.Key)
			}
			s.mu.Unlock()

			s.logger.V(4).Info("Retry failed, rescheduled",
				"resourceType", item.Resource.ResourceType,
				"key", item.Resource.Key,
				"attempts", item.Attempts,
				"nextRetry", item.NextRetry)
		} else {
			s.logger.V(4).Info("Retry succeeded",
				"resourceType", item.Resource.ResourceType,
				"key", item.Resource.Key,
				"attempts", item.Attempts)
		}
	}
}

// calculateBackoff determines the delay before the next retry
func (s *BufferedSender) calculateBackoff(attempt int) time.Duration {
	// Exponential backoff: baseDelay * 2^(attempt-1), capped at maxDelay
	// 1st retry: baseDelay
	// 2nd retry: baseDelay * 2
	// 3rd retry: baseDelay * 4
	// ...
	multiplier := 1 << uint(attempt-1)
	delay := s.baseRetryDelay * time.Duration(multiplier)

	if delay > s.maxRetryDelay {
		delay = s.maxRetryDelay
	}

	// Add a small random jitter (±10%) to avoid thundering herd
	jitter := int64(float64(delay) * (0.9 + 0.2*float64(time.Now().Nanosecond())/float64(1000000000)))
	return time.Duration(jitter)
}

// Helper function to compare resources for equality
func bufferItemsEqual(a, b collector.CollectedResource) bool {
	return a.ResourceType == b.ResourceType && a.Key == b.Key && a.EventType == b.EventType
}
