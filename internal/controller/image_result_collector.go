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
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/devzero-inc/zxporter/api/v1"
)

const (
	// maxCommandLength is the max length for a layer command string stored in the CRD.
	maxCommandLength = 200

	// maxWastedFiles is the max number of wasted file entries stored per image.
	maxWastedFiles = 50

	// maxWorkloadRefs is the max number of workload references stored per image.
	maxWorkloadRefs = 200

	// CRD label keys for ImageAnalysisResult.
	LabelImageRegistry = "devzero.io/image-registry"
	LabelImageRepo     = "devzero.io/image-repo"
	LabelScanPassed    = "devzero.io/scan-passed"
	LabelAnalyzedNode  = "devzero.io/analyzed-node"
	// LabelScanID is reused from image_job_manager.go

	// Annotation keys.
	AnnotationFullImageRef = "devzero.io/full-image-ref"
)

// --- Dive JSON types (matches dive's export format) ---

// BatchImageResult is a single NDJSON line from the batch analyzer script.
type BatchImageResult struct {
	Digest     string      `json:"digest"`
	Ref        string      `json:"ref"`
	Source     string      `json:"source"` // "local-containerd", "remote-pull", "failed"
	DurationMs int64       `json:"durationMs"`
	Error      string      `json:"error"`
	Result     *DiveResult `json:"result"` // null if failed
}

// DiveResult is dive's top-level JSON export structure.
type DiveResult struct {
	Image DiveImage   `json:"image"`
	Layer []DiveLayer `json:"layer"`
}

// DiveImage contains the image-level metrics from dive.
type DiveImage struct {
	SizeBytes        uint64        `json:"sizeBytes"`
	InefficientBytes uint64        `json:"inefficientBytes"`
	EfficiencyScore  float64       `json:"efficiencyScore"`
	FileReference    []DiveFileRef `json:"fileReference"`
}

// DiveFileRef is a file contributing to wasted space in the image.
type DiveFileRef struct {
	Count     int    `json:"count"`
	SizeBytes uint64 `json:"sizeBytes"`
	File      string `json:"file"`
}

// DiveLayer contains per-layer details from dive.
type DiveLayer struct {
	Index     int        `json:"index"`
	ID        string     `json:"id"`
	DigestID  string     `json:"digestId"`
	SizeBytes uint64     `json:"sizeBytes"`
	Command   string     `json:"command"`
	FileList  []DiveFile `json:"fileList"`
}

// DiveFile is a file entry in a dive layer's file list.
type DiveFile struct {
	Path     string `json:"path"`
	Size     uint64 `json:"size"`
	TypeFlag int    `json:"typeFlag"`
	IsDir    bool   `json:"isDir"`
}

// --- Collection result types ---

// CollectionResult holds the outcome of collecting results from a single batch Job.
type CollectionResult struct {
	JobName          string
	NodeName         string
	ScanID           string
	Succeeded        []BatchImageResult // images that were analyzed successfully
	Failed           []BatchImageResult // images that failed (source="failed" or error!="")
	ParseErrors      int                // count of NDJSON lines that couldn't be parsed
	TotalLines       int                // total NDJSON lines parsed
	LocalContainerd  int                // count of images acquired via containerd
	RemotePull       int                // count of images acquired via remote pull
	AcquisitionFailed int               // count of images where acquisition failed
}

// --- ResultCollector ---

// ResultCollector reads Job pod logs, parses NDJSON output, maps to CRDs, and persists them.
type ResultCollector struct {
	k8sClient   kubernetes.Interface
	crdClient   client.Client
	config      ImageAnalysisConfig
	log         logr.Logger
}

// NewResultCollector creates a new ResultCollector.
func NewResultCollector(
	k8sClient kubernetes.Interface,
	crdClient client.Client,
	config ImageAnalysisConfig,
	log logr.Logger,
) *ResultCollector {
	return &ResultCollector{
		k8sClient: k8sClient,
		crdClient: crdClient,
		config:    config,
		log:       log.WithName("result-collector"),
	}
}

