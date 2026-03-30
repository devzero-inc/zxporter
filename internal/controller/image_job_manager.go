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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// Label keys used on batch analysis Jobs.
	LabelComponent    = "devzero.io/component"
	LabelAnalysisNode = "devzero.io/analysis-node"
	LabelBatchIndex   = "devzero.io/batch-index"
	LabelScanID       = "devzero.io/scan-id"

	// LabelComponentValue is the value for the component label on analysis jobs.
	LabelComponentValue = "image-analysis"

	// jobNamePrefix is used in all analysis job names.
	jobNamePrefix = "dive"

	// maxJobNameLength is the K8s resource name limit.
	maxJobNameLength = 63

	// ttlSecondsAfterFinished controls Job auto-cleanup after completion.
	ttlSecondsAfterFinished = 3600 // 1 hour
)

// BatchJobSpec describes a single batch job to be created.
type BatchJobSpec struct {
	JobName    string
	NodeName   string
	BatchIndex int
	ScanID     string
	Images     []ImageInfo
}

// JobManager creates and monitors batch analysis Jobs.
type JobManager struct {
	client kubernetes.Interface
	config ImageAnalysisConfig
	log    logr.Logger
}

// NewJobManager creates a new JobManager.
func NewJobManager(client kubernetes.Interface, config ImageAnalysisConfig, log logr.Logger) *JobManager {
	return &JobManager{
		client: client,
		config: config,
		log:    log.WithName("job-manager"),
	}
}

// GenerateScanID returns a unique scan identifier based on current Unix timestamp.
func GenerateScanID() string {
	return strconv.FormatInt(time.Now().Unix(), 10)
}

// PlanBatchJobs takes a NodeImageMap and splits each node's images into batches,
// returning a prioritized list of BatchJobSpecs (busiest nodes first).
func (m *JobManager) PlanBatchJobs(nodeImages NodeImageMap, scanID string) []BatchJobSpec {
	// Build sorted node list (descending by image count for priority).
	type nodeEntry struct {
		nodeName string
		batch    *NodeBatch
	}
	nodes := make([]nodeEntry, 0, len(nodeImages))
	for name, batch := range nodeImages {
		nodes = append(nodes, nodeEntry{nodeName: name, batch: batch})
	}
	sort.Slice(nodes, func(i, j int) bool {
		return len(nodes[i].batch.Images) > len(nodes[j].batch.Images)
	})

	var specs []BatchJobSpec
	batchSize := m.config.BatchSize
	if batchSize <= 0 {
		batchSize = 10
	}

	for _, entry := range nodes {
		images := entry.batch.Images
		for i := 0; i < len(images); i += batchSize {
			end := i + batchSize
			if end > len(images) {
				end = len(images)
			}
			batchIndex := i / batchSize
			jobName := generateJobName(entry.nodeName, batchIndex, scanID)

			specs = append(specs, BatchJobSpec{
				JobName:    jobName,
				NodeName:   entry.nodeName,
				BatchIndex: batchIndex,
				ScanID:     scanID,
				Images:     images[i:end],
			})
		}
	}

	return specs
}

// CreateJob creates a single batch analysis Job in the cluster.
func (m *JobManager) CreateJob(ctx context.Context, spec BatchJobSpec) (*batchv1.Job, error) {
	job, err := m.buildJobObject(spec)
	if err != nil {
		return nil, fmt.Errorf("building job object for %s: %w", spec.JobName, err)
	}

	created, err := m.client.BatchV1().Jobs(m.config.JobNamespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("creating job %s: %w", spec.JobName, err)
	}

	m.log.Info("created batch job",
		"job", created.Name,
		"node", spec.NodeName,
		"batchIndex", spec.BatchIndex,
		"imageCount", len(spec.Images),
		"scanID", spec.ScanID,
	)

	return created, nil
}

