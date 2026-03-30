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
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func jobManagerConfig() ImageAnalysisConfig {
	cfg := DefaultImageAnalysisConfig()
	cfg.ExcludedNamespaces = nil
	cfg.ExcludedImages = nil
	return cfg
}

func makeNodeImageMap(nodes map[string]int) NodeImageMap {
	nim := make(NodeImageMap)
	for nodeName, imageCount := range nodes {
		batch := &NodeBatch{NodeName: nodeName}
		for i := 0; i < imageCount; i++ {
			batch.Images = append(batch.Images, ImageInfo{
				Digest: fmt.Sprintf("%s-img-%d@sha256:digest%d", nodeName, i, i),
				Ref:    fmt.Sprintf("registry.example.com/%s-img-%d:v1", nodeName, i),
			})
		}
		nim[nodeName] = batch
	}
	return nim
}

// --- GenerateScanID ---

func TestGenerateScanID(t *testing.T) {
	id := GenerateScanID()
	assert.NotEmpty(t, id)

	// Should be a valid Unix timestamp (all digits).
	_, err := strconv.ParseInt(id, 10, 64)
	assert.NoError(t, err)

	// Should be reasonably recent (after 2020).
	ts, _ := strconv.ParseInt(id, 10, 64)
	assert.Greater(t, ts, int64(1577836800)) // 2020-01-01
}

// --- PlanBatchJobs ---

func TestPlanBatchJobs_BasicSplitting(t *testing.T) {
	cfg := jobManagerConfig()
	cfg.BatchSize = 10
	client := fake.NewSimpleClientset()
	mgr := NewJobManager(client, cfg, logr.Discard())

	nim := makeNodeImageMap(map[string]int{
		"node-1": 25,
	})
	scanID := "1709420400"

	specs := mgr.PlanBatchJobs(nim, scanID)

	// 25 images / 10 per batch = 3 batches
	assert.Len(t, specs, 3)

	assert.Equal(t, 0, specs[0].BatchIndex)
	assert.Len(t, specs[0].Images, 10)

	assert.Equal(t, 1, specs[1].BatchIndex)
	assert.Len(t, specs[1].Images, 10)

	assert.Equal(t, 2, specs[2].BatchIndex)
	assert.Len(t, specs[2].Images, 5)

	// All specs should reference the same node and scan ID.
	for _, s := range specs {
		assert.Equal(t, "node-1", s.NodeName)
		assert.Equal(t, scanID, s.ScanID)
	}
}

func TestPlanBatchJobs_MultipleNodes_SortedByImageCount(t *testing.T) {
	cfg := jobManagerConfig()
	cfg.BatchSize = 5
	client := fake.NewSimpleClientset()
	mgr := NewJobManager(client, cfg, logr.Discard())

	nim := makeNodeImageMap(map[string]int{
		"small-node": 3,
		"big-node":   12,
		"mid-node":   7,
	})
	scanID := "1709420400"

	specs := mgr.PlanBatchJobs(nim, scanID)

	// big-node: 12/5 = 3 batches, mid-node: 7/5 = 2 batches, small-node: 3/5 = 1 batch
	assert.Len(t, specs, 6)

	// First specs should be from the biggest node.
	assert.Equal(t, "big-node", specs[0].NodeName)
	assert.Equal(t, "big-node", specs[1].NodeName)
	assert.Equal(t, "big-node", specs[2].NodeName)

	// Then mid-node.
	assert.Equal(t, "mid-node", specs[3].NodeName)
	assert.Equal(t, "mid-node", specs[4].NodeName)

	// Then small-node.
	assert.Equal(t, "small-node", specs[5].NodeName)
}

func TestPlanBatchJobs_SingleImagePerBatch(t *testing.T) {
	cfg := jobManagerConfig()
	cfg.BatchSize = 1
	client := fake.NewSimpleClientset()
	mgr := NewJobManager(client, cfg, logr.Discard())

	nim := makeNodeImageMap(map[string]int{"node-1": 3})

	specs := mgr.PlanBatchJobs(nim, "123")
	assert.Len(t, specs, 3)
	for _, s := range specs {
		assert.Len(t, s.Images, 1)
	}
}