// CollectFromJob reads pod logs from a completed batch Job and parses the NDJSON output.
func (rc *ResultCollector) CollectFromJob(ctx context.Context, job *batchv1.Job) (*CollectionResult, error) {
	result := &CollectionResult{
		JobName:  job.Name,
		NodeName: job.Labels[LabelAnalysisNode],
		ScanID:   job.Labels[LabelScanID],
	}

	// Find the pod(s) for this Job.
	podList, err := rc.k8sClient.CoreV1().Pods(job.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("job-name=%s", job.Name),
	})
	if err != nil {
		return nil, fmt.Errorf("listing pods for job %s: %w", job.Name, err)
	}

	if len(podList.Items) == 0 {
		return nil, fmt.Errorf("no pods found for job %s", job.Name)
	}

	// Use the most recently created pod (in case of retries, this is the last attempt).
	pod := findLatestPod(podList.Items)

	// Read pod logs.
	logStream, err := rc.k8sClient.CoreV1().Pods(job.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: "analyzer",
	}).Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading logs from pod %s: %w", pod.Name, err)
	}
	defer logStream.Close()

	// Parse NDJSON lines.
	scanner := bufio.NewScanner(logStream)
	// Allow up to 10MB per line (dive JSON can be large).
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		// Skip non-JSON lines (progress messages go to stderr, but just in case).
		if !strings.HasPrefix(line, "{") {
			continue
		}

		result.TotalLines++

		var imgResult BatchImageResult
		if err := json.Unmarshal([]byte(line), &imgResult); err != nil {
			rc.log.V(1).Info("failed to parse NDJSON line", "job", job.Name, "error", err)
			result.ParseErrors++
			continue
		}

		// Classify the result.
		switch {
		case imgResult.Source == "failed" || imgResult.Error != "":
			result.Failed = append(result.Failed, imgResult)
			result.AcquisitionFailed++
		case imgResult.Source == "local-containerd":
			result.Succeeded = append(result.Succeeded, imgResult)
			result.LocalContainerd++
		case imgResult.Source == "remote-pull":
			result.Succeeded = append(result.Succeeded, imgResult)
			result.RemotePull++
		default:
			result.Succeeded = append(result.Succeeded, imgResult)
		}
	}

	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("scanning logs from pod %s: %w", pod.Name, err)
	}

	rc.log.Info("collected batch results",
		"job", job.Name,
		"succeeded", len(result.Succeeded),
		"failed", len(result.Failed),
		"parseErrors", result.ParseErrors,
	)

	return result, nil
}

// ProcessAndSave maps a successful BatchImageResult to an ImageAnalysisResult CRD and persists it.
// workloadRefs comes from the discovery phase (global workload reference map).
func (rc *ResultCollector) ProcessAndSave(
	ctx context.Context,
	imgResult BatchImageResult,
	jobName string,
	nodeName string,
	scanID string,
	workloadRefs WorkloadRefMap,
) error {
	crdName := generateCRDName(imgResult.Digest)

	// Build the CRD spec.
	spec, err := rc.buildSpec(imgResult, jobName, nodeName, workloadRefs)
	if err != nil {
		return fmt.Errorf("building spec for %s: %w", imgResult.Ref, err)
	}

	// Build status.
	status := v1.ImageAnalysisResultStatus{
		Phase:      "Completed",
		AnalyzedAt: metav1.NewTime(time.Now()),
	}
	if imgResult.DurationMs > 0 {
		// Round up so sub-second durations don't show as 0.
		status.AnalysisDurationSeconds = int((imgResult.DurationMs + 999) / 1000)
	}

	// Build labels and annotations.
	registry, repo := parseImageRegistryAndRepo(imgResult.Ref)
	labels := map[string]string{
		LabelImageRegistry: sanitizeLabelValue(registry),
		LabelImageRepo:     sanitizeLabelValue(repo),
		LabelScanPassed:    strconv.FormatBool(spec.Analysis.Passed),
		LabelAnalyzedNode:  sanitizeLabelValue(nodeName),
		LabelScanID:        scanID,
	}
	annotations := map[string]string{
		AnnotationFullImageRef: imgResult.Ref,
	}

	// Try to get existing CRD.
	existing := &v1.ImageAnalysisResult{}
	err = rc.crdClient.Get(ctx, types.NamespacedName{Name: crdName}, existing)

	if errors.IsNotFound(err) {
		// Create new CRD.
		crd := &v1.ImageAnalysisResult{
			ObjectMeta: metav1.ObjectMeta{
				Name:        crdName,
				Labels:      labels,
				Annotations: annotations,
			},
			Spec: spec,
		}
		if err := rc.crdClient.Create(ctx, crd); err != nil {
			return fmt.Errorf("creating ImageAnalysisResult %s: %w", crdName, err)
		}

		// Update status subresource.
		crd.Status = status
		if err := rc.crdClient.Status().Update(ctx, crd); err != nil {
			rc.log.Error(err, "failed to update status after create", "name", crdName)
		}

		rc.log.V(1).Info("created ImageAnalysisResult", "name", crdName, "image", imgResult.Ref)
		return nil
	} else if err != nil {
		return fmt.Errorf("getting existing ImageAnalysisResult %s: %w", crdName, err)
	}

	// Update existing CRD.
	existing.Labels = labels
	existing.Annotations = annotations
	existing.Spec = spec
	if err := rc.crdClient.Update(ctx, existing); err != nil {
		return fmt.Errorf("updating ImageAnalysisResult %s: %w", crdName, err)
	}

	// Update status subresource.
	existing.Status = status
	if err := rc.crdClient.Status().Update(ctx, existing); err != nil {
		rc.log.Error(err, "failed to update status", "name", crdName)
	}

	rc.log.V(1).Info("updated ImageAnalysisResult", "name", crdName, "image", imgResult.Ref)
	return nil
}

