// internal/transport/dakr_client.go
package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"github.com/devzero-inc/zxporter/internal/collector"
	"github.com/go-logr/logr"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	genconnect "github.com/devzero-inc/zxporter/gen/api/v1/apiv1connect"
	dto "github.com/prometheus/client_model/go"
)

// RetryPolicy defines the parameters for retrying.
type RetryPolicy struct {
	MaxAttempts    int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	IsRetryable    func(err error) bool
}

// RealDakrClient implements communication with Dakr service
type RealDakrClient struct {
	logger       logr.Logger
	client       genconnect.MetricsCollectorServiceClient
	clusterToken string
}

// NewDakrClient creates a new client for Dakr service
func NewDakrClient(dakrBaseURL string, clusterToken string, logger logr.Logger) DakrClient {
	// Define the retry policy
	retryPolicy := RetryPolicy{
		MaxAttempts:    3,
		InitialBackoff: 250 * time.Millisecond,
		MaxBackoff:     3 * time.Second,
		IsRetryable: func(err error) bool {
			code := connect.CodeOf(err)
			return code == connect.CodeUnavailable ||
				code == connect.CodeDeadlineExceeded ||
				code == connect.CodeResourceExhausted ||
				code == connect.CodeUnknown ||
				code == connect.CodeCanceled ||
				code == connect.CodeAborted
		},
	}

	// Create the retry interceptor directly using connect.UnaryInterceptorFunc
	retryLog := logger.WithName("retry-interceptor")
	retryInterceptor := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return connect.UnaryFunc(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			var lastErr error
			for currentAttempt := 0; currentAttempt < retryPolicy.MaxAttempts; currentAttempt++ {
				if currentAttempt > 0 { // Backoff only for retries
					multiplier := time.Duration(1 << (currentAttempt - 1))
					backoffDuration := retryPolicy.InitialBackoff * multiplier
					if backoffDuration > retryPolicy.MaxBackoff {
						backoffDuration = retryPolicy.MaxBackoff
					}
					retryLog.V(1).Info("Retrying request",
						"attempt", currentAttempt,
						"procedure", req.Spec().Procedure,
						"delay", backoffDuration.String(),
						"error", lastErr)
					select {
					case <-time.After(backoffDuration):
					case <-ctx.Done():
						retryLog.Info("Context cancelled during backoff", "procedure", req.Spec().Procedure, "error", ctx.Err())
						return nil, ctx.Err()
					}
				}

				resp, err := next(ctx, req)
				if err == nil {
					return resp, nil
				}
				lastErr = err

				if retryPolicy.IsRetryable != nil && retryPolicy.IsRetryable(err) {
					if currentAttempt == retryPolicy.MaxAttempts-1 { // Last attempt, don't retry further
						retryLog.Info("Max retry attempts reached",
							"procedure", req.Spec().Procedure,
							"attempts", retryPolicy.MaxAttempts,
							"error", lastErr)
						break
					}
					continue // Continue to next attempt in the loop
				} else { // Non-retryable error
					retryLog.V(1).Info("Non-retryable error or IsRetryable not defined",
						"procedure", req.Spec().Procedure,
						"error", lastErr)
					break // Exit loop for non-retryable errors
				}
			}
			return nil, lastErr // Return the last error encountered
		})
	})

	// Create custom HTTP client with 30-second timeout and keepalive
	httpClient := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
			DisableKeepAlives:   false,
			DisableCompression:  false,
		},
	}

	// We're using the connect client for the Dakr service
	client := genconnect.NewMetricsCollectorServiceClient(
		httpClient,
		dakrBaseURL,
		connect.WithGRPC(),
		connect.WithInterceptors(retryInterceptor),
		connect.WithSendMaxBytes(1024*1024*16), // 16MB max send size
		connect.WithReadMaxBytes(1024*1024*16), // 16MB max read size
	)

	return &RealDakrClient{
		logger:       logger.WithName("dakr-client"),
		client:       client,
		clusterToken: clusterToken,
	}
}

