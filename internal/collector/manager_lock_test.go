package collector

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// lockTestTimeout bounds every "does this call come back" check below. The calls under
// test are map reads and writes, so anything near this long is a deadlock, not slowness.
const lockTestTimeout = 2 * time.Second

func newLockTestManager() *CollectionManager {
	return NewCollectionManager(
		&CollectionConfig{BufferSize: 10},
		nil,
		getTestTelemetryMetrics(),
		logr.Discard(),
		&noopTelemetryLogger{},
		nil,
	)
}

// completesWithin runs fn on its own goroutine and reports whether it returned within
// lockTestTimeout. On a timeout the goroutine is left blocked: that is the bug being detected,
// and there is no way to unblock a goroutine parked on a leaked mutex.
func completesWithin(fn func()) bool {
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(lockTestTimeout):
		return false
	}
}

// assertManagerUsable fails the test if the manager's mutex is no longer obtainable, by
// exercising one reader and one writer that both take it.
func assertManagerUsable(t *testing.T, mgr *CollectionManager) {
	t.Helper()
	ok := completesWithin(func() {
		_ = mgr.GetCollectorTypes()
		_ = mgr.RegisterCollector(&mockCollector{
			collectorType: "after_failed_start",
			resourceCh:    make(chan []CollectedResource, 1),
		})
	})
	require.True(t, ok, "the manager mutex was not released: a later lock holder never returned")
}

// TestStartCollector_UnregisteredTypeReleasesLock is the preprod incident: a Start for a
// type that was never registered (the KEDA name/key mismatch) returned with the manager
// mutex still held, wedging every later caller — including the node informer's handler.
func TestStartCollector_UnregisteredTypeReleasesLock(t *testing.T) {
	mgr := newLockTestManager()

	err := mgr.StartCollector(context.Background(), "keda_scaled_job")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not registered")

	assertManagerUsable(t, mgr)
}

// TestStartCollector_AlreadyRunningReleasesLock covers the other early return.
func TestStartCollector_AlreadyRunningReleasesLock(t *testing.T) {
	mgr := newLockTestManager()
	mock := &mockCollector{collectorType: "test_resource", resourceCh: make(chan []CollectedResource, 1)}
	require.NoError(t, mgr.RegisterCollector(mock))
	require.NoError(t, mgr.StartCollector(context.Background(), "test_resource"))

	err := mgr.StartCollector(context.Background(), "test_resource")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already running")

	assertManagerUsable(t, mgr)
	assert.True(t, completesWithin(func() { _ = mgr.StopCollector("test_resource") }))
}

// TestGetCollector_DoesNotWaitOnManagerMutex pins the hardening: GetCollector is called
// from informer event handlers (node lifecycle fallback, DisruptionBlocked
// classification), and a handler that blocks stalls that informer's whole listener. It
// must stay answerable while something else holds the manager mutex — for instance
// stopCollectorInternal, which holds it across a collector's Stop and a 5s drain wait.
func TestGetCollector_DoesNotWaitOnManagerMutex(t *testing.T) {
	mgr := newLockTestManager()
	mock := &mockCollector{collectorType: "karpenter", resourceCh: make(chan []CollectedResource, 1)}
	require.NoError(t, mgr.RegisterCollector(mock))

	mgr.mu.Lock()
	defer mgr.mu.Unlock()

	var got ResourceCollector
	require.True(t, completesWithin(func() { got = mgr.GetCollector("karpenter") }),
		"GetCollector blocked on the manager mutex")
	assert.Same(t, mock, got)
	assert.True(t, completesWithin(func() { got = mgr.GetCollector("missing") }))
	assert.Nil(t, got)
}

// TestGetCollector_TracksRegistrationAndRemoval checks the lock-free view stays in step
// with the registry across register, stop and re-register.
func TestGetCollector_TracksRegistrationAndRemoval(t *testing.T) {
	mgr := newLockTestManager()
	first := &mockCollector{collectorType: "node", resourceCh: make(chan []CollectedResource, 1)}
	require.NoError(t, mgr.RegisterCollector(first))
	assert.Same(t, first, mgr.GetCollector("node"))

	require.NoError(t, mgr.StartCollector(context.Background(), "node"))
	require.NoError(t, mgr.StopCollector("node"))
	assert.Nil(t, mgr.GetCollector("node"), "a stopped collector is removed from the registry")

	second := &mockCollector{collectorType: "node", resourceCh: make(chan []CollectedResource, 1)}
	require.NoError(t, mgr.RegisterCollector(second))
	assert.Same(t, second, mgr.GetCollector("node"))

	require.NoError(t, mgr.DeregisterCollector("node"))
	assert.Nil(t, mgr.GetCollector("node"))
}

// TestNodeLifecycleFallback_DoesNotBlockOnManagerMutex is the amplifier from the incident,
// end to end: the node informer's handler consults the Karpenter NodeClaim cache through
// the manager. With the manager mutex held it must still return.
func TestNodeLifecycleFallback_DoesNotBlockOnManagerMutex(t *testing.T) {
	mgr := newLockTestManager()
	c, _ := newLifecycleTestNodeCollector()
	c.SetNodeClaimSource(mgr)

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "ip-10-0-9-9", Labels: map[string]string{}}}

	mgr.mu.Lock()
	defer mgr.mu.Unlock()

	var managed bool
	require.True(t, completesWithin(func() { managed = c.nodeManagedByKarpenter(node) }),
		"the node informer handler blocked on the collection manager mutex")
	assert.False(t, managed)
}
