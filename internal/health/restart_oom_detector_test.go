package health

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const (
	testNamespace     = "devzero-system"
	testPodName       = "devzero-zxporter-controller-manager-abc123"
	testContainerName = "manager"
)

// newTestPod builds a Pod fixture with a single container status, optionally
// carrying a previous termination state.
func newTestPod(containerName string, terminated *corev1.ContainerStateTerminated, restartCount int32) *corev1.Pod {
	status := corev1.ContainerStatus{
		Name:         containerName,
		RestartCount: restartCount,
	}
	if terminated != nil {
		status.LastTerminationState = corev1.ContainerState{Terminated: terminated}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testPodName,
			Namespace: testNamespace,
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{status},
		},
	}
}

func TestRestartOOMDetector_ReportsOOMKilledByReason(t *testing.T) {
	finishedAt := metav1.NewTime(time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC))
	pod := newTestPod(testContainerName, &corev1.ContainerStateTerminated{
		Reason:     "OOMKilled",
		ExitCode:   137,
		FinishedAt: finishedAt,
	}, 3)
	clientset := fake.NewSimpleClientset(pod)
	tl := &recordingTelemetryLogger{}

	d := NewRestartOOMDetector(logr.Discard(), tl, clientset, testNamespace, testPodName, testContainerName)
	d.Check(context.Background())

	require.Equal(t, 1, tl.count())
	report := tl.last()
	assert.Equal(t, "RestartOOMDetector", report.source)
	assert.Equal(t, "Previous instance was OOM-killed", report.message)
	assert.Equal(t, testContainerName, report.fields["container_name"])
	assert.Equal(t, "OOMKilled", report.fields["reason"])
	assert.Equal(t, "137", report.fields["exit_code"])
	assert.Equal(t, "3", report.fields["restart_count"])
	assert.Equal(t, finishedAt.Format(time.RFC3339), report.fields["finished_at"])
	assert.NotEmpty(t, report.fields["zxporter_version"])
}

func TestRestartOOMDetector_ReportsOOMKilledByExitCodeOnly(t *testing.T) {
	// Some runtimes have been observed to omit Reason entirely while still
	// recording the SIGKILL exit code — exit code 137 with no reason should
	// still trigger.
	pod := newTestPod(testContainerName, &corev1.ContainerStateTerminated{
		Reason:     "",
		ExitCode:   137,
		FinishedAt: metav1.Now(),
	}, 1)
	clientset := fake.NewSimpleClientset(pod)
	tl := &recordingTelemetryLogger{}

	d := NewRestartOOMDetector(logr.Discard(), tl, clientset, testNamespace, testPodName, testContainerName)
	d.Check(context.Background())

	require.Equal(t, 1, tl.count())
}

func TestRestartOOMDetector_NonOOMExitCode137WithExplicitReason_NoOp(t *testing.T) {
	// Exit code 137 (128+SIGKILL) isn't OOM-specific: a container killed after
	// exceeding its terminationGracePeriod during a rollout/eviction, or by a
	// failing liveness probe, also exits 137 — but carries an explicit
	// non-OOM Reason. Only the empty-Reason fallback (the previous test)
	// should treat exit 137 alone as OOM; an explicit non-OOM Reason must not
	// be overridden by the exit code, or normal rollouts/evictions/liveness
	// kills would be misreported as OOM.
	pod := newTestPod(testContainerName, &corev1.ContainerStateTerminated{
		Reason:     "Error",
		ExitCode:   137,
		FinishedAt: metav1.Now(),
	}, 1)
	clientset := fake.NewSimpleClientset(pod)
	tl := &recordingTelemetryLogger{}

	d := NewRestartOOMDetector(logr.Discard(), tl, clientset, testNamespace, testPodName, testContainerName)
	d.Check(context.Background())

	assert.Equal(t, 0, tl.count())
}

func TestRestartOOMDetector_NoPreviousTermination_NoOp(t *testing.T) {
	pod := newTestPod(testContainerName, nil, 0)
	clientset := fake.NewSimpleClientset(pod)
	tl := &recordingTelemetryLogger{}

	d := NewRestartOOMDetector(logr.Discard(), tl, clientset, testNamespace, testPodName, testContainerName)
	d.Check(context.Background())

	assert.Equal(t, 0, tl.count())
}

func TestRestartOOMDetector_NonOOMTermination_NoOp(t *testing.T) {
	pod := newTestPod(testContainerName, &corev1.ContainerStateTerminated{
		Reason:     "Completed",
		ExitCode:   0,
		FinishedAt: metav1.Now(),
	}, 1)
	clientset := fake.NewSimpleClientset(pod)
	tl := &recordingTelemetryLogger{}

	d := NewRestartOOMDetector(logr.Discard(), tl, clientset, testNamespace, testPodName, testContainerName)
	d.Check(context.Background())

	assert.Equal(t, 0, tl.count())
}

func TestRestartOOMDetector_ContainerNotFound_NoOp(t *testing.T) {
	pod := newTestPod("some-other-container", &corev1.ContainerStateTerminated{
		Reason:   "OOMKilled",
		ExitCode: 137,
	}, 1)
	clientset := fake.NewSimpleClientset(pod)
	tl := &recordingTelemetryLogger{}

	d := NewRestartOOMDetector(logr.Discard(), tl, clientset, testNamespace, testPodName, testContainerName)
	d.Check(context.Background())

	assert.Equal(t, 0, tl.count())
}

func TestRestartOOMDetector_PodNotFound_NoOp(t *testing.T) {
	clientset := fake.NewSimpleClientset() // no pod registered
	tl := &recordingTelemetryLogger{}

	d := NewRestartOOMDetector(logr.Discard(), tl, clientset, testNamespace, testPodName, testContainerName)
	d.Check(context.Background())

	assert.Equal(t, 0, tl.count())
}

func TestRestartOOMDetector_MissingNamespaceOrPodName_NoOp(t *testing.T) {
	pod := newTestPod(testContainerName, &corev1.ContainerStateTerminated{
		Reason:   "OOMKilled",
		ExitCode: 137,
	}, 1)
	clientset := fake.NewSimpleClientset(pod)

	t.Run("missing namespace", func(t *testing.T) {
		tl := &recordingTelemetryLogger{}
		d := NewRestartOOMDetector(logr.Discard(), tl, clientset, "", testPodName, testContainerName)
		d.Check(context.Background())
		assert.Equal(t, 0, tl.count())
	})

	t.Run("missing pod name", func(t *testing.T) {
		tl := &recordingTelemetryLogger{}
		d := NewRestartOOMDetector(logr.Discard(), tl, clientset, testNamespace, "", testContainerName)
		d.Check(context.Background())
		assert.Equal(t, 0, tl.count())
	})
}

func TestRestartOOMDetector_NilClientset_NoOp(t *testing.T) {
	tl := &recordingTelemetryLogger{}
	d := NewRestartOOMDetector(logr.Discard(), tl, nil, testNamespace, testPodName, testContainerName)
	d.Check(context.Background())
	assert.Equal(t, 0, tl.count())
}

func TestRestartOOMDetector_NilTelemetryLogger_DoesNotPanic(t *testing.T) {
	pod := newTestPod(testContainerName, &corev1.ContainerStateTerminated{
		Reason:   "OOMKilled",
		ExitCode: 137,
	}, 1)
	clientset := fake.NewSimpleClientset(pod)

	d := NewRestartOOMDetector(logr.Discard(), nil, clientset, testNamespace, testPodName, testContainerName)
	assert.NotPanics(t, func() {
		d.Check(context.Background())
	})
}
