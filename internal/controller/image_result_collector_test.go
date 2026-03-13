/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"encoding/json"
	"fmt"
	"testing"

	v1 "github.com/devzero-inc/zxporter/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// =============================================================================
// Unit Tests: generateCRDName
// =============================================================================

func TestGenerateCRDName_BasicSha256(t *testing.T) {
	name := generateCRDName("sha256:a1b2c3d4e5f6a7b8c9d0e1f2")
	if name != "sha-a1b2c3d4e5f6" {
		t.Errorf("expected sha-a1b2c3d4e5f6, got %s", name)
	}
}

func TestGenerateCRDName_WithRegistryPrefix(t *testing.T) {
	name := generateCRDName("docker.io/library/nginx@sha256:abcdef123456789abcdef")
	if name != "sha-abcdef123456" {
		t.Errorf("expected sha-abcdef123456, got %s", name)
	}
}

func TestGenerateCRDName_ShortHex(t *testing.T) {
	name := generateCRDName("sha256:abc")
	if name != "sha-abc" {
		t.Errorf("expected sha-abc, got %s", name)
	}
}

func TestGenerateCRDName_UppercaseHex(t *testing.T) {
	name := generateCRDName("sha256:AABBCCDDEE11")
	if name != "sha-aabbccddee11" {
		t.Errorf("expected sha-aabbccddee11, got %s", name)
	}
}

func TestGenerateCRDName_NoSha256Prefix(t *testing.T) {
	// Falls through to cleaned hex logic — no sha256: prefix, so it tries to use the whole string as hex.
	name := generateCRDName("notahexstring")
	// Should fall back to sanitizeDNSName since no valid hex chars after filtering.
	if name == "" {
		t.Error("expected non-empty name")
	}
	if len(name) < 4 { // at minimum "sha-"
		t.Errorf("expected name starting with sha-, got %s", name)
	}
}

func TestGenerateCRDName_EmptyDigest(t *testing.T) {
	// Empty digest has no hex chars and sanitizeDNSName("") returns "", so we get "sha-".
	// This is an edge case that shouldn't happen in practice (digest is always non-empty).
	name := generateCRDName("")
	if name == "" {
		t.Error("expected non-empty name")
	}
}

// =============================================================================
// Unit Tests: parseImageRegistryAndRepo
// =============================================================================

func TestParseImageRegistryAndRepo(t *testing.T) {
	tests := []struct {
		ref          string
		wantRegistry string
		wantRepo     string
	}{
		{"docker.io/library/nginx:1.25.3", "docker.io", "library-nginx"},
		{"gcr.io/myproject/myapp:v1", "gcr.io", "myproject-myapp"},
		{"nginx:latest", "docker.io", "nginx"},
		{"nginx", "docker.io", "nginx"},
		{"registry.example.com:5000/repo/image:tag", "registry.example.com:5000", "repo-image"},
		{"quay.io/prometheus/node-exporter:v1.6.0", "quay.io", "prometheus-node-exporter"},
		{"docker.io/library/nginx@sha256:abc123", "docker.io", "library-nginx"},
		{"library/nginx", "docker.io", "library-nginx"},
	}

	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			reg, repo := parseImageRegistryAndRepo(tt.ref)
			if reg != tt.wantRegistry {
				t.Errorf("registry: got %q, want %q", reg, tt.wantRegistry)
			}
			if repo != tt.wantRepo {
				t.Errorf("repo: got %q, want %q", repo, tt.wantRepo)
			}
		})
	}
}

// =============================================================================
// Unit Tests: computeUserWastedPercent
// =============================================================================

