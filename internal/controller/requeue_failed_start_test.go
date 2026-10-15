package controller

import (
	"testing"

	"github.com/devzero-inc/zxporter/internal/collector"
	"github.com/go-logr/logr"
)

// TestRequeueFailedStart_DeregistersAndQueuesFreshInstance covers the restart and
// re-enable paths: after their Start fails, the failed instance is removed (so a
// replacement is not refused as "already registered") and the type is queued, with a
// factory that builds a fresh collector registered under the same key.
func TestRequeueFailedStart_DeregistersAndQueuesFreshInstance(t *testing.T) {
	r, metricsClient := newSpecTestReconciler(t)
	key := collector.DaemonSet.String()
	failed := &keyedCollector{collectorType: key, resourceCh: make(chan []collector.CollectedResource, 1)}
	if err := r.CollectionManager.RegisterCollector(failed); err != nil {
		t.Fatal(err)
	}

	r.requeueFailedStart(logr.Discard(), key, key, metricsClient)
	r.requeueFailedStart(logr.Discard(), key, key, metricsClient) // idempotent

	if r.CollectionManager.GetCollector(key) != nil {
		t.Error("the instance whose Start failed is still registered")
	}
	if len(r.pendingCollectors) != 1 || r.pendingCollectors[0].name != collector.DaemonSet {
		t.Fatalf("want exactly one pending %q entry, got %+v", key, r.pendingCollectors)
	}
	fresh := r.pendingCollectors[0].factory()
	if fresh == nil || fresh == collector.ResourceCollector(failed) {
		t.Fatal("the pending factory must build a fresh collector")
	}
	if got := fresh.GetType(); got != key {
		t.Errorf("fresh collector GetType() = %q, want %q", got, key)
	}
}
