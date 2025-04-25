// internal/transport/dakr_client.go
package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"connectrpc.com/connect"
	"github.com/devzero-inc/zxporter/internal/collector"
	"github.com/go-logr/logr"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	genconnect "github.com/devzero-inc/zxporter/gen/api/v1/apiv1connect"
)

// RealDakrClient implements communication with Dakr service
type RealDakrClient struct {
	logger       logr.Logger
	client       genconnect.MetricsCollectorServiceClient
	clusterToken string
}

// NewDakrClient creates a new client for Dakr service
func NewDakrClient(dakrBaseURL string, clusterToken string, logger logr.Logger) DakrClient {
	// We're using the connect client for the Dakr service
	client := genconnect.NewMetricsCollectorServiceClient(
		http.DefaultClient,
		dakrBaseURL,
		connect.WithGRPC(),
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
		EventType:    resource.EventType,
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
			EventType:    resource.EventType,
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

func attachClusterToken[T any](req *connect.Request[T], clusterToken string) {
	req.Header().Set("Authorization", fmt.Sprintf("Bearer %s", clusterToken))
}
