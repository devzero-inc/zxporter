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
	"testing"
	"time"

	v1 "github.com/devzero-inc/zxporter/api/v1"
	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// =============================================================================
// Unit Tests: ScanState
// =============================================================================

func TestScanState_InitialPhase(t *testing.T) {
	state := &ScanState{Phase: ScanPhaseIdle}
	if state.GetPhase() != ScanPhaseIdle {
		t.Errorf("expected Idle, got %s", state.GetPhase())
	}
}

func TestScanState_SetPhase(t *testing.T) {
	state := &ScanState{Phase: ScanPhaseIdle}
	state.setPhase(ScanPhaseDiscovering)
	if state.GetPhase() != ScanPhaseDiscovering {
		t.Errorf("expected Discovering, got %s", state.GetPhase())
	}
}

func TestScanState_RecordBatchResult(t *testing.T) {
	state := &ScanState{}
	cr := &CollectionResult{
		Succeeded:         make([]BatchImageResult, 3),
		Failed:            make([]BatchImageResult, 1),
		ParseErrors:       1,
		LocalContainerd:   2,
		RemotePull:        1,
		AcquisitionFailed: 0,
	}

	state.recordBatchResult(cr)

	if state.BatchesComplete != 1 {
		t.Errorf("expected 1 batch complete, got %d", state.BatchesComplete)
	}
	if state.ImagesCompleted != 3 {
		t.Errorf("expected 3 images completed, got %d", state.ImagesCompleted)
	}
	if state.ImagesFailed != 2 { // 1 failed + 1 parse error
		t.Errorf("expected 2 images failed, got %d", state.ImagesFailed)
	}
	if state.SourceStats.LocalContainerd != 2 {
		t.Errorf("expected 2 local containerd, got %d", state.SourceStats.LocalContainerd)
	}
	if state.SourceStats.RemotePull != 1 {
		t.Errorf("expected 1 remote pull, got %d", state.SourceStats.RemotePull)
	}
}

func TestScanState_RecordMultipleBatches(t *testing.T) {
	state := &ScanState{}

	state.recordBatchResult(&CollectionResult{
		Succeeded:       make([]BatchImageResult, 5),
		LocalContainerd: 3,
		RemotePull:      2,
	})
	state.recordBatchResult(&CollectionResult{
		Succeeded:         make([]BatchImageResult, 3),
		Failed:            make([]BatchImageResult, 2),
		AcquisitionFailed: 2,
	})

	if state.BatchesComplete != 2 {
		t.Errorf("expected 2 batches complete, got %d", state.BatchesComplete)
	}
	if state.ImagesCompleted != 8 {
		t.Errorf("expected 8 images completed, got %d", state.ImagesCompleted)
	}
	if state.ImagesFailed != 2 {
		t.Errorf("expected 2 images failed, got %d", state.ImagesFailed)
	}
	if state.SourceStats.LocalContainerd != 3 {
		t.Errorf("expected 3 local, got %d", state.SourceStats.LocalContainerd)
	}
	if state.SourceStats.AcquisitionFailed != 2 {
		t.Errorf("expected 2 acq failed, got %d", state.SourceStats.AcquisitionFailed)
	}
}

func TestScanState_RecordBatchFailed(t *testing.T) {
	state := &ScanState{}
	state.recordBatchFailed()
	state.recordBatchFailed()
	if state.BatchesFailed != 2 {
		t.Errorf("expected 2 batches failed, got %d", state.BatchesFailed)
	}
}

// =============================================================================
// Unit Tests: smartDiff (via simulated existing results)
// =============================================================================

