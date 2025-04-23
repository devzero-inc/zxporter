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
	"google.golang.org/protobuf/types/known/structpb"

	apiv1 "github.com/devzero-inc/zxporter/gen/api/v1"
	apiv1connect "github.com/devzero-inc/zxporter/gen/api/v1/apiv1connect"
)

// PodResourceUsage represents resource usage for a pod
type PodResourceUsage struct {
	Requests   map[string]string            `json:"requests,omitempty"`
	Limits     map[string]string            `json:"limits,omitempty"`
	Containers map[string]map[string]string `json:"containers,omitempty"`
}

// NodeResourceUsage represents resource usage for a node
type NodeResourceUsage struct {
	Capacity    map[string]string `json:"capacity,omitempty"`
	Allocatable map[string]string `json:"allocatable,omitempty"`
	Usage       map[string]string `json:"usage,omitempty"`
}

// Stats represents the statistics about received messages
type Stats struct {
	TotalMessages    int                          `json:"total_messages"`
	MessagesByType   map[string]int               `json:"messages_by_type"`
	UniqueResources  map[string]int               `json:"unique_resources"`
	FirstMessageTime *time.Time                   `json:"first_message_time,omitempty"`
	UsageReportPods  map[string]PodResourceUsage  `json:"usage_report_pods,omitempty"`
	UsageReportNodes map[string]NodeResourceUsage `json:"usage_report_nodes,omitempty"`
}

