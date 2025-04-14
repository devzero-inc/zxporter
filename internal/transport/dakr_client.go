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

func attachClusterToken[T any](req *connect.Request[T], clusterToken string) {
	req.Header().Set("Authorization", fmt.Sprintf("Bearer %s", clusterToken))
}
