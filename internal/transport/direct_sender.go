package transport

import (
	"context"
	"time"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	"github.com/devzero-inc/zxporter/internal/collector"
	"github.com/go-logr/logr"
)

// directSenderImpl implements the DirectSender interface, sending data directly without buffering.
// It is unexported as users should interact via the DirectSender interface.
type directSenderImpl struct {
	dakrClient DakrClient
	logger     logr.Logger
}

// NewDirectSender creates a new DirectSender.
// It expects a non-nil DakrClient.
func NewDirectSender(dakrClient DakrClient, logger logr.Logger) DirectSender {
	return &directSenderImpl{
		dakrClient: dakrClient,
		logger:     logger.WithName("direct-sender"),
	}
}

// SendBatch transmits a batch of resources of the same type directly using the DakrClient.
func (s *directSenderImpl) SendBatch(ctx context.Context, resources []collector.CollectedResource, resourceType collector.ResourceType) (string, error) {
	s.logger.V(1).Info("Sending resource batch directly", "type", resourceType, "count", len(resources))

	clusterID, err := s.dakrClient.SendResourceBatch(ctx, resources, resourceType)
	if err != nil {
		s.logger.Error(err, "Failed to send resource batch directly", "type", resourceType)
		return clusterID, err
	}
	return clusterID, nil
}

// Send transmits a single resource directly using the DakrClient.
func (s *directSenderImpl) Send(ctx context.Context, resource collector.CollectedResource) (string, error) {
	s.logger.V(1).Info("Sending single resource directly", "type", resource.ResourceType, "key", resource.Key)

	clusterID, err := s.dakrClient.SendResource(ctx, resource)
	if err != nil {
		s.logger.Error(err, "Failed to send single resource directly", "type", resource.ResourceType, "key", resource.Key)
		return clusterID, err
	}
	return clusterID, nil
}

// Foword the snapshot data to stream to the control plane
func (s *directSenderImpl) SendClusterSnapshotStream(ctx context.Context, snapshot *gen.ClusterSnapshot, snapshotID string, timestamp time.Time) (string, *gen.ClusterSnapshot, error) {
	s.logger.V(1).Info("Sending cluster snapshot via streaming", "snapshotId", snapshotID)

	clusterID, missingResources, err := s.dakrClient.SendClusterSnapshotStream(ctx, snapshot, snapshotID, timestamp)
	if err != nil {
		s.logger.Error(err, "Failed to send cluster snapshot via streaming", "snapshotId", snapshotID)
		return clusterID, nil, err
	}
	return clusterID, missingResources, nil
}

// SendTelemetryLogs forwards a telemetry log request directly using the DakrClient.
func (s *directSenderImpl) SendTelemetryLogs(ctx context.Context, in *gen.SendTelemetryLogsRequest) (*gen.SendTelemetryLogsResponse, error) {
	s.logger.V(1).Info("Sending telemetry logs directly", "count", len(in.Logs))
	resp, err := s.dakrClient.SendTelemetryLogs(ctx, in)
	if err != nil {
		s.logger.Error(err, "Failed to send telemetry logs directly")
		return nil, err
	}
	return resp, nil
}