// SubmitBatches creates Jobs respecting cluster-level and per-node concurrency limits.
// It returns the list of Jobs that were successfully created, and any error.
// It does NOT block waiting for Jobs to complete — the caller should watch for completion.
func (m *JobManager) SubmitBatches(ctx context.Context, specs []BatchJobSpec) ([]*batchv1.Job, error) {
	if len(specs) == 0 {
		return nil, nil
	}

	scanID := specs[0].ScanID

	// Count currently active jobs for this scan.
	activeJobs, activePerNode, err := m.countActiveJobs(ctx, scanID)
	if err != nil {
		return nil, fmt.Errorf("counting active jobs: %w", err)
	}

	maxCluster := m.config.MaxConcurrentJobs
	if maxCluster <= 0 {
		maxCluster = 10
	}
	maxPerNode := m.config.MaxJobsPerNode
	if maxPerNode <= 0 {
		maxPerNode = 1
	}

	var created []*batchv1.Job

	for _, spec := range specs {
		// Check cluster-level concurrency.
		if activeJobs >= maxCluster {
			m.log.V(1).Info("cluster concurrency limit reached",
				"active", activeJobs, "max", maxCluster,
				"remaining", len(specs)-len(created))
			break
		}

		// Check per-node concurrency.
		nodeActive := activePerNode[spec.NodeName]
		if nodeActive >= maxPerNode {
			m.log.V(1).Info("node concurrency limit reached",
				"node", spec.NodeName, "active", nodeActive, "max", maxPerNode)
			continue
		}

		job, err := m.CreateJob(ctx, spec)
		if err != nil {
			m.log.Error(err, "failed to create batch job", "job", spec.JobName)
			continue
		}

		created = append(created, job)
		activeJobs++
		activePerNode[spec.NodeName]++
	}

	m.log.Info("batch submission complete",
		"planned", len(specs),
		"submitted", len(created),
		"activeTotal", activeJobs,
	)

	return created, nil
}