// SendResource sends the resource to Dakr through gRPC
func (c *RealDakrClient) SendResource(ctx context.Context, resource collector.CollectedResource) (string, error) {
	c.logger.Info("Sending resource to Dakr",
		"type", resource.ResourceType,
		"key", resource.Key,
		"event", resource.EventType)

	// Convert resource.Object to a protobuf struct
	var dataStruct *structpb.Struct
	var err error

	// First try to convert directly
	dataStruct, err = structpb.NewStruct(map[string]interface{}{})
	if err != nil {
		c.logger.Error(err, "Failed to create empty struct")
		return "", err
	}

	// Marshal the object to JSON then unmarshal to the struct
	jsonBytes, err := json.Marshal(resource.Object)
	if err != nil {
		c.logger.Error(err, "Failed to marshal resource data to JSON")
		return "", fmt.Errorf("failed to marshal resource data: %w", err)
	}

	err = dataStruct.UnmarshalJSON(jsonBytes)
	if err != nil {
		c.logger.Error(err, "Failed to unmarshal JSON to protobuf struct")
		return "", fmt.Errorf("failed to convert resource data to protobuf struct: %w", err)
	}

	// Create the request
	res := &gen.SendResourceRequest{
		ResourceType: resource.ResourceType.ProtoType(),
		Key:          resource.Key,
		Timestamp:    timestamppb.New(resource.Timestamp),
		EventType:    resource.EventType.ProtoType(),
		Data:         dataStruct,
	}

	req := connect.NewRequest(res)
	attachClusterToken(req, c.clusterToken)

	if ctx.Value("cluster_id") != nil {
		clusterString, _ := ctx.Value("cluster_id").(string)
		req.Header().Set("cluster_id", clusterString)
	}

	// Send to Dakr
	resp, err := c.client.SendResource(ctx, req)
	if err != nil {
		c.logger.Error(err, "Failed to send resource to Dakr")
		return "", fmt.Errorf("failed to send resource to Dakr: %w", err)
	}

	return resp.Msg.ClusterIdentifier, nil
}

// SendResourceBatch sends a batch of resources to Dakr through gRPC
func (c *RealDakrClient) SendResourceBatch(ctx context.Context, resources []collector.CollectedResource, resourceType collector.ResourceType) (string, error) {
	c.logger.Info("Sending resource batch to Dakr",
		"type", resourceType,
		"count", len(resources))

	resourceItems := make([]*gen.ResourceItem, 0, len(resources))

	for _, resource := range resources {
		// Convert resource.Object to a protobuf struct
		var dataStruct *structpb.Struct
		var err error

		dataStruct, err = structpb.NewStruct(map[string]interface{}{})
		if err != nil {
			c.logger.Error(err, "Failed to create empty struct for batch item", "key", resource.Key)
			// For now, let's skip and log
			continue
		}

		// Marshal the object to JSON then unmarshal to the struct
		jsonBytes, err := json.Marshal(resource.Object)
		if err != nil {
			c.logger.Error(err, "Failed to marshal resource data to JSON for batch item", "key", resource.Key)
			// Skip this item
			continue
		}

		err = dataStruct.UnmarshalJSON(jsonBytes)
		if err != nil {
			c.logger.Error(err, "Failed to unmarshal JSON to protobuf struct for batch item", "key", resource.Key)
			// Skip this item
			continue
		}

		// Create the resource item
		item := &gen.ResourceItem{
			Key:          resource.Key,
			Timestamp:    timestamppb.New(resource.Timestamp),
			EventType:    resource.EventType.ProtoType(),
			Data:         dataStruct,
			ResourceType: resource.ResourceType.ProtoType(), // Add ResourceType here
		}
		resourceItems = append(resourceItems, item)
	}

	if len(resourceItems) == 0 {
		c.logger.Info("No valid resources to send in the batch", "type", resourceType)
		// Return empty string and nil error, as technically no send operation failed
		// Or potentially return an error indicating an empty batch? Let's return nil for now.
		return "", nil
	}

	// Create the batch request
	batchReq := &gen.SendResourceBatchRequest{
		Resources: resourceItems,
	}

	req := connect.NewRequest(batchReq)
	attachClusterToken(req, c.clusterToken)

	if ctx.Value("cluster_id") != nil {
		clusterString, _ := ctx.Value("cluster_id").(string)
		req.Header().Set("cluster_id", clusterString)
	}

	// Send to Dakr
	resp, err := c.client.SendResourceBatch(ctx, req)
	if err != nil {
		c.logger.Error(err, "Failed to send resource batch to Dakr", "type", resourceType)
		return "", fmt.Errorf("failed to send resource batch to Dakr: %w", err)
	}

	c.logger.Info("Successfully sent resource batch to Dakr", "type", resourceType, "count", len(resourceItems))
	return resp.Msg.ClusterIdentifier, nil
}

