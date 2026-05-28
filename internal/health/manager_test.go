package health

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// observedTransition records a single TransitionObserver invocation.
type observedTransition struct {
	component string
	oldStatus HealthStatus
	newStatus HealthStatus
	message   string
	metadata  map[string]string
}

// observerFixture wraps a HealthManager pre-wired with a transition observer
// that records every call. Use snapshot() to read calls under the lock.
type observerFixture struct {
	hm    *HealthManager
	mu    sync.Mutex
	calls []observedTransition
}

// newObserverFixture builds a HealthManager with the given components registered
// and a recording observer attached. t.Helper() is set so any failures inside
// this constructor surface at the calling test line, not inside the helper.
func newObserverFixture(t *testing.T, components ...string) *observerFixture {
	t.Helper()
	f := &observerFixture{hm: NewHealthManager()}
	for _, c := range components {
		f.hm.Register(c)
	}
	f.hm.SetTransitionObserver(func(component string, oldStatus, newStatus HealthStatus, message string, metadata map[string]string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, observedTransition{component, oldStatus, newStatus, message, metadata})
	})
	return f
}

// snapshot returns a copy of the recorded transitions taken under the lock so
// the caller can assert on it without racing the observer.
func (f *observerFixture) snapshot() []observedTransition {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]observedTransition, len(f.calls))
	copy(out, f.calls)
	return out
}

const testCollectorManager = "collector_manager"

// Test cases for HealthManager
func TestRegisterAndUpdateStatus_CollectorManager(t *testing.T) {
	healthMgr := NewHealthManager()
	component := testCollectorManager
	metadata := map[string]string{"active": "55", "total": "60"}

	healthMgr.Register(component)
	healthMgr.UpdateStatus(component, HealthStatusHealthy, "55/60 collectors active", metadata)

	status, exists := healthMgr.GetStatus(component)
	assert.True(t, exists, "collector_manager should be registered")
	assert.Equal(t, HealthStatusHealthy, status.Status)
	assert.Equal(t, "55/60 collectors active", status.Message)
	assert.Equal(t, metadata, status.Metadata)
}

func TestUpdateStatus_Degraded(t *testing.T) {
	healthMgr := NewHealthManager()
	component := testCollectorManager
	metadata := map[string]string{"active": "30", "total": "60"}

	healthMgr.Register(component)
	healthMgr.UpdateStatus(component, HealthStatusDegraded, "30/60 collectors active", metadata)

	status, exists := healthMgr.GetStatus(component)
	assert.True(t, exists, "collector_manager should be registered")
	assert.Equal(t, HealthStatusDegraded, status.Status)
	assert.Equal(t, "30/60 collectors active", status.Message)
	assert.Equal(t, metadata, status.Metadata)
}

func TestUpdateStatus_Unhealthy(t *testing.T) {
	healthMgr := NewHealthManager()
	component := testCollectorManager
	metadata := map[string]string{"active": "0", "total": "60"}

	healthMgr.Register(component)
	healthMgr.UpdateStatus(component, HealthStatusUnhealthy, "No collectors running", metadata)

	status, exists := healthMgr.GetStatus(component)
	assert.True(t, exists, "collector_manager should be registered")
	assert.Equal(t, HealthStatusUnhealthy, status.Status)
	assert.Equal(t, "No collectors running", status.Message)
	assert.Equal(t, metadata, status.Metadata)
}

func TestConcurrentUpdatesAndReads(t *testing.T) {
	healthMgr := NewHealthManager()
	component := testCollectorManager
	healthMgr.Register(component)

	statuses := []HealthStatus{HealthStatusHealthy, HealthStatusDegraded, HealthStatusUnhealthy}
	messages := []string{"All good", "Some issues", "Down"}

	var wg sync.WaitGroup
	updateCount := 100

	// Start goroutines to update status
	for i := range updateCount {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			status := statuses[i%len(statuses)]
			msg := messages[i%len(messages)]
			meta := map[string]string{"iteration": strconv.Itoa(i)}
			healthMgr.UpdateStatus(component, status, msg, meta)
		}(i)
	}

	// Start goroutines to read status
	for range updateCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = healthMgr.GetStatus(component)
			_ = healthMgr.BuildReport()
		}()
	}

	wg.Wait()

	// Final state should be one of the possible values
	status, exists := healthMgr.GetStatus(component)
	assert.True(t, exists, "collector_manager should be registered")
	assert.Contains(t, statuses, status.Status)
	assert.Contains(t, messages, status.Message)
}

