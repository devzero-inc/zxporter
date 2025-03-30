// internal/transport/pulse_client.go
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

// RealPulseClient implements communication with Pulse service
type RealPulseClient struct {
	logger       logr.Logger
	client       genconnect.MetricsCollectorServiceClient
	clusterToken string
}

// NewPulseClient creates a new client for Pulse service
func NewPulseClient(pulseBaseURL string, clusterToken string, logger logr.Logger) PulseClient {
	// We're using the connect client for the Pulse service
	client := genconnect.NewMetricsCollectorServiceClient(
		http.DefaultClient,
		pulseBaseURL,
		connect.WithGRPC(),
	)

	return &RealPulseClient{
		logger:       logger.WithName("pulse-client"),
		client:       client,
		clusterToken: clusterToken,
	}
}

// SendResource sends the resource to Pulse through gRPC
func (c *RealPulseClient) SendResource(ctx context.Context, resource collector.CollectedResource) (string, error) {
	c.logger.Info("Sending resource to Pulse",
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

	// Send to Pulse
	resp, err := c.client.SendResource(ctx, req)
	if err != nil {
		c.logger.Error(err, "Failed to send resource to Pulse")
		return "", fmt.Errorf("failed to send resource to Pulse: %w", err)
	}

	return resp.Msg.ClusterIdentifier, nil
}

func attachClusterToken[T any](req *connect.Request[T], clusterToken string) {
	req.Header().Set("Authorization", fmt.Sprintf("Bearer %s", clusterToken))
}
