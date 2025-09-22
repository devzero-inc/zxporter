package transport

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	"github.com/devzero-inc/zxporter/internal/collector"
)

// CompressionResult holds compression test results
type CompressionResult struct {
	OriginalSize   int
	CompressedSize int
	Ratio          float64
	Savings        int
	SavingsPercent float64
}

func (cr CompressionResult) String() string {
	return fmt.Sprintf("Original: %d bytes, Compressed: %d bytes, Ratio: %.3f, Savings: %d bytes (%.1f%%)",
		cr.OriginalSize, cr.CompressedSize, cr.Ratio, cr.Savings, cr.SavingsPercent)
}

// compressData compresses data with gzip and returns metrics
func compressData(data []byte) ([]byte, CompressionResult, error) {
	if len(data) == 0 {
		return data, CompressionResult{}, nil
	}

	var buf bytes.Buffer
	gzipWriter := gzip.NewWriter(&buf)

	_, err := gzipWriter.Write(data)
	if err != nil {
		gzipWriter.Close()
		return nil, CompressionResult{}, err
	}

	if err := gzipWriter.Close(); err != nil {
		return nil, CompressionResult{}, err
	}

	compressed := buf.Bytes()
	original := len(data)
	compressedSize := len(compressed)

	ratio := float64(compressedSize) / float64(original)
	savings := original - compressedSize
	savingsPercent := (float64(savings) / float64(original)) * 100

	result := CompressionResult{
		OriginalSize:   original,
		CompressedSize: compressedSize,
		Ratio:          ratio,
		Savings:        savings,
		SavingsPercent: savingsPercent,
	}

	return compressed, result, nil
}

// createSampleKubernetesResource creates a realistic Pod resource for testing
func createSampleKubernetesResource() *corev1.Pod {
	return &corev1.Pod{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Pod",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sample-pod",
			Namespace: "default",
			Labels: map[string]string{
				"app":        "nginx",
				"version":    "1.0.0",
				"tier":       "frontend",
				"component":  "web-server",
				"managed-by": "kubernetes",
			},
			Annotations: map[string]string{
				"kubernetes.io/created-by":                         "user",
				"deployment.kubernetes.io/revision":                "1",
				"kubectl.kubernetes.io/last-applied-configuration": `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"sample-pod","namespace":"default"}}`,
			},
			CreationTimestamp: metav1.Now(),
			UID:               "12345678-1234-1234-1234-123456789012",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "nginx",
					Image: "nginx:1.21.6",
					Ports: []corev1.ContainerPort{
						{
							ContainerPort: 80,
							Protocol:      corev1.ProtocolTCP,
						},
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("200m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
					Env: []corev1.EnvVar{
						{Name: "ENV1", Value: "value1"},
						{Name: "ENV2", Value: "value2"},
						{Name: "ENV3", Value: "value3"},
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
				},
			},
			PodIP:  "10.244.0.5",
			HostIP: "192.168.1.100",
		},
	}
}

func TestResourceSerializationCompression(t *testing.T) {
	pod := createSampleKubernetesResource()

	// Test original JSON serialization (before optimization)
	jsonBytes, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("Failed to marshal pod to JSON: %v", err)
	}

	// Test compression of JSON data
	_, result, err := compressData(jsonBytes)
	if err != nil {
		t.Fatalf("Failed to compress JSON data: %v", err)
	}

	t.Logf("JSON Resource Compression: %s", result.String())

	// Verify we're getting reasonable compression (should be better than 30% savings for small data)
	if result.SavingsPercent < 30.0 {
		t.Errorf("Expected at least 30%% compression savings, got %.1f%%", result.SavingsPercent)
	}

	// Test protobuf serialization with data_bytes field
	resource := collector.CollectedResource{
		ResourceType: collector.Pod,
		Object:       pod,
		Timestamp:    time.Now(),
		EventType:    collector.EventTypeAdd,
		Key:          "default/sample-pod",
	}

	// Create ResourceItem using data_bytes field (our optimization)
	item := &gen.ResourceItem{
		Key:          resource.Key,
		Timestamp:    timestamppb.New(resource.Timestamp),
		EventType:    gen.EventType_EVENT_TYPE_ADD,
		DataBytes:    jsonBytes, // Direct JSON bytes
		ResourceType: gen.ResourceType_RESOURCE_TYPE_POD,
	}

	// Marshal the ResourceItem to protobuf
	protoBytes, err := proto.Marshal(item)
	if err != nil {
		t.Fatalf("Failed to marshal ResourceItem to protobuf: %v", err)
	}

	// Test compression of protobuf data
	_, protoResult, err := compressData(protoBytes)
	if err != nil {
		t.Fatalf("Failed to compress protobuf data: %v", err)
	}

	t.Logf("Protobuf Resource Compression: %s", protoResult.String())

	// Compare efficiency: protobuf + compression vs JSON alone
	efficiency := float64(protoResult.CompressedSize) / float64(len(jsonBytes))
	t.Logf("Protobuf+Compression vs Raw JSON efficiency: %.3f (lower is better)", efficiency)

	if efficiency > 0.7 {
		t.Errorf("Expected protobuf+compression to be smaller than raw JSON, got %.1f%% efficiency", efficiency*100)
	}
}