// Test case for HealthStatus String method
func TestHealthStatus_String(t *testing.T) {
	tests := []struct {
		status   HealthStatus
		expected string
	}{
		{HealthStatusUnspecified, "unspecified"},
		{HealthStatusHealthy, "healthy"},
		{HealthStatusDegraded, "degraded"},
		{HealthStatusUnhealthy, "unhealthy"},
		{HealthStatus(99), "unspecified"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.expected, tt.status.String())
	}
}

/*
Readiness and Healthiness tests
*/func TestLivenessCheck_Healthy(t *testing.T) {
	hm := NewHealthManager()
	hm.Register(ComponentCollectorManager)
	hm.UpdateStatus(ComponentCollectorManager, HealthStatusHealthy, "all good", nil)

	err := hm.LivenessCheck()
	assert.NoError(t, err)
}

func TestLivenessCheck_DegradedIsStillAlive(t *testing.T) {
	hm := NewHealthManager()
	hm.Register(ComponentCollectorManager)
	hm.UpdateStatus(ComponentCollectorManager, HealthStatusDegraded, "some issues", nil)

	err := hm.LivenessCheck()
	assert.NoError(t, err)
}

func TestLivenessCheck_UnhealthyCollectorFails(t *testing.T) {
	hm := NewHealthManager()
	hm.Register(ComponentCollectorManager)
	hm.UpdateStatus(ComponentCollectorManager, HealthStatusUnhealthy, "no collectors running", nil)

	err := hm.LivenessCheck()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "collector_manager")
}

func TestLivenessCheck_UnspecifiedPasses(t *testing.T) {
	hm := NewHealthManager()
	hm.Register(ComponentCollectorManager)
	// Status is Unspecified (startup) — should still pass liveness
	err := hm.LivenessCheck()
	assert.NoError(t, err)
}

func TestReadinessCheck_AllReady(t *testing.T) {
	hm := NewHealthManager()
	hm.Register(ComponentCollectorManager)
	hm.Register(ComponentDakrTransport)
	hm.UpdateStatus(ComponentCollectorManager, HealthStatusHealthy, "running", nil)
	hm.UpdateStatus(ComponentDakrTransport, HealthStatusHealthy, "connected", nil)

	err := hm.ReadinessCheck()
	assert.NoError(t, err)
}

func TestReadinessCheck_DegradedIsReady(t *testing.T) {
	hm := NewHealthManager()
	hm.Register(ComponentCollectorManager)
	hm.Register(ComponentDakrTransport)
	hm.UpdateStatus(ComponentCollectorManager, HealthStatusDegraded, "some issues", nil)
	hm.UpdateStatus(ComponentDakrTransport, HealthStatusDegraded, "retrying", nil)

	err := hm.ReadinessCheck()
	assert.NoError(t, err)
}

func TestReadinessCheck_CollectorUnspecifiedNotReady(t *testing.T) {
	hm := NewHealthManager()
	hm.Register(ComponentCollectorManager)
	hm.Register(ComponentDakrTransport)
	// collector_manager still Unspecified (not started yet)
	hm.UpdateStatus(ComponentDakrTransport, HealthStatusHealthy, "connected", nil)

	err := hm.ReadinessCheck()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "collector_manager")
}

