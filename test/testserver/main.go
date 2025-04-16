package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"sync"

	apiv1 "github.com/devzero-inc/zxporter/gen/api/v1"
	"google.golang.org/grpc"
)

// MetricsServer implements the MetricsCollectorServiceServer interface
type MetricsServer struct {
	apiv1.UnimplementedMetricsCollectorServiceServer
	outputFile string
	mu         sync.Mutex
}

// SendResource implements the SendResource RPC method
func (s *MetricsServer) SendResource(ctx context.Context, req *apiv1.SendResourceRequest) (*apiv1.SendResourceResponse, error) {
	// Convert the request to JSON
	jsonData, err := json.Marshal(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error marshaling request to JSON: %v\n", err)
		return nil, err
	}

	// Write the JSON to the output file
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := os.OpenFile(s.outputFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening output file: %v\n", err)
		return nil, err
	}
	defer f.Close()

	if _, err := f.WriteString(string(jsonData) + "\n"); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing to output file: %v\n", err)
		return nil, err
	}

	// Return a response
	return &apiv1.SendResourceResponse{
		ResourceType:      req.ResourceType,
		ClusterIdentifier: req.ClusterId,
	}, nil
}

func main() {
	// Define command-line flags
	outputFile := flag.String("output", "requests.json", "Path to the output file for request data")
	flag.Parse()

	// Print startup message to stderr
	fmt.Fprintf(os.Stderr, "Starting testserver with output file: %s\n", *outputFile)

	// Log current working directory to help with debugging
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error getting current working directory: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "Current working directory: %s\n", cwd)
	}

	// Create a listener on TCP port
	fmt.Fprintf(os.Stderr, "Attempting to listen on :50051\n")
	lis, err := net.Listen("tcp", ":50051")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to listen: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "Successfully listening on :50051\n")

	// Create a gRPC server
	grpcServer := grpc.NewServer()

	// Register our service
	server := &MetricsServer{
		outputFile: *outputFile,
	}
	apiv1.RegisterMetricsCollectorServiceServer(grpcServer, server)

	// Start serving
	fmt.Fprintf(os.Stderr, "Starting gRPC server on :50051\n")
	if err := grpcServer.Serve(lis); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to serve: %v\n", err)
		os.Exit(1)
	}
}