// SaveFailedResult creates/updates a CRD with Failed status for an image that couldn't be analyzed.
func (rc *ResultCollector) SaveFailedResult(
	ctx context.Context,
	imgResult BatchImageResult,
	jobName string,
	nodeName string,
	scanID string,
) error {
	crdName := generateCRDName(imgResult.Digest)

	labels := map[string]string{
		LabelScanPassed:   "false",
		LabelAnalyzedNode: sanitizeLabelValue(nodeName),
		LabelScanID:       scanID,
	}

	status := v1.ImageAnalysisResultStatus{
		Phase:      "Failed",
		AnalyzedAt: metav1.NewTime(time.Now()),
		Message:    imgResult.Error,
	}

	existing := &v1.ImageAnalysisResult{}
	err := rc.crdClient.Get(ctx, types.NamespacedName{Name: crdName}, existing)

	if errors.IsNotFound(err) {
		crd := &v1.ImageAnalysisResult{
			ObjectMeta: metav1.ObjectMeta{
				Name:   crdName,
				Labels: labels,
			},
			Spec: v1.ImageAnalysisResultSpec{
				ImageRef:    imgResult.Ref,
				ImageDigest: imgResult.Digest,
				ImageSource: v1.ImageSourceInfo{
					Type:         imgResult.Source,
					NodeName:     nodeName,
					BatchJobName: jobName,
				},
				Analysis: v1.ImageAnalysis{
					Efficiency:        "0",
					WastedBytes:       0,
					UserWastedPercent: "0",
					Passed:            false,
					LayerCount:        0,
				},
			},
		}
		if err := rc.crdClient.Create(ctx, crd); err != nil {
			return fmt.Errorf("creating failed ImageAnalysisResult %s: %w", crdName, err)
		}
		crd.Status = status
		if err := rc.crdClient.Status().Update(ctx, crd); err != nil {
			rc.log.Error(err, "failed to update failed status after create", "name", crdName)
		}
		return nil
	} else if err != nil {
		return fmt.Errorf("getting existing ImageAnalysisResult %s: %w", crdName, err)
	}

	// Update existing with failed spec and status.
	existing.Labels = labels
	existing.Spec.ImageRef = imgResult.Ref
	existing.Spec.ImageDigest = imgResult.Digest
	existing.Spec.ImageSource = v1.ImageSourceInfo{
		Type:         imgResult.Source,
		NodeName:     nodeName,
		BatchJobName: jobName,
	}
	existing.Spec.Analysis.Passed = false
	if err := rc.crdClient.Update(ctx, existing); err != nil {
		return fmt.Errorf("updating failed ImageAnalysisResult %s: %w", crdName, err)
	}

	existing.Status = status
	if err := rc.crdClient.Status().Update(ctx, existing); err != nil {
		return fmt.Errorf("updating failed status for %s: %w", crdName, err)
	}
	return nil
}

