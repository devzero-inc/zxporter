// internal/transport/sender.go
package transport

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"connectrpc.com/connect"
	genConnect "github.com/devzero-inc/zxporter/gen/api/v1/apiv1connect"
	"github.com/devzero-inc/zxporter/internal/collector"
	"github.com/go-logr/logr"
)

// DirectPulseSender sends resources directly to Pulse without buffering
type DirectPulseSender struct {
	pulseClient PulseClient
	logger      logr.Logger
}

// // NewDirectPulseSender creates a new direct sender for Pulse
// func NewDirectPulseSender(pulseClient PulseClient, logger logr.Logger) Sender {
// 	return &DirectPulseSender{
// 		pulseClient: pulseClient,
// 		logger:      logger.WithName("direct-pulse-sender"),
// 	}
// }

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
func (c *SimplePulseClient) SendResource(ctx context.Context, resource collector.CollectedResource) error {
	// For now, just log that we would send something
	c.logger.Info("Would send resource to Pulse",
		"resourceType", resource.ResourceType,
		"dataType", fmt.Sprintf("%T", resource.Object),
		"data", resource.Object)
	return nil
}

// Helper function to get client and context
func getClientAndContext() (genConnect.K8SServiceClient, context.Context, context.CancelFunc) {
	client := genConnect.NewK8SServiceClient(
		http.DefaultClient,
		"http://localhost:9990", // TODO: this needs to come from config
		connect.WithGRPC(),
	)

	// Create timeout context
	timeout := time.Duration(10) * time.Second // TODO: some reasonable timeout
	ctx, cancel := context.WithTimeout(context.Background(), timeout)

	return client, ctx, cancel
}