func TestComputeUserWastedPercent(t *testing.T) {
	tests := []struct {
		name       string
		inefficient uint64
		size       uint64
		want       string
	}{
		{"zero size", 100, 0, "0"},
		{"no waste", 0, 1000000, "0"},
		{"10% waste", 100000, 1000000, "0.1"},
		{"50% waste", 500000, 1000000, "0.5"},
		{"100% waste", 1000000, 1000000, "1"},
		{"tiny fraction", 1, 1000000, "0"}, // formatFloat uses 4 decimal places, 0.000001 rounds to "0"
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeUserWastedPercent(tt.inefficient, tt.size)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// =============================================================================
// Unit Tests: formatFloat
// =============================================================================

func TestFormatFloat(t *testing.T) {
	tests := []struct {
		input float64
		want  string
	}{
		{0.0, "0"},
		{1.0, "1"},
		{0.95, "0.95"},
		{0.9516, "0.9516"},
		{0.12340, "0.1234"},
		{0.10, "0.1"},
		{0.001, "0.001"},
		{99.99, "99.99"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := formatFloat(tt.input)
			if got != tt.want {
				t.Errorf("formatFloat(%v) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// =============================================================================
// Unit Tests: truncateString
// =============================================================================

func TestTruncateString(t *testing.T) {
	if got := truncateString("hello", 10); got != "hello" {
		t.Errorf("expected hello, got %s", got)
	}
	if got := truncateString("hello world", 5); got != "hello" {
		t.Errorf("expected hello, got %s", got)
	}
	if got := truncateString("", 5); got != "" {
		t.Errorf("expected empty, got %s", got)
	}
	if got := truncateString("exact", 5); got != "exact" {
		t.Errorf("expected exact, got %s", got)
	}
}

// =============================================================================
// Unit Tests: parseByteSize
// =============================================================================

func TestParseByteSize(t *testing.T) {
	tests := []struct {
		input string
		want  int64
	}{
		{"", 0},
		{"0", 0},
		{"100B", 100},
		{"100", 100},
		{"1KB", 1000},
		{"1K", 1000},
		{"50MB", 50000000},
		{"50M", 50000000},
		{"1GB", 1000000000},
		{"1G", 1000000000},
		{"1TB", 1000000000000},
		{"1KiB", 1024},
		{"1MiB", 1048576},
		{"1GiB", 1073741824},
		{"1.5GB", 1500000000},
		{"0.5MB", 500000},
		{"invalid", 0},
		{"MB", 0},
		{"  50MB  ", 50000000},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseByteSize(tt.input)
			if got != tt.want {
				t.Errorf("parseByteSize(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

// =============================================================================
// Unit Tests: computeFileAnalysis
// =============================================================================

func TestComputeFileAnalysis_EmptyLayers(t *testing.T) {
	result := computeFileAnalysis(nil)
	if result != nil {
		t.Error("expected nil for empty layers")
	}
}

func TestComputeFileAnalysis_NoFileData(t *testing.T) {
	layers := []DiveLayer{
		{Index: 0, Command: "FROM alpine"},
		{Index: 1, Command: "RUN apk add bash"},
	}
	result := computeFileAnalysis(layers)
	if result != nil {
		t.Error("expected nil when no layers have file lists")
	}
}

func TestComputeFileAnalysis_WithFiles(t *testing.T) {
	layers := []DiveLayer{
		{
			Index:   0,
			Command: "FROM alpine",
			FileList: []DiveFile{
				{Path: "/bin/sh", Size: 100, IsDir: false},
				{Path: "/etc/passwd", Size: 200, IsDir: false},
				{Path: "/usr", Size: 0, IsDir: true}, // dirs should be skipped
			},
		},
		{
			Index:   1,
			Command: "RUN apk add bash",
			FileList: []DiveFile{
				{Path: "/bin/bash", Size: 500, IsDir: false},    // new file → added
				{Path: "/etc/passwd", Size: 250, IsDir: false},  // already seen → modified
			},
		},
	}

	result := computeFileAnalysis(layers)
	if result == nil {
		t.Fatal("expected non-nil result")
	}

	// /bin/sh added in layer 0, /etc/passwd added in layer 0, /bin/bash added in layer 1
	if result.AddedFiles != 3 {
		t.Errorf("AddedFiles: got %d, want 3", result.AddedFiles)
	}
	// /etc/passwd modified in layer 1
	if result.ModifiedFiles != 1 {
		t.Errorf("ModifiedFiles: got %d, want 1", result.ModifiedFiles)
	}
	// Unique files: /bin/sh, /etc/passwd, /bin/bash
	if result.TotalFiles != 3 {
		t.Errorf("TotalFiles: got %d, want 3", result.TotalFiles)
	}
}

// =============================================================================
// Unit Tests: buildWorkloadData
// =============================================================================

func TestBuildWorkloadData_EmptyRefs(t *testing.T) {
	refs, summary := buildWorkloadData("sha256:abc123", WorkloadRefMap{})
	if refs != nil {
		t.Error("expected nil refs for empty map")
	}
	if summary.TotalWorkloads != 0 {
		t.Error("expected zero total workloads")
	}
}

func TestBuildWorkloadData_SingleRef(t *testing.T) {
	workloadRefs := WorkloadRefMap{
		"sha256:abc123": {
			{
				Namespace:      "default",
				WorkloadType:   "Deployment",
				WorkloadName:   "nginx",
				WorkloadUID:    "uid-123",
				ContainerNames: map[string]struct{}{"web": {}, "sidecar": {}},
			},
		},
	}

	refs, summary := buildWorkloadData("sha256:abc123", workloadRefs)

	if len(refs) != 1 {
		t.Fatalf("expected 1 ref, got %d", len(refs))
	}
	if refs[0].Namespace != "default" {
		t.Errorf("namespace: got %q", refs[0].Namespace)
	}
	if refs[0].WorkloadType != "Deployment" {
		t.Errorf("workloadType: got %q", refs[0].WorkloadType)
	}
	if len(refs[0].ContainerNames) != 2 {
		t.Errorf("containerNames: got %d, want 2", len(refs[0].ContainerNames))
	}
	if summary.TotalContainers != 2 {
		t.Errorf("totalContainers: got %d, want 2", summary.TotalContainers)
	}
	if summary.TotalWorkloads != 1 {
		t.Errorf("totalWorkloads: got %d, want 1", summary.TotalWorkloads)
	}
	if len(summary.Namespaces) != 1 || summary.Namespaces[0] != "default" {
		t.Errorf("namespaces: got %v", summary.Namespaces)
	}
}

func TestBuildWorkloadData_MultipleNamespaces(t *testing.T) {
	workloadRefs := WorkloadRefMap{
		"sha256:abc123": {
			{
				Namespace:      "prod",
				WorkloadType:   "Deployment",
				WorkloadName:   "app",
				WorkloadUID:    "uid-1",
				ContainerNames: map[string]struct{}{"main": {}},
			},
			{
				Namespace:      "staging",
				WorkloadType:   "Deployment",
				WorkloadName:   "app",
				WorkloadUID:    "uid-2",
				ContainerNames: map[string]struct{}{"main": {}},
			},
			{
				Namespace:      "prod",
				WorkloadType:   "DaemonSet",
				WorkloadName:   "monitor",
				WorkloadUID:    "uid-3",
				ContainerNames: map[string]struct{}{"agent": {}, "sidecar": {}, "exporter": {}},
			},
		},
	}

	refs, summary := buildWorkloadData("sha256:abc123", workloadRefs)

	if len(refs) != 3 {
		t.Fatalf("expected 3 refs, got %d", len(refs))
	}

	// Should be sorted by container count desc → DaemonSet (3) first.
	if refs[0].WorkloadName != "monitor" {
		t.Errorf("first ref should be monitor (3 containers), got %s (%d containers)",
			refs[0].WorkloadName, len(refs[0].ContainerNames))
	}

	if summary.TotalWorkloads != 3 {
		t.Errorf("totalWorkloads: got %d, want 3", summary.TotalWorkloads)
	}
	if summary.TotalContainers != 5 {
		t.Errorf("totalContainers: got %d, want 5", summary.TotalContainers)
	}

	// Namespaces should be sorted.
	if len(summary.Namespaces) != 2 {
		t.Fatalf("expected 2 namespaces, got %d", len(summary.Namespaces))
	}
	if summary.Namespaces[0] != "prod" || summary.Namespaces[1] != "staging" {
		t.Errorf("namespaces: got %v, want [prod staging]", summary.Namespaces)
	}
}

func TestBuildWorkloadData_WrongDigest(t *testing.T) {
	workloadRefs := WorkloadRefMap{
		"sha256:abc123": {
			{
				Namespace:      "default",
				WorkloadType:   "Deployment",
				WorkloadName:   "nginx",
				WorkloadUID:    "uid-123",
				ContainerNames: map[string]struct{}{"web": {}},
			},
		},
	}

	refs, summary := buildWorkloadData("sha256:different", workloadRefs)
	if refs != nil {
		t.Error("expected nil refs for non-matching digest")
	}
	if summary.TotalWorkloads != 0 {
		t.Error("expected zero total workloads")
	}
}

func TestBuildWorkloadData_ContainerNamesSorted(t *testing.T) {
	workloadRefs := WorkloadRefMap{
		"sha256:abc123": {
			{
				Namespace:      "default",
				WorkloadType:   "Deployment",
				WorkloadName:   "app",
				WorkloadUID:    "uid-1",
				ContainerNames: map[string]struct{}{"zebra": {}, "alpha": {}, "mid": {}},
			},
		},
	}

	refs, _ := buildWorkloadData("sha256:abc123", workloadRefs)
	if len(refs) != 1 {
		t.Fatal("expected 1 ref")
	}

	names := refs[0].ContainerNames
	if len(names) != 3 {
		t.Fatalf("expected 3 names, got %d", len(names))
	}
	if names[0] != "alpha" || names[1] != "mid" || names[2] != "zebra" {
		t.Errorf("expected [alpha mid zebra], got %v", names)
	}
}

// =============================================================================
// Unit Tests: evaluateThresholds
// =============================================================================

func TestEvaluateThresholds_AllPass(t *testing.T) {
	rc := &ResultCollector{
		config: ImageAnalysisConfig{
			LowestEfficiency:         0.9,
			HighestWastedBytes:       "50MB",
			HighestUserWastedPercent: 0.1,
		},
	}

	analysis := v1.ImageAnalysis{
		Efficiency:        "0.95",
		WastedBytes:       1000000, // 1MB < 50MB
		UserWastedPercent: "0.01",  // 1% < 10%
	}

	if !rc.evaluateThresholds(analysis) {
		t.Error("expected pass")
	}
}

func TestEvaluateThresholds_FailEfficiency(t *testing.T) {
	rc := &ResultCollector{
		config: ImageAnalysisConfig{
			LowestEfficiency:         0.9,
			HighestWastedBytes:       "50MB",
			HighestUserWastedPercent: 0.1,
		},
	}

	analysis := v1.ImageAnalysis{
		Efficiency:        "0.85", // Below 0.9
		WastedBytes:       1000000,
		UserWastedPercent: "0.01",
	}

	if rc.evaluateThresholds(analysis) {
		t.Error("expected fail due to low efficiency")
	}
}

func TestEvaluateThresholds_FailWastedBytes(t *testing.T) {
	rc := &ResultCollector{
		config: ImageAnalysisConfig{
			LowestEfficiency:         0.9,
			HighestWastedBytes:       "50MB",
			HighestUserWastedPercent: 0.1,
		},
	}

	analysis := v1.ImageAnalysis{
		Efficiency:        "0.95",
		WastedBytes:       60000000, // 60MB > 50MB
		UserWastedPercent: "0.01",
	}

	if rc.evaluateThresholds(analysis) {
		t.Error("expected fail due to high wasted bytes")
	}
}

func TestEvaluateThresholds_FailUserWastedPercent(t *testing.T) {
	rc := &ResultCollector{
		config: ImageAnalysisConfig{
			LowestEfficiency:         0.9,
			HighestWastedBytes:       "50MB",
			HighestUserWastedPercent: 0.1,
		},
	}

	analysis := v1.ImageAnalysis{
		Efficiency:        "0.95",
		WastedBytes:       1000000,
		UserWastedPercent: "0.15", // 15% > 10%
	}

	if rc.evaluateThresholds(analysis) {
		t.Error("expected fail due to high user wasted percent")
	}
}

func TestEvaluateThresholds_ZeroThresholds(t *testing.T) {
	// Zero thresholds: maxWastedBytes=0 means no check.
	rc := &ResultCollector{
		config: ImageAnalysisConfig{
			LowestEfficiency:         0.0,
			HighestWastedBytes:       "",
			HighestUserWastedPercent: 0.0,
		},
	}

	analysis := v1.ImageAnalysis{
		Efficiency:        "0.5",
		WastedBytes:       999999999,
		UserWastedPercent: "0",
	}

	// Everything >= 0 for efficiency, no wasted bytes check, userWasted=0 is not > 0.
	if !rc.evaluateThresholds(analysis) {
		t.Error("expected pass with zero thresholds")
	}
}

// =============================================================================
// Unit Tests: findLatestPod
// =============================================================================

func TestFindLatestPod_Empty(t *testing.T) {
	pod := findLatestPod(nil)
	if pod != nil {
		t.Error("expected nil for empty slice")
	}
}

func TestFindLatestPod_Single(t *testing.T) {
	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-1"}},
	}
	pod := findLatestPod(pods)
	if pod.Name != "pod-1" {
		t.Errorf("expected pod-1, got %s", pod.Name)
	}
}

func TestFindLatestPod_Multiple(t *testing.T) {
	now := metav1.Now()
	earlier := metav1.NewTime(now.Add(-60 * 1e9))
	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-old", CreationTimestamp: earlier}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-new", CreationTimestamp: now}},
	}
	pod := findLatestPod(pods)
	if pod.Name != "pod-new" {
		t.Errorf("expected pod-new, got %s", pod.Name)
	}
}

// =============================================================================
// Unit Tests: Dive JSON types (marshal/unmarshal)
// =============================================================================

func TestBatchImageResult_Unmarshal(t *testing.T) {
	jsonLine := `{"digest":"sha256:abc123","ref":"docker.io/nginx:1.25","source":"local-containerd","durationMs":5000,"error":"","result":{"image":{"sizeBytes":50000000,"inefficientBytes":500000,"efficiencyScore":0.99,"fileReference":[{"count":2,"sizeBytes":1024,"file":"/tmp/cache"}]},"layer":[{"index":0,"id":"layer0","digestId":"sha256:layer0","sizeBytes":30000000,"command":"FROM alpine","fileList":[]}]}}`

	var result BatchImageResult
	if err := json.Unmarshal([]byte(jsonLine), &result); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if result.Digest != "sha256:abc123" {
		t.Errorf("digest: got %q", result.Digest)
	}
	if result.Source != "local-containerd" {
		t.Errorf("source: got %q", result.Source)
	}
	if result.DurationMs != 5000 {
		t.Errorf("durationMs: got %d", result.DurationMs)
	}
	if result.Result == nil {
		t.Fatal("result should not be nil")
	}
	if result.Result.Image.SizeBytes != 50000000 {
		t.Errorf("sizeBytes: got %d", result.Result.Image.SizeBytes)
	}
	if result.Result.Image.EfficiencyScore != 0.99 {
		t.Errorf("efficiencyScore: got %f", result.Result.Image.EfficiencyScore)
	}
	if len(result.Result.Image.FileReference) != 1 {
		t.Fatalf("fileReference count: got %d", len(result.Result.Image.FileReference))
	}
	if result.Result.Image.FileReference[0].File != "/tmp/cache" {
		t.Errorf("fileRef file: got %q", result.Result.Image.FileReference[0].File)
	}
	if len(result.Result.Layer) != 1 {
		t.Fatalf("layer count: got %d", len(result.Result.Layer))
	}
	if result.Result.Layer[0].Command != "FROM alpine" {
		t.Errorf("layer command: got %q", result.Result.Layer[0].Command)
	}
}

func TestBatchImageResult_FailedImage(t *testing.T) {
	jsonLine := `{"digest":"sha256:def456","ref":"private.registry/app:v1","source":"failed","durationMs":1000,"error":"image acquisition failed","result":null}`

	var result BatchImageResult
	if err := json.Unmarshal([]byte(jsonLine), &result); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if result.Source != "failed" {
		t.Errorf("source: got %q", result.Source)
	}
	if result.Error != "image acquisition failed" {
		t.Errorf("error: got %q", result.Error)
	}
	if result.Result != nil {
		t.Error("result should be nil for failed image")
	}
}

// =============================================================================
// Unit Tests: buildSpec
// =============================================================================

func TestBuildSpec_FullResult(t *testing.T) {
	rc := &ResultCollector{
		config: ImageAnalysisConfig{
			LowestEfficiency:         0.9,
			HighestWastedBytes:       "50MB",
			HighestUserWastedPercent: 0.1,
		},
	}

	imgResult := BatchImageResult{
		Digest:     "sha256:abc123def456",
		Ref:        "docker.io/library/nginx:1.25",
		Source:     "local-containerd",
		DurationMs: 3000,
		Result: &DiveResult{
			Image: DiveImage{
				SizeBytes:        100000000,
				InefficientBytes: 5000000,
				EfficiencyScore:  0.95,
				FileReference: []DiveFileRef{
					{Count: 2, SizeBytes: 2000000, File: "/var/cache/apt"},
					{Count: 1, SizeBytes: 1000000, File: "/tmp/build"},
					{Count: 3, SizeBytes: 3000000, File: "/root/.cache"},
				},
			},
			Layer: []DiveLayer{
				{Index: 0, DigestID: "sha256:base", SizeBytes: 50000000, Command: "FROM alpine:3.21"},
				{Index: 1, DigestID: "sha256:layer1", SizeBytes: 30000000, Command: "RUN apk add --no-cache bash jq coreutils"},
				{Index: 2, DigestID: "sha256:layer2", SizeBytes: 20000000, Command: "COPY --from=gcr.io/go-containerregistry/crane:latest /ko-app/crane /usr/local/bin/crane"},
			},
		},
	}

	workloadRefs := WorkloadRefMap{
		"sha256:abc123def456": {
			{
				Namespace:      "production",
				WorkloadType:   "Deployment",
				WorkloadName:   "web-server",
				WorkloadUID:    "uid-web",
				ContainerNames: map[string]struct{}{"nginx": {}},
			},
		},
	}

	spec, err := rc.buildSpec(imgResult, "analyze-job-1", "node-1", workloadRefs)
	if err != nil {
		t.Fatalf("buildSpec error: %v", err)
	}

	// Basic fields.
	if spec.ImageRef != "docker.io/library/nginx:1.25" {
		t.Errorf("imageRef: got %q", spec.ImageRef)
	}
	if spec.ImageDigest != "sha256:abc123def456" {
		t.Errorf("imageDigest: got %q", spec.ImageDigest)
	}
	if spec.ImageSizeBytes != 100000000 {
		t.Errorf("imageSizeBytes: got %d", spec.ImageSizeBytes)
	}

	// Source info.
	if spec.ImageSource.Type != "local-containerd" {
		t.Errorf("source type: got %q", spec.ImageSource.Type)
	}
	if spec.ImageSource.NodeName != "node-1" {
		t.Errorf("source node: got %q", spec.ImageSource.NodeName)
	}
	if spec.ImageSource.BatchJobName != "analyze-job-1" {
		t.Errorf("source job: got %q", spec.ImageSource.BatchJobName)
	}

	// Analysis.
	if spec.Analysis.Efficiency != "0.95" {
		t.Errorf("efficiency: got %q", spec.Analysis.Efficiency)
	}
	if spec.Analysis.WastedBytes != 5000000 {
		t.Errorf("wastedBytes: got %d", spec.Analysis.WastedBytes)
	}
	if spec.Analysis.UserWastedPercent != "0.05" {
		t.Errorf("userWastedPercent: got %q", spec.Analysis.UserWastedPercent)
	}
	if spec.Analysis.LayerCount != 3 {
		t.Errorf("layerCount: got %d", spec.Analysis.LayerCount)
	}
	if !spec.Analysis.Passed {
		t.Error("expected passed=true (efficiency 0.95 >= 0.9, wasted 5MB < 50MB, userWasted 5% < 10%)")
	}

	// Layers.
	if len(spec.Analysis.Layers) != 3 {
		t.Fatalf("layers count: got %d", len(spec.Analysis.Layers))
	}
	if spec.Analysis.Layers[0].Digest != "sha256:base" {
		t.Errorf("layer 0 digest: got %q", spec.Analysis.Layers[0].Digest)
	}

	// Wasted files: sorted by size desc, so /root/.cache (3MB) first.
	if len(spec.Analysis.WastedFiles) != 3 {
		t.Fatalf("wastedFiles count: got %d", len(spec.Analysis.WastedFiles))
	}
	if spec.Analysis.WastedFiles[0].Path != "/root/.cache" {
		t.Errorf("first wasted file: got %q", spec.Analysis.WastedFiles[0].Path)
	}
	if spec.Analysis.WastedFiles[0].SizeBytes != 3000000 {
		t.Errorf("first wasted file size: got %d", spec.Analysis.WastedFiles[0].SizeBytes)
	}

	// Workload references.
	if len(spec.WorkloadReferences) != 1 {
		t.Fatalf("workloadRefs count: got %d", len(spec.WorkloadReferences))
	}
	if spec.WorkloadReferences[0].WorkloadName != "web-server" {
		t.Errorf("workload name: got %q", spec.WorkloadReferences[0].WorkloadName)
	}
	if spec.WorkloadSummary.TotalWorkloads != 1 {
		t.Errorf("total workloads: got %d", spec.WorkloadSummary.TotalWorkloads)
	}
}

func TestBuildSpec_NilResult(t *testing.T) {
	rc := &ResultCollector{
		config: DefaultImageAnalysisConfig(),
	}

	imgResult := BatchImageResult{
		Digest: "sha256:abc123",
		Ref:    "nginx:latest",
		Result: nil,
	}

	_, err := rc.buildSpec(imgResult, "job-1", "node-1", WorkloadRefMap{})
	if err == nil {
		t.Error("expected error for nil result")
	}
}

func TestBuildSpec_LayerCommandTruncation(t *testing.T) {
	rc := &ResultCollector{
		config: DefaultImageAnalysisConfig(),
	}

	// Create a very long command.
	longCommand := ""
	for i := 0; i < 300; i++ {
		longCommand += "x"
	}

	imgResult := BatchImageResult{
		Digest: "sha256:abc123",
		Ref:    "nginx:latest",
		Source: "local-containerd",
		Result: &DiveResult{
			Image: DiveImage{SizeBytes: 100, EfficiencyScore: 1.0},
			Layer: []DiveLayer{
				{Index: 0, Command: longCommand, SizeBytes: 100},
			},
		},
	}

	spec, err := rc.buildSpec(imgResult, "job-1", "node-1", WorkloadRefMap{})
	if err != nil {
		t.Fatalf("buildSpec error: %v", err)
	}

	if len(spec.Analysis.Layers[0].Command) != maxCommandLength {
		t.Errorf("command length: got %d, want %d", len(spec.Analysis.Layers[0].Command), maxCommandLength)
	}
}

func TestBuildSpec_WastedFilesCapped(t *testing.T) {
	rc := &ResultCollector{
		config: DefaultImageAnalysisConfig(),
	}

	// Create more than maxWastedFiles file references.
	fileRefs := make([]DiveFileRef, 100)
	for i := 0; i < 100; i++ {
		fileRefs[i] = DiveFileRef{
			Count:     1,
			SizeBytes: uint64(100 - i), // descending to test sort
			File:      fmt.Sprintf("/file-%d", i),
		}
	}

	imgResult := BatchImageResult{
		Digest: "sha256:abc123",
		Ref:    "nginx:latest",
		Source: "local-containerd",
		Result: &DiveResult{
			Image: DiveImage{
				SizeBytes:       100000,
				EfficiencyScore: 0.95,
				FileReference:   fileRefs,
			},
			Layer: []DiveLayer{{Index: 0, SizeBytes: 100000}},
		},
	}

	spec, err := rc.buildSpec(imgResult, "job-1", "node-1", WorkloadRefMap{})
	if err != nil {
		t.Fatalf("buildSpec error: %v", err)
	}

	if len(spec.Analysis.WastedFiles) != maxWastedFiles {
		t.Errorf("wasted files count: got %d, want %d", len(spec.Analysis.WastedFiles), maxWastedFiles)
	}

	// Verify sorted by size desc — first should be the largest.
	if spec.Analysis.WastedFiles[0].SizeBytes != 100 {
		t.Errorf("first wasted file size: got %d, want 100", spec.Analysis.WastedFiles[0].SizeBytes)
	}
}

// =============================================================================
// Unit Tests: NDJSON parsing edge cases (via classification logic)
// =============================================================================

func TestCollectionResult_Classification(t *testing.T) {
	// Simulate the classification logic from CollectFromJob.
	tests := []struct {
		name     string
		result   BatchImageResult
		wantCat  string // "succeeded" or "failed"
	}{
		{
			name:    "local-containerd success",
			result:  BatchImageResult{Source: "local-containerd", Error: ""},
			wantCat: "succeeded",
		},
		{
			name:    "remote-pull success",
			result:  BatchImageResult{Source: "remote-pull", Error: ""},
			wantCat: "succeeded",
		},
		{
			name:    "failed source",
			result:  BatchImageResult{Source: "failed", Error: "acquisition failed"},
			wantCat: "failed",
		},
		{
			name:    "success source with error",
			result:  BatchImageResult{Source: "local-containerd", Error: "dive failed"},
			wantCat: "failed",
		},
		{
			name:    "unknown source no error",
			result:  BatchImageResult{Source: "unknown", Error: ""},
			wantCat: "succeeded",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cr := &CollectionResult{}

			switch {
			case tt.result.Source == "failed" || tt.result.Error != "":
				cr.Failed = append(cr.Failed, tt.result)
			case tt.result.Source == "local-containerd":
				cr.Succeeded = append(cr.Succeeded, tt.result)
				cr.LocalContainerd++
			case tt.result.Source == "remote-pull":
				cr.Succeeded = append(cr.Succeeded, tt.result)
				cr.RemotePull++
			default:
				cr.Succeeded = append(cr.Succeeded, tt.result)
			}

			if tt.wantCat == "succeeded" && len(cr.Succeeded) != 1 {
				t.Errorf("expected 1 succeeded, got %d", len(cr.Succeeded))
			}
			if tt.wantCat == "failed" && len(cr.Failed) != 1 {
				t.Errorf("expected 1 failed, got %d", len(cr.Failed))
			}
		})
	}
}