func TestReadinessCheck_TransportUnhealthyNotReady(t *testing.T) {
	hm := NewHealthManager()
	hm.Register(ComponentCollectorManager)
	hm.Register(ComponentDakrTransport)
	hm.UpdateStatus(ComponentCollectorManager, HealthStatusHealthy, "running", nil)
	hm.UpdateStatus(ComponentDakrTransport, HealthStatusUnhealthy, "cannot reach dakr", nil)

	err := hm.ReadinessCheck()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "dakr_transport")
}

// Readiness suppression tests — ensures leader election delay doesn't block readiness

func TestReadinessCheck_SuppressedDuringGracePeriod(t *testing.T) {
	hm := NewHealthManager()
	hm.Register(ComponentCollectorManager)
	hm.Register(ComponentDakrTransport)
	// Both components are Unspecified (startup) — readiness normally fails
	err := hm.ReadinessCheck()
	assert.Error(t, err)

	// Suppress readiness for leader election grace
	hm.SuppressReadiness(5 * time.Minute)

	// Now readiness should pass even though components are Unspecified
	err = hm.ReadinessCheck()
	assert.NoError(t, err)
}

func TestReadinessCheck_ClearSuppression(t *testing.T) {
	hm := NewHealthManager()
	hm.Register(ComponentCollectorManager)
	hm.Register(ComponentDakrTransport)

	hm.SuppressReadiness(5 * time.Minute)
	assert.NoError(t, hm.ReadinessCheck()) // suppressed

	// Clear suppression — should fail again since components are still Unspecified
	hm.ClearReadinessSuppression()
	assert.Error(t, hm.ReadinessCheck())
}

func TestReadinessCheck_GracePeriodExpires(t *testing.T) {
	hm := NewHealthManager()
	hm.Register(ComponentCollectorManager)
	hm.Register(ComponentDakrTransport)

	// Set a grace period that is already expired
	hm.mu.Lock()
	hm.readinessGraceUntil = time.Now().Add(-1 * time.Second)
	hm.mu.Unlock()

	// Should fail because grace period has expired and components are Unspecified
	err := hm.ReadinessCheck()
	assert.Error(t, err)
}

func TestReadinessCheck_FullStartupCycle(t *testing.T) {
	hm := NewHealthManager()
	hm.Register(ComponentCollectorManager)
	hm.Register(ComponentDakrTransport)

	// Simulate startup: suppress readiness while waiting for leader election
	hm.SuppressReadiness(5 * time.Minute)
	assert.NoError(t, hm.ReadinessCheck()) // passes during grace

	// Simulate controller starting: components become healthy, clear suppression
	hm.UpdateStatus(ComponentCollectorManager, HealthStatusHealthy, "running", nil)
	hm.UpdateStatus(ComponentDakrTransport, HealthStatusHealthy, "connected", nil)
	hm.ClearReadinessSuppression()
	assert.NoError(t, hm.ReadinessCheck()) // passes normally
}

// Liveness suppression tests — ensures planned restarts don't cause pod kills

func TestLivenessCheck_SuppressedDuringGracePeriod(t *testing.T) {
	hm := NewHealthManager()
	hm.Register(ComponentCollectorManager)
	hm.UpdateStatus(ComponentCollectorManager, HealthStatusUnhealthy, "stopped for restart", nil)

	// Without suppression, liveness should fail
	err := hm.LivenessCheck()
	assert.Error(t, err)

	// Suppress liveness for planned restart
	hm.SuppressLiveness(5 * time.Minute)

	// Now liveness should pass even though collector_manager is Unhealthy
	err = hm.LivenessCheck()
	assert.NoError(t, err)
}

func TestLivenessCheck_ClearSuppression(t *testing.T) {
	hm := NewHealthManager()
	hm.Register(ComponentCollectorManager)
	hm.UpdateStatus(ComponentCollectorManager, HealthStatusUnhealthy, "stopped", nil)

	hm.SuppressLiveness(5 * time.Minute)
	assert.NoError(t, hm.LivenessCheck()) // suppressed

	// Clear suppression — should fail again since still Unhealthy
	hm.ClearLivenessSuppression()
	assert.Error(t, hm.LivenessCheck())
}

