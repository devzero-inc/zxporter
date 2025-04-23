package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"connectrpc.com/connect"

	apiv1 "github.com/devzero-inc/zxporter/gen/api/v1"
	apiv1connect "github.com/devzero-inc/zxporter/gen/api/v1/apiv1connect"
)

// Stats represents the statistics about received messages
type Stats struct {
	TotalMessages    int            `json:"total_messages"`
	MessagesByType   map[string]int `json:"messages_by_type"`
	FirstMessageTime *time.Time     `json:"first_message_time,omitempty"`
}

// MetricsServer implements the MetricsCollectorServiceHandler interface
type MetricsServer struct {
	outputFile string
	mu         sync.Mutex
	stats      Stats
}

// SendResource implements the SendResource RPC method
func (s *MetricsServer) SendResource(ctx context.Context, req *connect.Request[apiv1.SendResourceRequest]) (*connect.Response[apiv1.SendResourceResponse], error) {
	// Convert the request to JSON
	jsonData, err := json.Marshal(req.Msg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error marshaling request to JSON: %v\n", err)
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("error marshaling request to JSON: %w", err))
	}

	// Write the JSON to the output file
	s.mu.Lock()
	defer s.mu.Unlock()

	// Update stats
	s.stats.TotalMessages++

	// Convert ResourceType to string
	resourceTypeStr := req.Msg.ResourceType.String()

	// Update messages by type
	if s.stats.MessagesByType == nil {
		s.stats.MessagesByType = make(map[string]int)
	}
	s.stats.MessagesByType[resourceTypeStr]++

	// Update first message time if not set
	if s.stats.FirstMessageTime == nil {
		now := time.Now()
		s.stats.FirstMessageTime = &now
	}

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

// StatsHandler handles the /stats HTTP endpoint
func (s *MetricsServer) StatsHandler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")

	// Marshal the stats to JSON with indentation
	jsonData, err := json.MarshalIndent(s.stats, "", "  ")
	if err != nil {
		http.Error(w, "Error generating stats", http.StatusInternalServerError)
		return
	}

	// Write the JSON data directly
	w.Write(jsonData)
}

func main() {
	// Define command-line flags
	outputFile := flag.String("output", "requests.json", "Path to the output file for request data")
	grpcPort := flag.String("grpc-port", "50051", "Port for the gRPC server")
	httpPort := flag.String("http-port", "8080", "Port for the HTTP server")
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
		stats: Stats{
			MessagesByType: make(map[string]int),
		},
	}

	// Create a Connect handler for the MetricsCollectorService
	path, handler := apiv1connect.NewMetricsCollectorServiceHandler(server)

	// Create a mux for the gRPC server
	grpcMux := http.NewServeMux()
	grpcMux.Handle(path, handler)

	// Create a mux for the HTTP server
	httpMux := http.NewServeMux()
	httpMux.HandleFunc("/stats", server.StatsHandler)

	// Start the HTTP server in a goroutine
	go func() {
		fmt.Fprintf(os.Stderr, "Starting HTTP server on :%s\n", *httpPort)
		if err := http.ListenAndServe(":"+*httpPort, httpMux); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to serve HTTP: %v\n", err)
			os.Exit(1)
		}
	}()

	// Start the gRPC server
	fmt.Fprintf(os.Stderr, "Starting Connect server on :%s\n", *grpcPort)
	if err := http.ListenAndServe(":"+*grpcPort, grpcMux); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to serve gRPC: %v\n", err)
		os.Exit(1)
	}
}