func TestPlanBatchJobs_ExactBatchSize(t *testing.T) {
	cfg := jobManagerConfig()
	cfg.BatchSize = 10
	client := fake.NewSimpleClientset()
	mgr := NewJobManager(client, cfg, logr.Discard())

	nim := makeNodeImageMap(map[string]int{"node-1": 10})

	specs := mgr.PlanBatchJobs(nim, "123")
	assert.Len(t, specs, 1)
	assert.Len(t, specs[0].Images, 10)
}

func TestPlanBatchJobs_EmptyNodeMap(t *testing.T) {
	cfg := jobManagerConfig()
	client := fake.NewSimpleClientset()
	mgr := NewJobManager(client, cfg, logr.Discard())

	specs := mgr.PlanBatchJobs(NodeImageMap{}, "123")
	assert.Empty(t, specs)
}

// --- generateJobName ---

func TestGenerateJobName_Basic(t *testing.T) {
	name := generateJobName("worker-01", 3, "1709420400")
	assert.Equal(t, "dive-worker-01-b3-1709420400", name)
	assert.LessOrEqual(t, len(name), maxJobNameLength)
}

func TestGenerateJobName_LongNodeName(t *testing.T) {
	longNode := strings.Repeat("a", 100)
	name := generateJobName(longNode, 0, "1709420400")
	assert.LessOrEqual(t, len(name), maxJobNameLength)
	assert.True(t, strings.HasPrefix(name, "dive-"))
	assert.True(t, strings.HasSuffix(name, "-b0-1709420400"))
}

func TestGenerateJobName_SpecialChars(t *testing.T) {
	name := generateJobName("ip-10-0-1-5.ec2.internal", 2, "12345")
	assert.Equal(t, "dive-ip-10-0-1-5-ec2-internal-b2-12345", name)
	assert.LessOrEqual(t, len(name), maxJobNameLength)
}

func TestGenerateJobName_UpperCase(t *testing.T) {
	name := generateJobName("Worker-Node-01", 0, "100")
	assert.Equal(t, "dive-worker-node-01-b0-100", name)
}

// --- sanitizeDNSName ---

func TestSanitizeDNSName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"worker-01", "worker-01"},
		{"Worker_Node.01", "worker-node-01"},
		{"ip-10-0-1-5.ec2.internal", "ip-10-0-1-5-ec2-internal"},
		{"-leading-", "leading"},
		{"UPPERCASE", "uppercase"},
		{"a--b---c", "a-b-c"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.expected, sanitizeDNSName(tt.input))
		})
	}
}

// --- sanitizeLabelValue ---

func TestSanitizeLabelValue_Long(t *testing.T) {
	long := strings.Repeat("x", 100)
	result := sanitizeLabelValue(long)
	assert.LessOrEqual(t, len(result), 63)
}

// --- parseDurationToSeconds ---

func TestParseDurationToSeconds(t *testing.T) {
	assert.Equal(t, 300, parseDurationToSeconds("5m"))
	assert.Equal(t, 60, parseDurationToSeconds("1m"))
	assert.Equal(t, 600, parseDurationToSeconds("10m"))
	assert.Equal(t, 30, parseDurationToSeconds("30s"))
	assert.Equal(t, 300, parseDurationToSeconds("invalid")) // fallback
	assert.Equal(t, 300, parseDurationToSeconds(""))        // fallback
	assert.Equal(t, 3600, parseDurationToSeconds("1h"))
}

// --- buildJobObject ---