// MetricsServer implements the MetricsCollectorServiceHandler interface
type MetricsServer struct {
	outputFile    string
	mu            sync.Mutex
	stats         Stats
	seenResources map[string]bool // Track unique resources by type+key
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

	// Track unique resources by type+key
	resourceKey := fmt.Sprintf("%s:%s", resourceTypeStr, req.Msg.Key)
	if !s.seenResources[resourceKey] {
		s.seenResources[resourceKey] = true

		// Update unique resources count by type
		if s.stats.UniqueResources == nil {
			s.stats.UniqueResources = make(map[string]int)
		}
		s.stats.UniqueResources[resourceTypeStr]++
	}

	// Update first message time if not set
	if s.stats.FirstMessageTime == nil {
		now := time.Now()
		s.stats.FirstMessageTime = &now
	}

	// Extract resource usage information based on resource type
	if req.Msg.Data != nil {
		switch req.Msg.ResourceType {
		case apiv1.ResourceType_RESOURCE_TYPE_POD:
			s.extractPodResourceInfo(req.Msg.Key, req.Msg.Data)
		case apiv1.ResourceType_RESOURCE_TYPE_CONTAINER_RESOURCE:
			s.extractContainerResourceInfo(req.Msg.Data)
		case apiv1.ResourceType_RESOURCE_TYPE_NODE_RESOURCE:
			s.extractNodeResourceInfo(req.Msg.Key, req.Msg.Data)
		}
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

// extractPodResourceInfo extracts resource information from a pod message
func (s *MetricsServer) extractPodResourceInfo(key string, data *structpb.Struct) {
	if data == nil {
		return
	}

	// Initialize pod resource usage if not exists
	if _, exists := s.stats.UsageReportPods[key]; !exists {
		s.stats.UsageReportPods[key] = PodResourceUsage{
			Requests:   make(map[string]string),
			Limits:     make(map[string]string),
			Containers: make(map[string]map[string]string),
		}
	}

	// Extract pod spec information
	podData := data.GetFields()
	if podData == nil {
		return
	}

	// Try to extract pod spec
	if specValue, ok := podData["spec"]; ok && specValue.GetStructValue() != nil {
		specData := specValue.GetStructValue().GetFields()

		// Extract container specs
		if containersValue, ok := specData["containers"]; ok && containersValue.GetListValue() != nil {
			containers := containersValue.GetListValue().GetValues()

			for _, containerValue := range containers {
				if containerValue.GetStructValue() == nil {
					continue
				}

				containerData := containerValue.GetStructValue().GetFields()
				containerName := containerData["name"].GetStringValue()

				// Initialize container usage if not exists
				podUsage := s.stats.UsageReportPods[key]
				if podUsage.Containers == nil {
					podUsage.Containers = make(map[string]map[string]string)
				}
				if _, exists := podUsage.Containers[containerName]; !exists {
					podUsage.Containers[containerName] = make(map[string]string)
				}
				s.stats.UsageReportPods[key] = podUsage

				// Extract resource requests and limits
				if resourcesValue, ok := containerData["resources"]; ok && resourcesValue.GetStructValue() != nil {
					resourcesData := resourcesValue.GetStructValue().GetFields()

					// Extract requests
					if requestsValue, ok := resourcesData["requests"]; ok && requestsValue.GetStructValue() != nil {
						requestsData := requestsValue.GetStructValue().GetFields()

						for resourceName, resourceValue := range requestsData {
							podUsage := s.stats.UsageReportPods[key]
							podUsage.Requests[resourceName] = resourceValue.GetStringValue()
							s.stats.UsageReportPods[key] = podUsage
						}
					}

					// Extract limits
					if limitsValue, ok := resourcesData["limits"]; ok && limitsValue.GetStructValue() != nil {
						limitsData := limitsValue.GetStructValue().GetFields()

						for resourceName, resourceValue := range limitsData {
							podUsage := s.stats.UsageReportPods[key]
							podUsage.Limits[resourceName] = resourceValue.GetStringValue()
							s.stats.UsageReportPods[key] = podUsage
						}
					}
				}
			}
		}
	}
}

// extractContainerResourceInfo extracts resource usage information from a container resource message
func (s *MetricsServer) extractContainerResourceInfo(data *structpb.Struct) {
	if data == nil {
		return
	}

	containerData := data.GetFields()
	if containerData == nil {
		return
	}

	// Extract pod and container names
	podNamespace := containerData["namespace"].GetStringValue()
	podName := containerData["podName"].GetStringValue()
	containerName := containerData["containerName"].GetStringValue()

	if podNamespace == "" || podName == "" || containerName == "" {
		return
	}

	// Create the pod key
	podKey := fmt.Sprintf("%s/%s", podNamespace, podName)

	// Initialize pod resource usage if not exists
	if _, exists := s.stats.UsageReportPods[podKey]; !exists {
		s.stats.UsageReportPods[podKey] = PodResourceUsage{
			Requests:   make(map[string]string),
			Limits:     make(map[string]string),
			Containers: make(map[string]map[string]string),
		}
	}

	// Initialize container usage if not exists
	podUsage := s.stats.UsageReportPods[podKey]
	if podUsage.Containers == nil {
		podUsage.Containers = make(map[string]map[string]string)
	}
	if _, exists := podUsage.Containers[containerName]; !exists {
		podUsage.Containers[containerName] = make(map[string]string)
	}

	// Extract CPU and memory usage
	if cpuUsage, ok := containerData["cpuUsageMillis"]; ok {
		podUsage.Containers[containerName]["used_cpu"] = fmt.Sprintf("%dm", int64(cpuUsage.GetNumberValue()))
	}

	if memoryUsage, ok := containerData["memoryUsageBytes"]; ok {
		podUsage.Containers[containerName]["used_memory"] = fmt.Sprintf("%d", int64(memoryUsage.GetNumberValue()))
	}

	// Update the pod usage
	s.stats.UsageReportPods[podKey] = podUsage
}

// extractNodeResourceInfo extracts resource information from a node resource message
func (s *MetricsServer) extractNodeResourceInfo(key string, data *structpb.Struct) {
	if data == nil {
		return
	}

	nodeData := data.GetFields()
	if nodeData == nil {
		return
	}

	// Initialize node resource usage if not exists
	if _, exists := s.stats.UsageReportNodes[key]; !exists {
		s.stats.UsageReportNodes[key] = NodeResourceUsage{
			Capacity:    make(map[string]string),
			Allocatable: make(map[string]string),
			Usage:       make(map[string]string),
		}
	}

	// Extract capacity
	if cpuCapacity, ok := nodeData["cpuCapacityMillis"]; ok {
		nodeUsage := s.stats.UsageReportNodes[key]
		nodeUsage.Capacity["cpu"] = fmt.Sprintf("%dm", int64(cpuCapacity.GetNumberValue()))
		s.stats.UsageReportNodes[key] = nodeUsage
	}

	if memoryCapacity, ok := nodeData["memoryCapacityBytes"]; ok {
		nodeUsage := s.stats.UsageReportNodes[key]
		nodeUsage.Capacity["memory"] = fmt.Sprintf("%d", int64(memoryCapacity.GetNumberValue()))
		s.stats.UsageReportNodes[key] = nodeUsage
	}

	// Extract allocatable
	if cpuAllocatable, ok := nodeData["cpuAllocatableMillis"]; ok {
		nodeUsage := s.stats.UsageReportNodes[key]
		nodeUsage.Allocatable["cpu"] = fmt.Sprintf("%dm", int64(cpuAllocatable.GetNumberValue()))
		s.stats.UsageReportNodes[key] = nodeUsage
	}

	if memoryAllocatable, ok := nodeData["memoryAllocatableBytes"]; ok {
		nodeUsage := s.stats.UsageReportNodes[key]
		nodeUsage.Allocatable["memory"] = fmt.Sprintf("%d", int64(memoryAllocatable.GetNumberValue()))
		s.stats.UsageReportNodes[key] = nodeUsage
	}

	// Extract usage
	if cpuUsage, ok := nodeData["cpuUsageMillis"]; ok {
		nodeUsage := s.stats.UsageReportNodes[key]
		nodeUsage.Usage["cpu"] = fmt.Sprintf("%dm", int64(cpuUsage.GetNumberValue()))
		s.stats.UsageReportNodes[key] = nodeUsage
	}

	if memoryUsage, ok := nodeData["memoryUsageBytes"]; ok {
		nodeUsage := s.stats.UsageReportNodes[key]
		nodeUsage.Usage["memory"] = fmt.Sprintf("%d", int64(memoryUsage.GetNumberValue()))
		s.stats.UsageReportNodes[key] = nodeUsage
	}
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
			MessagesByType:   make(map[string]int),
			UniqueResources:  make(map[string]int),
			UsageReportPods:  make(map[string]PodResourceUsage),
			UsageReportNodes: make(map[string]NodeResourceUsage),
		},
		seenResources: make(map[string]bool),
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
