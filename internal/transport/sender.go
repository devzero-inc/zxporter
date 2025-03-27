// internal/transport/sender.go
package transport

import (
	"context"
	"fmt"

	"github.com/devzero-inc/zxporter/internal/collector"
	"github.com/go-logr/logr"
)

// DirectPulseSender sends resources directly to Pulse without buffering
type DirectPulseSender struct {
	pulseClient PulseClient
	logger      logr.Logger
}

func NewDirectPulseSender(pulseClient PulseClient, logger logr.Logger) Sender {
	if pulseClient == nil {
		// Create a simple pulse client if none is provided
		pulseClient = &SimplePulseClient{
			logger: logger.WithName("default-pulse-client"),
		}
	}

	return &DirectPulseSender{
		pulseClient: pulseClient,
		logger:      logger.WithName("direct-pulse-sender"),
	}
}

// Send transmits a resource directly to Pulse
func (s *DirectPulseSender) Send(ctx context.Context, resource collector.CollectedResource) error {
	if s.pulseClient == nil {
		return fmt.Errorf("pulse client is nil, cannot send resource")
	}
	return s.pulseClient.SendResource(ctx, resource)
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
func (c *SimplePulseClient) SendResource(ctx context.Context, resource collector.CollectedResource) error {
	// For now, just log that we would send something
	c.logger.Info("Would send resource to Pulse",
		"resourceType", resource.ResourceType,
		"dataType", fmt.Sprintf("%T", resource.Object),
		"data", resource.Object)
	return nil
}
