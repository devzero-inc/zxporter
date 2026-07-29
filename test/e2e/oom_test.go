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

package e2e

import (
	"context"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:golint,revive
	. "github.com/onsi/gomega"    //nolint:golint,revive

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	"github.com/devzero-inc/zxporter/internal/health"
)

// oomVictimNamespace is separate from the "controller" Describe block's
// `namespace` const (devzero-zxporter) — this suite doesn't deploy the
// zxporter manager at all, so it doesn't need to share that namespace or
// its Prometheus/cert-manager BeforeAll/AfterAll setup.
const oomVictimNamespace = "oom-flow-e2e"

// oomVictimImage is built and `kind load docker-image`'d by the CI action
// (see .github/actions/oom-flow-kind) from the repo's existing
// Dockerfile.stress (stress-ng), tagged locally so this suite doesn't need
// network access to pull an image mid-test.
const oomVictimImage = "zxporter-oom-victim:e2e-ci"

// recordingTelemetryLogger is a minimal telemetry_logger.Logger double so
// this suite can assert on exactly what RestartOOMDetector reports, without
// a real Dakr backend. Kept local to this file (rather than imported from
// internal/health's own test helper of the same shape) since it's a
// different package and the helper isn't exported.
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

// Labeled "oom" so CI can select just this spec
// (`--ginkgo.label-filter=oom`) without also running the pre-existing
// "controller" Describe block above, which needs Prometheus/cert-manager
// and a full `make deploy` — unrelated setup this suite doesn't need.
var _ = Describe("OOM detection (retroactive, RestartOOMDetector)", Ordered, Label("oom"), func() {
	var clientset kubernetes.Interface

	BeforeAll(func() {
		var err error
		clientset, err = kubernetes.NewForConfig(ctrl.GetConfigOrDie())
		Expect(err).NotTo(HaveOccurred())

		By("creating the oom-flow-e2e namespace")
		_, err = clientset.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: oomVictimNamespace},
		}, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}
	})

	AfterAll(func() {
		By("removing the oom-flow-e2e namespace")
		_ = clientset.CoreV1().Namespaces().Delete(context.Background(), oomVictimNamespace, metav1.DeleteOptions{})
	})

	// This is the negative-space companion to the "real OOM" spec below: a
	// pod with no restart history at all must never be reported as
	// previously OOM-killed. internal/health's own unit tests already cover
	// this against a fake clientset (TestRestartOOMDetector_NoPreviousTermination_NoOp);
	// this spec re-proves it against a real API server's actual JSON shape
	// for a container status that has never terminated, which a fake
	// clientset can't validate.
	It("does not report a pod that has never been OOM-killed", func() {
		ctx := context.Background()
		podName := "oom-negative-control"

		By("deploying a healthy pod with no memory pressure")
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: oomVictimNamespace},
			Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyAlways,
				Containers: []corev1.Container{{
					Name:    "stress",
					Image:   oomVictimImage,
					Command: []string{"stress-ng", "--vm", "1", "--vm-bytes", "8M", "--vm-keep", "--timeout", "600s"},
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{corev1.ResourceMemory: resourceQuantity("128Mi")},
					},
				}},
			},
		}
		_, err := clientset.CoreV1().Pods(oomVictimNamespace).Create(ctx, pod, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_ = clientset.CoreV1().Pods(oomVictimNamespace).Delete(context.Background(), podName, metav1.DeleteOptions{})
		})

		By("waiting for it to reach Running with zero restarts")
		Eventually(func() bool {
			p, err := clientset.CoreV1().Pods(oomVictimNamespace).Get(ctx, podName, metav1.GetOptions{})
			if err != nil || len(p.Status.ContainerStatuses) == 0 {
				return false
			}
			return p.Status.Phase == corev1.PodRunning && p.Status.ContainerStatuses[0].RestartCount == 0
		}, 2*time.Minute, 2*time.Second).Should(BeTrue())

		By("running the real RestartOOMDetector against it")
		tl := &recordingTelemetryLogger{}
		d := health.NewRestartOOMDetector(logr.Discard(), tl, clientset, oomVictimNamespace, podName, "stress")
		d.Check(ctx)

		Expect(tl.count()).To(Equal(0), "a pod with no termination history must never be reported as OOM-killed")
	})

	// A second negative case, distinct from the one above: this pod *does*
	// crash and restart — proving the detector isn't just silent because it
	// never saw any termination at all — but for an ordinary reason (a
	// plain non-zero exit), not an OOM kill. Confirms RestartOOMDetector
	// doesn't over-fire on every restart, only on ones the kernel actually
	// attributes to memory pressure. The existing fake-clientset unit tests
	// (TestRestartOOMDetector_NonOOMTermination_NoOp) already assert this
	// against synthetic termination states; this re-proves it against a
	// real container's real (kubelet-assigned) Reason/ExitCode for an
	// ordinary crash, which a fake clientset can't validate.
	It("does not report a pod that crashed for a non-OOM reason", func() {
		ctx := context.Background()
		podName := "oom-non-oom-crash"

		By("deploying a pod that exits non-zero without any memory pressure")
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: oomVictimNamespace},
			Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyAlways,
				Containers: []corev1.Container{{
					Name:    "stress",
					Image:   oomVictimImage,
					Command: []string{"/bin/sh", "-c", "exit 1"},
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{corev1.ResourceMemory: resourceQuantity("128Mi")},
					},
				}},
			},
		}
		_, err := clientset.CoreV1().Pods(oomVictimNamespace).Create(ctx, pod, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_ = clientset.CoreV1().Pods(oomVictimNamespace).Delete(context.Background(), podName, metav1.DeleteOptions{})
		})

		By("waiting for a real, non-OOM termination to be recorded")
		var crashed *corev1.Pod
		Eventually(func() bool {
			p, err := clientset.CoreV1().Pods(oomVictimNamespace).Get(ctx, podName, metav1.GetOptions{})
			if err != nil || len(p.Status.ContainerStatuses) == 0 {
				return false
			}
			term := p.Status.ContainerStatuses[0].LastTerminationState.Terminated
			if term == nil {
				return false
			}
			crashed = p
			return true
		}, 2*time.Minute, 2*time.Second).Should(BeTrue(),
			"expected the container to exit and kubelet to record a termination state")
		term := crashed.Status.ContainerStatuses[0].LastTerminationState.Terminated
		Expect(term.Reason).NotTo(Equal("OOMKilled"),
			"test setup bug: this pod should crash for an ordinary reason, not OOM — got reason=%s", term.Reason)
		Expect(term.ExitCode).NotTo(Equal(int32(137)),
			"test setup bug: this pod should not exit 137 — a real OOM would make this assertion meaningless")

		By("running the real RestartOOMDetector against it")
		tl := &recordingTelemetryLogger{}
		d := health.NewRestartOOMDetector(logr.Discard(), tl, clientset, oomVictimNamespace, podName, "stress")
		d.Check(ctx)

		Expect(tl.count()).To(Equal(0), "an ordinary (non-OOM) crash must never be reported as OOM-killed")
	})

	// The core spec: deliberately OOM-kill a real pod (stress-ng, via
	// Dockerfile.stress, given a memory limit far below what it's told to
	// allocate) and confirm the kernel/kubelet record the kill the exact
	// way RestartOOMDetector reads it back (status.containerStatuses[].
	// lastState.terminated), then run the real detector against that real
	// object and assert on what it reports.
	//
	// A single pod can't both get OOM-killed for real *and* later recover
	// within the same test (its memory limit is immutable for the pod's
	// lifetime — kubelet just keeps CrashLoopBackOff-restarting the same
	// under-provisioned container forever; changing the limit requires a
	// Deployment rollout, which creates a brand-new Pod object with no
	// restart history). So rather than requiring the victim to become
	// healthy again, this drives the detector directly against the crashed
	// pod's live-cluster-recorded state — the same real data a surviving
	// sibling/next replica would read from the Kubernetes API, without
	// needing this specific pod to be the one still running.
	It("reports a pod that was actually OOM-killed by the kernel", func() {
		ctx := context.Background()
		podName := "oom-victim"

		By("deploying a pod that allocates far more memory than its limit allows")
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: oomVictimNamespace},
			Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyAlways,
				Containers: []corev1.Container{{
					Name:  "stress",
					Image: oomVictimImage,
					// Touches (not just reserves) 64Mi against an 8Mi limit —
					// --vm-keep forces it to actually commit the pages rather
					// than allocate-and-free in a loop, so the kernel OOM
					// killer fires promptly and deterministically.
					Command: []string{"stress-ng", "--vm", "1", "--vm-bytes", "64M", "--vm-keep", "--timeout", "60s"},
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{corev1.ResourceMemory: resourceQuantity("8Mi")},
					},
				}},
			},
		}
		_, err := clientset.CoreV1().Pods(oomVictimNamespace).Create(ctx, pod, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_ = clientset.CoreV1().Pods(oomVictimNamespace).Delete(context.Background(), podName, metav1.DeleteOptions{})
		})

		By("waiting for the kubelet to record a real OOMKilled termination")
		var crashed *corev1.Pod
		Eventually(func() bool {
			p, err := clientset.CoreV1().Pods(oomVictimNamespace).Get(ctx, podName, metav1.GetOptions{})
			if err != nil || len(p.Status.ContainerStatuses) == 0 {
				return false
			}
			term := p.Status.ContainerStatuses[0].LastTerminationState.Terminated
			if term == nil || term.Reason != "OOMKilled" {
				return false
			}
			crashed = p
			return true
		}, 3*time.Minute, 2*time.Second).Should(BeTrue(),
			"expected the kubelet to OOM-kill and record lastState.terminated.reason=OOMKilled")
		Expect(crashed.Status.ContainerStatuses[0].LastTerminationState.Terminated.ExitCode).To(Equal(int32(137)))

		By("running the real RestartOOMDetector against the real crashed pod")
		tl := &recordingTelemetryLogger{}
		d := health.NewRestartOOMDetector(logr.Discard(), tl, clientset, oomVictimNamespace, podName, "stress")
		d.Check(ctx)

		Expect(tl.count()).To(Equal(1), "a genuinely OOM-killed pod must be reported exactly once")
		report := tl.last()
		Expect(report.source).To(Equal("RestartOOMDetector"))
		Expect(report.message).To(Equal("Previous instance was OOM-killed"))
		Expect(report.fields["container_name"]).To(Equal("stress"))
		Expect(report.fields["reason"]).To(Equal("OOMKilled"))
		Expect(report.fields["exit_code"]).To(Equal("137"))
		Expect(report.fields["restart_count"]).NotTo(BeEmpty())
	})
})

func resourceQuantity(s string) resource.Quantity {
	return resource.MustParse(s)
}
