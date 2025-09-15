// internal/transport/dakr_client.go
package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"github.com/devzero-inc/zxporter/internal/collector"
	"github.com/go-logr/logr"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	genconnect "github.com/devzero-inc/zxporter/gen/api/v1/apiv1connect"
	dto "github.com/prometheus/client_model/go"
)

const (
	// Maximum size per chunk (in bytes) - set conservatively to avoid gRPC limits
	maxChunkSize = 3 * 1024 * 1024 // 3MB per chunk
	// Maximum batch size for regular resource sends (leaving buffer for headers)
	maxBatchSize = 25 * 1024 * 1024 // 25MB max per batch

	// Maximum send batch size for grpc client
	maxSendBatchSize = 30 * 1024 * 1024 // 30MB
	// Maximum read batch size for grpc client
	maxReadBatchSize = 30 * 1024 * 1024 // 30MB
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

	// Create custom HTTP client with improved timeout and connection pool settings
	httpClient := &http.Client{
		Timeout: 120 * time.Second,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 20,
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
		connect.WithSendMaxBytes(maxSendBatchSize),
		connect.WithReadMaxBytes(maxReadBatchSize),
	)

	return &RealDakrClient{
		logger:       logger.WithName("dakr-client"),
		client:       client,
		clusterToken: clusterToken,
	}
}

// SendResource sends the resource to Dakr through gRPC
func (c *RealDakrClient) SendResource(ctx context.Context, resource collector.CollectedResource) (string, error) {
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

// estimateResourceSize estimates the size of a resource item in bytes
func estimateResourceSize(item *gen.ResourceItem) int {
	// Marshal to get accurate size
	data, err := proto.Marshal(item)
	if err != nil {
		// Fallback to rough estimate if marshaling fails
		return 1024 // 1KB default estimate
	}
	return len(data)
}

// splitBatchBySize splits resources into batches that don't exceed maxBatchSize
func (c *RealDakrClient) splitBatchBySize(resourceItems []*gen.ResourceItem) [][]*gen.ResourceItem {
	var batches [][]*gen.ResourceItem
	var currentBatch []*gen.ResourceItem
	currentSize := 0

	for _, item := range resourceItems {
		itemSize := estimateResourceSize(item)

		// If single item exceeds max size, send it alone (will likely fail but log it)
		if itemSize > maxBatchSize {
			c.logger.Error(nil, "Single resource exceeds max batch size",
				"size", itemSize, "maxSize", maxBatchSize, "key", item.Key)
			// Add current batch if not empty
			if len(currentBatch) > 0 {
				batches = append(batches, currentBatch)
				currentBatch = nil
				currentSize = 0
			}
			// Add oversized item alone
			batches = append(batches, []*gen.ResourceItem{item})
			continue
		}

		// If adding this item would exceed the limit, start a new batch
		if currentSize+itemSize > maxBatchSize && len(currentBatch) > 0 {
			batches = append(batches, currentBatch)
			currentBatch = nil
			currentSize = 0
		}

		currentBatch = append(currentBatch, item)
		currentSize += itemSize
	}

	// Add remaining batch
	if len(currentBatch) > 0 {
		batches = append(batches, currentBatch)
	}

	return batches
}

// SendResourceBatch sends a batch of resources to Dakr through gRPC
func (c *RealDakrClient) SendResourceBatch(ctx context.Context, resources []collector.CollectedResource, resourceType collector.ResourceType) (string, error) {
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
		return "", nil
	}

	// Split into smaller batches if needed
	batches := c.splitBatchBySize(resourceItems)
	c.logger.Info("Splitting resources into batches",
		"originalCount", len(resourceItems),
		"batchCount", len(batches),
		"resourceType", resourceType)

	var lastClusterID string
	var lastError error

	// Send each batch
	for i, batch := range batches {
		// Create the batch request
		batchReq := &gen.SendResourceBatchRequest{
			Resources: batch,
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
			c.logger.Error(err, "Failed to send resource batch to Dakr",
				"batchIndex", i+1,
				"totalBatches", len(batches),
				"batchSize", len(batch),
				"type", resourceType)
			lastError = err
			// Continue trying other batches even if one fails
			continue
		}

		if resp.Msg != nil {
			lastClusterID = resp.Msg.ClusterIdentifier
		}

		c.logger.Info("Successfully sent batch",
			"batchIndex", i+1,
			"totalBatches", len(batches),
			"batchSize", len(batch))
	}

	// If all batches failed, return the last error
	if lastError != nil && lastClusterID == "" {
		return "", fmt.Errorf("failed to send resource batch to Dakr: %w", lastError)
	}

	return lastClusterID, nil
}

