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
	SendResourceBatch(
		ctx context.Context,
		resources []collector.CollectedResource,
		resourceType collector.ResourceType,
	) (string, error)
	// SendTelemetryMetrics sends telemetry metrics to Dakr
	SendTelemetryMetrics(ctx context.Context, metrics []*dto.MetricFamily) (int32, error)
	// SendClusterSnapshotStream sends cluster snapshot data via streaming for large payloads
	SendClusterSnapshotStream(
		ctx context.Context,
		snapshot *gen.ClusterSnapshot,
		snapshotID string,
		timestamp time.Time,
	) (string, *gen.ClusterSnapshot, error)
	// OpenClusterSnapshotBatchStream opens a per-resource-type batched
	// snapshot stream (memory-bounded successor of SendClusterSnapshotStream).
	OpenClusterSnapshotBatchStream(
		ctx context.Context,
		snapshotID string,
		timestamp time.Time,
		isFullSnapshot bool,
	) (SnapshotBatchStream, error)
	// telemetry_logger.TelemetryLogSender sends a batch of log entries to Dakr
	telemetry_logger.TelemetryLogSender
	// ExchangePATForClusterToken exchanges a personal access token for a
	// cluster token. KubeSystemUID and ClusterID let dakr re-attach to an
	// existing cluster instead of creating one (KUB-158).
	ExchangePATForClusterToken(ctx context.Context, req ClusterTokenExchangeRequest) (ClusterTokenExchangeResult, error)

	// SendNetworkTrafficMetrics pushes network traffic metrics from a node
	SendNetworkTrafficMetrics(
		ctx context.Context,
		req *gen.SendNetworkTrafficMetricsRequest,
	) (*gen.SendNetworkTrafficMetricsResponse, error)

	// SendScalerMetrics pushes a batch of KEDA scaler metric samples from a cluster.
	SendScalerMetrics(
		ctx context.Context,
		clusterID string,
		teamID string,
		samples []*gen.ScalerMetricSample,
	) error

	// SendContainerRedMetrics pushes per-container inbound RED metrics from a node.
	// It is a separate call from SendNetworkTrafficMetrics rather than a field on it
	// because the two carry different grains -- flows between endpoints versus
	// per-container per-minute aggregates -- and because a failure to deliver one
	// must not discard the other.
	SendContainerRedMetrics(
		ctx context.Context,
		req *gen.SendContainerRedMetricsRequest,
	) (*gen.SendContainerRedMetricsResponse, error)

	// ReportHealth
	ReportHealth(ctx context.Context, req *gen.ReportHealthRequest) error

	// NewPercentileFetcher creates a PercentileFetcher that retrieves
	// pre-computed workload percentiles from the DAKR control plane.
	NewPercentileFetcher() *DakrPercentileFetcher
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
	SendBatch(
		ctx context.Context,
		resource []collector.CollectedResource,
		resourceType collector.ResourceType,
	) (string, error)

	// Send transmits a resource to the target system
	Send(ctx context.Context, resource collector.CollectedResource) (string, error)

	// SendClusterSnapshotStream sends large cluster snapshot data using streaming
	SendClusterSnapshotStream(
		ctx context.Context,
		snapshot *gen.ClusterSnapshot,
		snapshotID string,
		timestamp time.Time,
	) (string, *gen.ClusterSnapshot, error)

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

// ClusterTokenExchangeRequest is the input to a PAT-for-cluster-token exchange.
type ClusterTokenExchangeRequest struct {
	PATToken      string
	ClusterName   string
	K8sProvider   string
	KubeSystemUID string
	ClusterID     string
}

// ClusterTokenExchangeResult is the cluster token dakr issued and the cluster it belongs to.
type ClusterTokenExchangeResult struct {
	Token      string
	ClusterID  string
	Reattached bool
}
