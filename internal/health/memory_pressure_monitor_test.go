package health

import (
	"context"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
)

// recordingTelemetryLogger captures Report calls for assertions instead of
// sending anything over the wire.
type recordingTelemetryLogger struct {
	mu      sync.Mutex
	reports []recordedReport
}

type recordedReport struct {
	level   gen.LogLevel
	source  string
	message string
	fields  map[string]string
}

func (r *recordingTelemetryLogger) Report(
	level gen.LogLevel,
	source string,
	msg string,
	_ error,
	fields map[string]string,
) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reports = append(r.reports, recordedReport{level: level, source: source, message: msg, fields: fields})
}

func (r *recordingTelemetryLogger) Stop() {}

func (r *recordingTelemetryLogger) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reports)
}

func (r *recordingTelemetryLogger) last() recordedReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reports[len(r.reports)-1]
}

func writeV2CgroupFixture(t *testing.T, root string, usageBytes, limitBytes uint64) {
	t.Helper()
	writeFile(t, filepath.Join(root, cgroupV2ControllersFile), "memory\n")
	writeFile(t, filepath.Join(root, cgroupV2CurrentFile), strconv.FormatUint(usageBytes, 10))
	writeFile(t, filepath.Join(root, cgroupV2MaxFile), strconv.FormatUint(limitBytes, 10))
}

func newTestMonitor(t *testing.T, root string) (*MemoryPressureMonitor, *recordingTelemetryLogger, *HealthManager) {
	t.Helper()
	tl := &recordingTelemetryLogger{}
	hm := NewHealthManager()
	m := NewMemoryPressureMonitor(logr.Discard(), tl, hm)
	m.cgroupRoot = root
	return m, tl, hm
}

func TestMemoryPressureMonitor_NoLimitConfigured_SkipsSilently(t *testing.T) {
	root := t.TempDir()
	writeV2CgroupFixture(t, root, 100, 200)
	// Overwrite max with the literal "max" sentinel for "no limit".
	writeFile(t, filepath.Join(root, cgroupV2MaxFile), "max\n")

	m, tl, _ := newTestMonitor(t, root)
	m.check()

	assert.Equal(t, 0, tl.count())
}

func TestMemoryPressureMonitor_BelowThreshold_NoReport(t *testing.T) {
	root := t.TempDir()
	writeV2CgroupFixture(t, root, 50, 100) // 50%

	m, tl, hm := newTestMonitor(t, root)
	m.check()

	assert.Equal(t, 0, tl.count())
	_, exists := hm.GetStatus(ComponentMemoryPressure)
	assert.True(t, exists) // registered, but status untouched (still Unspecified)
}

func TestMemoryPressureMonitor_CrossingThreshold_ReportsOnce(t *testing.T) {
	root := t.TempDir()
	writeV2CgroupFixture(t, root, 90, 100) // 90% >= 85%

	m, tl, hm := newTestMonitor(t, root)
	m.check()

	require.Equal(t, 1, tl.count())
	rep := tl.last()
	assert.Equal(t, gen.LogLevel_LOG_LEVEL_WARN, rep.level)
	assert.Equal(t, "MemoryPressureMonitor", rep.source)
	assert.Equal(t, "90", rep.fields["usage_bytes"])
	assert.Equal(t, "100", rep.fields["limit_bytes"])
	assert.Equal(t, "90.00", rep.fields["usage_percent"])
	assert.NotEmpty(t, rep.fields["zxporter_version"])

	status, _ := hm.GetStatus(ComponentMemoryPressure)
	assert.Equal(t, HealthStatusDegraded, status.Status)

	// A second consecutive check still above threshold, but within the
	// reaffirm window, must not report again — this is the spam guard.
	m.check()
	assert.Equal(t, 1, tl.count())
}

func TestMemoryPressureMonitor_ReaffirmsAfterInterval(t *testing.T) {
	root := t.TempDir()
	writeV2CgroupFixture(t, root, 90, 100)

	m, tl, _ := newTestMonitor(t, root)
	m.check()
	require.Equal(t, 1, tl.count())

	// Simulate the reaffirm window having elapsed.
	m.mu.Lock()
	m.lastReported = time.Now().Add(-MemoryPressureReaffirmInterval - time.Second)
	m.mu.Unlock()

	m.check()
	assert.Equal(t, 2, tl.count())
	assert.Equal(t, gen.LogLevel_LOG_LEVEL_WARN, tl.last().level)
}

func TestMemoryPressureMonitor_RecoversBelowThreshold_ReportsBackToNormal(t *testing.T) {
	root := t.TempDir()
	writeV2CgroupFixture(t, root, 90, 100)

	m, tl, hm := newTestMonitor(t, root)
	m.check()
	require.Equal(t, 1, tl.count())

	// Usage drops back down.
	writeFile(t, filepath.Join(root, cgroupV2CurrentFile), "10\n")
	m.check()

	require.Equal(t, 2, tl.count())
	rep := tl.last()
	assert.Equal(t, gen.LogLevel_LOG_LEVEL_INFO, rep.level)
	assert.Equal(t, "Memory usage back to normal", rep.message)

	status, _ := hm.GetStatus(ComponentMemoryPressure)
	assert.Equal(t, HealthStatusHealthy, status.Status)

	// Staying below threshold on subsequent checks should not re-report.
	m.check()
	assert.Equal(t, 2, tl.count())
}

