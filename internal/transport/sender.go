// internal/transport/sender.go
package transport

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
)

// DirectPulseSender sends resources directly to Pulse without buffering
type DirectPulseSender struct {
	pulseClient PulseClient
	logger      logr.Logger
}

// NewDirectPulseSender creates a new direct sender for Pulse
func NewDirectPulseSender(pulseClient PulseClient, logger logr.Logger) Sender {
	return &DirectPulseSender{
		pulseClient: pulseClient,
		logger:      logger.WithName("direct-pulse-sender"),
	}
}

// Send transmits a resource directly to Pulse
func (s *DirectPulseSender) Send(ctx context.Context, resourceType string, data interface{}) error {
	return s.pulseClient.SendResource(ctx, resourceType, data)
}

// Start initializes the sender (no-op for direct sender)
func (s *DirectPulseSender) Start(ctx context.Context) error {
	s.logger.Info("Direct pulse sender started")
	return nil
}

// Stop cleans up resources (no-op for direct sender)
func (s *DirectPulseSender) Stop() error {
	s.logger.Info("Direct pulse sender stopped")
	return nil
}

// BufferedItem represents an item in the buffer
type BufferedItem struct {
	ResourceType string
	Data         interface{}
	Attempts     int
	NextTry      time.Time
}

// SimplePulseClient is a placeholder implementation of PulseClient
type SimplePulseClient struct {
	logger logr.Logger
}

// NewSimplePulseClient creates a new simple Pulse client for development/testing
func NewSimplePulseClient(logger logr.Logger) PulseClient {
	return &SimplePulseClient{
		logger: logger.WithName("simple-pulse-client"),
	}
}

// SendResource logs the resource data (for development/testing)
func (c *SimplePulseClient) SendResource(ctx context.Context, resourceType string, data interface{}) error {
	// For now, just log that we would send something
	c.logger.Info("Would send resource to Pulse",
		"resourceType", resourceType,
		"dataType", fmt.Sprintf("%T", data))
	return nil
}
