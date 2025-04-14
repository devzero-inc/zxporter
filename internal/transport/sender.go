// internal/transport/sender.go
package transport

import (
	"context"
	"fmt"

	"github.com/devzero-inc/zxporter/internal/collector"
	"github.com/go-logr/logr"
)

// DirectDakrSender sends resources directly to Dakr without buffering
type DirectDakrSender struct {
	dakrClient DakrClient
	logger     logr.Logger
	clusterID  string
}

func (s *DirectDakrSender) SetClusterID(clusterID string) {
	s.clusterID = clusterID
}

func NewDirectDakrSender(dakrClient DakrClient, logger logr.Logger) Sender {
	if dakrClient == nil {
		// Create a simple dakr client if none is provided
		dakrClient = &SimpleDakrClient{
			logger: logger.WithName("default-dakr-client"),
		}
	}

	return &DirectDakrSender{
		dakrClient: dakrClient,
		logger:     logger.WithName("direct-dakr-sender"),
	}
}

// Send transmits a resource directly to Dakr
func (s *DirectDakrSender) Send(ctx context.Context, resource collector.CollectedResource) (string, error) {
	if s.dakrClient == nil {
		return "", fmt.Errorf("dakr client is nil, cannot send resource")
	}

	ctxWithCluster := context.WithValue(ctx, "cluster_id", s.clusterID)
	clusterID, err := s.dakrClient.SendResource(ctxWithCluster, resource)
	if clusterID != "" {
		s.SetClusterID(clusterID)
	}

	return clusterID, err
}

// Start initializes the sender (no-op for direct sender)
func (s *DirectDakrSender) Start(ctx context.Context) error {
	s.logger.Info("Direct dakr sender started")
	return nil
}

// Stop cleans up resources (no-op for direct sender)
func (s *DirectDakrSender) Stop() error {
	s.logger.Info("Direct dakr sender stopped")
	return nil
}

// SimpleDakrClient is a placeholder implementation of DakrClient
type SimpleDakrClient struct {
	logger logr.Logger
}

// NewSimpleDakrClient creates a new simple Dakr client for development/testing
func NewSimpleDakrClient(logger logr.Logger) DakrClient {
	return &SimpleDakrClient{
		logger: logger.WithName("simple-dakr-client"),
	}
}

// SendResource logs the resource data (for development/testing)
func (c *SimpleDakrClient) SendResource(ctx context.Context, resource collector.CollectedResource) (string, error) {
	// For now, just log that we would send something
	c.logger.Info("Would send resource to Dakr",
		"resourceType", resource.ResourceType,
		"dataType", fmt.Sprintf("%T", resource.Object),
		"data", resource.Object)
	return "", nil
}