func TestBuildJobObject_Structure(t *testing.T) {
	cfg := jobManagerConfig()
	cfg.RegistryAuthSecret = "my-registry-secret"
	client := fake.NewSimpleClientset()
	mgr := NewJobManager(client, cfg, logr.Discard())

	spec := BatchJobSpec{
		JobName:    "dive-node1-b0-123",
		NodeName:   "node-1",
		BatchIndex: 0,
		ScanID:     "123",
		Images: []ImageInfo{
			{Digest: "nginx@sha256:aaa", Ref: "nginx:1.25"},
			{Digest: "redis@sha256:bbb", Ref: "redis:7.2"},
		},
	}

	job, err := mgr.buildJobObject(spec)
	require.NoError(t, err)

	// Metadata.
	assert.Equal(t, "dive-node1-b0-123", job.Name)
	assert.Equal(t, "devzero-zxporter", job.Namespace)
	assert.Equal(t, LabelComponentValue, job.Labels[LabelComponent])
	assert.Equal(t, "123", job.Labels[LabelScanID])
	assert.Equal(t, "0", job.Labels[LabelBatchIndex])

	// Spec.
	assert.Equal(t, int32(2), *job.Spec.BackoffLimit)         // MaxRetries default
	assert.Equal(t, int64(1800), *job.Spec.ActiveDeadlineSeconds) // 30 min
	assert.Equal(t, int32(3600), *job.Spec.TTLSecondsAfterFinished)

	// Pod spec.
	podSpec := job.Spec.Template.Spec
	assert.Equal(t, corev1.RestartPolicyNever, podSpec.RestartPolicy)
	assert.Equal(t, "image-analyzer", podSpec.ServiceAccountName)
	assert.Equal(t, "node-1", podSpec.NodeName)

	// Container.
	require.Len(t, podSpec.Containers, 1)
	container := podSpec.Containers[0]
	assert.Equal(t, "analyzer", container.Name)
	assert.Equal(t, cfg.AnalyzerImage, container.Image)

	// Env vars.
	envMap := make(map[string]string)
	for _, e := range container.Env {
		envMap[e.Name] = e.Value
	}
	assert.Contains(t, envMap, "IMAGES_JSON")
	assert.Equal(t, "true", envMap["PREFER_LOCAL"])
	assert.Equal(t, "true", envMap["FALLBACK_REMOTE"])
	assert.Equal(t, "/run/containerd/containerd.sock", envMap["CONTAINERD_SOCK"])
	assert.Equal(t, "k8s.io", envMap["CONTAINERD_NS"])
	assert.Equal(t, "300", envMap["REMOTE_PULL_TIMEOUT"])

	// Verify IMAGES_JSON is valid JSON with correct images.
	var images []struct {
		Digest string `json:"digest"`
		Ref    string `json:"ref"`
	}
	err = json.Unmarshal([]byte(envMap["IMAGES_JSON"]), &images)
	require.NoError(t, err)
	assert.Len(t, images, 2)
	assert.Equal(t, "nginx@sha256:aaa", images[0].Digest)
	assert.Equal(t, "redis:7.2", images[1].Ref)

	// Resources.
	assert.Equal(t, "500m", container.Resources.Requests.Cpu().String())
	assert.Equal(t, "1", container.Resources.Limits.Cpu().String())
	assert.Equal(t, "1Gi", container.Resources.Requests.Memory().String())
	assert.Equal(t, "4Gi", container.Resources.Limits.Memory().String())

	// Security context.
	require.NotNil(t, container.SecurityContext)
	assert.Equal(t, int64(0), *container.SecurityContext.RunAsUser)
	assert.False(t, *container.SecurityContext.AllowPrivilegeEscalation)

	// Volumes — should have 3 (containerd-sock, workspace, registry-auth).
	assert.Len(t, podSpec.Volumes, 3)
	assert.Len(t, container.VolumeMounts, 3)

	// Verify containerd socket volume.
	var sockVol *corev1.Volume
	for i := range podSpec.Volumes {
		if podSpec.Volumes[i].Name == "containerd-sock" {
			sockVol = &podSpec.Volumes[i]
		}
	}
	require.NotNil(t, sockVol)
	assert.Equal(t, "/run/containerd/containerd.sock", sockVol.HostPath.Path)
	assert.Equal(t, corev1.HostPathSocket, *sockVol.HostPath.Type)

	// Verify workspace volume.
	var wsVol *corev1.Volume
	for i := range podSpec.Volumes {
		if podSpec.Volumes[i].Name == "workspace" {
			wsVol = &podSpec.Volumes[i]
		}
	}
	require.NotNil(t, wsVol)
	assert.Equal(t, "10Gi", wsVol.EmptyDir.SizeLimit.String())

	// Verify registry auth volume.
	var authVol *corev1.Volume
	for i := range podSpec.Volumes {
		if podSpec.Volumes[i].Name == "registry-auth" {
			authVol = &podSpec.Volumes[i]
		}
	}
	require.NotNil(t, authVol)
	assert.Equal(t, "my-registry-secret", authVol.Secret.SecretName)
	assert.True(t, *authVol.Secret.Optional)
}

