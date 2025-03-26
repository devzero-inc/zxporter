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

	pulsev1 "github.com/devzero-inc/services/pulse/gen/api/v1"
	genconnect "github.com/devzero-inc/services/pulse/gen/api/v1/apiv1connect"
)

// // PulseClient defines methods for sending data to Pulse
// type PulseClient interface {
// 	// SendResource sends any resource data to Pulse
// 	SendResource(ctx context.Context, resource collector.CollectedResource) error
// }

// RealPulseClient implements communication with Pulse service
type RealPulseClient struct {
	logger logr.Logger
	client genconnect.PulseServiceClient
}

// NewPulseClient creates a new client for Pulse service
func NewPulseClient(pulseBaseURL string, logger logr.Logger) PulseClient {
	// We're using the connect client for the Pulse service
	client := genconnect.NewPulseServiceClient(
		http.DefaultClient,
		pulseBaseURL,
	)

	return &RealPulseClient{
		logger: logger.WithName("pulse-client"),
		client: client,
	}
}

// SendResource sends the resource to Pulse through gRPC
func (c *RealPulseClient) SendResource(ctx context.Context, resource collector.CollectedResource) error {
	c.logger.V(4).Info("Sending resource to Pulse",
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
		return err
	}

	// Marshal the object to JSON then unmarshal to the struct
	jsonBytes, err := json.Marshal(resource.Object)
	if err != nil {
		c.logger.Error(err, "Failed to marshal resource data to JSON")
		return fmt.Errorf("failed to marshal resource data: %w", err)
	}

	err = dataStruct.UnmarshalJSON(jsonBytes)
	if err != nil {
		c.logger.Error(err, "Failed to unmarshal JSON to protobuf struct")
		return fmt.Errorf("failed to convert resource data to protobuf struct: %w", err)
	}

	// Create the request
	res := &pulsev1.SendResourceRequest{
		ResourceType: resource.ResourceType,
		Key:          resource.Key,
		Timestamp:    timestamppb.New(resource.Timestamp),
		EventType:    resource.EventType,
		Data:         dataStruct,
	}

	// // Create the request
	// req := connect.NewRequest(&pulsev1.SendResourceRequest{
	// 	Usages: []*pulsev1.SendResourceRequest{res},
	// })
	req := connect.NewRequest(res)

	// Send to Pulse
	resp, err := c.client.SendResource(ctx, req)
	if err != nil {
		c.logger.Error(err, "Failed to send resource to Pulse")
		return fmt.Errorf("failed to send resource to Pulse: %w", err)
	}

	if !resp.Msg.Success {
		c.logger.Error(fmt.Errorf(resp.Msg.GetMessage()), "Pulse rejected resource")
		return fmt.Errorf("Pulse server rejected resource: %s", resp.Msg)
	}

	return nil
}

// // SimplePulseClient is a placeholder implementation for development/testing
// type SimplePulseClient struct {
// 	logger logr.Logger
// }

// // NewSimplePulseClient creates a simple client that just logs the data
// func NewSimplePulseClient(logger logr.Logger) PulseClient {
// 	return &SimplePulseClient{
// 		logger: logger.WithName("simple-pulse-client"),
// 	}
// }

// // SendResource just logs the resource (for development/testing)
// func (c *SimplePulseClient) SendResource(ctx context.Context, resource collector.CollectedResource) error {
// 	// For now, just log that we would send something
// 	c.logger.Info("Would send resource to Pulse",
// 		"resourceType", resource.ResourceType,
// 		"eventType", resource.EventType,
// 		"key", resource.Key,
// 		"dataType", fmt.Sprintf("%T", resource.Object))

// 	return nil
// }
