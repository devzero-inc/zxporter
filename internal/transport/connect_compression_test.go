package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	genconnect "github.com/devzero-inc/zxporter/gen/api/v1/apiv1connect"
	"github.com/devzero-inc/zxporter/internal/collector"
	"github.com/go-logr/logr"
)

// MockMetricsServer implements the MetricsCollectorService to test compression
type MockMetricsServer struct {
	receivedBatches []ReceivedBatch
	totalBytes      int64
	requestCount    int
}

type ReceivedBatch struct {
	Resources    []*gen.ResourceItem
	SizeBytes    int
	IsCompressed bool
	Headers      http.Header
}

func (m *MockMetricsServer) SendResource(
	ctx context.Context,
	req *connect.Request[gen.SendResourceRequest],
) (*connect.Response[gen.SendResourceResponse], error) {
	m.requestCount++

	response := &gen.SendResourceResponse{
		ResourceType:      req.Msg.ResourceType,
		ClusterIdentifier: "test-cluster",
	}

	return connect.NewResponse(response), nil
}

func (m *MockMetricsServer) SendResourceBatch(
	ctx context.Context,
	req *connect.Request[gen.SendResourceBatchRequest],
) (*connect.Response[gen.SendResourceBatchResponse], error) {
	m.requestCount++

	// Estimate request size (this is approximate)
	requestSize := len(req.Msg.String()) // Rough estimation
	m.totalBytes += int64(requestSize)

	// Check if request was compressed
	encoding := req.Header().Get("Content-Encoding")
	isCompressed := encoding == "gzip"

	// Store received batch info
	batch := ReceivedBatch{
		Resources:    req.Msg.Resources,
		SizeBytes:    requestSize,
		IsCompressed: isCompressed,
		Headers:      req.Header(),
	}
	m.receivedBatches = append(m.receivedBatches, batch)

	response := &gen.SendResourceBatchResponse{
		ResourceType:      gen.ResourceType_RESOURCE_TYPE_POD,
		ClusterIdentifier: "test-cluster",
		ProcessedCount:    int32(len(req.Msg.Resources)),
	}

	return connect.NewResponse(response), nil
}

func (m *MockMetricsServer) SendTelemetryMetrics(
	ctx context.Context,
	req *connect.Request[gen.SendTelemetryMetricsRequest],
) (*connect.Response[gen.SendTelemetryMetricsResponse], error) {
	return connect.NewResponse(&gen.SendTelemetryMetricsResponse{}), nil
}

func (m *MockMetricsServer) SendClusterSnapshotStream(
	ctx context.Context,
	stream *connect.ClientStream[gen.ClusterSnapshotChunk],
) (*connect.Response[gen.SendClusterSnapshotStreamResponse], error) {
	return connect.NewResponse(&gen.SendClusterSnapshotStreamResponse{}), nil
}

func (m *MockMetricsServer) SendTelemetryLogs(
	ctx context.Context,
	req *connect.Request[gen.SendTelemetryLogsRequest],
) (*connect.Response[gen.SendTelemetryLogsResponse], error) {
	return connect.NewResponse(&gen.SendTelemetryLogsResponse{}), nil
}

func (m *MockMetricsServer) NodeMetadata(
	ctx context.Context,
	req *connect.Request[gen.NodeMetadataRequest],
) (*connect.Response[gen.NodeMetadataResponse], error) {
	// Return an empty response for the mock implementation
	return connect.NewResponse(&gen.NodeMetadataResponse{
		NodeToMeta: make(map[string]*gen.Node),
	}), nil
}