func TestBuildJobObject_NoRegistrySecret(t *testing.T) {
	cfg := jobManagerConfig()
	cfg.RegistryAuthSecret = "" // no secret
	client := fake.NewSimpleClientset()
	mgr := NewJobManager(client, cfg, logr.Discard())

	spec := BatchJobSpec{
		JobName: "dive-test-b0-1", NodeName: "node-1",
		Images: []ImageInfo{{Digest: "a@sha256:x", Ref: "a:v1"}},
	}

	job, err := mgr.buildJobObject(spec)
	require.NoError(t, err)

	// Should only have 2 volumes (no registry-auth).
	assert.Len(t, job.Spec.Template.Spec.Volumes, 2)
	assert.Len(t, job.Spec.Template.Spec.Containers[0].VolumeMounts, 2)
}

func TestBuildJobObject_WithTolerations(t *testing.T) {
	cfg := jobManagerConfig()
	cfg.JobTolerations = []corev1.Toleration{
		{Key: "dedicated", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
	}
	client := fake.NewSimpleClientset()
	mgr := NewJobManager(client, cfg, logr.Discard())

	spec := BatchJobSpec{
		JobName: "dive-test-b0-1", NodeName: "node-1",
		Images: []ImageInfo{{Digest: "a@sha256:x", Ref: "a:v1"}},
	}

	job, err := mgr.buildJobObject(spec)
	require.NoError(t, err)

	require.Len(t, job.Spec.Template.Spec.Tolerations, 1)
	assert.Equal(t, "dedicated", job.Spec.Template.Spec.Tolerations[0].Key)
}

// --- CreateJob ---

func TestCreateJob_Success(t *testing.T) {
	cfg := jobManagerConfig()
	client := fake.NewSimpleClientset()
	mgr := NewJobManager(client, cfg, logr.Discard())

	spec := BatchJobSpec{
		JobName:    "dive-node1-b0-123",
		NodeName:   "node-1",
		BatchIndex: 0,
		ScanID:     "123",
		Images: []ImageInfo{
			{Digest: "nginx@sha256:aaa", Ref: "nginx:1.25"},
		},
	}

	job, err := mgr.CreateJob(context.Background(), spec)
	require.NoError(t, err)
	assert.Equal(t, "dive-node1-b0-123", job.Name)

	// Verify it exists in the fake client.
	fetched, err := client.BatchV1().Jobs(cfg.JobNamespace).Get(
		context.Background(), "dive-node1-b0-123", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "dive-node1-b0-123", fetched.Name)
}

// --- SubmitBatches ---

func TestSubmitBatches_RespectsClusterLimit(t *testing.T) {
	cfg := jobManagerConfig()
	cfg.MaxConcurrentJobs = 3
	cfg.MaxJobsPerNode = 10 // high per-node limit so it doesn't interfere
	client := fake.NewSimpleClientset()
	mgr := NewJobManager(client, cfg, logr.Discard())

	// Create 5 specs all on different nodes.
	specs := make([]BatchJobSpec, 5)
	for i := 0; i < 5; i++ {
		specs[i] = BatchJobSpec{
			JobName:    fmt.Sprintf("dive-node%d-b0-123", i),
			NodeName:   fmt.Sprintf("node-%d", i),
			BatchIndex: 0,
			ScanID:     "123",
			Images:     []ImageInfo{{Digest: fmt.Sprintf("img%d@sha256:x", i), Ref: fmt.Sprintf("img%d:v1", i)}},
		}
	}

	created, err := mgr.SubmitBatches(context.Background(), specs)
	require.NoError(t, err)

	// Only 3 should be created (cluster limit).
	assert.Len(t, created, 3)
}

func TestSubmitBatches_RespectsPerNodeLimit(t *testing.T) {
	cfg := jobManagerConfig()
	cfg.MaxConcurrentJobs = 100 // high cluster limit
	cfg.MaxJobsPerNode = 1
	client := fake.NewSimpleClientset()
	mgr := NewJobManager(client, cfg, logr.Discard())

	// 3 specs on the same node.
	specs := make([]BatchJobSpec, 3)
	for i := 0; i < 3; i++ {
		specs[i] = BatchJobSpec{
			JobName:    fmt.Sprintf("dive-node1-b%d-123", i),
			NodeName:   "node-1",
			BatchIndex: i,
			ScanID:     "123",
			Images:     []ImageInfo{{Digest: fmt.Sprintf("img%d@sha256:x", i), Ref: fmt.Sprintf("img%d:v1", i)}},
		}
	}

	created, err := mgr.SubmitBatches(context.Background(), specs)
	require.NoError(t, err)

	// Only 1 should be created (per-node limit).
	assert.Len(t, created, 1)
}

func TestSubmitBatches_MixedNodes(t *testing.T) {
	cfg := jobManagerConfig()
	cfg.MaxConcurrentJobs = 5
	cfg.MaxJobsPerNode = 1
	client := fake.NewSimpleClientset()
	mgr := NewJobManager(client, cfg, logr.Discard())

	// 2 batches on node-1, 2 on node-2, 1 on node-3.
	specs := []BatchJobSpec{
		{JobName: "dive-n1-b0-1", NodeName: "node-1", ScanID: "1", Images: []ImageInfo{{Digest: "a@sha256:1", Ref: "a:1"}}},
		{JobName: "dive-n1-b1-1", NodeName: "node-1", ScanID: "1", Images: []ImageInfo{{Digest: "b@sha256:2", Ref: "b:1"}}},
		{JobName: "dive-n2-b0-1", NodeName: "node-2", ScanID: "1", Images: []ImageInfo{{Digest: "c@sha256:3", Ref: "c:1"}}},
		{JobName: "dive-n2-b1-1", NodeName: "node-2", ScanID: "1", Images: []ImageInfo{{Digest: "d@sha256:4", Ref: "d:1"}}},
		{JobName: "dive-n3-b0-1", NodeName: "node-3", ScanID: "1", Images: []ImageInfo{{Digest: "e@sha256:5", Ref: "e:1"}}},
	}

	created, err := mgr.SubmitBatches(context.Background(), specs)
	require.NoError(t, err)

	// Per-node=1: node-1 gets 1, node-2 gets 1, node-3 gets 1 = 3 total.
	assert.Len(t, created, 3)

	nodesSeen := map[string]int{}
	for _, j := range created {
		nodesSeen[j.Spec.Template.Spec.NodeName]++
	}
	assert.Equal(t, 1, nodesSeen["node-1"])
	assert.Equal(t, 1, nodesSeen["node-2"])
	assert.Equal(t, 1, nodesSeen["node-3"])
}

func TestSubmitBatches_Empty(t *testing.T) {
	cfg := jobManagerConfig()
	client := fake.NewSimpleClientset()
	mgr := NewJobManager(client, cfg, logr.Discard())

	created, err := mgr.SubmitBatches(context.Background(), nil)
	require.NoError(t, err)
	assert.Nil(t, created)
}

func TestSubmitBatches_AccountsForExistingActiveJobs(t *testing.T) {
	cfg := jobManagerConfig()
	cfg.MaxConcurrentJobs = 3
	cfg.MaxJobsPerNode = 2

	// Pre-create 2 active Jobs for this scan.
	existingJob1 := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dive-node1-b0-123",
			Namespace: cfg.JobNamespace,
			Labels: map[string]string{
				LabelComponent:    LabelComponentValue,
				LabelScanID:       "123",
				LabelAnalysisNode: "node-1",
			},
		},
		Status: batchv1.JobStatus{Active: 1}, // still running
	}
	existingJob2 := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dive-node2-b0-123",
			Namespace: cfg.JobNamespace,
			Labels: map[string]string{
				LabelComponent:    LabelComponentValue,
				LabelScanID:       "123",
				LabelAnalysisNode: "node-2",
			},
		},
		Status: batchv1.JobStatus{Active: 1}, // still running
	}

	client := fake.NewSimpleClientset(existingJob1, existingJob2)
	mgr := NewJobManager(client, cfg, logr.Discard())

	// Try to submit 3 more.
	specs := []BatchJobSpec{
		{JobName: "dive-node3-b0-123", NodeName: "node-3", ScanID: "123", Images: []ImageInfo{{Digest: "a@sha256:1", Ref: "a:1"}}},
		{JobName: "dive-node4-b0-123", NodeName: "node-4", ScanID: "123", Images: []ImageInfo{{Digest: "b@sha256:2", Ref: "b:1"}}},
		{JobName: "dive-node5-b0-123", NodeName: "node-5", ScanID: "123", Images: []ImageInfo{{Digest: "c@sha256:3", Ref: "c:1"}}},
	}

	created, err := mgr.SubmitBatches(context.Background(), specs)
	require.NoError(t, err)

	// 2 existing active + max 3 = only 1 slot available.
	assert.Len(t, created, 1)
}

