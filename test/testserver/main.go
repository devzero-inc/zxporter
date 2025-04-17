package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sync"

	"connectrpc.com/connect"

	apiv1 "github.com/devzero-inc/zxporter/gen/api/v1"
	apiv1connect "github.com/devzero-inc/zxporter/gen/api/v1/apiv1connect"
)

// MetricsServer implements the MetricsCollectorServiceHandler interface
type MetricsServer struct {
	outputFile string
	mu         sync.Mutex
}

// SendResource implements the SendResource RPC method
func (s *MetricsServer) SendResource(ctx context.Context, req *connect.Request[apiv1.SendResourceRequest]) (*connect.Response[apiv1.SendResourceResponse], error) {
	// Log the request
	fmt.Fprintf(os.Stderr, "Received SendResource request\n")

	// Convert the request to JSON
	jsonData, err := json.Marshal(req.Msg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error marshaling request to JSON: %v\n", err)
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("error marshaling request to JSON: %w", err))
	}

	// Write the JSON to the output file
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := os.OpenFile(s.outputFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening output file: %v\n", err)
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("error opening output file: %w", err))
	}
	defer f.Close()

	if _, err := f.WriteString(string(jsonData) + "\n"); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing to output file: %v\n", err)
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("error writing to output file: %w", err))
	}

	// Return a response
	resp := connect.NewResponse(&apiv1.SendResourceResponse{
		ResourceType:      req.Msg.ResourceType,
		ClusterIdentifier: req.Msg.ClusterId,
	})

	return resp, nil
}

func main() {
	// Define command-line flags
	outputFile := flag.String("output", "requests.json", "Path to the output file for request data")
	flag.Parse()

	// Print startup message to stderr
	fmt.Fprintf(os.Stderr, "Starting Connect testserver with output file: %s\n", *outputFile)

	// Log current working directory to help with debugging
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error getting current working directory: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "Current working directory: %s\n", cwd)
	}

	// Create a new server
	server := &MetricsServer{
		outputFile: *outputFile,
	}

	// Create a Connect handler for the MetricsCollectorService
	path, handler := apiv1connect.NewMetricsCollectorServiceHandler(server)

	// Create a mux and register the handler
	mux := http.NewServeMux()
	mux.Handle(path, handler)

	// Start the HTTP server
	fmt.Fprintf(os.Stderr, "Starting Connect server on :50051\n")
	if err := http.ListenAndServe(":50051", mux); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to serve: %v\n", err)
		os.Exit(1)
	}
}
