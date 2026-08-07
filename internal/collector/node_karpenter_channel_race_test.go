// internal/collector/node_karpenter_channel_race_test.go
package collector

import (
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

// These are the regression tests for the same bug class the automated reviewer caught (and
// this PR already fixed) in cluster_autoscaler_status_collector.go: an informer callback
// sending directly on a channel that Stop() can close concurrently, with nothing tracking
// the callback goroutine to make the two mutually exclusive. NodeCollector.handleNodeEvent /
// sendNodeLifecycleTransition and KarpenterCollector's batchChan senders had the identical
// gap — chanMu now closes it in both, the same way it already does for CAS.

// newRaceTestNodeCollector builds a NodeCollector with a real batcher wired up (start()ed,
// so resourceChan is actually closed when Stop's batcher.stop() runs) and nothing else —
// the fields sendResourceEvent and Stop touch.
func newRaceTestNodeCollector() *NodeCollector {
	batchChan := make(chan CollectedResource, 64)
	resourceChan := make(chan []CollectedResource, 64)
	batcher := NewResourcesBatcher(DefaultBatchSize, DefaultMaxBatchTime, batchChan, resourceChan, logr.Discard())
	batcher.start()

	return &NodeCollector{
		batchChan:     batchChan,
		resourceChan:  resourceChan,
		batcher:       batcher,
		stopCh:        make(chan struct{}),
		excludedNodes: map[string]bool{},
		logger:        logr.Discard(),
		nodeToPodsMap: make(map[string]map[string]*corev1.Pod),
		nodeLifecycle: make(map[string]*nodeLifecycleState),
	}
}

func raceTestNode() *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}
}

// TestNodeCollector_StopIsSafeAfterStopped pins the deterministic half: once Stop has run,
// a direct send must no-op rather than touch the now-closed resourceChan.
func TestNodeCollector_StopIsSafeAfterStopped(t *testing.T) {
	c := newRaceTestNodeCollector()

	require.NoError(t, c.Stop())

	require.NotPanics(t, func() {
		c.handleNodeEvent(raceTestNode(), EventTypeAdd)
	}, "handleNodeEvent after Stop must be a no-op, not a send on the closed resourceChan")
	require.NotPanics(t, func() {
		c.sendNodeLifecycleTransition(raceTestNode(), "Ready", time.Now())
	}, "sendNodeLifecycleTransition after Stop must be a no-op, not a send on the closed resourceChan")
}

// TestNodeCollector_StopDoesNotRaceConcurrentSends drives real concurrency — many goroutines
// racing direct resourceChan sends against a concurrent Stop — rather than asserting the fix
// mechanism directly, so it would have caught the original gap: on the pre-fix code (no
// chanMu guard) this panics under -race nearly every run.
func TestNodeCollector_StopDoesNotRaceConcurrentSends(t *testing.T) {
	c := newRaceTestNodeCollector()

	const senders = 32
	var wg sync.WaitGroup
	panics := make(chan any, senders)

	// Drain resourceChan concurrently, the way the real CollectionManager's
	// GetResourceChannel() reader does — without this, the 32 senders below fill its
	// bounded buffer almost immediately and every subsequent send blocks holding chanMu's
	// read lock, which would make Stop's write-lock acquisition (and so the test) hang
	// rather than exercise the actual close race.
	go func() {
		for range c.resourceChan { //nolint:revive // draining is the point
		}
	}()

	wg.Add(senders)
	for i := 0; i < senders; i++ {
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					panics <- r
				}
			}()
			// Simulate the Node informer dispatching a burst of Add/Update callbacks
			// concurrently with the collector shutting down.
			for j := 0; j < 50; j++ {
				c.handleNodeEvent(raceTestNode(), EventTypeAdd)
				c.sendNodeLifecycleTransition(raceTestNode(), "Ready", time.Now())
			}
		}()
	}

	// Give the senders a head start so at least some are genuinely in-flight when Stop
	// runs, rather than Stop trivially winning a race that never happens.
	time.Sleep(time.Millisecond)
	require.NoError(t, c.Stop())

	wg.Wait()
	close(panics)

	for p := range panics {
		t.Fatalf("a direct resourceChan send panicked concurrently with Stop: %v", p)
	}
}

// newRaceTestKarpenterCollector builds a KarpenterCollector with a real batcher wired up
// and nothing else — the fields the batchChan senders and Stop touch.
func newRaceTestKarpenterCollector() *KarpenterCollector {
	batchChan := make(chan CollectedResource, 64)
	resourceChan := make(chan []CollectedResource, 64)
	batcher := NewResourcesBatcher(DefaultBatchSize, DefaultMaxBatchTime, batchChan, resourceChan, logr.Discard())
	batcher.start()

	return &KarpenterCollector{
		batchChan:           batchChan,
		resourceChan:        resourceChan,
		batcher:             batcher,
		stopCh:              make(chan struct{}),
		logger:              logr.Discard(),
		informers:           make(map[string]cache.SharedIndexInformer),
		informerStopChs:     make(map[string]chan struct{}),
		excludedResources:   make(map[string]map[string]bool),
		nodeClaimConditions: make(map[string]map[string]nodeClaimConditionState),
	}
}

// TestKarpenterCollector_StopIsSafeAfterStopped mirrors
// TestNodeCollector_StopIsSafeAfterStopped for KarpenterCollector's batchChan senders.
func TestKarpenterCollector_StopIsSafeAfterStopped(t *testing.T) {
	c := newRaceTestKarpenterCollector()

	require.NoError(t, c.Stop())

	require.NotPanics(t, func() {
		c.sendBatchResource(CollectedResource{ResourceType: Karpenter, Key: "k1"})
	}, "sendBatchResource after Stop must be a no-op, not a send on the closed batchChan")
}

// TestKarpenterCollector_StopDoesNotRaceConcurrentSends mirrors
// TestNodeCollector_StopDoesNotRaceConcurrentSends: KarpenterCollector.Stop() had NO
// synchronization at all against in-flight informer callbacks before this fix (unlike
// NodeCollector, which at least waited on loopWG for its periodic sweep) — every
// processNodeClaim/handleKarpenterResourceEvent call sending on batchChan could race Stop's
// close outright.
func TestKarpenterCollector_StopDoesNotRaceConcurrentSends(t *testing.T) {
	c := newRaceTestKarpenterCollector()

	const senders = 32
	var wg sync.WaitGroup
	panics := make(chan any, senders)

	wg.Add(senders)
	for i := 0; i < senders; i++ {
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					panics <- r
				}
			}()
			for j := 0; j < 50; j++ {
				c.sendBatchResource(CollectedResource{ResourceType: Karpenter, Key: "k1"})
			}
		}()
	}

	time.Sleep(time.Millisecond)
	require.NoError(t, c.Stop())

	wg.Wait()
	close(panics)

	for p := range panics {
		t.Fatalf("a batchChan send panicked concurrently with Stop: %v", p)
	}
}