// --- GetJobStatus ---

func TestGetJobStatus(t *testing.T) {
	cfg := jobManagerConfig()

	jobs := []batchv1.Job{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "job-active", Namespace: cfg.JobNamespace,
				Labels: map[string]string{LabelComponent: LabelComponentValue, LabelScanID: "100"},
			},
			Status: batchv1.JobStatus{Active: 1},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "job-succeeded", Namespace: cfg.JobNamespace,
				Labels: map[string]string{LabelComponent: LabelComponentValue, LabelScanID: "100"},
			},
			Status: batchv1.JobStatus{Succeeded: 1},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "job-failed", Namespace: cfg.JobNamespace,
				Labels: map[string]string{LabelComponent: LabelComponentValue, LabelScanID: "100"},
			},
			Status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
				},
			},
		},
		{
			// Different scan — should not be counted.
			ObjectMeta: metav1.ObjectMeta{
				Name: "job-other-scan", Namespace: cfg.JobNamespace,
				Labels: map[string]string{LabelComponent: LabelComponentValue, LabelScanID: "999"},
			},
			Status: batchv1.JobStatus{Active: 1},
		},
	}

	jobList := &batchv1.JobList{Items: jobs}
	client := fake.NewSimpleClientset(jobList)
	mgr := NewJobManager(client, cfg, logr.Discard())

	active, succeeded, failed, err := mgr.GetJobStatus(context.Background(), "100")
	require.NoError(t, err)
	assert.Equal(t, 1, active)
	assert.Equal(t, 1, succeeded)
	assert.Equal(t, 1, failed)
}

