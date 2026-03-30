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
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/devzero-inc/zxporter/api/v1"
)

// ScanPhase represents the current phase of a scan cycle.
type ScanPhase string

const (
	ScanPhaseIdle        ScanPhase = "Idle"
	ScanPhaseDiscovering ScanPhase = "Discovering"
	ScanPhaseAnalyzing   ScanPhase = "Analyzing"
	ScanPhaseCollecting  ScanPhase = "Collecting"
	ScanPhaseCompleted   ScanPhase = "Completed"
	ScanPhaseFailed      ScanPhase = "Failed"

	// jobPollInterval is how often to check job statuses during the analysis phase.
	jobPollInterval = 30 * time.Second
)

// ImageSourceStats tracks how images were acquired during a scan.
type ImageSourceStats struct {
	LocalContainerd   int
	RemotePull        int
	AcquisitionFailed int
}

// ScanState holds the in-memory state for a single scan cycle.
type ScanState struct {
	mu sync.RWMutex

	Phase   ScanPhase
	ScanID  string
	StartAt time.Time

	// Discovery stats.
	TotalUniqueImages int
	TotalNodes        int
	TotalBatches      int

	// Progress counters.
	ImagesCompleted int
	ImagesFailed    int
	BatchesTotal    int
	BatchesComplete int
	BatchesFailed   int

	// Source stats.
	SourceStats ImageSourceStats

	// Error from the scan.
	Error string
}

// GetPhase returns the current scan phase (thread-safe).
func (s *ScanState) GetPhase() ScanPhase {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Phase
}

// setPhase updates the scan phase (thread-safe).
func (s *ScanState) setPhase(phase ScanPhase) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Phase = phase
}

// recordBatchResult updates counters from a single batch collection.
func (s *ScanState) recordBatchResult(cr *CollectionResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.BatchesComplete++
	s.ImagesCompleted += len(cr.Succeeded)
	s.ImagesFailed += len(cr.Failed) + cr.ParseErrors
	s.SourceStats.LocalContainerd += cr.LocalContainerd
	s.SourceStats.RemotePull += cr.RemotePull
	s.SourceStats.AcquisitionFailed += cr.AcquisitionFailed
}

// recordBatchFailed records a batch that failed entirely (couldn't read logs, etc.).
func (s *ScanState) recordBatchFailed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.BatchesFailed++
}

// ImageAnalysisController runs periodic image analysis scans.
// It implements manager.Runnable so it can be added via mgr.Add().
type ImageAnalysisController struct {
	k8sClient kubernetes.Interface
	crdClient client.Client
	log       logr.Logger
	config    ImageAnalysisConfig
	state     *ScanState
}

// NewImageAnalysisController creates a new image analysis controller.
func NewImageAnalysisController(
	k8sClient kubernetes.Interface,
	crdClient client.Client,
	log logr.Logger,
	config ImageAnalysisConfig,
) *ImageAnalysisController {
	return &ImageAnalysisController{
		k8sClient: k8sClient,
		crdClient: crdClient,
		log:       log.WithName("image-analysis"),
		config:    config,
		state: &ScanState{
			Phase: ScanPhaseIdle,
		},
	}
}

// NeedLeaderElection returns true — only the leader should run image analysis.
func (c *ImageAnalysisController) NeedLeaderElection() bool {
	return true
}

// Start implements manager.Runnable. It runs an initial scan attempt on resume,
// then enters the periodic scan loop.
func (c *ImageAnalysisController) Start(ctx context.Context) error {
	c.log.Info("image analysis controller starting",
		"interval", fmt.Sprintf("%dd", c.config.IntervalDays),
		"batchSize", c.config.BatchSize,
		"maxConcurrentJobs", c.config.MaxConcurrentJobs,
		"analyzerImage", c.config.AnalyzerImage,
	)

	// Check if there's an in-progress scan from a previous controller instance.
	if resumed := c.tryResumeScan(ctx); resumed {
		c.log.Info("resumed in-progress scan from previous instance")
	}

	// Run the periodic scan loop.
	interval := time.Duration(c.config.IntervalDays) * 24 * time.Hour
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Run first scan immediately if we didn't resume one.
	if c.state.GetPhase() == ScanPhaseIdle {
		c.runScan(ctx)
	}

	for {
		select {
		case <-ticker.C:
			// Re-read config each cycle to pick up ConfigMap changes.
			newConfig, err := LoadImageAnalysisConfigFromEnv()
			if err != nil {
				c.log.Error(err, "failed to reload config, using previous")
			} else {
				c.config = newConfig
				if !c.config.Enabled {
					c.log.Info("image analysis disabled via config, skipping scan")
					continue
				}
			}
			c.runScan(ctx)
		case <-ctx.Done():
			c.log.Info("image analysis controller stopping")
			return nil
		}
	}
}