func TestLivenessCheck_GracePeriodExpires(t *testing.T) {
	hm := NewHealthManager()
	hm.Register(ComponentCollectorManager)
	hm.UpdateStatus(ComponentCollectorManager, HealthStatusUnhealthy, "stopped", nil)

	// Set a grace period that is already expired
	hm.mu.Lock()
	hm.livenessGraceUntil = time.Now().Add(-1 * time.Second)
	hm.mu.Unlock()

	// Should fail because grace period has expired
	err := hm.LivenessCheck()
	assert.Error(t, err)
}

func TestLivenessCheck_FullRestartCycle(t *testing.T) {
	hm := NewHealthManager()
	hm.Register(ComponentCollectorManager)
	hm.UpdateStatus(ComponentCollectorManager, HealthStatusHealthy, "running", nil)
	assert.NoError(t, hm.LivenessCheck())

	// Simulate planned restart: suppress, then set unhealthy
	hm.SuppressLiveness(5 * time.Minute)
	hm.UpdateStatus(ComponentCollectorManager, HealthStatusUnhealthy, "stopped for restart", nil)
	assert.NoError(t, hm.LivenessCheck()) // still passes

	// Simulate collectors coming back: set healthy and clear suppression
	hm.ClearLivenessSuppression()
	hm.UpdateStatus(ComponentCollectorManager, HealthStatusHealthy, "restarted", nil)
	assert.NoError(t, hm.LivenessCheck()) // passes normally
}

// TestReadinessCheck_StandbyPassesWhenComponentsUnspecified verifies that a
// standby (non-leader) pod passes readiness even though its components are
// unspecified — it is healthy and ready to take over leadership.
func TestReadinessCheck_StandbyPassesWhenComponentsUnspecified(t *testing.T) {
	hm := NewHealthManager()
	hm.Register(ComponentCollectorManager)
	hm.Register(ComponentDakrTransport)
	// Components remain Unspecified (collectors never started on non-leader)

	hm.SetStandby(true)
	assert.NoError(t, hm.ReadinessCheck())
}

// TestReadinessCheck_StandbyClearedEnforcesNormalChecks verifies that after
// winning leader election (SetStandby(false)), normal readiness rules apply.
func TestReadinessCheck_StandbyClearedEnforcesNormalChecks(t *testing.T) {
	hm := NewHealthManager()
	hm.Register(ComponentCollectorManager)
	hm.Register(ComponentDakrTransport)

	hm.SetStandby(true)
	assert.NoError(t, hm.ReadinessCheck()) // standby: passes

	hm.SetStandby(false)
	assert.Error(t, hm.ReadinessCheck()) // components still Unspecified → fails

	hm.UpdateStatus(ComponentCollectorManager, HealthStatusHealthy, "ok", nil)
	hm.UpdateStatus(ComponentDakrTransport, HealthStatusHealthy, "ok", nil)
	assert.NoError(t, hm.ReadinessCheck()) // now passes
}

// TestTransitionObserver_FiresOnlyOnChange asserts the observer is called for
// the initial Unspecified→Healthy flip and the subsequent Healthy→Degraded
// flip, but NOT for an idempotent Healthy→Healthy update. This is the contract
// the telemetry pipeline relies on — without it, every periodic probe success
// would emit a duplicate log.
func TestTransitionObserver_FiresOnlyOnChange(t *testing.T) {
	f := newObserverFixture(t, ComponentPrometheus)

	f.hm.UpdateStatus(ComponentPrometheus, HealthStatusHealthy, "available", map[string]string{"url": "http://prom"})
	f.hm.UpdateStatus(ComponentPrometheus, HealthStatusHealthy, "still available", map[string]string{"url": "http://prom"})
	f.hm.UpdateStatus(ComponentPrometheus, HealthStatusDegraded, "probe failed", map[string]string{"url": "http://prom", "error": "connection refused"})

	calls := f.snapshot()
	require.Len(t, calls, 2, "observer should only fire on actual transitions, not idempotent updates")

	assert.Equal(t, ComponentPrometheus, calls[0].component)
	assert.Equal(t, HealthStatusUnspecified, calls[0].oldStatus)
	assert.Equal(t, HealthStatusHealthy, calls[0].newStatus)
	assert.Equal(t, "available", calls[0].message)
	assert.Equal(t, "http://prom", calls[0].metadata["url"])

	assert.Equal(t, HealthStatusHealthy, calls[1].oldStatus)
	assert.Equal(t, HealthStatusDegraded, calls[1].newStatus)
	assert.Equal(t, "probe failed", calls[1].message)
	assert.Equal(t, "connection refused", calls[1].metadata["error"])
}