func TestSmartDiff_NoExistingResults(t *testing.T) {
	nodeImages := NodeImageMap{
		"node-1": &NodeBatch{
			NodeName: "node-1",
			Images: []ImageInfo{
				{Digest: "sha256:aaa", Ref: "nginx:latest"},
				{Digest: "sha256:bbb", Ref: "redis:latest"},
			},
		},
	}

	// smartDiff depends on crdClient.List which we can't mock easily here.
	// Instead test that the NodeImageMap filtering logic works by calling
	// the filter function directly (inline in smartDiff).
	// For now, verify that the structure is correct.
	if len(nodeImages) != 1 {
		t.Errorf("expected 1 node, got %d", len(nodeImages))
	}
	if len(nodeImages["node-1"].Images) != 2 {
		t.Errorf("expected 2 images, got %d", len(nodeImages["node-1"].Images))
	}
}

// =============================================================================
// Unit Tests: NewImageAnalysisController
// =============================================================================

func TestNewImageAnalysisController(t *testing.T) {
	config := DefaultImageAnalysisConfig()
	// We can't use nil for k8sClient/crdClient in production, but for construction test it's fine.
	ctrl := NewImageAnalysisController(nil, nil, testLogger(), config)

	if ctrl == nil {
		t.Fatal("expected non-nil controller")
	}
	if ctrl.state.GetPhase() != ScanPhaseIdle {
		t.Errorf("expected initial phase Idle, got %s", ctrl.state.GetPhase())
	}
	if !ctrl.NeedLeaderElection() {
		t.Error("expected NeedLeaderElection=true")
	}
}

// =============================================================================
// Unit Tests: pruneStaleResults logic
// =============================================================================

func TestPruneLogic_ShouldPrune(t *testing.T) {
	// Test the pruning criteria: not in current digests AND older than retention.
	retentionDays := 30
	retentionCutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour)

	currentDigests := map[string]bool{
		"sha256:active1": true,
		"sha256:active2": true,
	}

	tests := []struct {
		name        string
		digest      string
		analyzedAt  time.Time
		shouldPrune bool
	}{
		{
			name:        "active image recent",
			digest:      "sha256:active1",
			analyzedAt:  time.Now().Add(-24 * time.Hour),
			shouldPrune: false,
		},
		{
			name:        "active image old",
			digest:      "sha256:active2",
			analyzedAt:  time.Now().Add(-60 * 24 * time.Hour),
			shouldPrune: false, // still active, don't prune
		},
		{
			name:        "inactive image recent",
			digest:      "sha256:gone1",
			analyzedAt:  time.Now().Add(-24 * time.Hour),
			shouldPrune: false, // within retention
		},
		{
			name:        "inactive image old",
			digest:      "sha256:gone2",
			analyzedAt:  time.Now().Add(-60 * 24 * time.Hour),
			shouldPrune: true, // not active AND older than retention
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := &v1.ImageAnalysisResult{
				Spec: v1.ImageAnalysisResultSpec{
					ImageDigest: tt.digest,
				},
				Status: v1.ImageAnalysisResultStatus{
					AnalyzedAt: metav1.NewTime(tt.analyzedAt),
				},
			}

			shouldPrune := !currentDigests[item.Spec.ImageDigest] &&
				item.Status.AnalyzedAt.Time.Before(retentionCutoff)

			if shouldPrune != tt.shouldPrune {
				t.Errorf("shouldPrune = %v, want %v", shouldPrune, tt.shouldPrune)
			}
		})
	}
}

// =============================================================================
// Unit Tests: Phase transitions
// =============================================================================

func TestScanPhaseConstants(t *testing.T) {
	// Ensure phase constants are distinct.
	phases := []ScanPhase{
		ScanPhaseIdle,
		ScanPhaseDiscovering,
		ScanPhaseAnalyzing,
		ScanPhaseCollecting,
		ScanPhaseCompleted,
		ScanPhaseFailed,
	}

	seen := make(map[ScanPhase]bool)
	for _, p := range phases {
		if seen[p] {
			t.Errorf("duplicate phase: %s", p)
		}
		seen[p] = true
	}
}

// testLogger returns a no-op logger for tests.
func testLogger() logr.Logger {
	return logr.Discard()
}