// buildSpec constructs the ImageAnalysisResultSpec from a BatchImageResult.
func (rc *ResultCollector) buildSpec(
	imgResult BatchImageResult,
	jobName string,
	nodeName string,
	workloadRefs WorkloadRefMap,
) (v1.ImageAnalysisResultSpec, error) {
	spec := v1.ImageAnalysisResultSpec{
		ImageRef:    imgResult.Ref,
		ImageDigest: imgResult.Digest,
		ImageSource: v1.ImageSourceInfo{
			Type:             imgResult.Source,
			NodeName:         nodeName,
			ExportDurationMs: imgResult.DurationMs,
			BatchJobName:     jobName,
		},
	}

	if imgResult.Result == nil {
		return spec, fmt.Errorf("nil dive result for %s", imgResult.Ref)
	}

	dive := imgResult.Result

	// Image size.
	spec.ImageSizeBytes = int64(dive.Image.SizeBytes)

	// Analysis metrics.
	spec.Analysis = v1.ImageAnalysis{
		Efficiency:        formatFloat(dive.Image.EfficiencyScore),
		WastedBytes:       int64(dive.Image.InefficientBytes),
		UserWastedPercent: computeUserWastedPercent(dive.Image.InefficientBytes, dive.Image.SizeBytes),
		LayerCount:        len(dive.Layer),
	}

	// Evaluate pass/fail against thresholds.
	spec.Analysis.Passed = rc.evaluateThresholds(spec.Analysis)

	// Map layers (command truncated to 200 chars).
	spec.Analysis.Layers = make([]v1.LayerInfo, 0, len(dive.Layer))
	for _, layer := range dive.Layer {
		spec.Analysis.Layers = append(spec.Analysis.Layers, v1.LayerInfo{
			Index:     layer.Index,
			Digest:    layer.DigestID,
			SizeBytes: int64(layer.SizeBytes),
			Command:   truncateString(layer.Command, maxCommandLength),
		})
	}

	// Map wasted files (capped at 50, sorted by size desc).
	if len(dive.Image.FileReference) > 0 {
		// Sort by size descending.
		refs := make([]DiveFileRef, len(dive.Image.FileReference))
		copy(refs, dive.Image.FileReference)
		sort.Slice(refs, func(i, j int) bool {
			return refs[i].SizeBytes > refs[j].SizeBytes
		})

		cap := maxWastedFiles
		if len(refs) < cap {
			cap = len(refs)
		}
		spec.Analysis.WastedFiles = make([]v1.WastedFile, 0, cap)
		for _, ref := range refs[:cap] {
			spec.Analysis.WastedFiles = append(spec.Analysis.WastedFiles, v1.WastedFile{
				Path:      ref.File,
				SizeBytes: int64(ref.SizeBytes),
			})
		}
	}

	// Compute file analysis from layer file lists (if available).
	spec.Analysis.FileAnalysis = computeFileAnalysis(dive.Layer)

	// Map workload references.
	spec.WorkloadReferences, spec.WorkloadSummary = buildWorkloadData(
		imgResult.Digest, workloadRefs,
	)

	return spec, nil
}

// evaluateThresholds checks if analysis metrics pass the configured thresholds.
func (rc *ResultCollector) evaluateThresholds(analysis v1.ImageAnalysis) bool {
	// Check efficiency >= lowestEfficiency.
	efficiency, err := strconv.ParseFloat(analysis.Efficiency, 64)
	if err == nil && efficiency < rc.config.LowestEfficiency {
		return false
	}

	// Check wastedBytes <= highestWastedBytes.
	maxWastedBytes := parseByteSize(rc.config.HighestWastedBytes)
	if maxWastedBytes > 0 && analysis.WastedBytes > maxWastedBytes {
		return false
	}

	// Check userWastedPercent <= highestUserWastedPercent.
	userWasted, err := strconv.ParseFloat(analysis.UserWastedPercent, 64)
	if err == nil && userWasted > rc.config.HighestUserWastedPercent {
		return false
	}

	return true
}