// SendTelemetryMetrics sends telemetry metrics to Dakr
func (c *RealDakrClient) SendTelemetryMetrics(ctx context.Context, metrics []*dto.MetricFamily) (int32, error) {
	c.logger.Info("Sending telemetry metrics to Dakr", "count", len(metrics))

	// Create the request
	telemetryReq := &gen.SendTelemetryMetricsRequest{
		MetricFamilies: metrics,
	}

	// Set cluster ID if available in context
	if ctx.Value("cluster_id") != nil {
		clusterString, _ := ctx.Value("cluster_id").(string)
		telemetryReq.ClusterId = clusterString
	}

	req := connect.NewRequest(telemetryReq)
	attachClusterToken(req, c.clusterToken)

	// Send to Dakr
	resp, err := c.client.SendTelemetryMetrics(ctx, req)
	if err != nil {
		c.logger.Error(err, "Failed to send telemetry metrics to Dakr")
		return 0, fmt.Errorf("failed to send telemetry metrics to Dakr: %w", err)
	}

	c.logger.Info("Successfully sent telemetry metrics to Dakr",
		"count", len(metrics),
		"processed", resp.Msg.ProcessedCount)

	return resp.Msg.ProcessedCount, nil
}

// SendClusterSnapshot sends cluster snapshot data to Dakr using the dedicated endpoint
func (c *RealDakrClient) SendClusterSnapshot(ctx context.Context, snapshotData interface{}, snapshotID string, timestamp time.Time) (string, error) {
	c.logger.Info("Sending cluster snapshot to Dakr", "snapshotId", snapshotID)

	// Convert snapshotData to a protobuf struct
	var dataStruct *structpb.Struct
	var err error

	// TODO (parthiba): change this to send a []byte instead -- structs are way too bloated to send over the wire
	dataStruct, err = structpb.NewStruct(map[string]interface{}{})
	if err != nil {
		c.logger.Error(err, "Failed to create empty struct for cluster snapshot")
		return "", err
	}

	// Marshal the snapshot data to JSON then unmarshal to the struct
	jsonBytes, err := json.Marshal(snapshotData)
	if err != nil {
		c.logger.Error(err, "Failed to marshal cluster snapshot data to JSON")
		return "", fmt.Errorf("failed to marshal cluster snapshot data: %w", err)
	}

	err = dataStruct.UnmarshalJSON(jsonBytes)
	if err != nil {
		c.logger.Error(err, "Failed to unmarshal JSON to protobuf struct for cluster snapshot")
		return "", fmt.Errorf("failed to convert cluster snapshot data to protobuf struct: %w", err)
	}

	// Create the cluster snapshot request
	snapshotReq := &gen.SendClusterSnapshotRequest{
		SnapshotData: dataStruct,
		SnapshotId:   snapshotID,
		Timestamp:    timestamppb.New(timestamp),
	}

	// Set cluster ID and team ID if available in context
	if ctx.Value("cluster_id") != nil {
		clusterString, _ := ctx.Value("cluster_id").(string)
		snapshotReq.ClusterId = clusterString
	}
	if ctx.Value("team_id") != nil {
		teamString, _ := ctx.Value("team_id").(string)
		snapshotReq.TeamId = teamString
	}

	req := connect.NewRequest(snapshotReq)
	attachClusterToken(req, c.clusterToken)

	// Send to Dakr
	resp, err := c.client.SendClusterSnapshot(ctx, req)
	if err != nil {
		c.logger.Error(err, "Failed to send cluster snapshot to Dakr")
		return "", fmt.Errorf("failed to send cluster snapshot to Dakr: %w", err)
	}

	c.logger.Info("Successfully sent cluster snapshot to Dakr",
		"snapshotId", snapshotID,
		"status", resp.Msg.Status)

	return resp.Msg.ClusterId, nil
}

func attachClusterToken[T any](req *connect.Request[T], clusterToken string) {
	req.Header().Set("Authorization", fmt.Sprintf("Bearer %s", clusterToken))
}
