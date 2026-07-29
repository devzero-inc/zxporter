package health

import (
	"context"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	telemetry_logger "github.com/devzero-inc/zxporter/internal/logger"
	"github.com/devzero-inc/zxporter/internal/version"
)

const (
	// oomKilledReason is the value Kubernetes sets on
	// ContainerStateTerminated.Reason when the kernel OOM killer took down the
	// container. exitCode 137 (128 + SIGKILL's 9) is checked too since some
	// runtimes/versions have been observed to omit Reason while still recording
	// the SIGKILL exit code.
	oomKilledReason = "OOMKilled"

	// oomKilledExitCode is the exit code a SIGKILL'd process reports (128 + 9).
	// The kubelet uses this as a corroborating signal alongside Reason.
	oomKilledExitCode = int32(137)

	// restartOOMDetectorTimeout bounds the single Pod Get call so a slow or
	// unreachable API server can't leave this startup goroutine hanging
	// indefinitely; it is not on the critical startup path (see Check), but an
	// unbounded goroutine is still worth avoiding.
	restartOOMDetectorTimeout = 10 * time.Second
)

// RestartOOMDetector implements the retroactive half of OOM visibility: when
// zxporter itself gets OOM-killed, the process dies before it can report
// anything about its own death (MemoryPressureMonitor's proactive warning is
// necessarily best-effort — a sudden spike can still race past it). But
// Kubernetes independently persists
// pod.status.containerStatuses[].lastState.terminated on the Pod object
// regardless of whether the dying process got to do anything, so the *next*
// instance can read its own previous death from the API server on startup and
// report it. The reporting happens from the survivor, not the corpse.
type RestartOOMDetector struct {
	logger logr.Logger
	// telemetryLogger is nil-safe: zxporter-nodemon has no telemetry sink wired
	// up today, so it runs this detector with a nil telemetryLogger and relies
	// on the local logr output instead (same convention as
	// MemoryPressureMonitor).
	telemetryLogger telemetry_logger.Logger
	// clientset is nil-safe for the same reason a missing namespace/pod name
	// is: this check degrades to a silent no-op rather than blocking startup.
	clientset     kubernetes.Interface
	namespace     string
	podName       string
	containerName string
}

// NewRestartOOMDetector builds a detector for the container named
// containerName inside the pod identified by namespace/podName. namespace and
// podName are typically sourced from the POD_NAMESPACE/POD_NAME downward-API
// env vars and may be empty (e.g. running outside a cluster); clientset and
// telemetryLogger may both be nil.
func NewRestartOOMDetector(
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
	clientset kubernetes.Interface,
	namespace string,
	podName string,
	containerName string,
) *RestartOOMDetector {
	return &RestartOOMDetector{
		logger:          logger.WithName("restart-oom-detector"),
		telemetryLogger: telemetryLogger,
		clientset:       clientset,
		namespace:       namespace,
		podName:         podName,
		containerName:   containerName,
	}
}

// Check runs the retroactive OOM check once. It is meant to be invoked a
// single time early in startup (see EnvBasedController.Start and
// cmd/zxporter-nodemon/main.go), not on a ticker like MemoryPressureMonitor —
// a previous termination only needs to be reported once, the first time the
// new instance notices it.
//
// Every failure path here — missing env vars, no clientset, a failed API
// call, no previous termination, a termination that wasn't an OOM kill — is a
// silent, low-verbosity no-op. This check must never prevent or delay normal
// startup, so callers should invoke it from a goroutine rather than inline on
// the startup path.
func (d *RestartOOMDetector) Check(ctx context.Context) {
	if d.namespace == "" || d.podName == "" {
		d.logger.V(1).Info("POD_NAMESPACE/POD_NAME not set, skipping retroactive OOM check")
		return
	}
	if d.clientset == nil {
		d.logger.V(1).Info("no kubernetes client configured, skipping retroactive OOM check")
		return
	}

	checkCtx, cancel := context.WithTimeout(ctx, restartOOMDetectorTimeout)
	defer cancel()

	pod, err := d.clientset.CoreV1().Pods(d.namespace).Get(checkCtx, d.podName, metav1.GetOptions{})
	if err != nil {
		d.logger.V(1).Info("failed to fetch own pod, skipping retroactive OOM check",
			"namespace", d.namespace, "pod", d.podName, "error", err)
		return
	}

	status := findContainerStatus(pod.Status.ContainerStatuses, d.containerName)
	if status == nil {
		d.logger.V(1).Info("container status not found on own pod, skipping retroactive OOM check",
			"container", d.containerName)
		return
	}

	terminated := status.LastTerminationState.Terminated
	if terminated == nil {
		d.logger.V(1).Info("no previous termination state recorded, nothing to report",
			"container", d.containerName)
		return
	}

	// Exit code 137 (128+SIGKILL) alone isn't OOM-specific — a container killed
	// after exceeding its terminationGracePeriod during a normal rollout/eviction,
	// or by a failing liveness probe, also exits 137, but carries an explicit
	// non-OOM Reason (e.g. "Error"). Only fall back to the exit-code check when
	// Reason is empty/unset, which is when some runtimes have been observed to
	// omit Reason while still recording the SIGKILL exit code.
	isOOM := terminated.Reason == oomKilledReason ||
		(terminated.Reason == "" && terminated.ExitCode == oomKilledExitCode)
	if !isOOM {
		d.logger.V(1).Info("previous termination was not an OOM kill, nothing to report",
			"container", d.containerName, "reason", terminated.Reason, "exitCode", terminated.ExitCode)
		return
	}

	message := "Previous instance was OOM-killed"
	fields := map[string]string{
		"container_name":   d.containerName,
		"reason":           terminated.Reason,
		"exit_code":        strconv.FormatInt(int64(terminated.ExitCode), 10),
		"finished_at":      terminated.FinishedAt.Format(time.RFC3339),
		"restart_count":    strconv.FormatInt(int64(status.RestartCount), 10),
		"zxporter_version": version.Get().String(),
	}

	d.logger.Info(message,
		"container", d.containerName,
		"reason", terminated.Reason,
		"exitCode", terminated.ExitCode,
		"finishedAt", terminated.FinishedAt.Time,
		"restartCount", status.RestartCount,
	)

	if d.telemetryLogger != nil {
		d.telemetryLogger.Report(gen.LogLevel_LOG_LEVEL_ERROR, "RestartOOMDetector", message, nil, fields)
	}
}

// findContainerStatus returns the status entry matching name, or nil if the
// container isn't present on the pod (e.g. a misconfigured container name).
func findContainerStatus(statuses []corev1.ContainerStatus, name string) *corev1.ContainerStatus {
	for i := range statuses {
		if statuses[i].Name == name {
			return &statuses[i]
		}
	}
	return nil
}