// ListScanJobs lists all Jobs for a given scan ID.
func (m *JobManager) ListScanJobs(ctx context.Context, scanID string) (*batchv1.JobList, error) {
	// If scanID is empty, list ALL image-analysis jobs (used for resume detection).
	labelSelector := fmt.Sprintf("%s=%s", LabelComponent, LabelComponentValue)
	if scanID != "" {
		labelSelector = fmt.Sprintf("%s,%s=%s", labelSelector, LabelScanID, scanID)
	}

	return m.client.BatchV1().Jobs(m.config.JobNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
}

// GetJobStatus returns summary counts of jobs in various states for a scan.
func (m *JobManager) GetJobStatus(ctx context.Context, scanID string) (active, succeeded, failed int, err error) {
	jobList, err := m.ListScanJobs(ctx, scanID)
	if err != nil {
		return 0, 0, 0, err
	}

	for i := range jobList.Items {
		job := &jobList.Items[i]
		switch {
		case job.Status.Succeeded > 0:
			succeeded++
		case isJobFailed(job):
			failed++
		default:
			active++
		}
	}

	return active, succeeded, failed, nil
}

// CleanupOldScanJobs deletes completed/failed Jobs from previous scans (not the current scanID).
func (m *JobManager) CleanupOldScanJobs(ctx context.Context, currentScanID string) (int, error) {
	labelSelector := fmt.Sprintf("%s=%s", LabelComponent, LabelComponentValue)

	jobList, err := m.client.BatchV1().Jobs(m.config.JobNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return 0, fmt.Errorf("listing old scan jobs: %w", err)
	}

	deleted := 0
	propagation := metav1.DeletePropagationBackground
	for i := range jobList.Items {
		job := &jobList.Items[i]
		jobScanID := job.Labels[LabelScanID]
		if jobScanID == currentScanID {
			continue
		}

		// Only delete completed or failed jobs.
		if job.Status.Succeeded == 0 && !isJobFailed(job) {
			continue
		}

		if err := m.client.BatchV1().Jobs(m.config.JobNamespace).Delete(
			ctx, job.Name, metav1.DeleteOptions{PropagationPolicy: &propagation},
		); err != nil {
			m.log.Error(err, "failed to delete old job", "job", job.Name, "scanID", jobScanID)
			continue
		}
		deleted++
	}

	if deleted > 0 {
		m.log.Info("cleaned up old scan jobs", "deleted", deleted)
	}

	return deleted, nil
}

// buildJobObject constructs the batchv1.Job K8s object for a batch spec.
func (m *JobManager) buildJobObject(spec BatchJobSpec) (*batchv1.Job, error) {
	// Serialize images list to JSON for the IMAGES_JSON env var.
	type imageEntry struct {
		Digest string `json:"digest"`
		Ref    string `json:"ref"`
	}
	entries := make([]imageEntry, len(spec.Images))
	for i, img := range spec.Images {
		entries[i] = imageEntry{Digest: img.Digest, Ref: img.Ref}
	}
	imagesJSON, err := json.Marshal(entries)
	if err != nil {
		return nil, fmt.Errorf("marshaling images JSON: %w", err)
	}

	// Parse resource quantities (validated at config load time, safe to use MustParse here).
	cpuRequest := resource.MustParse(m.config.JobCPURequest)
	cpuLimit := resource.MustParse(m.config.JobCPULimit)
	memRequest := resource.MustParse(m.config.JobMemoryRequest)
	memLimit := resource.MustParse(m.config.JobMemoryLimit)
	workspaceSize := resource.MustParse(m.config.WorkspaceSizeLimit)

	// Compute activeDeadlineSeconds from config.
	activeDeadlineSeconds := int64(m.config.JobTimeoutMinutes * 60)

	// backoffLimit from config.
	retries := m.config.MaxRetries
	if retries <= 0 {
		retries = 1
	} else if retries > 10 {
		// Prevent overflow and unreasonable retry counts.
		retries = 10
	}
	backoffLimit := int32(retries)

	ttl := int32(ttlSecondsAfterFinished)

	// Parse remote pull timeout to seconds for the env var.
	remotePullTimeoutSec := parseDurationToSeconds(m.config.RemotePullTimeout)

	// Build env vars.
	envVars := []corev1.EnvVar{
		{Name: "IMAGES_JSON", Value: string(imagesJSON)},
		{Name: "PREFER_LOCAL", Value: strconv.FormatBool(m.config.PreferLocal)},
		{Name: "FALLBACK_REMOTE", Value: strconv.FormatBool(m.config.FallbackToRemote)},
		{Name: "CONTAINERD_SOCK", Value: m.config.ContainerdSocketPath},
		{Name: "CONTAINERD_NS", Value: m.config.ContainerdNamespace},
		{Name: "REMOTE_PULL_TIMEOUT", Value: strconv.Itoa(remotePullTimeoutSec)},
	}

	// Build volumes and mounts.
	volumes := []corev1.Volume{
		{
			Name: "containerd-sock",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: m.config.ContainerdSocketPath,
					Type: hostPathTypePtr(corev1.HostPathSocket),
				},
			},
		},
		{
			Name: "workspace",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					SizeLimit: &workspaceSize,
				},
			},
		},
	}

	volumeMounts := []corev1.VolumeMount{
		{
			Name:      "containerd-sock",
			MountPath: "/run/containerd/containerd.sock",
			ReadOnly:  true,
		},
		{
			Name:      "workspace",
			MountPath: "/tmp/workspace",
		},
	}

	// Optionally mount registry auth secret.
	if m.config.RegistryAuthSecret != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "registry-auth",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: m.config.RegistryAuthSecret,
					Optional:   boolPtr(true),
				},
			},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      "registry-auth",
			MountPath: "/root/.docker",
			ReadOnly:  true,
		})
	}

	// Build labels.
	labels := map[string]string{
		LabelComponent:    LabelComponentValue,
		LabelAnalysisNode: sanitizeLabelValue(spec.NodeName),
		LabelBatchIndex:   strconv.Itoa(spec.BatchIndex),
		LabelScanID:       spec.ScanID,
	}

	var runAsUser int64 = 0
	allowPrivEsc := false

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.JobName,
			Namespace: m.config.JobNamespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			ActiveDeadlineSeconds:   &activeDeadlineSeconds,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: "image-analyzer",
					NodeName:           spec.NodeName,
					Tolerations:        m.config.JobTolerations,
					Containers: []corev1.Container{
						{
							Name:  "analyzer",
							Image: m.config.AnalyzerImage,
							Env:   envVars,
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    cpuRequest,
									corev1.ResourceMemory: memRequest,
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    cpuLimit,
									corev1.ResourceMemory: memLimit,
								},
							},
							VolumeMounts: volumeMounts,
							SecurityContext: &corev1.SecurityContext{
								RunAsUser:                &runAsUser,
								AllowPrivilegeEscalation: &allowPrivEsc,
							},
						},
					},
					Volumes: volumes,
				},
			},
		},
	}

	return job, nil
}