// --- Helper functions ---

// findLatestPod returns the most recently created pod from a list.
func findLatestPod(pods []corev1.Pod) *corev1.Pod {
	if len(pods) == 0 {
		return nil
	}
	latest := &pods[0]
	for i := 1; i < len(pods); i++ {
		if pods[i].CreationTimestamp.After(latest.CreationTimestamp.Time) {
			latest = &pods[i]
		}
	}
	return latest
}

// generateCRDName creates a deterministic CRD name from an image digest.
// Input: "docker.io/library/nginx@sha256:a1b2c3d4e5f6..." or "sha256:a1b2c3d4e5f6..."
// Output: "sha-a1b2c3d4e5f6"
func generateCRDName(digest string) string {
	// Extract hex portion after "sha256:".
	hex := digest
	if idx := strings.LastIndex(digest, "sha256:"); idx >= 0 {
		hex = digest[idx+len("sha256:"):]
	}

	// Take first 12 hex chars.
	if len(hex) > 12 {
		hex = hex[:12]
	}

	// Ensure only hex characters.
	cleaned := strings.Map(func(r rune) rune {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			return r
		}
		return -1
	}, strings.ToLower(hex))

	if cleaned == "" {
		// Fallback: hash the whole digest string.
		cleaned = sanitizeDNSName(digest)
		if len(cleaned) > 12 {
			cleaned = cleaned[:12]
		}
	}

	return "sha-" + cleaned
}

// parseImageRegistryAndRepo extracts the registry and repository from an image reference.
// "docker.io/library/nginx:1.25.3" → ("docker.io", "library-nginx")
// "gcr.io/myproject/myapp:v1" → ("gcr.io", "myproject-myapp")
func parseImageRegistryAndRepo(ref string) (registry, repo string) {
	// Strip tag or digest.
	if idx := strings.LastIndex(ref, ":"); idx > 0 {
		// Make sure this isn't a port number.
		afterColon := ref[idx+1:]
		if !strings.Contains(afterColon, "/") {
			ref = ref[:idx]
		}
	}
	if idx := strings.Index(ref, "@"); idx > 0 {
		ref = ref[:idx]
	}

	parts := strings.SplitN(ref, "/", 2)
	if len(parts) == 1 {
		// No registry prefix (e.g., "nginx").
		return "docker.io", parts[0]
	}

	// Check if first part looks like a registry (contains a dot or colon).
	if strings.Contains(parts[0], ".") || strings.Contains(parts[0], ":") {
		registry = parts[0]
		repo = strings.ReplaceAll(parts[1], "/", "-")
	} else {
		// No registry (e.g., "library/nginx").
		registry = "docker.io"
		repo = strings.ReplaceAll(ref, "/", "-")
	}

	return registry, repo
}

// computeUserWastedPercent calculates the ratio of wasted bytes to total image size.
func computeUserWastedPercent(inefficientBytes, sizeBytes uint64) string {
	if sizeBytes == 0 {
		return "0"
	}
	ratio := float64(inefficientBytes) / float64(sizeBytes)
	return formatFloat(ratio)
}

// computeFileAnalysis aggregates file statistics from dive layer file lists.
func computeFileAnalysis(layers []DiveLayer) *v1.FileAnalysis {
	if len(layers) == 0 {
		return nil
	}

	// Check if any layer has file list data.
	hasFileData := false
	for _, l := range layers {
		if len(l.FileList) > 0 {
			hasFileData = true
			break
		}
	}
	if !hasFileData {
		return nil
	}

	// Track files across layers: path → first layer index seen.
	seenFiles := make(map[string]int)
	fa := &v1.FileAnalysis{}

	for _, layer := range layers {
		for _, file := range layer.FileList {
			if file.IsDir {
				continue
			}

			if _, seen := seenFiles[file.Path]; !seen {
				// First time seeing this file → added.
				seenFiles[file.Path] = layer.Index
				fa.AddedFiles++
			} else {
				// Seen before → modified (or removed and re-added).
				fa.ModifiedFiles++
			}
		}
	}

	fa.TotalFiles = len(seenFiles)
	// Note: removedFiles requires whiteout detection (typeFlag analysis).
	// For now, we leave it as 0 since dive's fileList doesn't clearly distinguish removals.

	return fa
}

