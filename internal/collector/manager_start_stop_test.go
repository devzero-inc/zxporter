package collector

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blockingStartCollector's Start parks until release is closed, so a test can stop it
// while its Start is still in flight. Its resource channel is never closed, so a
// processor attached to it would run forever; resourceChannelCalls counts processor
// launches (processCollectorChannel's first action is GetResourceChannel).
type blockingStartCollector struct {
	collectorType        string
	entered              chan struct{}
	release              chan struct{}
	resourceCh           chan []CollectedResource
	stopCalls            atomic.Int32
	resourceChannelCalls atomic.Int32
}

func newBlockingStartCollector(collectorType string) *blockingStartCollector {
	return &blockingStartCollector{
		collectorType: collectorType,
		entered:       make(chan struct{}),
		release:       make(chan struct{}),
		resourceCh:    make(chan []CollectedResource),
	}
}

func (c *blockingStartCollector) Start(context.Context) error {
	close(c.entered)
	<-c.release
	return nil
}
func (c *blockingStartCollector) Stop() error     { c.stopCalls.Add(1); return nil }
func (c *blockingStartCollector) GetType() string { return c.collectorType }
func (c *blockingStartCollector) GetResourceChannel() <-chan []CollectedResource {
	c.resourceChannelCalls.Add(1)
	return c.resourceCh
}
func (c *blockingStartCollector) IsAvailable(context.Context) bool       { return true }
func (c *blockingStartCollector) AddResource(resource interface{}) error { return nil }

// TestStopWhileStarting_NoProcessorAndStoppedOnce covers a stop that lands while the
// collector's Start is still running. The stop must not wait for Start (the starter needs
// the mutex to finish), the collector must be Stopped exactly once (by whichever side
// owns it at that moment, never both), and no processor may be attached to a collector
// that was already stopped: its channel is never closed, so that goroutine would leak.
func TestStopWhileStarting_NoProcessorAndStoppedOnce(t *testing.T) {
	stops := map[string]func(*CollectionManager, string){
		"StopCollector":       func(m *CollectionManager, ct string) { _ = m.StopCollector(ct) },
		"DeregisterCollector": func(m *CollectionManager, ct string) { _ = m.DeregisterCollector(ct) },
		"StopAll": func(m *CollectionManager, _ string) {
			m.mu.Lock()
			m.started = true // StopAll is a no-op on a manager StartAll never started
			m.mu.Unlock()
			_ = m.StopAll()
		},
	}
	for name, stop := range stops {
		t.Run(name, func(t *testing.T) {
			mgr := newLockTestManager()
			c := newBlockingStartCollector("slow")
			require.NoError(t, mgr.RegisterCollector(c))

			startErr := make(chan error, 1)
			go func() { startErr <- mgr.StartCollector(context.Background(), "slow") }()
			<-c.entered

			require.True(t, completesWithin(func() { stop(mgr, "slow") }),
				"stopping a collector whose Start is in flight must not wait for that Start")

			close(c.release)
			select {
			case err := <-startErr:
				assert.Error(t, err, "a start overtaken by a stop reports that it did not take effect")
			case <-time.After(lockTestTimeout):
				t.Fatal("StartCollector did not return after its Start was released")
			}

			// A processor is spawned asynchronously; give one that was wrongly launched the
			// chance to run before asserting that none was.
			time.Sleep(50 * time.Millisecond)
			assert.Equal(t, int32(1), c.stopCalls.Load(), "the collector is stopped exactly once")
			assert.Equal(t, int32(0), c.resourceChannelCalls.Load(),
				"no processor may be attached to a collector stopped mid-start")
			assert.False(t, mgr.IsCollectorRunning("slow"))
		})
	}
}

// TestStopCollector_RemovesRegisteredButNotRunning covers a collector whose Start failed
// or never ran: stopping it must clear it, or the replacement a restart registers next is
// refused as "already registered".
func TestStopCollector_RemovesRegisteredButNotRunning(t *testing.T) {
	mgr := newLockTestManager()
	require.NoError(t, mgr.RegisterCollector(
		&mockCollector{collectorType: "pod", resourceCh: make(chan []CollectedResource, 1)}))

	_ = mgr.StopCollector("pod")

	assert.Nil(t, mgr.GetCollector("pod"))
	assert.NoError(t, mgr.RegisterCollector(
		&mockCollector{collectorType: "pod", resourceCh: make(chan []CollectedResource, 1)}))
}