// GetState returns the current scan state (for health reporting).
func (c *ImageAnalysisController) GetState() *ScanState {
	return c.state
}

// tryResumeScan checks for in-progress jobs from a previous controller instance.
// Returns true if a scan was resumed.
func (c *ImageAnalysisController) tryResumeScan(ctx context.Context) bool {
	jm := NewJobManager(c.k8sClient, c.config, c.log)

	// List all image-analysis jobs.
	jobList, err := jm.ListScanJobs(ctx, "")
	if err != nil {
		c.log.V(1).Info("could not list existing jobs for resume check", "error", err)
		return false
	}

	if len(jobList.Items) == 0 {
		return false
	}

	// Find the most recent scan ID.
	latestScanID := ""
	var latestTime time.Time
	for i := range jobList.Items {
		scanID := jobList.Items[i].Labels[LabelScanID]
		if jobList.Items[i].CreationTimestamp.After(latestTime) {
			latestTime = jobList.Items[i].CreationTimestamp.Time
			latestScanID = scanID
		}
	}

	if latestScanID == "" {
		return false
	}

	// Check if any jobs from this scan are still active (not completed/failed).
	hasActive := false
	for i := range jobList.Items {
		if jobList.Items[i].Labels[LabelScanID] != latestScanID {
			continue
		}
		if jobList.Items[i].Status.Succeeded == 0 && !isJobFailed(&jobList.Items[i]) {
			hasActive = true
			break
		}
	}

	if !hasActive {
		// All jobs from the latest scan are done. Collect any uncollected results.
		c.log.Info("found completed scan from previous instance, collecting results",
			"scanID", latestScanID)
		c.collectRemainingResults(ctx, latestScanID, nil)
		return true
	}

	// There are still active jobs — monitor them.
	c.log.Info("resuming active scan from previous instance", "scanID", latestScanID)
	c.state.ScanID = latestScanID
	c.state.setPhase(ScanPhaseAnalyzing)
	c.monitorAndCollect(ctx, latestScanID, nil)
	return true
}

// runScan executes a full scan cycle: discover → plan → submit → monitor → collect → prune.
func (c *ImageAnalysisController) runScan(ctx context.Context) {
	c.state = &ScanState{
		Phase:   ScanPhaseDiscovering,
		StartAt: time.Now(),
	}

	c.log.Info("starting image analysis scan")

	// Phase 1: Discover images.
	discoverer := NewImageDiscoverer(c.k8sClient, c.config, c.log)
	discoveryResult, err := discoverer.Discover(ctx)
	if err != nil {
		c.log.Error(err, "image discovery failed")
		c.state.setPhase(ScanPhaseFailed)
		c.state.Error = fmt.Sprintf("discovery failed: %v", err)
		return
	}

	totalImages := 0
	for _, batch := range discoveryResult.NodeImages {
		totalImages += len(batch.Images)
	}
	c.log.Info("discovery complete",
		"uniqueImages", totalImages,
		"nodes", len(discoveryResult.NodeImages),
	)

	if totalImages == 0 {
		c.log.Info("no images to analyze, scan complete")
		c.state.setPhase(ScanPhaseCompleted)
		return
	}

	c.state.TotalUniqueImages = totalImages
	c.state.TotalNodes = len(discoveryResult.NodeImages)

	// Phase 2: Smart diff — skip images already analyzed in this scan window.
	filteredNodeImages := c.smartDiff(ctx, discoveryResult.NodeImages)
	filteredTotal := 0
	for _, batch := range filteredNodeImages {
		filteredTotal += len(batch.Images)
	}

	if filteredTotal == 0 {
		c.log.Info("all images already analyzed within scan window, skipping")
		c.state.setPhase(ScanPhaseCompleted)
		return
	}

	c.log.Info("after smart diff", "toAnalyze", filteredTotal, "skipped", totalImages-filteredTotal)

	// Phase 3: Plan and submit batch jobs.
	c.state.setPhase(ScanPhaseAnalyzing)
	jm := NewJobManager(c.k8sClient, c.config, c.log)
	scanID := GenerateScanID()
	c.state.ScanID = scanID

	batches := jm.PlanBatchJobs(filteredNodeImages, scanID)
	c.state.TotalBatches = len(batches)
	c.state.BatchesTotal = len(batches)

	c.log.Info("planned batch jobs", "batches", len(batches), "scanID", scanID)

	createdJobs, err := jm.SubmitBatches(ctx, batches)
	if err != nil {
		c.log.Error(err, "batch submission failed", "submitted", len(createdJobs))
		// Continue — some batches may have been submitted.
	}

	if len(createdJobs) == 0 {
		c.log.Error(nil, "no batches could be submitted")
		c.state.setPhase(ScanPhaseFailed)
		c.state.Error = "no batches submitted"
		return
	}

	c.log.Info("submitted batch jobs", "submitted", len(createdJobs), "total", len(batches))

	// Phase 4: Monitor jobs and collect results.
	c.monitorAndCollect(ctx, scanID, discoveryResult.WorkloadRefs)

	// Phase 5: Prune stale results.
	c.pruneStaleResults(ctx, discoveryResult)

	// Phase 6: Clean up old scan jobs.
	if _, err := jm.CleanupOldScanJobs(ctx, scanID); err != nil {
		c.log.Error(err, "failed to cleanup old scan jobs")
	}

	c.state.setPhase(ScanPhaseCompleted)
	c.log.Info("scan complete",
		"scanID", scanID,
		"imagesCompleted", c.state.ImagesCompleted,
		"imagesFailed", c.state.ImagesFailed,
		"localContainerd", c.state.SourceStats.LocalContainerd,
		"remotePull", c.state.SourceStats.RemotePull,
		"acquisitionFailed", c.state.SourceStats.AcquisitionFailed,
		"duration", time.Since(c.state.StartAt).Round(time.Second),
	)
}