// TestTransitionObserver_RunsOutsideLock verifies that the observer can call
// back into HealthManager (e.g. to read sibling component status) without
// deadlocking. UpdateStatus must release the write lock before invoking the
// observer — otherwise this test would hang on hm.GetStatus.
func TestTransitionObserver_RunsOutsideLock(t *testing.T) {
	hm := NewHealthManager()
	hm.Register(ComponentPrometheus)
	hm.Register(ComponentCollectorManager)
	hm.UpdateStatus(ComponentCollectorManager, HealthStatusHealthy, "ok", nil)

	observed := make(chan ComponentStatus, 1)
	hm.SetTransitionObserver(func(_ string, _, _ HealthStatus, _ string, _ map[string]string) {
		// If UpdateStatus held the write lock here, this read would deadlock.
		s, _ := hm.GetStatus(ComponentCollectorManager)
		observed <- s
	})

	done := make(chan struct{})
	go func() {
		hm.UpdateStatus(ComponentPrometheus, HealthStatusHealthy, "available", nil)
		close(done)
	}()

	select {
	case <-done:
		// Good — UpdateStatus returned without deadlocking.
	case <-time.After(2 * time.Second):
		t.Fatal("UpdateStatus deadlocked while invoking observer")
	}

	select {
	case s := <-observed:
		assert.Equal(t, HealthStatusHealthy, s.Status, "observer should have read sibling status")
	case <-time.After(time.Second):
		t.Fatal("observer was never invoked")
	}
}

// TestTransitionObserver_NotInvokedForUnregisteredComponent ensures we don't
// emit telemetry for components that were never registered (UpdateStatus is a
// no-op in that case, so there's no transition to report).
func TestTransitionObserver_NotInvokedForUnregisteredComponent(t *testing.T) {
	// Intentionally pass no components — ComponentPrometheus stays unregistered.
	f := newObserverFixture(t)

	f.hm.UpdateStatus(ComponentPrometheus, HealthStatusHealthy, "available", nil)

	assert.Empty(t, f.snapshot(), "observer should not fire for unregistered components")
}

// TestTransitionObserver_ReplaceAndClear verifies that SetTransitionObserver
// replaces an existing observer and that passing nil clears it.
func TestTransitionObserver_ReplaceAndClear(t *testing.T) {
	// Don't use the fixture here — we deliberately swap observers mid-test.
	hm := NewHealthManager()
	hm.Register(ComponentPrometheus)

	firstCalls := 0
	secondCalls := 0
	hm.SetTransitionObserver(func(string, HealthStatus, HealthStatus, string, map[string]string) {
		firstCalls++
	})
	hm.UpdateStatus(ComponentPrometheus, HealthStatusHealthy, "ok", nil)
	require.Equal(t, 1, firstCalls)

	hm.SetTransitionObserver(func(string, HealthStatus, HealthStatus, string, map[string]string) {
		secondCalls++
	})
	hm.UpdateStatus(ComponentPrometheus, HealthStatusDegraded, "uh", nil)
	assert.Equal(t, 1, firstCalls, "first observer should not fire after replacement")
	assert.Equal(t, 1, secondCalls)

	hm.SetTransitionObserver(nil)
	hm.UpdateStatus(ComponentPrometheus, HealthStatusUnhealthy, "down", nil)
	assert.Equal(t, 1, secondCalls, "second observer should not fire after being cleared")
}