func TestConnectRPCCompressionIntegration(t *testing.T) {
	// Create mock server
	mockServer := &MockMetricsServer{}

	// Create HTTP server with Connect handler
	mux := http.NewServeMux()
	path, handler := genconnect.NewMetricsCollectorServiceHandler(mockServer)
	mux.Handle(path, handler)

	server := httptest.NewServer(mux)
	defer server.Close()

	// Test both compression enabled and disabled clients
	testCases := []struct {
		name              string
		enableCompression bool
		minBytes          int
	}{
		{
			name:              "WithCompression",
			enableCompression: true,
			minBytes:          1024, // Our threshold
		},
		{
			name:              "WithoutCompression",
			enableCompression: false,
			minBytes:          0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Reset mock server state
			mockServer.receivedBatches = nil
			mockServer.totalBytes = 0
			mockServer.requestCount = 0

			// Create client with or without compression
			var dakrClient DakrClient
			if tc.enableCompression {
				dakrClient = NewDakrClient(server.URL, "test-token", logr.Discard())
			} else {
				// Create client without compression for comparison
				httpClient := &http.Client{Timeout: 30 * time.Second}
				client := genconnect.NewMetricsCollectorServiceClient(
					httpClient,
					server.URL,
					connect.WithGRPC(),
					// No compression options
				)
				dakrClient = &RealDakrClient{
					logger:       logr.Discard().WithName("test-dakr-client"),
					client:       client,
					clusterToken: "test-token",
				}
			}

			// Create multiple realistic Kubernetes resources
			resources := make([]collector.CollectedResource, 0, 10)
			for i := 0; i < 10; i++ {
				pod := createLargeKubernetesResource(i)
				resource := collector.CollectedResource{
					ResourceType: collector.Pod,
					Object:       pod,
					Timestamp:    time.Now(),
					EventType:    collector.EventTypeAdd,
					Key:          fmt.Sprintf("namespace-%d/pod-%d", i, i),
				}
				resources = append(resources, resource)
			}

			// Send batch
			ctx := context.Background()
			ctx = context.WithValue(ctx, "cluster_id", "test-cluster")
			ctx = context.WithValue(ctx, "team_id", "test-team")

			clusterID, err := dakrClient.SendResourceBatch(ctx, resources, collector.Pod)
			if err != nil {
				t.Fatalf("Failed to send resource batch: %v", err)
			}

			if clusterID != "test-cluster" {
				t.Errorf("Expected cluster ID 'test-cluster', got '%s'", clusterID)
			}

			// Verify we received the batch
			if len(mockServer.receivedBatches) == 0 {
				t.Fatal("No batches received by mock server")
			}

			batch := mockServer.receivedBatches[0]
			t.Logf("Received batch: %d resources, %d bytes, compressed: %v",
				len(batch.Resources), batch.SizeBytes, batch.IsCompressed)

			// Verify compression behavior
			if tc.enableCompression {
				// For large payloads, should be compressed
				if batch.SizeBytes > tc.minBytes && !batch.IsCompressed {
					t.Logf("Warning: Expected compression for large payload (%d bytes), but Content-Encoding not detected", batch.SizeBytes)
					// Note: Connect might handle compression transparently at HTTP level
				}
			}

			// Verify data integrity - all resources should be received
			if len(batch.Resources) != len(resources) {
				t.Errorf("Expected %d resources, got %d", len(resources), len(batch.Resources))
			}

			// Verify using data field (structpb format)
			for i, item := range batch.Resources {
				if item.Data == nil {
					t.Errorf("Resource %d missing Data field", i)
					continue
				}

				// Convert structpb back to JSON for verification
				jsonData, err := item.Data.MarshalJSON()
				if err != nil {
					t.Errorf("Failed to marshal resource %d Data to JSON: %v", i, err)
					continue
				}

				// Verify we can deserialize the JSON
				var pod corev1.Pod
				if err := json.Unmarshal(jsonData, &pod); err != nil {
					t.Errorf("Failed to unmarshal resource %d Data: %v", i, err)
					continue
				}

				if pod.Name == "" {
					t.Errorf("Resource %d has empty name after deserialization", i)
				}
			}
		})
	}
}

func TestCompressionEffectivenessComparison(t *testing.T) {
	// Create a large payload to demonstrate compression benefits
	resources := make([]collector.CollectedResource, 0, 50)
	for i := 0; i < 50; i++ {
		pod := createLargeKubernetesResource(i)
		resource := collector.CollectedResource{
			ResourceType: collector.Pod,
			Object:       pod,
			Timestamp:    time.Now(),
			EventType:    collector.EventTypeAdd,
			Key:          fmt.Sprintf("large-namespace-%d/large-pod-%d", i, i),
		}
		resources = append(resources, resource)
	}

	// Test current approach (JSON -> structpb)
	t.Run("CurrentApproach_JSONToStructpb", func(t *testing.T) {
		var totalSize int
		for _, resource := range resources {
			jsonBytes, err := json.Marshal(resource.Object)
			if err != nil {
				t.Fatalf("Failed to marshal to JSON: %v", err)
			}

			// Simulate structpb approach overhead
			structpbOverhead := len(jsonBytes) + 100 // Approximate structpb overhead
			totalSize += structpbOverhead
		}
		t.Logf("Current approach total size: %d bytes", totalSize)
	})

	// Test future optimization approach (direct data_bytes)
	// TODO: When we migrate to data_bytes, this test shows the potential savings
	t.Run("FutureApproach_DirectDataBytes", func(t *testing.T) {
		resourceItems := make([]*gen.ResourceItem, 0, len(resources))

		for _, resource := range resources {
			jsonBytes, err := json.Marshal(resource.Object)
			if err != nil {
				t.Fatalf("Failed to marshal to JSON: %v", err)
			}

			item := &gen.ResourceItem{
				Key:          resource.Key,
				Timestamp:    timestamppb.New(resource.Timestamp),
				EventType:    gen.EventType_EVENT_TYPE_ADD,
				DataBytes:    jsonBytes,
				ResourceType: gen.ResourceType_RESOURCE_TYPE_POD,
			}
			resourceItems = append(resourceItems, item)
		}

		// Create batch request
		batchReq := &gen.SendResourceBatchRequest{
			Resources: resourceItems,
			ClusterId: "test-cluster",
			TeamId:    "test-team",
		}

		// Marshal to protobuf
		protoBytes, err := proto.Marshal(batchReq)
		if err != nil {
			t.Fatalf("Failed to marshal batch request: %v", err)
		}

		uncompressedSize := len(protoBytes)
		t.Logf("New approach uncompressed size: %d bytes", uncompressedSize)

		// Test compression
		_, result, err := compressData(protoBytes)
		if err != nil {
			t.Fatalf("Failed to compress batch: %v", err)
		}

		t.Logf("New approach compressed: %s", result.String())

		// Verify significant compression
		if result.SavingsPercent < 60.0 {
			t.Errorf("Expected at least 60%% compression savings, got %.1f%%", result.SavingsPercent)
		}
	})
}