// countActiveJobs returns the number of currently running (not completed/failed) jobs
// for the given scan ID, both total and per-node.
func (m *JobManager) countActiveJobs(ctx context.Context, scanID string) (int, map[string]int, error) {
	jobList, err := m.ListScanJobs(ctx, scanID)
	if err != nil {
		return 0, nil, err
	}

	total := 0
	perNode := make(map[string]int)

	for i := range jobList.Items {
		job := &jobList.Items[i]
		// Skip completed/failed jobs.
		if job.Status.Succeeded > 0 || isJobFailed(job) {
			continue
		}
		total++
		nodeName := job.Labels[LabelAnalysisNode]
		perNode[nodeName]++
	}

	return total, perNode, nil
}

// isJobFailed checks if a Job has a "Failed" condition.
func isJobFailed(job *batchv1.Job) bool {
	for _, cond := range job.Status.Conditions {
		if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// generateJobName creates a DNS-safe Job name within the 63-char K8s limit.
// Format: dive-<sanitized-node>-b<index>-<scanID>
func generateJobName(nodeName string, batchIndex int, scanID string) string {
	sanitized := sanitizeDNSName(nodeName)

	suffix := fmt.Sprintf("-b%d-%s", batchIndex, scanID)

	// Reserve space for prefix "dive-" (5 chars) and suffix.
	maxNodeLen := maxJobNameLength - len(jobNamePrefix) - 1 - len(suffix)
	if maxNodeLen < 1 {
		maxNodeLen = 1
	}
	if len(sanitized) > maxNodeLen {
		sanitized = sanitized[:maxNodeLen]
	}

	// Trim trailing hyphens after truncation.
	sanitized = strings.TrimRight(sanitized, "-")

	return fmt.Sprintf("%s-%s%s", jobNamePrefix, sanitized, suffix)
}

// dnsNameRegex matches characters NOT allowed in DNS subdomain names.
var dnsNameRegex = regexp.MustCompile(`[^a-z0-9-]`)

// sanitizeDNSName converts a string into a valid DNS subdomain label.
func sanitizeDNSName(name string) string {
	name = strings.ToLower(name)
	name = dnsNameRegex.ReplaceAllString(name, "-")

	// Collapse multiple hyphens.
	for strings.Contains(name, "--") {
		name = strings.ReplaceAll(name, "--", "-")
	}

	// Trim leading/trailing hyphens.
	name = strings.Trim(name, "-")

	return name
}

// sanitizeLabelValue ensures a label value is valid (≤63 chars, alphanumeric start/end).
func sanitizeLabelValue(value string) string {
	value = sanitizeDNSName(value)
	if len(value) > 63 {
		value = value[:63]
	}
	value = strings.Trim(value, "-")
	return value
}

// parseDurationToSeconds parses a Go-style duration string (e.g. "5m", "300s")
// and returns the value in seconds. Falls back to 300 if parsing fails.
func parseDurationToSeconds(s string) int {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 300
	}
	return int(d.Seconds())
}

func boolPtr(b bool) *bool {
	return &b
}

func hostPathTypePtr(t corev1.HostPathType) *corev1.HostPathType {
	return &t
}