// --- CleanupOldScanJobs ---

func TestCleanupOldScanJobs(t *testing.T) {
	cfg := jobManagerConfig()

	jobs := []batchv1.Job{
		{
			// Old scan, completed — should be deleted.
			ObjectMeta: metav1.ObjectMeta{
				Name: "old-completed", Namespace: cfg.JobNamespace,
				Labels: map[string]string{LabelComponent: LabelComponentValue, LabelScanID: "old"},
			},
			Status: batchv1.JobStatus{Succeeded: 1},
		},
		{
			// Old scan, failed — should be deleted.
			ObjectMeta: metav1.ObjectMeta{
				Name: "old-failed", Namespace: cfg.JobNamespace,
				Labels: map[string]string{LabelComponent: LabelComponentValue, LabelScanID: "old"},
			},
			Status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
				},
			},
		},
		{
			// Old scan, still active — should NOT be deleted.
			ObjectMeta: metav1.ObjectMeta{
				Name: "old-active", Namespace: cfg.JobNamespace,
				Labels: map[string]string{LabelComponent: LabelComponentValue, LabelScanID: "old"},
			},
			Status: batchv1.JobStatus{Active: 1},
		},
		{
			// Current scan — should NOT be deleted.
			ObjectMeta: metav1.ObjectMeta{
				Name: "current-job", Namespace: cfg.JobNamespace,
				Labels: map[string]string{LabelComponent: LabelComponentValue, LabelScanID: "current"},
			},
			Status: batchv1.JobStatus{Succeeded: 1},
		},
	}

	jobList := &batchv1.JobList{Items: jobs}
	client := fake.NewSimpleClientset(jobList)
	mgr := NewJobManager(client, cfg, logr.Discard())

	deleted, err := mgr.CleanupOldScanJobs(context.Background(), "current")
	require.NoError(t, err)
	assert.Equal(t, 2, deleted) // old-completed + old-failed

	// Verify what remains.
	remaining, err := client.BatchV1().Jobs(cfg.JobNamespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)

	names := make(map[string]bool)
	for _, j := range remaining.Items {
		names[j.Name] = true
	}
	assert.True(t, names["old-active"], "active old job should remain")
	assert.True(t, names["current-job"], "current scan job should remain")
	assert.False(t, names["old-completed"], "old completed should be deleted")
	assert.False(t, names["old-failed"], "old failed should be deleted")
}