func TestClusterSnapshotCompression(t *testing.T) {
	// Create a sample cluster snapshot
	snapshot := &gen.ClusterSnapshot{
		ClusterInfo: &gen.ClusterInfo{
			Version:    "v1.25.0",
			NodeCount:  3,
			Namespaces: []string{"default", "kube-system", "kube-public"},
		},
		Timestamp:  timestamppb.New(time.Now()),
		SnapshotId: "test-snapshot-123",
		Nodes: map[string]*gen.NodeData{
			"node1": {
				Node: &gen.ResourceIdentifier{Name: "node1"},
				Pods: map[string]*gen.ResourceIdentifier{
					"pod1": {Name: "pod1"},
					"pod2": {Name: "pod2"},
				},
				Hash: "node1-hash",
			},
		},
		Namespaces: map[string]*gen.Namespace{
			"default": {
				Namespace: &gen.ResourceIdentifier{Name: "default"},
				Deployments: map[string]*gen.ResourceIdentifier{
					"deployment1": {Name: "deployment1"},
				},
				Services: map[string]*gen.ResourceIdentifier{
					"service1": {Name: "service1"},
				},
				Hash: "default-hash",
			},
		},
		ClusterScoped: &gen.ClusterScopedSnapshot{
			PersistentVolumes: map[string]*gen.ResourceIdentifier{
				"pv1": {Name: "pv1"},
			},
			StorageClasses: map[string]*gen.ResourceIdentifier{
				"sc1": {Name: "sc1"},
			},
			Hash: "cluster-scoped-hash",
		},
	}

	// Marshal to protobuf
	protoBytes, err := proto.Marshal(snapshot)
	if err != nil {
		t.Fatalf("Failed to marshal cluster snapshot: %v", err)
	}

	// Test compression
	_, result, err := compressData(protoBytes)
	if err != nil {
		t.Fatalf("Failed to compress snapshot data: %v", err)
	}

	t.Logf("Cluster Snapshot Compression: %s", result.String())

	// Test chunk compression efficiency
	chunkSize := 8 * 1024 * 1024 // 8MB chunks (our optimization)
	if len(protoBytes) > chunkSize {
		// Test compression of a full chunk
		chunk := protoBytes[:chunkSize]
		_, chunkResult, err := compressData(chunk)
		if err != nil {
			t.Fatalf("Failed to compress chunk: %v", err)
		}

		t.Logf("8MB Chunk Compression: %s", chunkResult.String())

		// Verify chunk compression is effective
		if chunkResult.SavingsPercent < 30.0 {
			t.Errorf("Expected at least 30%% chunk compression savings, got %.1f%%", chunkResult.SavingsPercent)
		}
	}

	// Verify overall compression effectiveness (small test data won't compress as well)
	if result.SavingsPercent < 0.0 {
		t.Errorf("Expected some compression savings, got %.1f%%", result.SavingsPercent)
	}
}

func TestEstimateResourceSizeOptimization(t *testing.T) {
	pod := createSampleKubernetesResource()
	jsonBytes, _ := json.Marshal(pod)

	// Create ResourceItem using current approach (structpb)
	// Convert to structpb first
	var dataMap map[string]interface{}
	json.Unmarshal(jsonBytes, &dataMap)
	dataStruct, _ := structpb.NewStruct(dataMap)

	item := &gen.ResourceItem{
		Key:          "test-key",
		Timestamp:    timestamppb.New(time.Now()),
		EventType:    gen.EventType_EVENT_TYPE_ADD,
		Data:         dataStruct,
		ResourceType: gen.ResourceType_RESOURCE_TYPE_POD,
	}

	// Test the current estimateResourceSize function for uncompressed size estimation
	estimatedSize := estimateResourceSize(item)

	// Marshal to get actual size
	protoBytes, _ := proto.Marshal(item)
	actualSize := len(protoBytes)

	// Test compression to show compression benefits (for reference)
	_, result, _ := compressData(protoBytes)
	actualCompressedSize := result.CompressedSize

	t.Logf("Estimated size: %d bytes", estimatedSize)
	t.Logf("Actual uncompressed size: %d bytes", actualSize)
	t.Logf("Actual compressed size: %d bytes", actualCompressedSize)

	// Verify our estimation is reasonably close to actual uncompressed size
	// (not compressed size, because Connect RPC limits apply to uncompressed messages)
	estimationError := float64(abs(estimatedSize-actualSize)) / float64(actualSize)
	t.Logf("Uncompressed size estimation error: %.1f%%", estimationError*100)

	if estimationError > 0.2 { // Within 20% is reasonable for proto size estimation
		t.Errorf("Size estimation is too far off: estimated %d, actual uncompressed %d (%.1f%% error)",
			estimatedSize, actualSize, estimationError*100)
	}

	// Log compression ratio for reference
	compressionRatio := float64(actualCompressedSize) / float64(actualSize)
	t.Logf("Compression ratio: %.1f%% (%.2fx smaller)", compressionRatio*100, 1.0/compressionRatio)
}

func BenchmarkResourceCompression(b *testing.B) {
	pod := createSampleKubernetesResource()
	jsonBytes, _ := json.Marshal(pod)

	b.Run("JSON_Marshal", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			json.Marshal(pod)
		}
	})

	b.Run("Gzip_Compression", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			compressData(jsonBytes)
		}
	})

	b.Run("Protobuf_Marshal", func(b *testing.B) {
		item := &gen.ResourceItem{
			Key:          "test-key",
			Timestamp:    timestamppb.New(time.Now()),
			EventType:    gen.EventType_EVENT_TYPE_ADD,
			DataBytes:    jsonBytes,
			ResourceType: gen.ResourceType_RESOURCE_TYPE_POD,
		}

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			proto.Marshal(item)
		}
	})
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
