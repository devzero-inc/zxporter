// internal/transport/interface.go
package transport

import (
	"context"
	"time"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	"github.com/devzero-inc/zxporter/internal/collector"
	telemetry_logger "github.com/devzero-inc/zxporter/internal/logger"
	dto "github.com/prometheus/client_model/go"
)

// DakrClient defines methods for sending data to Dakr
type DakrClient interface {
	// SendResource sends any resource data to Dakr
	SendResource(ctx context.Context, resource collector.CollectedResource) (string, error)
	// SendResourceBatch sends a batch of resources of the same type to Dakr
	SendResourceBatch(ctx context.Context, resources []collector.CollectedResource, resourceType collector.ResourceType) (string, error)
	// SendTelemetryMetrics sends telemetry metrics to Dakr
	SendTelemetryMetrics(ctx context.Context, metrics []*dto.MetricFamily) (int32, error)
	// SendClusterSnapshotStream sends cluster snapshot data via streaming for large payloads
	SendClusterSnapshotStream(ctx context.Context, snapshot *gen.ClusterSnapshot, snapshotID string, timestamp time.Time) (string, *gen.ClusterSnapshot, error)
	// telemetry_logger.TelemetryLogSender sends a batch of log entries to Dakr
	telemetry_logger.TelemetryLogSender
}

// Sender defines methods for sending data to external systems
type Sender interface {
	// Send transmits a resource to the target system
	Send(ctx context.Context, resource collector.CollectedResource) (string, error)

	// Start initializes the sender (establishing connections, etc.)
	Start(ctx context.Context) error

	// Stop cleans up resources
	Stop() error
}

// DirectSender defines methods for sending data to external systems directly
type DirectSender interface {
	// SendBatch transmits a batch of resources of the same type to the target system
	SendBatch(ctx context.Context, resource []collector.CollectedResource, resourceType collector.ResourceType) (string, error)

	// Send transmits a resource to the target system
	Send(ctx context.Context, resource collector.CollectedResource) (string, error)

	// SendClusterSnapshotStream sends large cluster snapshot data using streaming
	SendClusterSnapshotStream(ctx context.Context, snapshot *gen.ClusterSnapshot, snapshotID string, timestamp time.Time) (string, *gen.ClusterSnapshot, error)

	// telemetry_logger.TelemetryLogSender sends a batch of log entries.
	telemetry_logger.TelemetryLogSender
}

// // BufferedSender adds buffering capabilities to handle connection issues
// type BufferedSender interface {
// 	Sender

// 	// GetBufferSize returns the current number of items in the buffer
// 	GetBufferSize() int

// 	// Flush attempts to send all buffered items
// 	Flush(ctx context.Context) error
// }
