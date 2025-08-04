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
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"

	apiv1 "github.com/devzero-inc/zxporter/gen/api/v1"
	apiv1connect "github.com/devzero-inc/zxporter/gen/api/v1/apiv1connect"
	"github.com/devzero-inc/zxporter/test/stats"
)

// MetricsServer implements the MetricsCollectorServiceHandler interface
type MetricsServer struct {
	outputFile    string
	mu            sync.Mutex
	stats         stats.Stats
	seenResources map[string]bool // Track unique resources by type+key
}

// SendTelemetryMetrics implements apiv1connect.MetricsCollectorServiceHandler.
func (s *MetricsServer) SendTelemetryMetrics(ctx context.Context, req *connect.Request[apiv1.SendTelemetryMetricsRequest]) (*connect.Response[apiv1.SendTelemetryMetricsResponse], error) {
	// Log all metric family names
	fmt.Fprintf(os.Stderr, "Received telemetry metrics - metric families: ")
	processedCount := 0
	for _, metricFamily := range req.Msg.MetricFamilies {
		if metricFamily.Name != nil {
			metricName := *metricFamily.Name
			fmt.Fprintf(os.Stderr, "%s, ", metricName)

			// Update metrics by type stats
			if s.stats.MessagesByType == nil {
				s.stats.MessagesByType = make(map[string]int)
			}
			metricTypeKey := fmt.Sprintf("METRIC:%s", metricName)
			s.stats.MessagesByType[metricTypeKey]++

			processedCount++
		}
	}
	fmt.Fprintln(os.Stderr)

	// Return a response
	resp := connect.NewResponse(&apiv1.SendTelemetryMetricsResponse{
		ClusterId:      req.Msg.ClusterId,
		ProcessedCount: int32(processedCount),
	})

	return resp, nil
}

// processResourceItem handles the statistics update and data extraction for a single resource item.
// NOTE: This function assumes the caller holds the mutex (s.mu).
func (s *MetricsServer) processResourceItem(resource *apiv1.ResourceItem) {
	if resource == nil {
		return // Should not happen, but good practice
	}
	resourceType := resource.ResourceType // Get type from the item itself

	// Update stats
	s.stats.TotalMessages++

	// Convert ResourceType to string
	resourceTypeStr := resourceType.String()

	// Update messages by type
	if s.stats.MessagesByType == nil {
		s.stats.MessagesByType = make(map[string]int)
	}
	s.stats.MessagesByType[resourceTypeStr]++

	// Track unique resources by type+key
	resourceKey := fmt.Sprintf("%s:%s", resourceTypeStr, resource.Key)
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
	if resource.Data != nil {
		switch resource.ResourceType { // Use resource.ResourceType here
		case apiv1.ResourceType_RESOURCE_TYPE_POD:
			s.extractPodResourceInfo(resource.Key, resource.Data)
		case apiv1.ResourceType_RESOURCE_TYPE_CONTAINER_RESOURCE:
			s.extractContainerResourceInfo(resource.Data)
		case apiv1.ResourceType_RESOURCE_TYPE_NODE_RESOURCE:
			s.extractNodeResourceInfo(resource.Key, resource.Data)
		case apiv1.ResourceType_RESOURCE_TYPE_CLUSTER:
			s.extractClusterResourceInfo(resource.Key, resource.Data)
		}
	}
}

// SendResource implements the SendResource RPC method
func (s *MetricsServer) SendResource(ctx context.Context, req *connect.Request[apiv1.SendResourceRequest]) (*connect.Response[apiv1.SendResourceResponse], error) {
	// Create a ResourceItem from the request message
	resourceItem := &apiv1.ResourceItem{
		Key:          req.Msg.Key,
		Timestamp:    req.Msg.Timestamp,
		EventType:    req.Msg.EventType,
		Data:         req.Msg.Data,
		ResourceType: req.Msg.ResourceType,
	}

	// Convert the request to JSON for logging
	jsonData, err := json.Marshal(req.Msg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error marshaling request to JSON: %v\n", err)
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("error marshaling request to JSON: %w", err))
	}

	// Write the JSON to the output file
	s.mu.Lock()
	defer s.mu.Unlock()

	// Process the single resource item
	s.processResourceItem(resourceItem) // Pass the constructed item

	// Write the original request JSON to the output file
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