// SendTelemetryMetrics sends telemetry metrics to Dakr
func (c *RealDakrClient) SendTelemetryMetrics(ctx context.Context, metrics []*dto.MetricFamily) (int32, error) {
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

	return resp.Msg.ProcessedCount, nil
}

func attachClusterToken[T any](req *connect.Request[T], clusterToken string) {
	req.Header().Set("Authorization", fmt.Sprintf("Bearer %s", clusterToken))
}

// SendClusterSnapshotStream sends cluster snapshot data in chunks via streaming
func (c *RealDakrClient) SendClusterSnapshotStream(ctx context.Context, snapshot *gen.ClusterSnapshot, snapshotID string, timestamp time.Time) (string, *gen.ClusterSnapshot, error) {
	c.logger.Info("Sending cluster snapshot", "snapshotId", snapshotID)

	protoBytes, err := proto.Marshal(snapshot)
	if err != nil {
		c.logger.Error(err, "Failed to proto.Marshal cluster snapshot")
		return "", nil, fmt.Errorf("failed to proto.Marshal cluster snapshot: %w", err)
	}

	totalSize := len(protoBytes)
	totalChunks := int(math.Ceil(float64(totalSize) / float64(maxChunkSize)))

	// Establish the stream
	stream := c.client.SendClusterSnapshotStream(ctx)
	stream.RequestHeader().Set("Authorization", fmt.Sprintf("Bearer %s", c.clusterToken))

	clusterID := ""
	teamID := ""
	if ctx.Value("cluster_id") != nil {
		clusterID, _ = ctx.Value("cluster_id").(string)
	}
	if ctx.Value("team_id") != nil {
		teamID, _ = ctx.Value("team_id").(string)
	}

	// Send chunks
	for chunkNum := 0; chunkNum < totalChunks; chunkNum++ {
		start := chunkNum * maxChunkSize
		end := start + maxChunkSize
		if end > totalSize {
			end = totalSize
		}

		chunkData := protoBytes[start:end]

		chunk := &gen.ClusterSnapshotChunk{
			ClusterId:    clusterID,
			TeamId:       teamID,
			Timestamp:    timestamppb.New(timestamp),
			SnapshotId:   snapshotID,
			ChunkNumber:  int32(chunkNum),
			TotalChunks:  int32(totalChunks),
			ChunkData:    chunkData,
			IsFinalChunk: chunkNum == totalChunks-1,
		}

		// Send the raw chunk message directly.
		if err := stream.Send(chunk); err != nil {
			c.logger.Error(err, "Failed to send chunk", "chunkNum", chunkNum)
			return "", nil, fmt.Errorf("failed to send chunk %d: %w", chunkNum, err)
		}

		c.logger.Info("Sent chunk", "chunkNum", chunkNum, "size", len(chunkData))
	}

	resp, err := stream.CloseAndReceive()
	if err != nil {
		c.logger.Error(err, "Failed to close stream and receive response")
		return "", nil, fmt.Errorf("failed to close stream and receive response: %w", err)
	}

	c.logger.Info("Successfully sent cluster snapshot via streaming",
		"snapshotId", snapshotID,
		"chunksReceived", resp.Msg.ChunksReceived,
		"status", resp.Msg.Status)

	return resp.Msg.ClusterId, resp.Msg.MissingResources, nil
}

// SendTelemetryLogs sends a batch of log entries to the Dakr service.
func (c *RealDakrClient) SendTelemetryLogs(ctx context.Context, in *gen.SendTelemetryLogsRequest) (*gen.SendTelemetryLogsResponse, error) {
	c.logger.V(1).Info("Sending telemetry logs to Dakr", "count", len(in.Logs))

	req := connect.NewRequest(in)
	attachClusterToken(req, c.clusterToken)

	// Set cluster and team IDs if they are not already in the request body
	// (this is where we set the cluster id and team id, my assumption is that we always have those thing in our parent context)
	if in.ClusterId == "" {
		if clusterID, ok := ctx.Value("cluster_id").(string); ok {
			in.ClusterId = clusterID
		}
	}
	if in.TeamId == "" {
		if teamID, ok := ctx.Value("team_id").(string); ok {
			in.TeamId = teamID
		}
	}

	// Send to Dakr
	resp, err := c.client.SendTelemetryLogs(ctx, req)
	if err != nil {
		c.logger.Error(err, "Failed to send telemetry logs to Dakr")
		return nil, fmt.Errorf("failed to send telemetry logs to Dakr: %w", err)
	}

	c.logger.V(1).Info("Successfully sent telemetry logs to Dakr", "processed_count", resp.Msg.ProcessedCount)
	return resp.Msg, nil
}
