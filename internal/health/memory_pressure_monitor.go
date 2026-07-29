package health

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/go-logr/logr"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	telemetry_logger "github.com/devzero-inc/zxporter/internal/logger"
	"github.com/devzero-inc/zxporter/internal/version"
)

const (
	// MemoryPressureCheckInterval matches NodeCollector's default resource-metrics
	// cadence (see NodeCollectorConfig.UpdateInterval) so cgroup polling runs at a
	// familiar frequency rather than an arbitrary one.
	MemoryPressureCheckInterval = 30 * time.Second

	// MemoryPressureThresholdPercent is the usage/limit ratio that triggers a
	// warning. Named so it's a one-line tune rather than a buried literal.
	MemoryPressureThresholdPercent = 85.0

	// MemoryPressureReaffirmInterval bounds how often a still-elevated reading is
	// re-reported, so sustained pressure doesn't spam Datadog on every tick.
	MemoryPressureReaffirmInterval = 10 * time.Minute
)

// MemoryPressureMonitor periodically compares the process's cgroup memory usage
// against its configured limit and reports the crossing before the kubelet's
// OOM killer acts. automemlimit (imported for its init-time side effect in both
// zxporter entrypoints) only sets GOMEMLIMIT once at startup; this is the
// periodic re-check that was otherwise missing, giving an early warning ahead
// of the OOMKilled event instead of only learning about it in hindsight.
type MemoryPressureMonitor struct {
	logger logr.Logger
	// telemetryLogger is nil-safe: zxporter-nodemon has no telemetry sink wired
	// up today, so it runs this monitor with a nil telemetryLogger and relies on
	// the local logr output instead.
	telemetryLogger telemetry_logger.Logger
	// healthManager is nil-safe and optional; when present, status is surfaced
	// under ComponentMemoryPressure for /healthz and the heartbeat report, but
	// report cadence (edge + reaffirm) is decided independently below because
	// HealthManager.UpdateStatus only notifies on an actual status change.
	healthManager *HealthManager
	cgroupRoot    string
	interval      time.Duration

	mu           sync.Mutex
	inPressure   bool
	lastReported time.Time
}

// NewMemoryPressureMonitor builds a monitor reading real cgroup files under
// /sys/fs/cgroup. telemetryLogger and healthManager may both be nil.
func NewMemoryPressureMonitor(
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
	healthManager *HealthManager,
) *MemoryPressureMonitor {
	if healthManager != nil {
		healthManager.Register(ComponentMemoryPressure)
	}
	return &MemoryPressureMonitor{
		logger:          logger.WithName("memory-pressure-monitor"),
		telemetryLogger: telemetryLogger,
		healthManager:   healthManager,
		cgroupRoot:      defaultCgroupRoot,
		interval:        MemoryPressureCheckInterval,
	}
}

// Start runs the check loop and blocks until ctx is cancelled, matching the
// controller-runtime Runnable shape used elsewhere for always-on, top-level
// processes (see GPURuntimeResolver.Start, EnvBasedController.Start) rather
// than returning immediately and requiring a separate Stop(). The
// controller-manager binary launches it as a goroutine from
// EnvBasedController.Start (internal/controller/custom.go), once the
// telemetry logger it needs has been initialized; zxporter-nodemon has no
// controller-runtime manager at all, so its entrypoint (cmd/zxporter-nodemon)
// launches it as a goroutine directly with a nil telemetry logger.
//
// This intentionally does not use the collector-manager Start/Stop-with-ticker
// shape (NodeCollector, ContainerResourceCollector): that machinery exists to
// let CollectionPolicy dynamically add/replace/remove per-resource collectors,
// which doesn't apply here — this monitor is a single always-on process-level
// check wired once at startup in both binaries.
func (m *MemoryPressureMonitor) Start(ctx context.Context) error {
	m.logger.Info("Starting memory pressure monitor",
		"interval", m.interval,
		"thresholdPercent", MemoryPressureThresholdPercent,
	)

	m.check()

	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			m.check()
		case <-ctx.Done():
			m.logger.Info("Stopping memory pressure monitor")
			return nil
		}
	}
}

// check reads current cgroup memory stats and, on a threshold crossing or a
// coarse reaffirmation interval while still elevated, reports it.
func (m *MemoryPressureMonitor) check() {
	usage, ok, err := readCgroupMemory(m.cgroupRoot)
	if err != nil {
		m.logger.V(1).Info("Failed to read cgroup memory stats", "error", err)
		return
	}
	if !ok || usage.LimitBytes == 0 {
		m.logger.V(1).Info("No cgroup memory limit configured, skipping pressure check")
		return
	}

	percent := float64(usage.UsageBytes) / float64(usage.LimitBytes) * 100
	above := percent >= MemoryPressureThresholdPercent

	shouldReport, backToNormal := m.recordAndDecide(above)
	if !shouldReport {
		return
	}

	level := gen.LogLevel_LOG_LEVEL_WARN
	message := "Approaching container memory limit"
	status := HealthStatusDegraded
	if backToNormal {
		level = gen.LogLevel_LOG_LEVEL_INFO
		message = "Memory usage back to normal"
		status = HealthStatusHealthy
	}

	fields := map[string]string{
		"usage_bytes":      strconv.FormatUint(usage.UsageBytes, 10),
		"limit_bytes":      strconv.FormatUint(usage.LimitBytes, 10),
		"usage_percent":    strconv.FormatFloat(percent, 'f', 2, 64),
		"zxporter_version": version.Get().String(),
	}

	m.logger.Info(message,
		"usageBytes", usage.UsageBytes,
		"limitBytes", usage.LimitBytes,
		"usagePercent", percent,
	)

	if m.telemetryLogger != nil {
		m.telemetryLogger.Report(level, "MemoryPressureMonitor", message, nil, fields)
	}
	if m.healthManager != nil {
		m.healthManager.UpdateStatus(ComponentMemoryPressure, status, message, fields)
	}
}

// recordAndDecide updates the edge-tracking state under lock and reports
// whether the current reading warrants a report: an edge-trigger on
// healthy<->pressure transitions, plus a coarse reaffirmation while sustained
// above threshold so Datadog doesn't get one entry per 30s tick during a
// prolonged incident.
func (m *MemoryPressureMonitor) recordAndDecide(above bool) (shouldReport, backToNormal bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	wasInPressure := m.inPressure
	transitionedToPressure := above && !wasInPressure
	transitionedToNormal := !above && wasInPressure
	reaffirm := above && wasInPressure && time.Since(m.lastReported) >= MemoryPressureReaffirmInterval

	m.inPressure = above

	if transitionedToPressure || reaffirm {
		m.lastReported = time.Now()
		return true, false
	}
	if transitionedToNormal {
		m.lastReported = time.Now()
		return true, true
	}
	return false, false
}