// SendResourceBatch implements the SendResourceBatch RPC method
func (s *MetricsServer) SendResourceBatch(ctx context.Context, req *connect.Request[apiv1.SendResourceBatchRequest]) (*connect.Response[apiv1.SendResourceBatchResponse], error) {
	// Convert the batch request to JSON for logging
	jsonData, err := json.Marshal(req.Msg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error marshaling batch request to JSON: %v\n", err)
		// Continue processing even if logging fails, but don't write the faulty JSON
	}

	// Lock for stats update and file writing
	s.mu.Lock()
	defer s.mu.Unlock()

	// Write the JSON to the output file if marshaling succeeded
	if err == nil {
		f, fileErr := os.OpenFile(s.outputFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
		if fileErr != nil {
			fmt.Fprintf(os.Stderr, "Error opening output file for batch: %v\n", fileErr)
			// Return error as file writing is crucial for testing
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("error opening output file for batch: %w", fileErr))
		}
		defer f.Close() // Ensure file is closed even if writing fails

		if _, writeErr := f.WriteString(string(jsonData) + "\n"); writeErr != nil {
			fmt.Fprintf(os.Stderr, "Error writing batch to output file: %v\n", writeErr)
			// Return error as file writing is crucial for testing
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("error writing batch to output file: %w", writeErr))
		}
	}

	// Process each resource in the batch using the helper
	processedCount := 0
	for _, resource := range req.Msg.Resources {
		s.processResourceItem(resource)
		processedCount++
	}

	// Return a response
	resp := connect.NewResponse(&apiv1.SendResourceBatchResponse{
		ClusterIdentifier: req.Msg.ClusterId,
		ProcessedCount:    int32(processedCount),
	})

	return resp, nil
}