// smartDiff filters out images that already have a recent ImageAnalysisResult.
func (c *ImageAnalysisController) smartDiff(ctx context.Context, nodeImages NodeImageMap) NodeImageMap {
	// List all existing ImageAnalysisResults.
	existingList := &v1.ImageAnalysisResultList{}
	if err := c.crdClient.List(ctx, existingList); err != nil {
		c.log.Error(err, "failed to list existing ImageAnalysisResults, analyzing all images")
		return nodeImages
	}

	// Build a set of recently analyzed digests.
	scanWindow := time.Duration(c.config.IntervalDays) * 24 * time.Hour
	cutoff := time.Now().Add(-scanWindow)
	recentlyAnalyzed := make(map[string]bool)

	for i := range existingList.Items {
		item := &existingList.Items[i]
		if item.Status.Phase == "Completed" && item.Status.AnalyzedAt.After(cutoff) {
			recentlyAnalyzed[item.Spec.ImageDigest] = true
		}
	}

	if len(recentlyAnalyzed) == 0 {
		return nodeImages
	}

	// Filter node images.
	filtered := make(NodeImageMap)
	for nodeName, batch := range nodeImages {
		var kept []ImageInfo
		for _, img := range batch.Images {
			if !recentlyAnalyzed[img.Digest] {
				kept = append(kept, img)
			}
		}
		if len(kept) > 0 {
			filtered[nodeName] = &NodeBatch{
				NodeName: nodeName,
				Images:   kept,
			}
		}
	}

	return filtered
}

// monitorAndCollect polls job statuses and collects results as jobs complete.
func (c *ImageAnalysisController) monitorAndCollect(
	ctx context.Context,
	scanID string,
	workloadRefs WorkloadRefMap,
) {
	c.state.setPhase(ScanPhaseCollecting)
	jm := NewJobManager(c.k8sClient, c.config, c.log)
	collector := NewResultCollector(c.k8sClient, c.crdClient, c.config, c.log)

	// Track which jobs we've already collected.
	collected := make(map[string]bool)
	timeout := time.Duration(c.config.JobTimeoutMinutes*2) * time.Minute
	deadline := time.Now().Add(timeout)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if time.Now().After(deadline) {
			c.log.Error(nil, "scan deadline reached, stopping collection", "scanID", scanID)
			c.state.Error = "scan deadline reached"
			return
		}

		jobList, err := jm.ListScanJobs(ctx, scanID)
		if err != nil {
			c.log.Error(err, "failed to list scan jobs")
			time.Sleep(jobPollInterval)
			continue
		}

		allDone := true
		for i := range jobList.Items {
			job := &jobList.Items[i]

			if collected[job.Name] {
				continue
			}

			if job.Status.Succeeded > 0 {
				// Job completed — collect results.
				cr, err := collector.CollectFromJob(ctx, job)
				if err != nil {
					c.log.Error(err, "failed to collect results from job", "job", job.Name)
					c.state.recordBatchFailed()
				} else {
					c.processCollectionResult(ctx, collector, cr, workloadRefs)
					c.state.recordBatchResult(cr)
				}
				collected[job.Name] = true
			} else if isJobFailed(job) {
				// Job failed entirely.
				c.log.Error(nil, "batch job failed", "job", job.Name,
					"node", job.Labels[LabelAnalysisNode])
				// Still try to collect partial results from failed jobs.
				cr, err := collector.CollectFromJob(ctx, job)
				if err != nil {
					c.log.V(1).Info("no results from failed job", "job", job.Name, "error", err)
					c.state.recordBatchFailed()
				} else {
					c.processCollectionResult(ctx, collector, cr, workloadRefs)
					c.state.recordBatchResult(cr)
				}
				collected[job.Name] = true
			} else {
				// Still running.
				allDone = false
			}
		}

		if allDone && len(collected) > 0 {
			c.log.Info("all batch jobs complete", "collected", len(collected))
			return
		}

		if len(jobList.Items) == 0 {
			c.log.Info("no jobs found for scan, may have been cleaned up", "scanID", scanID)
			return
		}

		time.Sleep(jobPollInterval)
	}
}