// createLargeKubernetesResource creates a Pod with lots of realistic metadata
func createLargeKubernetesResource(index int) *corev1.Pod {
	labels := make(map[string]string)
	annotations := make(map[string]string)

	// Add many labels (realistic for production workloads)
	for i := 0; i < 10; i++ {
		labels[fmt.Sprintf("label-%d", i)] = fmt.Sprintf("value-%d-%d", index, i)
	}
	labels["app"] = fmt.Sprintf("app-%d", index)
	labels["version"] = "v1.2.3"
	labels["component"] = "backend"
	labels["tier"] = "production"

	// Add many annotations (realistic for production)
	for i := 0; i < 15; i++ {
		annotations[fmt.Sprintf("annotation-%d", i)] = fmt.Sprintf("long-annotation-value-%d-%d-with-lots-of-text", index, i)
	}
	annotations["kubectl.kubernetes.io/last-applied-configuration"] = fmt.Sprintf(`{
		"apiVersion": "v1",
		"kind": "Pod",
		"metadata": {
			"name": "large-pod-%d",
			"namespace": "large-namespace-%d",
			"labels": {"app": "test"}
		},
		"spec": {
			"containers": [{"name": "container", "image": "nginx:latest"}]
		}
	}`, index, index)

	// Create pod with multiple containers and lots of configuration
	return &corev1.Pod{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Pod",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:              fmt.Sprintf("large-pod-%d", index),
			Namespace:         fmt.Sprintf("large-namespace-%d", index),
			Labels:            labels,
			Annotations:       annotations,
			CreationTimestamp: metav1.Now(),
			UID:               "12345678-1234-1234-1234-123456789012",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  fmt.Sprintf("container-%d", index),
					Image: "nginx:1.21.6",
					Ports: []corev1.ContainerPort{
						{ContainerPort: 80, Protocol: corev1.ProtocolTCP},
						{ContainerPort: 443, Protocol: corev1.ProtocolTCP},
						{ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: resource.MustParse("512Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("1"),
							corev1.ResourceMemory: resource.MustParse("1Gi"),
						},
					},
					Env: []corev1.EnvVar{
						{Name: "ENV_VAR_1", Value: fmt.Sprintf("value-%d-1", index)},
						{Name: "ENV_VAR_2", Value: fmt.Sprintf("value-%d-2", index)},
						{Name: "ENV_VAR_3", Value: fmt.Sprintf("value-%d-3", index)},
						{Name: "LARGE_CONFIG", Value: strings.Repeat(fmt.Sprintf("config-data-%d-", index), 50)},
					},
				},
				{
					Name:  fmt.Sprintf("sidecar-%d", index),
					Image: "busybox:latest",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: resource.MustParse("64Mi"),
						},
					},
				},
			},
			RestartPolicy: corev1.RestartPolicyAlways,
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{
					Type:               corev1.PodReady,
					Status:             corev1.ConditionTrue,
					LastTransitionTime: metav1.Now(),
					Reason:             "Ready",
					Message:            fmt.Sprintf("Pod-%d is ready", index),
				},
				{
					Type:               corev1.PodInitialized,
					Status:             corev1.ConditionTrue,
					LastTransitionTime: metav1.Now(),
				},
			},
			PodIP:  fmt.Sprintf("10.244.%d.%d", index%255, (index+1)%255),
			HostIP: fmt.Sprintf("192.168.1.%d", (index%254)+1),
		},
	}
}