// buildWorkloadData constructs WorkloadReferences and WorkloadSummary from discovery data.
func buildWorkloadData(digest string, workloadRefs WorkloadRefMap) ([]v1.WorkloadReference, v1.WorkloadSummary) {
	discoveredRefs := workloadRefs[digest]
	if len(discoveredRefs) == 0 {
		return nil, v1.WorkloadSummary{}
	}

	// Convert DiscoveredWorkloadRef → v1.WorkloadReference.
	refs := make([]v1.WorkloadReference, 0, len(discoveredRefs))
	namespaceSet := make(map[string]struct{})
	totalContainers := 0

	for _, dRef := range discoveredRefs {
		containerNames := make([]string, 0, len(dRef.ContainerNames))
		for name := range dRef.ContainerNames {
			containerNames = append(containerNames, name)
		}
		sort.Strings(containerNames)

		refs = append(refs, v1.WorkloadReference{
			Namespace:      dRef.Namespace,
			WorkloadType:   dRef.WorkloadType,
			WorkloadName:   dRef.WorkloadName,
			WorkloadUID:    dRef.WorkloadUID,
			ContainerNames: containerNames,
			// Note: Replicas is not available from discovery (would require querying each workload).
		})

		namespaceSet[dRef.Namespace] = struct{}{}
		totalContainers += len(containerNames)
	}

	// Sort by replica count desc (since we don't have replicas, sort by container count desc).
	sort.Slice(refs, func(i, j int) bool {
		return len(refs[i].ContainerNames) > len(refs[j].ContainerNames)
	})

	// Cap at maxWorkloadRefs.
	if len(refs) > maxWorkloadRefs {
		refs = refs[:maxWorkloadRefs]
	}

	// Build namespace list.
	namespaces := make([]string, 0, len(namespaceSet))
	for ns := range namespaceSet {
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)

	summary := v1.WorkloadSummary{
		TotalContainers: totalContainers,
		TotalWorkloads:  len(discoveredRefs),
		Namespaces:      namespaces,
		// NodesRunningImage is not available from discovery (would require cross-referencing).
	}

	return refs, summary
}

// formatFloat formats a float64 to a string with up to 4 decimal places, trimming trailing zeros.
func formatFloat(f float64) string {
	s := strconv.FormatFloat(f, 'f', 4, 64)
	// Trim trailing zeros after decimal point.
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimRight(s, ".")
	}
	if s == "" {
		return "0"
	}
	return s
}

// truncateString truncates a string to maxLen characters.
func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}

// parseByteSize parses a human-readable byte size string (e.g., "50MB", "1GB", "100KB") to bytes.
// Returns 0 if the string can't be parsed.
func parseByteSize(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}

	// Split into numeric part and unit suffix.
	i := 0
	for i < len(s) && (s[i] == '.' || (s[i] >= '0' && s[i] <= '9')) {
		i++
	}

	numStr := s[:i]
	unit := strings.TrimSpace(s[i:])

	num, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0
	}

	// Normalize unit to uppercase, trimmed.
	unit = strings.ToUpper(strings.TrimFunc(unit, func(r rune) bool {
		return unicode.IsSpace(r)
	}))

	var multiplier float64
	switch unit {
	case "B", "":
		multiplier = 1
	case "KB", "K":
		multiplier = 1000
	case "MB", "M":
		multiplier = 1000 * 1000
	case "GB", "G":
		multiplier = 1000 * 1000 * 1000
	case "TB", "T":
		multiplier = 1000 * 1000 * 1000 * 1000
	case "KIB", "KI":
		multiplier = 1024
	case "MIB", "MI":
		multiplier = 1024 * 1024
	case "GIB", "GI":
		multiplier = 1024 * 1024 * 1024
	default:
		return 0
	}

	return int64(num * multiplier)
}
