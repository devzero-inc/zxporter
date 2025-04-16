package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"

	apiv1 "github.com/devzero-inc/zxporter/gen/api/v1"
	"google.golang.org/grpc"
)

// MetricsServer implements the MetricsCollectorServiceServer interface
type MetricsServer struct {
	apiv1.UnimplementedMetricsCollectorServiceServer
}

// SendResource implements the SendResource RPC method
func (s *MetricsServer) SendResource(ctx context.Context, req *apiv1.SendResourceRequest) (*apiv1.SendResourceResponse, error) {
	// Convert the request to JSON and print to stdout
	jsonData, err := json.Marshal(req)
	if err != nil {
		// Don't print to stdout/stderr, just return the error
		return nil, err
	}

	// Print the JSON to stdout
	fmt.Println(string(jsonData))

	// Return a response
	return &apiv1.SendResourceResponse{
		ResourceType:      req.ResourceType,
		ClusterIdentifier: req.ClusterId,
	}, nil
}

func main() {
	// Redirect log output to a file to avoid printing to stdout/stderr
	logFile, err := os.OpenFile("server.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		// This will print to stderr, but it's before the server starts
		// and is necessary to report critical startup errors
		log.Fatalf("Failed to open log file: %v", err)
	}
	defer logFile.Close()
	log.SetOutput(logFile)

	// Create a listener on TCP port
	lis, err := net.Listen("tcp", ":50051")
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}

	// Create a gRPC server
	grpcServer := grpc.NewServer()

	// Register our service
	apiv1.RegisterMetricsCollectorServiceServer(grpcServer, &MetricsServer{})

	// Start serving
	log.Println("Starting gRPC server on :50051")
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("Failed to serve: %v", err)
	}
}