// --- isJobFailed ---

func TestIsJobFailed(t *testing.T) {
	assert.False(t, isJobFailed(&batchv1.Job{}))
	assert.False(t, isJobFailed(&batchv1.Job{
		Status: batchv1.JobStatus{Active: 1},
	}))
	assert.True(t, isJobFailed(&batchv1.Job{
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
			},
		},
	}))
	assert.False(t, isJobFailed(&batchv1.Job{
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobFailed, Status: corev1.ConditionFalse},
			},
		},
	}))
}

// --- ListScanJobs ---

func TestListScanJobs(t *testing.T) {
	cfg := jobManagerConfig()

	jobs := []batchv1.Job{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "scan-a-j1", Namespace: cfg.JobNamespace,
				Labels: map[string]string{LabelComponent: LabelComponentValue, LabelScanID: "a"},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "scan-a-j2", Namespace: cfg.JobNamespace,
				Labels: map[string]string{LabelComponent: LabelComponentValue, LabelScanID: "a"},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "scan-b-j1", Namespace: cfg.JobNamespace,
				Labels: map[string]string{LabelComponent: LabelComponentValue, LabelScanID: "b"},
			},
		},
	}

	client := fake.NewSimpleClientset(&batchv1.JobList{Items: jobs})
	mgr := NewJobManager(client, cfg, logr.Discard())

	list, err := mgr.ListScanJobs(context.Background(), "a")
	require.NoError(t, err)
	assert.Len(t, list.Items, 2)
}

// --- End-to-end: Plan → Submit ---

func TestPlanAndSubmit_EndToEnd(t *testing.T) {
	cfg := jobManagerConfig()
	cfg.BatchSize = 3
	cfg.MaxConcurrentJobs = 5
	cfg.MaxJobsPerNode = 1
	client := fake.NewSimpleClientset()
	mgr := NewJobManager(client, cfg, logr.Discard())

	nim := makeNodeImageMap(map[string]int{
		"node-a": 7, // → 3 batches
		"node-b": 2, // → 1 batch
	})
	scanID := "1700000000"

	specs := mgr.PlanBatchJobs(nim, scanID)
	// node-a: 3 batches (7/3 = 3), node-b: 1 batch (2/3 = 1) = 4 total
	assert.Len(t, specs, 4)

	// Submit — per-node=1 means only 1 per node = 2 jobs submitted.
	created, err := mgr.SubmitBatches(context.Background(), specs)
	require.NoError(t, err)
	assert.Len(t, created, 2)

	// Verify created jobs in cluster.
	allJobs, err := client.BatchV1().Jobs(cfg.JobNamespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, allJobs.Items, 2)

	// Verify one per node.
	nodesSeen := map[string]bool{}
	for _, j := range allJobs.Items {
		nodesSeen[j.Spec.Template.Spec.NodeName] = true
		assert.Equal(t, scanID, j.Labels[LabelScanID])
		assert.Equal(t, LabelComponentValue, j.Labels[LabelComponent])
	}
	assert.True(t, nodesSeen["node-a"])
	assert.True(t, nodesSeen["node-b"])
}
