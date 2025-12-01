// internal/transport/sender.go
package transport

import (
	"context"
	"fmt"
	"time"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	"github.com/devzero-inc/zxporter/internal/collector"
	"github.com/go-logr/logr"
	dto "github.com/prometheus/client_model/go"
)

// DirectDakrSender sends resources directly to Dakr without buffering
type DirectDakrSender struct {
	dakrClient DakrClient
	logger     logr.Logger
	clusterID  string
	teamID     string
}

func (s *DirectDakrSender) SetClusterID(clusterID string) {
	s.clusterID = clusterID
}

func (s *DirectDakrSender) SetTeamID(teamID string) {
	s.teamID = teamID
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
	ctxWithTeam := context.WithValue(ctxWithCluster, "team_id", s.teamID)
	clusterID, err := s.dakrClient.SendResource(ctxWithTeam, resource)
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

// SendBatch transmits a batch of resources of the same type to Dakr
func (s *DirectDakrSender) SendBatch(ctx context.Context, resources []collector.CollectedResource, resourceType collector.ResourceType) (string, error) {
	if s.dakrClient == nil {
		return "", fmt.Errorf("dakr client is nil, cannot send resource batch")
	}

	ctxWithCluster := context.WithValue(ctx, "cluster_id", s.clusterID)
	ctxWithTeam := context.WithValue(ctxWithCluster, "team_id", s.teamID)

	clusterID, err := s.dakrClient.SendResourceBatch(ctxWithTeam, resources, resourceType)
	if clusterID != "" {
		s.SetClusterID(clusterID)
	}

	return clusterID, err
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
		"FULL_RESOURCE", resource)
	return "", nil
}

// SendResourceBatch logs the batch resource data (for development/testing)
func (c *SimpleDakrClient) SendResourceBatch(ctx context.Context, resources []collector.CollectedResource, resourceType collector.ResourceType) (string, error) {
	// For now, just log that we would send something
	c.logger.Info("Would send resource batch to Dakr",
		"resourceType", resourceType,
		"count", len(resources))
	return "", nil
}

// SendTelemetryMetrics logs the telemetry metrics data (for development/testing)
func (c *SimpleDakrClient) SendTelemetryMetrics(ctx context.Context, metrics []*dto.MetricFamily) (int32, error) {
	// For now, just log that we would send something
	c.logger.Info("Would send telemetry metrics to Dakr", "count", len(metrics))

	// Log some details about the metrics
	for i, metric := range metrics {
		if i >= 5 { // Only log the first 5 metrics to avoid flooding logs
			c.logger.Info("... and more metrics")
			break
		}

		c.logger.Info("Metric details", "metric", metric)
	}

	return int32(len(metrics)), nil
}

// Update sender.go with fallback logic
func (s *DirectDakrSender) SendClusterSnapshotStream(ctx context.Context, snapshot *gen.ClusterSnapshot, snapshotID string, timestamp time.Time) (string, *gen.ClusterSnapshot, error) {
	if s.dakrClient == nil {
		return "", nil, fmt.Errorf("dakr client is nil, cannot send cluster snapshot stream")
	}

	ctxWithCluster := context.WithValue(ctx, "cluster_id", s.clusterID)
	ctxWithTeam := context.WithValue(ctxWithCluster, "team_id", s.teamID) // Assuming you add teamID field

	clusterID, missingResources, err := s.dakrClient.SendClusterSnapshotStream(ctxWithTeam, snapshot, snapshotID, timestamp)
	if clusterID != "" {
		s.SetClusterID(clusterID)
	}

	return clusterID, missingResources, err
}

// Update sender.go with fallback logic
func (c *SimpleDakrClient) SendClusterSnapshotStream(ctx context.Context, snapshot *gen.ClusterSnapshot, snapshotID string, timestamp time.Time) (string, *gen.ClusterSnapshot, error) {
	// For now, just log that we would send something
	c.logger.Info("Would send cluster snapshot to Dakr",
		"snapshotId", snapshotID,
		"timestamp", timestamp,
		"dataType", fmt.Sprintf("%T", snapshot))
	return "", nil, nil
}

// SendTelemetryLogs implements SimpleDakrClient.
func (c *SimpleDakrClient) SendTelemetryLogs(ctx context.Context, in *gen.SendTelemetryLogsRequest) (*gen.SendTelemetryLogsResponse, error) {
	//
	return nil, nil
}

// ExchangePATForClusterToken implements SimpleDakrClient.
func (c *SimpleDakrClient) ExchangePATForClusterToken(ctx context.Context, patToken, clusterName, k8sProvider string) (string, string, error) {
	c.logger.Info("Would exchange PAT token for cluster token",
		"clusterName", clusterName,
		"k8sProvider", k8sProvider)
	// Return empty values for simple client
	return "", "", fmt.Errorf("PAT token exchange not supported in simple client")
}