func TestMemoryPressureMonitor_NilTelemetryLoggerAndHealthManager_DoesNotPanic(t *testing.T) {
	root := t.TempDir()
	writeV2CgroupFixture(t, root, 90, 100)

	m := NewMemoryPressureMonitor(logr.Discard(), nil, nil)
	m.cgroupRoot = root

	assert.NotPanics(t, func() {
		m.check()
	})
}

func TestMemoryPressureMonitor_UnreadableCgroup_DoesNotReport(t *testing.T) {
	root := filepath.Join(t.TempDir(), "does-not-exist")

	m, tl, _ := newTestMonitor(t, root)
	m.check()

	assert.Equal(t, 0, tl.count())
}

func TestMemoryPressureMonitor_Start_StopsOnContextCancel(t *testing.T) {
	root := t.TempDir()
	writeV2CgroupFixture(t, root, 10, 100)

	m, _, _ := newTestMonitor(t, root)
	m.interval = 5 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := m.Start(ctx)
	assert.NoError(t, err)
}

// --- Integration-style tests below drive the real Start()/ticker loop end to
// end, rather than calling check() directly, so the spam-guard and the
// no-limit/below-threshold skip paths are exercised the way production
// actually runs them (many ticks over the process lifetime), not just via a
// single hand-invoked check().

// TestMemoryPressureMonitor_Integration_SustainedHealthy_NeverReports runs the
// real ticker loop for many ticks with usage safely below threshold the
// entire time, and asserts zero telemetry reports ever fire. This is the
// negative-space analog of TestMemoryPressureMonitor_CrossingThreshold_ReportsOnce:
// a monitor that's silent when nothing is wrong is just as important a
// guarantee as one that speaks up when something is.
func TestMemoryPressureMonitor_Integration_SustainedHealthy_NeverReports(t *testing.T) {
	root := t.TempDir()
	writeV2CgroupFixture(t, root, 10, 100) // 10%, well below the 85% threshold

	m, tl, hm := newTestMonitor(t, root)
	m.interval = 5 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond) // ~16 ticks
	defer cancel()

	require.NoError(t, m.Start(ctx))

	assert.Equal(t, 0, tl.count(), "a monitor that never crosses threshold must never report, across the full ticker lifetime")
	status, exists := hm.GetStatus(ComponentMemoryPressure)
	assert.True(t, exists)
	assert.Equal(t, HealthStatusUnspecified, status.Status, "health manager should not be touched at all while never crossing threshold")
}

// TestMemoryPressureMonitor_Integration_NoLimitConfigured_NeverReports runs
// the real ticker loop for many ticks against a cgroup fixture with no
// memory limit configured, and asserts the monitor stays silent for the
// monitor's entire real lifetime — not just on a single check().
func TestMemoryPressureMonitor_Integration_NoLimitConfigured_NeverReports(t *testing.T) {
	root := t.TempDir()
	writeV2CgroupFixture(t, root, 100, 200)
	writeFile(t, filepath.Join(root, cgroupV2MaxFile), "max\n") // no limit

	m, tl, _ := newTestMonitor(t, root)
	m.interval = 5 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	require.NoError(t, m.Start(ctx))

	assert.Equal(t, 0, tl.count(), "no configured limit means nothing to report, for the monitor's entire lifetime")
}

// TestMemoryPressureMonitor_Integration_SustainedPressure_SpamGuardHoldsAcrossManyRealTicks
// drives the real ticker through many cycles while usage stays above
// threshold the whole time. It proves the spam guard holds up when driven by
// the actual Start() loop, not just by two manually sequenced check() calls
// (as TestMemoryPressureMonitor_CrossingThreshold_ReportsOnce already covers) —
// the negative assertion here is "no report on ticks 2 through N", which is
// exactly the behavior a regression could silently break by, e.g., losing
// the mutex-guarded state across ticker iterations.
func TestMemoryPressureMonitor_Integration_SustainedPressure_SpamGuardHoldsAcrossManyRealTicks(t *testing.T) {
	root := t.TempDir()
	writeV2CgroupFixture(t, root, 90, 100) // 90%, above the 85% threshold

	m, tl, _ := newTestMonitor(t, root)
	m.interval = 5 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond) // ~16 ticks
	defer cancel()

	require.NoError(t, m.Start(ctx))

	// Real 10-minute reaffirm window can't elapse inside a fast unit test, so
	// the only correct outcome across this many real ticks while sustained
	// above threshold is exactly the single edge-triggered report.
	assert.Equal(t, 1, tl.count(), "sustained pressure across many real ticks, well inside the reaffirm window, must report exactly once")
}