// processCollectionResult saves individual image results from a batch to CRDs.
func (c *ImageAnalysisController) processCollectionResult(
	ctx context.Context,
	collector *ResultCollector,
	cr *CollectionResult,
	workloadRefs WorkloadRefMap,
) {
	// Save successful results.
	for _, img := range cr.Succeeded {
		if err := collector.ProcessAndSave(ctx, img, cr.JobName, cr.NodeName, cr.ScanID, workloadRefs); err != nil {
			c.log.Error(err, "failed to save result", "image", img.Ref)
		}
	}

	// Save failed results.
	for _, img := range cr.Failed {
		if err := collector.SaveFailedResult(ctx, img, cr.JobName, cr.NodeName, cr.ScanID); err != nil {
			c.log.Error(err, "failed to save failed result", "image", img.Ref)
		}
	}
}

// collectRemainingResults collects results from completed jobs that haven't been processed yet.
func (c *ImageAnalysisController) collectRemainingResults(
	ctx context.Context,
	scanID string,
	workloadRefs WorkloadRefMap,
) {
	jm := NewJobManager(c.k8sClient, c.config, c.log)
	collector := NewResultCollector(c.k8sClient, c.crdClient, c.config, c.log)

	jobList, err := jm.ListScanJobs(ctx, scanID)
	if err != nil {
		c.log.Error(err, "failed to list jobs for result collection", "scanID", scanID)
		return
	}

	for i := range jobList.Items {
		job := &jobList.Items[i]
		if job.Status.Succeeded == 0 && !isJobFailed(job) {
			continue // still running
		}

		// Check if CRDs already exist for this job's images.
		// We do best-effort collection — if CRD already exists, ProcessAndSave will update it.
		cr, err := collector.CollectFromJob(ctx, job)
		if err != nil {
			c.log.V(1).Info("could not collect from job", "job", job.Name, "error", err)
			continue
		}

		c.processCollectionResult(ctx, collector, cr, workloadRefs)
	}
}

// pruneStaleResults removes ImageAnalysisResult CRDs for images no longer running in the cluster.
func (c *ImageAnalysisController) pruneStaleResults(ctx context.Context, discovery *DiscoveryResult) {
	if c.config.ResultRetentionDays <= 0 {
		return
	}

	existingList := &v1.ImageAnalysisResultList{}
	if err := c.crdClient.List(ctx, existingList); err != nil {
		c.log.Error(err, "failed to list ImageAnalysisResults for pruning")
		return
	}

	// Build set of currently discovered image digests.
	currentDigests := make(map[string]bool)
	for _, batch := range discovery.NodeImages {
		for _, img := range batch.Images {
			currentDigests[img.Digest] = true
		}
	}

	retentionCutoff := time.Now().Add(-time.Duration(c.config.ResultRetentionDays) * 24 * time.Hour)
	pruned := 0

	for i := range existingList.Items {
		item := &existingList.Items[i]
		// Only prune if the image is no longer discovered AND the result is older than retention.
		if !currentDigests[item.Spec.ImageDigest] && item.Status.AnalyzedAt.Time.Before(retentionCutoff) {
			if err := c.crdClient.Delete(ctx, item); err != nil {
				c.log.Error(err, "failed to prune stale result", "name", item.Name)
			} else {
				pruned++
			}
		}
	}

	if pruned > 0 {
		c.log.Info("pruned stale ImageAnalysisResults", "count", pruned)
	}
}