// SendClusterSnapshotStream implements streaming ingestion for the test server
// This method:
// 1. Collects all chunks from the stream
// 2. Reassembles the complete snapshot data
// 3. Deserializes the cluster snapshot
// 4. Extracts detailed resource statistics
// 5. Stores the statistics for testing validation
func (s *MetricsServer) SendClusterSnapshotStream(
	ctx context.Context,
	stream *connect.ClientStream[apiv1.ClusterSnapshotChunk],
) (*connect.Response[apiv1.SendClusterSnapshotStreamResponse], error) {
	fmt.Fprintf(os.Stderr, "=== Starting cluster snapshot stream processing ===\n")

	var snapshotID string
	var clusterID string
	var chunksReceived int32
	var totalChunks int32
	var receivedAt time.Time
	var totalSize int64

	// Step 1: Collect all chunks and reassemble the data
	fmt.Fprintf(os.Stderr, "Step 1: Collecting snapshot chunks...\n")
	chunks := make(map[int32][]byte) // chunk_number -> chunk_data

	for stream.Receive() {
		chunk := stream.Msg()
		chunksReceived++

		if chunksReceived == 1 {
			snapshotID = chunk.GetSnapshotId()
			clusterID = chunk.GetClusterId()
			totalChunks = chunk.GetTotalChunks()
			receivedAt = time.Now()
			fmt.Fprintf(os.Stderr, "  Snapshot ID: %s, Cluster ID: %s, Expected chunks: %d\n",
				snapshotID, clusterID, totalChunks)
		}

		chunkSize := len(chunk.GetChunkData())
		totalSize += int64(chunkSize)
		chunks[chunk.GetChunkNumber()] = chunk.GetChunkData()

		fmt.Fprintf(os.Stderr, "  Received chunk %d/%d (size: %d bytes)\n",
			chunk.GetChunkNumber()+1, chunk.GetTotalChunks(), chunkSize)
	}

	if err := stream.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "Stream error in SendClusterSnapshotStream: %v\n", err)
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("stream error: %w", err))
	}

	fmt.Fprintf(os.Stderr, "Step 1 Complete: Collected %d/%d chunks, total size: %d bytes\n",
		chunksReceived, totalChunks, totalSize)

	// Step 2: Reassemble the complete snapshot data
	fmt.Fprintf(os.Stderr, "Step 2: Reassembling snapshot data...\n")
	var completeData []byte
	for i := int32(0); i < totalChunks; i++ {
		if chunkData, exists := chunks[i]; exists {
			completeData = append(completeData, chunkData...)
		} else {
			fmt.Fprintf(os.Stderr, "ERROR: Missing chunk %d\n", i)
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("missing chunk %d", i))
		}
	}
	fmt.Fprintf(os.Stderr, "Step 2 Complete: Reassembled %d bytes of snapshot data\n", len(completeData))

	// Step 3: Deserialize the cluster snapshot
	fmt.Fprintf(os.Stderr, "Step 3: Deserializing cluster snapshot...\n")
	var clusterSnapshot apiv1.ClusterSnapshot
	if err := json.Unmarshal(completeData, &clusterSnapshot); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: Failed to deserialize cluster snapshot: %v\n", err)
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("failed to deserialize snapshot: %w", err))
	}
	fmt.Fprintf(os.Stderr, "Step 3 Complete: Successfully deserialized cluster snapshot\n")

	// Step 4: Extract detailed resource statistics
	fmt.Fprintf(os.Stderr, "Step 4: Extracting resource statistics...\n")
	snapshotStats := s.extractSnapshotStatistics(&clusterSnapshot, snapshotID, clusterID, chunksReceived, totalSize, receivedAt)
	fmt.Fprintf(os.Stderr, "Step 4 Complete: Extracted statistics for %d nodes, %d namespaces\n",
		len(snapshotStats.NodeResources), len(snapshotStats.NamespaceResources))

	// Step 5: Store statistics and update global stats
	fmt.Fprintf(os.Stderr, "Step 5: Storing statistics...\n")
	s.mu.Lock()
	defer s.mu.Unlock()

	// Update global message stats
	s.stats.TotalMessages++
	if s.stats.MessagesByType == nil {
		s.stats.MessagesByType = make(map[string]int)
	}
	s.stats.MessagesByType["CLUSTER_SNAPSHOT_STREAM"]++
	if s.stats.FirstMessageTime == nil {
		now := time.Now()
		s.stats.FirstMessageTime = &now
	}

	if s.stats.SnapshotStats == nil {
		s.stats.SnapshotStats = make(map[string]stats.SnapshotStats)
	}
	s.stats.SnapshotStats[snapshotID] = snapshotStats

	fmt.Fprintf(os.Stderr, "Step 5 Complete: Stored snapshot statistics\n")

	processingDuration := time.Since(receivedAt)
	fmt.Fprintf(os.Stderr, "=== Snapshot processing complete ===\n")
	fmt.Fprintf(os.Stderr, "Summary:\n")
	fmt.Fprintf(os.Stderr, "  - Snapshot ID: %s\n", snapshotID)
	fmt.Fprintf(os.Stderr, "  - Cluster ID: %s\n", clusterID)
	fmt.Fprintf(os.Stderr, "  - Chunks: %d/%d\n", chunksReceived, totalChunks)
	fmt.Fprintf(os.Stderr, "  - Total size: %d bytes\n", totalSize)
	fmt.Fprintf(os.Stderr, "  - Processing time: %v\n", processingDuration)
	fmt.Fprintf(os.Stderr, "  - Nodes: %d\n", len(snapshotStats.NodeResources))
	fmt.Fprintf(os.Stderr, "  - Namespaces: %d\n", len(snapshotStats.NamespaceResources))
	fmt.Fprintf(os.Stderr, "  - Cluster-scoped resources: %d types\n", len(snapshotStats.ClusterScopedResources))

	resp := connect.NewResponse(&apiv1.SendClusterSnapshotStreamResponse{
		ClusterId:      clusterID,
		SnapshotId:     snapshotID,
		Status:         "processed",
		ChunksReceived: chunksReceived,
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
		s.stats.UsageReportPods[key] = stats.PodResourceUsage{
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
		s.stats.UsageReportPods[podKey] = stats.PodResourceUsage{
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
		s.stats.UsageReportNodes[key] = stats.NodeResourceUsage{
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

// extractClusterResourceInfo extracts resource information from a cluster resource message
func (s *MetricsServer) extractClusterResourceInfo(key string, data *structpb.Struct) {
	if data == nil {
		return
	}

	clusterData := data.GetFields()
	if clusterData == nil {
		return
	}

	// Initialize cluster resource usage if not exists
	// Initialize if not exists
	if _, exists := s.stats.UsageReportCluster[key]; !exists {
		s.stats.UsageReportCluster[key] = stats.ClusterResourceUsage{}
	}

	// Extract and update cluster resource data
	entry := s.stats.UsageReportCluster[key]
	entry.FullDump = protojson.Format(&structpb.Struct{Fields: clusterData})

	if zxporterVersion, ok := clusterData["zxporter_version"]; ok {
		entry.ZxporterVersion = zxporterVersion.GetStringValue()
	}
	if zxporterGitCommit, ok := clusterData["zxporter_git_commit"]; ok {
		entry.ZxporterGitCommit = zxporterGitCommit.GetStringValue()
	}
	if zxporterBuildDate, ok := clusterData["zxporter_build_date"]; ok {
		entry.ZxporterBuildDate = zxporterBuildDate.GetStringValue()
	}
	if version, ok := clusterData["version"]; ok {
		entry.Version = version.GetStringValue()
	}
	if name, ok := clusterData["name"]; ok {
		entry.Name = name.GetStringValue()
	}
	if provider, ok := clusterData["provider"]; ok {
		entry.Provider = provider.GetStringValue()
	}

	// Save the modified struct back into the map
	s.stats.UsageReportCluster[key] = entry
}

func (s *MetricsServer) extractSnapshotStatistics(
	snapshot *apiv1.ClusterSnapshot,
	snapshotID, clusterID string,
	chunksCount int32,
	totalSize int64,
	timestamp time.Time,
) stats.SnapshotStats {
	fmt.Fprintf(os.Stderr, "    Analyzing cluster snapshot structure...\n")

	snapshotStats := stats.SnapshotStats{
		SnapshotID:             snapshotID,
		ClusterID:              clusterID,
		Timestamp:              timestamp,
		ChunksCount:            chunksCount,
		TotalSize:              totalSize,
		NodeResources:          make(map[string]int),
		NamespaceResources:     make(map[string]int),
		ClusterScopedResources: make(map[string]int),
		NamespaceBreakdown:     make(map[string]stats.NamespaceResourceBreakdown),
	}

	if snapshot.ClusterInfo != nil {
		snapshotStats.ClusterVersion = snapshot.ClusterInfo.Version
		snapshotStats.NodeCount = snapshot.ClusterInfo.NodeCount
		snapshotStats.Namespaces = snapshot.ClusterInfo.Namespaces
		fmt.Fprintf(os.Stderr, "    Cluster version: %s, Node count: %d, Namespaces: %d\n",
			snapshotStats.ClusterVersion, snapshotStats.NodeCount, len(snapshotStats.Namespaces))
	}

	fmt.Fprintf(os.Stderr, "    Analyzing node resources...\n")
	for nodeUID, nodeData := range snapshot.Nodes {
		if nodeData != nil {
			podCount := len(nodeData.Pods)
			snapshotStats.NodeResources[nodeUID] = podCount
			fmt.Fprintf(os.Stderr, "      Node %s: %d pods\n", nodeUID, podCount)
		}
	}

	fmt.Fprintf(os.Stderr, "    Analyzing namespace resources...\n")
	for namespaceUID, namespaceData := range snapshot.Namespaces {
		if namespaceData == nil {
			continue
		}

		breakdown := stats.NamespaceResourceBreakdown{}
		if namespaceData.Namespace != nil {
			breakdown.NamespaceName = namespaceData.Namespace.Name
		}

		breakdown.Deployments = len(namespaceData.Deployments)
		breakdown.StatefulSets = len(namespaceData.StatefulSets)
		breakdown.DaemonSets = len(namespaceData.DaemonSets)
		breakdown.ReplicaSets = len(namespaceData.ReplicaSets)
		breakdown.Services = len(namespaceData.Services)
		breakdown.Secrets = len(namespaceData.Secrets)
		breakdown.PVCs = len(namespaceData.Pvcs)
		breakdown.Jobs = len(namespaceData.Jobs)
		breakdown.CronJobs = len(namespaceData.CronJobs)
		breakdown.Ingresses = len(namespaceData.Ingresses)
		breakdown.NetworkPolicies = len(namespaceData.NetworkPolicies)
		breakdown.ServiceAccounts = len(namespaceData.ServiceAccounts)
		breakdown.Roles = len(namespaceData.Roles)
		breakdown.RoleBindings = len(namespaceData.RoleBindings)
		breakdown.PodDisruptionBudgets = len(namespaceData.PodDisruptionBudgets)
		breakdown.Endpoints = len(namespaceData.Endpoints)
		breakdown.LimitRanges = len(namespaceData.LimitRanges)
		breakdown.ResourceQuotas = len(namespaceData.ResourceQuotas)
		breakdown.UnscheduledPods = len(namespaceData.UnscheduledPods)
		breakdown.HorizontalPodAutoscalers = len(namespaceData.HorizontalPodAutoscalers)
		breakdown.KedaScaledJobs = len(namespaceData.KedaScaledJobs)
		breakdown.KedaScaledObjects = len(namespaceData.KedaScaledObjects)

		breakdown.TotalResources = breakdown.Deployments + breakdown.StatefulSets + breakdown.DaemonSets +
			breakdown.ReplicaSets + breakdown.Services + breakdown.ConfigMaps + breakdown.Secrets +
			breakdown.PVCs + breakdown.Jobs + breakdown.CronJobs + breakdown.Ingresses +
			breakdown.NetworkPolicies + breakdown.ServiceAccounts + breakdown.Roles + breakdown.RoleBindings +
			breakdown.PodDisruptionBudgets + breakdown.Endpoints + breakdown.LimitRanges + breakdown.ResourceQuotas +
			breakdown.UnscheduledPods + breakdown.HorizontalPodAutoscalers + breakdown.Events +
			breakdown.KedaScaledJobs + breakdown.KedaScaledObjects

		snapshotStats.NamespaceResources[namespaceUID] = breakdown.TotalResources
		snapshotStats.NamespaceBreakdown[namespaceUID] = breakdown

		fmt.Fprintf(os.Stderr, "      Namespace %s (%s): %d total resources\n",
			breakdown.NamespaceName, namespaceUID, breakdown.TotalResources)
		fmt.Fprintf(os.Stderr, "        - Deployments: %d, StatefulSets: %d, DaemonSets: %d\n",
			breakdown.Deployments, breakdown.StatefulSets, breakdown.DaemonSets)
		fmt.Fprintf(os.Stderr, "        - Services: %d, ConfigMaps: %d, Secrets: %d\n",
			breakdown.Services, breakdown.ConfigMaps, breakdown.Secrets)
		fmt.Fprintf(os.Stderr, "        - PVCs: %d, Jobs: %d, CronJobs: %d\n",
			breakdown.PVCs, breakdown.Jobs, breakdown.CronJobs)
		fmt.Fprintf(os.Stderr, "        - HPAs: %d, Events: %d, UnscheduledPods: %d\n",
			breakdown.HorizontalPodAutoscalers, breakdown.Events, breakdown.UnscheduledPods)
	}

	fmt.Fprintf(os.Stderr, "    Analyzing cluster-scoped resources...\n")
	if snapshot.ClusterScoped != nil {
		snapshotStats.ClusterScopedResources["PersistentVolumes"] = len(snapshot.ClusterScoped.PersistentVolumes)
		snapshotStats.ClusterScopedResources["StorageClasses"] = len(snapshot.ClusterScoped.StorageClasses)
		snapshotStats.ClusterScopedResources["ClusterRoles"] = len(snapshot.ClusterScoped.ClusterRoles)
		snapshotStats.ClusterScopedResources["ClusterRoleBindings"] = len(snapshot.ClusterScoped.ClusterRoleBindings)
		snapshotStats.ClusterScopedResources["CustomResourceDefinitions"] = len(snapshot.ClusterScoped.CustomResourceDefinitions)
		snapshotStats.ClusterScopedResources["IngressClasses"] = len(snapshot.ClusterScoped.IngressClasses)
		snapshotStats.ClusterScopedResources["CSINodes"] = len(snapshot.ClusterScoped.CsiNodes)

		for resourceType, count := range snapshotStats.ClusterScopedResources {
			if count > 0 {
				fmt.Fprintf(os.Stderr, "      %s: %d\n", resourceType, count)
			}
		}
	}

	fmt.Fprintf(os.Stderr, "    Snapshot analysis complete\n")
	return snapshotStats
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
		stats: stats.Stats{
			MessagesByType:     make(map[string]int),
			UniqueResources:    make(map[string]int),
			UsageReportPods:    make(map[string]stats.PodResourceUsage),
			UsageReportNodes:   make(map[string]stats.NodeResourceUsage),
			UsageReportCluster: make(map[string]stats.ClusterResourceUsage),
			SnapshotStats:      make(map[string]stats.SnapshotStats),
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
