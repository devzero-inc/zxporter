package health

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
)

// specsForServer points both probe endpoints at a local test server, standing in
// for a Pod IP and its health container port.
func specsForServer(t *testing.T, server *httptest.Server) (healthz, readyz probeEndpointSpec) {
	t.Helper()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	require.NoError(t, err)
	return probeEndpointSpec{scheme: "http", port: port, path: defaultHealthzPath},
		probeEndpointSpec{scheme: "http", port: port, path: defaultReadyzPath}
}

// loopbackPod is a probeable Pod whose IP resolves to the local test server.
func loopbackPod(name string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-system"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "127.0.0.1"},
	}
}

func TestNodeOperatorMonitor_ProbeHealth(t *testing.T) {
	t.Run("healthy endpoint returns OK", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		mon := NewNodeOperatorMonitor(logr.Discard(), nil, &http.Client{Timeout: 2 * time.Second})
		healthz, readyz := specsForServer(t, server)

		result := mon.probePodHealth(context.Background(), []corev1.Pod{loopbackPod("a")}, healthz, readyz)
		assert.Equal(t, probeOutcomeOK, result.healthz)
		assert.Equal(t, probeOutcomeOK, result.readyz)
		assert.Equal(t, 1, result.attempted)
	})

	t.Run("endpoint answering not-ok returns NotOK", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer server.Close()

		mon := NewNodeOperatorMonitor(logr.Discard(), nil, &http.Client{Timeout: 2 * time.Second})
		healthz, readyz := specsForServer(t, server)

		result := mon.probePodHealth(context.Background(), []corev1.Pod{loopbackPod("a")}, healthz, readyz)
		assert.Equal(t, probeOutcomeNotOK, result.healthz)
		assert.Equal(t, probeOutcomeNotOK, result.readyz)
	})

	// The distinction this change turns on. Nothing answered, so nothing is
	// known — reporting "false" here is what made a healthy 2/2 Karpenter look
	// like a controller failing its own health checks.
	t.Run("unreachable endpoint returns Unknown, not NotOK", func(t *testing.T) {
		mon := NewNodeOperatorMonitor(logr.Discard(), nil, &http.Client{Timeout: 100 * time.Millisecond})
		healthz := probeEndpointSpec{scheme: "http", port: "1", path: defaultHealthzPath}
		readyz := probeEndpointSpec{scheme: "http", port: "1", path: defaultReadyzPath}

		result := mon.probePodHealth(context.Background(), []corev1.Pod{loopbackPod("a")}, healthz, readyz)
		assert.Equal(t, probeOutcomeUnknown, result.healthz)
		assert.Equal(t, probeOutcomeUnknown, result.readyz)
	})

	// Probing Karpenter's metrics port produces exactly this: something listens
	// and 404s. It means the target is wrong, not that the controller is sick.
	t.Run("404 returns Unknown because that port serves no health endpoint", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer server.Close()

		mon := NewNodeOperatorMonitor(logr.Discard(), nil, &http.Client{Timeout: 2 * time.Second})
		healthz, readyz := specsForServer(t, server)

		result := mon.probePodHealth(context.Background(), []corev1.Pod{loopbackPod("a")}, healthz, readyz)
		assert.Equal(t, probeOutcomeUnknown, result.healthz)
		assert.Equal(t, probeOutcomeUnknown, result.readyz)
	})

	t.Run("no probeable pods reports Unknown and probes nothing", func(t *testing.T) {
		mon := NewNodeOperatorMonitor(logr.Discard(), nil, &http.Client{Timeout: 2 * time.Second})

		result := mon.probePodHealth(context.Background(), nil, probeEndpointSpec{}, probeEndpointSpec{})
		assert.Equal(t, probeOutcomeUnknown, result.healthz)
		assert.Equal(t, probeOutcomeUnknown, result.readyz)
		assert.Equal(t, 0, result.attempted)
	})

	// One wedged replica must not be hidden by a healthy one: both are probed and
	// the definite answer wins.
	t.Run("a single unhealthy replica makes the aggregate NotOK", func(t *testing.T) {
		var hits int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits++
			if hits <= 2 { // first Pod's healthz and readyz
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()

		mon := NewNodeOperatorMonitor(logr.Discard(), nil, &http.Client{Timeout: 2 * time.Second})
		healthz, readyz := specsForServer(t, server)

		result := mon.probePodHealth(context.Background(),
			[]corev1.Pod{loopbackPod("a"), loopbackPod("b")}, healthz, readyz)
		assert.Equal(t, probeOutcomeNotOK, result.healthz)
		assert.Equal(t, probeOutcomeNotOK, result.readyz)
		assert.Equal(t, 2, result.attempted)
	})
}

// Both directions are asserted: a one-way check also passes on a merge that
// always returns its argument.
func TestProbeOutcome_Merge(t *testing.T) {
	t.Run("a definite not-ok beats OK in either order", func(t *testing.T) {
		assert.Equal(t, probeOutcomeNotOK, probeOutcomeOK.merge(probeOutcomeNotOK))
		assert.Equal(t, probeOutcomeNotOK, probeOutcomeNotOK.merge(probeOutcomeOK))
	})

	t.Run("unknown never overwrites an answer we got", func(t *testing.T) {
		assert.Equal(t, probeOutcomeOK, probeOutcomeOK.merge(probeOutcomeUnknown))
		assert.Equal(t, probeOutcomeOK, probeOutcomeUnknown.merge(probeOutcomeOK))
		assert.Equal(t, probeOutcomeNotOK, probeOutcomeNotOK.merge(probeOutcomeUnknown))
		assert.Equal(t, probeOutcomeNotOK, probeOutcomeUnknown.merge(probeOutcomeNotOK))
	})

	t.Run("the zero value is unknown so it seeds an aggregate", func(t *testing.T) {
		var zero probeOutcome
		assert.Equal(t, probeOutcomeUnknown, zero)
		assert.Equal(t, probeOutcomeOK, zero.merge(probeOutcomeOK))
	})

	t.Run("metadata rendering keeps true and false, adds unknown", func(t *testing.T) {
		assert.Equal(t, "true", probeOutcomeOK.String())
		assert.Equal(t, "false", probeOutcomeNotOK.String())
		assert.Equal(t, "unknown", probeOutcomeUnknown.String())
	})
}

func TestNodeOperatorMonitor_AggregateDeploymentStatus(t *testing.T) {
	tests := []struct {
		name           string
		replicas       int32
		readyReplicas  int32
		expectedDeploy HealthStatus
	}{
		{
			name:           "all replicas ready",
			replicas:       2,
			readyReplicas:  2,
			expectedDeploy: HealthStatusHealthy,
		},
		{
			name:           "partial replicas ready",
			replicas:       2,
			readyReplicas:  1,
			expectedDeploy: HealthStatusDegraded,
		},
		{
			name:           "no replicas ready",
			replicas:       1,
			readyReplicas:  0,
			expectedDeploy: HealthStatusUnhealthy,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deployStatus, deployMsg, deployMeta := aggregateDeploymentStatus(tt.replicas, tt.readyReplicas, tt.replicas)
			assert.Equal(t, tt.expectedDeploy, deployStatus)
			assert.NotEmpty(t, deployMsg)
			assert.NotNil(t, deployMeta)
		})
	}
}

func TestNodeOperatorMonitor_DiscoverDeployment(t *testing.T) {
	t.Run("finds devzero karpenter by image prefix", func(t *testing.T) {
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "karpenter",
				Namespace: "kube-system",
				Labels: map[string]string{
					"app.kubernetes.io/name":     "karpenter",
					"app.kubernetes.io/instance": "karpenter",
					"app.kubernetes.io/version":  "1.7.8",
				},
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: int32Ptr(2),
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app.kubernetes.io/name": "karpenter",
					},
				},
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "controller",
								Image: "public.ecr.aws/devzeroinc/dzkarp-aws/controller:abc123",
							},
						},
					},
				},
			},
			Status: appsv1.DeploymentStatus{
				Replicas:          2,
				ReadyReplicas:     2,
				AvailableReplicas: 2,
			},
		}
		clientset := fake.NewSimpleClientset(dep)
		mon := NewNodeOperatorMonitor(logr.Discard(), clientset, &http.Client{})

		found, err := mon.discoverDeployment(context.Background())
		require.NoError(t, err)
		require.NotNil(t, found)
		assert.Equal(t, "karpenter", found.Name)
		assert.Equal(t, "kube-system", found.Namespace)
	})

	// Regression: dzKarp Helm charts name the deployment "<release>-<chart>"
	// (e.g. "karpenter-dzkarp-aws-karpenter") and set
	// app.kubernetes.io/name to the chart name, NOT "karpenter". The old
	// name=karpenter selector missed these entirely, so the node operator
	// showed as "not installed". Discovery must key on the instance label.
	t.Run("finds dzKarp deployment whose name label is the chart name", func(t *testing.T) {
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "karpenter-dzkarp-aws-karpenter",
				Namespace: "devzero-system",
				Labels: map[string]string{
					"app.kubernetes.io/name":     "dzkarp-aws-karpenter",
					"app.kubernetes.io/instance": "karpenter",
					"app.kubernetes.io/version":  "1.7.16",
				},
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: int32Ptr(1),
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app.kubernetes.io/instance": "karpenter",
					},
				},
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "controller",
								Image: "docker.io/devzeroinc/dzkarp-aws-controller:1.7.16",
							},
						},
					},
				},
			},
			Status: appsv1.DeploymentStatus{
				Replicas:          1,
				ReadyReplicas:     1,
				AvailableReplicas: 1,
			},
		}
		clientset := fake.NewSimpleClientset(dep)
		mon := NewNodeOperatorMonitor(logr.Discard(), clientset, &http.Client{})

		found, err := mon.discoverDeployment(context.Background())
		require.NoError(t, err)
		require.NotNil(t, found)
		assert.Equal(t, "karpenter-dzkarp-aws-karpenter", found.Name)
		assert.Equal(t, "devzero-system", found.Namespace)
	})

	// Regression: the dz-installer installs the dzkarp chart with Helm release
	// name "dzkarp" (dz-installer/pkg/component/dzkarp: HelmReleaseName), which
	// Helm stamps as app.kubernetes.io/instance=dzkarp and names the resources
	// "dzkarp-karpenter". The release name is deliberately NOT "karpenter" so it
	// can coexist with a pre-existing OSS Karpenter release during migration.
	// A selector hard-coded to instance=karpenter misses this install entirely,
	// so the node operator stays "not installed" forever and the Karpenter
	// migration cutover gate never opens even though dzkarp is running healthy.
	t.Run("finds dz-installer dzkarp release (instance=dzkarp)", func(t *testing.T) {
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "dzkarp-karpenter",
				Namespace: "devzero-system",
				Labels: map[string]string{
					"app.kubernetes.io/name":     "karpenter",
					"app.kubernetes.io/instance": "dzkarp",
					"app.kubernetes.io/version":  "1.7.17",
				},
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: int32Ptr(2),
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app.kubernetes.io/instance": "dzkarp",
					},
				},
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "controller",
								Image: "public.ecr.aws/devzeroinc/dzkarp-aws/controller:1.7.17",
							},
						},
					},
				},
			},
			Status: appsv1.DeploymentStatus{
				Replicas:          2,
				ReadyReplicas:     2,
				AvailableReplicas: 2,
			},
		}
		clientset := fake.NewSimpleClientset(dep)
		mon := NewNodeOperatorMonitor(logr.Discard(), clientset, &http.Client{})

		found, err := mon.discoverDeployment(context.Background())
		require.NoError(t, err)
		require.NotNil(t, found)
		assert.Equal(t, "dzkarp-karpenter", found.Name)
		assert.Equal(t, "devzero-system", found.Namespace)
	})

	t.Run("ignores upstream karpenter without devzero image", func(t *testing.T) {
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "karpenter",
				Namespace: "karpenter",
				Labels: map[string]string{
					"app.kubernetes.io/name":     "karpenter",
					"app.kubernetes.io/instance": "karpenter",
				},
			},
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "controller",
								Image: "public.ecr.aws/karpenter/controller:0.37.7",
							},
						},
					},
				},
			},
		}
		clientset := fake.NewSimpleClientset(dep)
		mon := NewNodeOperatorMonitor(logr.Discard(), clientset, &http.Client{})

		found, err := mon.discoverDeployment(context.Background())
		require.NoError(t, err)
		assert.Nil(t, found)
	})

	t.Run("returns nil when no karpenter found", func(t *testing.T) {
		clientset := fake.NewSimpleClientset()
		mon := NewNodeOperatorMonitor(logr.Discard(), clientset, &http.Client{})

		found, err := mon.discoverDeployment(context.Background())
		require.NoError(t, err)
		assert.Nil(t, found)
	})
}

// karpenterDeployment builds a DevZero-managed Karpenter controller Deployment
// for the coexistence tests. instance is the Helm release name (what
// karpenterLabelName selects on) and version doubles as the chart version label
// and the image tag, so a report built from the wrong object is identifiable.
// karpenterPodLabels mirrors the chart's selector — name plus release — which is
// what keeps two coexisting releases apart.
func karpenterPodLabels(instance string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     "karpenter",
		"app.kubernetes.io/instance": instance,
	}
}

// karpenterContainer mirrors the container as it actually ships, read off a live
// 1.7.17 install: metrics on a port named "http-metrics", health on a separate
// port named "http", and kubelet probes addressing that port by name. The port
// names are the load-bearing detail — the health port is never named "health",
// and "http-metrics" is the only name the Service ever carries.
func karpenterContainer(version string) corev1.Container {
	return corev1.Container{
		Name:  "controller",
		Image: "public.ecr.aws/devzeroinc/dzkarp-aws/controller:" + version,
		Ports: []corev1.ContainerPort{
			{Name: "http-metrics", ContainerPort: 8080},
			{Name: "http", ContainerPort: 8081},
		},
		LivenessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: "/healthz", Port: intstr.FromString("http"),
		}}},
		ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: "/readyz", Port: intstr.FromString("http"),
		}}},
	}
}

func karpenterDeployment(name, namespace, instance, version string, desired, ready int32, created time.Time) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			CreationTimestamp: metav1.NewTime(created),
			Labels: map[string]string{
				"app.kubernetes.io/name":     "karpenter",
				"app.kubernetes.io/instance": instance,
				"app.kubernetes.io/version":  version,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(desired),
			Selector: &metav1.LabelSelector{MatchLabels: karpenterPodLabels(instance)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: karpenterPodLabels(instance)},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{karpenterContainer(version)},
				},
			},
		},
		Status: appsv1.DeploymentStatus{
			Replicas:          desired,
			ReadyReplicas:     ready,
			AvailableReplicas: ready,
		},
	}
}

// karpenterPod is a running, ready replica of the given release.
func karpenterPod(name, namespace, instance, ip string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    karpenterPodLabels(instance),
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			PodIP:      ip,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
}

// Regression: a migration cutover leaves the superseded Karpenter release in the
// cluster — commonly scaled to zero rather than uninstalled — while the new
// dzkarp release runs alongside it. That coexistence is the whole reason
// dz-installer uses a distinct release name.
//
// karpenterLabelName accepts both release names and discoverDeployment lists
// cluster-wide, so both Deployments match and both pass isDevZeroImage. The old
// "first image match wins" loop could therefore report the dead release: 0 ready
// replicas and the old app.kubernetes.io/version. A zero-replica Deployment has
// no Pods to probe either, so the component reads "0/0 replicas ready" at a stale
// version while the real controller is ready and healthy — with nothing in the
// report to say a different object was measured.
// A note on fixture layout, because it decides whether these tests prove
// anything at all. List order here is NOT insertion order: client-go's fake
// tracker sorts by namespace then name ("Sort res to get deterministic order",
// client-go testing/fixture.go), which also matches what a real cluster-wide
// List returns. A fixture where the live release sorts first therefore passes
// under the old first-match loop too, and asserting "both insertion orders"
// proves nothing — both orders produce the identical List.
//
// These fixtures put the superseded release FIRST in namespace/name order:
// dzkarp overridden into kube-system, with the release it replaced left behind
// in devzero-system. Under dz-installer defaults the sort happens to favour the
// live release and the old loop was accidentally right — that accident is what
// is being removed.
func TestNodeOperatorMonitor_DiscoverDeployment_Coexistence(t *testing.T) {
	// Fixed timestamps: creation time is a tiebreak input, so it must not depend
	// on the wall clock.
	cutover := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	// The superseded release, scaled to zero at the old chart version. Sorts
	// first: "devzero-system" < "kube-system".
	newStale := func() *appsv1.Deployment {
		return karpenterDeployment("karpenter", "devzero-system", "karpenter", "1.7.9", 0, 0, cutover)
	}
	// The live release, listed second.
	newLive := func() *appsv1.Deployment {
		return karpenterDeployment("dzkarp-karpenter", "kube-system", "dzkarp", "1.7.11", 2, 2, cutover.Add(24*time.Hour))
	}

	t.Run("prefers the running release over a superseded one listed before it", func(t *testing.T) {
		mon := NewNodeOperatorMonitor(logr.Discard(), fake.NewSimpleClientset(newStale(), newLive()), &http.Client{})

		found, err := mon.discoverDeployment(context.Background())
		require.NoError(t, err)
		require.NotNil(t, found)
		assert.Equal(t, "dzkarp-karpenter", found.Name)
		assert.Equal(t, "kube-system", found.Namespace)
		assert.Equal(t, "1.7.11", found.Labels["app.kubernetes.io/version"])
	})

	// End to end on the reported symptom: these two Deployments must produce a
	// healthy 2/2 report at 1.7.11, not an unhealthy 0/0 at 1.7.9. No Service
	// exists here, so this also exercises the endpoint-discovery fallback path.
	t.Run("report describes the live release, not the superseded one", func(t *testing.T) {
		mon := NewNodeOperatorMonitor(logr.Discard(), fake.NewSimpleClientset(newStale(), newLive()), &http.Client{})

		report, version, commit, _ := mon.BuildNodeOperatorReport(context.Background())
		require.NotNil(t, report)
		assert.Equal(t, "1.7.11", version)
		assert.Equal(t, "1.7.11", commit)

		comp := report[ComponentKarpenterDeployment]
		assert.Equal(t, HealthStatusHealthy, comp.Status)
		assert.Equal(t, "2/2 replicas ready", comp.Message)
		assert.Equal(t, "2", comp.Metadata["ready_replicas"])
	})

	// Guard, not a discriminator: this layout passes with or without
	// preferDeployment. It keeps the common case covered while the fixtures
	// above deliberately use the uncommon one.
	t.Run("default layout still resolves to the live release", func(t *testing.T) {
		stale := karpenterDeployment("karpenter", "kube-system", "karpenter", "1.7.9", 0, 0, cutover)
		live := karpenterDeployment("dzkarp-karpenter", "devzero-system", "dzkarp", "1.7.11", 2, 2, cutover.Add(24*time.Hour))
		mon := NewNodeOperatorMonitor(logr.Discard(), fake.NewSimpleClientset(stale, live), &http.Client{})

		found, err := mon.discoverDeployment(context.Background())
		require.NoError(t, err)
		require.NotNil(t, found)
		assert.Equal(t, "dzkarp-karpenter", found.Name)
	})

	t.Run("prefers the newer release when both are running", func(t *testing.T) {
		older := karpenterDeployment("karpenter", "devzero-system", "karpenter", "1.7.9", 2, 2, cutover)
		newer := karpenterDeployment("dzkarp-karpenter", "kube-system", "dzkarp", "1.7.11", 2, 2, cutover.Add(24*time.Hour))
		mon := NewNodeOperatorMonitor(logr.Discard(), fake.NewSimpleClientset(older, newer), &http.Client{})

		found, err := mon.discoverDeployment(context.Background())
		require.NoError(t, err)
		require.NotNil(t, found)
		assert.Equal(t, "dzkarp-karpenter", found.Name)
	})

	// A controller failing to come up must still be reported. When neither
	// release is ready, liveness cannot discriminate, so the newer one wins and
	// the real problem surfaces instead of being masked by an equally dead
	// older release.
	t.Run("reports the newer release when neither is ready", func(t *testing.T) {
		oldDead := karpenterDeployment("karpenter", "devzero-system", "karpenter", "1.7.9", 2, 0, cutover)
		newDead := karpenterDeployment("dzkarp-karpenter", "kube-system", "dzkarp", "1.7.11", 2, 0, cutover.Add(24*time.Hour))
		mon := NewNodeOperatorMonitor(logr.Discard(), fake.NewSimpleClientset(oldDead, newDead), &http.Client{})

		found, err := mon.discoverDeployment(context.Background())
		require.NoError(t, err)
		require.NotNil(t, found)
		assert.Equal(t, "dzkarp-karpenter", found.Name)
	})
}

// The namespace/name tiebreak is unreachable through discoverDeployment with
// realistic fixtures — it needs two releases created in the same nanosecond —
// but it is what makes the choice independent of List order, so it is asserted
// directly. Both directions are checked: a one-way assertion would pass on a
// comparison that always returns true.
func TestPreferDeployment_TieBreaksDeterministically(t *testing.T) {
	sameInstant := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	t.Run("earlier namespace wins", func(t *testing.T) {
		a := karpenterDeployment("karpenter", "devzero-system", "dzkarp", "1.7.11", 1, 1, sameInstant)
		b := karpenterDeployment("karpenter", "kube-system", "karpenter", "1.7.11", 1, 1, sameInstant)
		assert.True(t, preferDeployment(a, b))
		assert.False(t, preferDeployment(b, a))
	})

	t.Run("earlier name wins within a namespace", func(t *testing.T) {
		a := karpenterDeployment("dzkarp-karpenter", "devzero-system", "dzkarp", "1.7.11", 1, 1, sameInstant)
		b := karpenterDeployment("karpenter", "devzero-system", "karpenter", "1.7.11", 1, 1, sameInstant)
		assert.True(t, preferDeployment(a, b))
		assert.False(t, preferDeployment(b, a))
	})
}

func TestNodeOperatorMonitor_IsDevZeroImage(t *testing.T) {
	t.Run("devzero public ECR image", func(t *testing.T) {
		dep := &appsv1.Deployment{
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Image: "public.ecr.aws/devzeroinc/dzkarp-aws/controller:abc123"},
						},
					},
				},
			},
		}
		assert.True(t, isDevZeroImage(dep))
	})

	t.Run("devzero private ECR image", func(t *testing.T) {
		dep := &appsv1.Deployment{
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Image: "123456789.dkr.ecr.us-east-1.amazonaws.com/devzeroinc/dzkarp-aws/controller:abc123"},
						},
					},
				},
			},
		}
		assert.True(t, isDevZeroImage(dep))
	})

	t.Run("devzero Azure ACR image", func(t *testing.T) {
		dep := &appsv1.Deployment{
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Image: "devzeroinc.azurecr.io/dzkarp-azure/controller:abc123"},
						},
					},
				},
			},
		}
		assert.True(t, isDevZeroImage(dep))
	})

	t.Run("devzero GCP image is under devzeroinc, not cloudpilotai", func(t *testing.T) {
		// Verified against the published chart:
		//   helm show values oci://public.ecr.aws/devzeroinc/dzkarp-gcp/karpenter
		// (chart 1.1.0) => controller.image.repository is
		//   "public.ecr.aws/devzeroinc/dzkarp-gcp/controller"
		// so DevZero's GCP controller carries "devzeroinc" like AWS and Azure.
		dep := &appsv1.Deployment{
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Image: "public.ecr.aws/devzeroinc/dzkarp-gcp/controller:1.1.0"},
						},
					},
				},
			},
		}
		assert.True(t, isDevZeroImage(dep))
	})

	t.Run("upstream cloudpilotai gcp provider is NOT devzero-managed", func(t *testing.T) {
		// A raw cloudpilot-ai karpenter-provider-gcp install is upstream OSS, not
		// a DevZero-managed dzkarp install. Treating it as DevZero-managed made
		// zxporter disagree with dakr's KarpenterResource.IsDevZeroManaged
		// (which matches only "devzeroinc"), so the same cluster could be
		// reported as both dzkarp-managed and not.
		dep := &appsv1.Deployment{
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Image: "public.ecr.aws/cloudpilotai/gcp/karpenter:abc123"},
						},
					},
				},
			},
		}
		assert.False(t, isDevZeroImage(dep))
	})

	t.Run("upstream karpenter image", func(t *testing.T) {
		dep := &appsv1.Deployment{
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Image: "public.ecr.aws/karpenter/controller:0.37.7"},
						},
					},
				},
			},
		}
		assert.False(t, isDevZeroImage(dep))
	})

	t.Run("no containers", func(t *testing.T) {
		dep := &appsv1.Deployment{}
		assert.False(t, isDevZeroImage(dep))
	})
}

// The regression these tests exist for, stated once: on a live 2/2 cluster the
// Karpenter Service publishes exactly one port, named "http-metrics" (8080),
// while /healthz and /readyz are served on container port 8081 named "http". A
// Service-port lookup for the name "http" or "health" therefore never matched,
// fell back to 8081, and dialed a port the Service does not expose — a
// blackholed request every cycle, reported as the controller failing its checks.
func TestNodeOperatorMonitor_HealthEndpoints(t *testing.T) {
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	t.Run("reads the port and paths from the container's own kubelet probes", func(t *testing.T) {
		dep := karpenterDeployment("karpenter", "kube-system", "karpenter", "1.7.17", 2, 2, created)
		mon := NewNodeOperatorMonitor(logr.Discard(), fake.NewSimpleClientset(), &http.Client{})

		healthz, readyz := mon.healthEndpoints(dep)

		// 8081, the port named "http" — not 8080, the metrics port that is the
		// only one the Service publishes.
		assert.Equal(t, "http://10.0.0.1:8081/healthz", healthz.url("10.0.0.1"))
		assert.Equal(t, "http://10.0.0.1:8081/readyz", readyz.url("10.0.0.1"))
	})

	t.Run("resolves a numeric probe port", func(t *testing.T) {
		dep := karpenterDeployment("karpenter", "kube-system", "karpenter", "1.7.17", 2, 2, created)
		c := &dep.Spec.Template.Spec.Containers[0]
		c.LivenessProbe.HTTPGet.Port = intstr.FromInt32(9000)
		c.ReadinessProbe.HTTPGet.Port = intstr.FromInt32(9000)
		mon := NewNodeOperatorMonitor(logr.Discard(), fake.NewSimpleClientset(), &http.Client{})

		healthz, readyz := mon.healthEndpoints(dep)
		assert.Equal(t, "http://10.0.0.1:9000/healthz", healthz.url("10.0.0.1"))
		assert.Equal(t, "http://10.0.0.1:9000/readyz", readyz.url("10.0.0.1"))
	})

	t.Run("honours non-default probe paths and the HTTPS scheme", func(t *testing.T) {
		dep := karpenterDeployment("karpenter", "kube-system", "karpenter", "1.7.17", 2, 2, created)
		c := &dep.Spec.Template.Spec.Containers[0]
		c.LivenessProbe.HTTPGet.Path = "/live"
		c.LivenessProbe.HTTPGet.Scheme = corev1.URISchemeHTTPS
		c.ReadinessProbe.HTTPGet.Path = "/ready"
		mon := NewNodeOperatorMonitor(logr.Discard(), fake.NewSimpleClientset(), &http.Client{})

		healthz, readyz := mon.healthEndpoints(dep)
		assert.Equal(t, "https://10.0.0.1:8081/live", healthz.url("10.0.0.1"))
		assert.Equal(t, "http://10.0.0.1:8081/ready", readyz.url("10.0.0.1"))
	})

	// A chart may probe by exec, or declare no probes. The container port named
	// "http" is the next best evidence — and note this is where that name
	// legitimately appears, which is why matching it on Service ports never
	// worked.
	t.Run("falls back to a container port named http when probes are not HTTP", func(t *testing.T) {
		dep := karpenterDeployment("karpenter", "kube-system", "karpenter", "1.7.17", 2, 2, created)
		c := &dep.Spec.Template.Spec.Containers[0]
		c.LivenessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{Command: []string{"true"}},
		}}
		c.ReadinessProbe = nil
		mon := NewNodeOperatorMonitor(logr.Discard(), fake.NewSimpleClientset(), &http.Client{})

		healthz, readyz := mon.healthEndpoints(dep)
		assert.Equal(t, "http://10.0.0.1:8081/healthz", healthz.url("10.0.0.1"))
		assert.Equal(t, "http://10.0.0.1:8081/readyz", readyz.url("10.0.0.1"))
	})

	t.Run("falls back to the default health port when nothing names one", func(t *testing.T) {
		dep := karpenterDeployment("karpenter", "kube-system", "karpenter", "1.7.17", 2, 2, created)
		c := &dep.Spec.Template.Spec.Containers[0]
		c.LivenessProbe = nil
		c.ReadinessProbe = nil
		c.Ports = []corev1.ContainerPort{{Name: "http-metrics", ContainerPort: 8080}}
		mon := NewNodeOperatorMonitor(logr.Discard(), fake.NewSimpleClientset(), &http.Client{})

		healthz, readyz := mon.healthEndpoints(dep)
		assert.Equal(t, "http://10.0.0.1:"+defaultHealthPort+"/healthz", healthz.url("10.0.0.1"))
		assert.Equal(t, "http://10.0.0.1:"+defaultHealthPort+"/readyz", readyz.url("10.0.0.1"))
	})

	// A probe naming a port the container does not declare is unusable — the
	// kubelet rejects it too — so it must not silently become part of a URL.
	t.Run("ignores a probe naming an undeclared port", func(t *testing.T) {
		dep := karpenterDeployment("karpenter", "kube-system", "karpenter", "1.7.17", 2, 2, created)
		c := &dep.Spec.Template.Spec.Containers[0]
		c.LivenessProbe.HTTPGet.Port = intstr.FromString("nonexistent")
		c.ReadinessProbe.HTTPGet.Port = intstr.FromString("nonexistent")
		mon := NewNodeOperatorMonitor(logr.Discard(), fake.NewSimpleClientset(), &http.Client{})

		healthz, _ := mon.healthEndpoints(dep)
		// Falls through to the container port named "http".
		assert.Equal(t, "http://10.0.0.1:8081/healthz", healthz.url("10.0.0.1"))
	})

	t.Run("brackets an IPv6 pod IP", func(t *testing.T) {
		spec := probeEndpointSpec{scheme: "http", port: "8081", path: defaultHealthzPath}
		assert.Equal(t, "http://[fd00::1]:8081/healthz", spec.url("fd00::1"))
	})
}

func TestNodeOperatorMonitor_DiscoverProbeTargets(t *testing.T) {
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	dep := karpenterDeployment("dzkarp-karpenter", "devzero-system", "dzkarp", "1.7.17", 2, 2, created)

	t.Run("returns the deployment's running pods", func(t *testing.T) {
		mon := NewNodeOperatorMonitor(logr.Discard(), fake.NewSimpleClientset(
			karpenterPod("dzkarp-karpenter-a", "devzero-system", "dzkarp", "192.168.27.204"),
			karpenterPod("dzkarp-karpenter-b", "devzero-system", "dzkarp", "192.168.85.221"),
		), &http.Client{})

		targets, err := mon.discoverProbeTargets(context.Background(), dep)
		require.NoError(t, err)
		require.Len(t, targets, 2)
		assert.Equal(t, "192.168.27.204", targets[0].Status.PodIP)
		assert.Equal(t, "192.168.85.221", targets[1].Status.PodIP)
	})

	// The superseded release's Pods answer nothing, and attributing their silence
	// to the live release is the failure mode the old Service scoping guarded
	// against. The decoy sorts first by name so a selector-free List would take it.
	t.Run("ignores another release's pods in the same namespace", func(t *testing.T) {
		mon := NewNodeOperatorMonitor(logr.Discard(), fake.NewSimpleClientset(
			karpenterPod("aaa-old-karpenter", "devzero-system", "karpenter", "192.168.1.1"),
			karpenterPod("dzkarp-karpenter-a", "devzero-system", "dzkarp", "192.168.27.204"),
		), &http.Client{})

		targets, err := mon.discoverProbeTargets(context.Background(), dep)
		require.NoError(t, err)
		require.Len(t, targets, 1)
		assert.Equal(t, "192.168.27.204", targets[0].Status.PodIP)
	})

	t.Run("ignores a same-release pod in another namespace", func(t *testing.T) {
		mon := NewNodeOperatorMonitor(logr.Discard(), fake.NewSimpleClientset(
			karpenterPod("aaa-decoy", "kube-system", "dzkarp", "192.168.1.1"),
			karpenterPod("dzkarp-karpenter-a", "devzero-system", "dzkarp", "192.168.27.204"),
		), &http.Client{})

		targets, err := mon.discoverProbeTargets(context.Background(), dep)
		require.NoError(t, err)
		require.Len(t, targets, 1)
		assert.Equal(t, "192.168.27.204", targets[0].Status.PodIP)
	})

	// Each of these would answer with a connection error that says nothing about
	// controller health, so counting them as probe failures would manufacture the
	// exact false alarm this monitor is being fixed to stop producing.
	t.Run("skips pods whose answer would mean nothing", func(t *testing.T) {
		pending := karpenterPod("dzkarp-karpenter-pending", "devzero-system", "dzkarp", "")
		pending.Status.Phase = corev1.PodPending
		noIP := karpenterPod("dzkarp-karpenter-noip", "devzero-system", "dzkarp", "")
		terminating := karpenterPod("dzkarp-karpenter-term", "devzero-system", "dzkarp", "192.168.1.2")
		now := metav1.NewTime(created)
		terminating.DeletionTimestamp = &now
		succeeded := karpenterPod("dzkarp-karpenter-done", "devzero-system", "dzkarp", "192.168.1.3")
		succeeded.Status.Phase = corev1.PodSucceeded
		live := karpenterPod("dzkarp-karpenter-live", "devzero-system", "dzkarp", "192.168.27.204")

		mon := NewNodeOperatorMonitor(logr.Discard(),
			fake.NewSimpleClientset(pending, noIP, terminating, succeeded, live), &http.Client{})

		targets, err := mon.discoverProbeTargets(context.Background(), dep)
		require.NoError(t, err)
		require.Len(t, targets, 1)
		assert.Equal(t, "192.168.27.204", targets[0].Status.PodIP)
	})

	// Every rolling update briefly has one: Running, not yet Ready, and correctly
	// answering /readyz with a non-200 because that is what not-ready means.
	// Probing it would append "readyz=false" to a healthy report on the strength
	// of a deploy in progress.
	t.Run("skips a running pod that is not ready yet", func(t *testing.T) {
		starting := karpenterPod("dzkarp-karpenter-aaa-starting", "devzero-system", "dzkarp", "192.168.1.4")
		starting.Status.Conditions = []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionFalse},
		}
		noConditions := karpenterPod("dzkarp-karpenter-aab-nocond", "devzero-system", "dzkarp", "192.168.1.5")
		noConditions.Status.Conditions = nil
		ready := karpenterPod("dzkarp-karpenter-zzz-ready", "devzero-system", "dzkarp", "192.168.27.204")

		mon := NewNodeOperatorMonitor(logr.Discard(),
			fake.NewSimpleClientset(starting, noConditions, ready), &http.Client{})

		targets, err := mon.discoverProbeTargets(context.Background(), dep)
		require.NoError(t, err)
		require.Len(t, targets, 1)
		assert.Equal(t, "192.168.27.204", targets[0].Status.PodIP)
	})

	// Which replicas get probed must not depend on List order, or a truncated
	// report could flip between cycles for that reason alone.
	t.Run("truncates deterministically by name", func(t *testing.T) {
		pods := []runtime.Object{}
		for _, suffix := range []string{"e", "c", "a", "d", "b"} {
			pods = append(pods, karpenterPod("dzkarp-karpenter-"+suffix, "devzero-system", "dzkarp", "192.168.1."+suffix))
		}
		mon := NewNodeOperatorMonitor(logr.Discard(), fake.NewSimpleClientset(pods...), &http.Client{})

		targets, err := mon.discoverProbeTargets(context.Background(), dep)
		require.NoError(t, err)
		require.Len(t, targets, maxProbedPods)
		assert.Equal(t, "dzkarp-karpenter-a", targets[0].Name)
		assert.Equal(t, "dzkarp-karpenter-b", targets[1].Name)
		assert.Equal(t, "dzkarp-karpenter-c", targets[2].Name)
	})

	t.Run("returns no targets when the release has no pods", func(t *testing.T) {
		mon := NewNodeOperatorMonitor(logr.Discard(), fake.NewSimpleClientset(), &http.Client{})

		targets, err := mon.discoverProbeTargets(context.Background(), dep)
		require.NoError(t, err)
		assert.Empty(t, targets)
	})

	// An empty selector matches every Pod in the namespace. Probing unrelated
	// workloads and reporting the answer as Karpenter's is worse than not
	// probing, so this is an error rather than a wildcard.
	t.Run("refuses a missing or empty pod selector", func(t *testing.T) {
		mon := NewNodeOperatorMonitor(logr.Discard(), fake.NewSimpleClientset(
			karpenterPod("unrelated", "devzero-system", "dzkarp", "192.168.1.1"),
		), &http.Client{})

		noSelector := karpenterDeployment("dzkarp-karpenter", "devzero-system", "dzkarp", "1.7.17", 2, 2, created)
		noSelector.Spec.Selector = nil
		_, err := mon.discoverProbeTargets(context.Background(), noSelector)
		require.Error(t, err)

		emptySelector := karpenterDeployment("dzkarp-karpenter", "devzero-system", "dzkarp", "1.7.17", 2, 2, created)
		emptySelector.Spec.Selector = &metav1.LabelSelector{}
		_, err = mon.discoverProbeTargets(context.Background(), emptySelector)
		require.Error(t, err)
	})
}

// hostAwareTransport answers 200 for wantHosts and refuses the connection for
// anything else. Refusing rather than answering 503 is the point: a wrong target
// produces a transport error, which is what the old code turned into
// "healthz=false" on a healthy controller.
type hostAwareTransport struct{ wantHosts []string }

func (t hostAwareTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !slices.Contains(t.wantHosts, req.URL.Host) {
		return nil, fmt.Errorf("connection refused: nothing listening on %s", req.URL.Host)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

// A cutover that is rolled back or abandoned leaves both releases in one
// namespace: dzkarp installed alongside the running release, scaled to zero, and
// left behind. Its Pods are gone, so probing them answers nothing — and
// attributing that silence to the live release is the whole failure mode.
//
// Fixture direction is what makes these discriminate. client-go's fake tracker
// sorts by namespace then name, as a real List does, so the abandoned release
// ("dzkarp-aws-karpenter") sorts before the live one ("karpenter") and a
// selector-free lookup would take it.
func TestNodeOperatorMonitor_ProbeTargets_Coexistence(t *testing.T) {
	cutover := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	// Live release: still serving, so discoverDeployment picks it on readiness.
	newLive := func() *appsv1.Deployment {
		return karpenterDeployment("karpenter", "kube-system", "karpenter", "1.7.11", 2, 2, cutover.Add(24*time.Hour))
	}
	// Abandoned dzkarp attempt, scaled to zero, sorts first by name.
	newStale := func() *appsv1.Deployment {
		return karpenterDeployment("dzkarp-aws-karpenter", "kube-system", "dzkarp", "1.7.9", 0, 0, cutover)
	}
	livePods := func() []runtime.Object {
		return []runtime.Object{
			karpenterPod("karpenter-1", "kube-system", "karpenter", "10.0.0.1"),
			karpenterPod("karpenter-2", "kube-system", "karpenter", "10.0.0.2"),
		}
	}
	// A leftover Pod of the dead release, sorting first, with nothing listening.
	stalePod := func() runtime.Object {
		return karpenterPod("aaa-dzkarp-aws-karpenter-1", "kube-system", "dzkarp", "10.9.9.9")
	}

	t.Run("probes the chosen release's pods, not the ones listed first", func(t *testing.T) {
		objs := append(livePods(), newLive(), newStale(), stalePod())
		mon := NewNodeOperatorMonitor(logr.Discard(), fake.NewSimpleClientset(objs...), &http.Client{})

		dep, err := mon.discoverDeployment(context.Background())
		require.NoError(t, err)
		require.NotNil(t, dep)
		require.Equal(t, "karpenter", dep.Name)

		targets, err := mon.discoverProbeTargets(context.Background(), dep)
		require.NoError(t, err)
		require.Len(t, targets, 2)
		assert.Equal(t, "10.0.0.1", targets[0].Status.PodIP)
		assert.Equal(t, "10.0.0.2", targets[1].Status.PodIP)
	})

	// End to end on the reported symptom: a healthy 2/2 controller must report
	// clean, with no probe-failure suffix and no "false" in metadata.
	t.Run("healthy report reaches the health port and gains no suffix", func(t *testing.T) {
		objs := append(livePods(), newLive(), newStale(), stalePod())
		mon := NewNodeOperatorMonitor(logr.Discard(), fake.NewSimpleClientset(objs...),
			// Only the live Pods' health port answers — 8081, the container port
			// named "http". The metrics port and the dead release stay silent.
			&http.Client{Transport: hostAwareTransport{wantHosts: []string{"10.0.0.1:8081", "10.0.0.2:8081"}}})

		report, version, _, _ := mon.BuildNodeOperatorReport(context.Background())
		require.NotNil(t, report)
		assert.Equal(t, "1.7.11", version)

		comp := report[ComponentKarpenterDeployment]
		assert.Equal(t, HealthStatusHealthy, comp.Status)
		assert.Equal(t, "2/2 replicas ready", comp.Message)
		assert.Equal(t, "true", comp.Metadata["controller_healthz"])
		assert.Equal(t, "true", comp.Metadata["controller_readyz"])
		assert.Equal(t, "2", comp.Metadata["probed_pods"])
	})

	// The abandoned release on its own: nothing to probe, so nothing is known.
	// The report must say 0/0 replicas and stop there — an unreachable endpoint is
	// not the controller failing its checks, and claiming otherwise is what sent
	// diagnosis after a network fault that did not exist.
	t.Run("a release with no pods reports unknown, not false", func(t *testing.T) {
		mon := NewNodeOperatorMonitor(logr.Discard(),
			fake.NewSimpleClientset(newStale()), &http.Client{Transport: hostAwareTransport{}})

		report, _, _, _ := mon.BuildNodeOperatorReport(context.Background())
		require.NotNil(t, report)

		comp := report[ComponentKarpenterDeployment]
		assert.Equal(t, HealthStatusUnhealthy, comp.Status)
		assert.Equal(t, "0/0 replicas ready", comp.Message)
		assert.NotContains(t, comp.Message, "healthz")
		assert.Equal(t, "unknown", comp.Metadata["controller_healthz"])
		assert.Equal(t, "unknown", comp.Metadata["controller_readyz"])
		assert.Equal(t, "0", comp.Metadata["probed_pods"])
	})
}

func TestNodeOperatorMonitor_ExtractVersionInfo(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"app.kubernetes.io/version": "1.7.8",
			},
		},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "controller",
							Image: "public.ecr.aws/devzeroinc/dzkarp-aws/snapshot/controller:feda6e1@sha256:8a1acd",
						},
					},
				},
			},
		},
	}

	version, commit := extractVersionInfo(dep)
	assert.Equal(t, "1.7.8", version)
	assert.Equal(t, "feda6e1", commit)
}

// loopbackDeployment is a 1/1 Karpenter release whose health container port is
// port, so a Pod IP of 127.0.0.1 makes the real probe path reach a local test
// server. No transport stubbing: this exercises URL construction, port
// resolution and dialing exactly as in-cluster.
func loopbackDeployment(t *testing.T, port int) (*appsv1.Deployment, *corev1.Pod) {
	t.Helper()
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	dep := karpenterDeployment("dzkarp-karpenter", "devzero-system", "dzkarp", "1.7.17", 1, 1, created)
	dep.Spec.Template.Spec.Containers[0].Ports = []corev1.ContainerPort{
		{Name: "http-metrics", ContainerPort: 8080},
		{Name: "http", ContainerPort: int32(port)},
	}
	return dep, karpenterPod("dzkarp-karpenter-1", "devzero-system", "dzkarp", "127.0.0.1")
}

func serverPort(t *testing.T, server *httptest.Server) int {
	t.Helper()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	require.NoError(t, err)
	n, err := strconv.Atoi(port)
	require.NoError(t, err)
	return n
}

func TestNodeOperatorMonitor_BuildReport(t *testing.T) {
	// The fix, end to end: a healthy controller is reached on its health port and
	// reports true. Before this, the probe dialed a port no Service exposed and
	// every cycle reported healthz=false readyz=false on a ready controller.
	t.Run("a reachable healthy controller reports true", func(t *testing.T) {
		var gotPaths []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPaths = append(gotPaths, r.URL.Path)
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		dep, pod := loopbackDeployment(t, serverPort(t, server))
		mon := NewNodeOperatorMonitor(logr.Discard(), fake.NewSimpleClientset(dep, pod), server.Client())

		report, version, commit, uptimeSince := mon.BuildNodeOperatorReport(context.Background())
		require.NotNil(t, report, "report should not be nil when dzKarp is found")
		assert.Equal(t, "1.7.17", version)
		assert.Equal(t, "1.7.17", commit)
		assert.False(t, uptimeSince.IsZero())
		assert.Len(t, report, 1)

		deployComp, ok := report[ComponentKarpenterDeployment]
		require.True(t, ok)
		assert.Equal(t, HealthStatusHealthy, deployComp.Status)
		assert.Equal(t, "1/1 replicas ready", deployComp.Message)
		assert.Equal(t, "true", deployComp.Metadata["controller_healthz"])
		assert.Equal(t, "true", deployComp.Metadata["controller_readyz"])
		assert.Equal(t, "1", deployComp.Metadata["probed_pods"])

		// The paths the kubelet probes, taken from the container's own probe spec.
		assert.Equal(t, []string{"/healthz", "/readyz"}, gotPaths)
	})

	// A controller that answers not-OK is worth saying out loud — but replica
	// health stays authoritative, because the kubelet is already acting on the
	// same answer by restarting or de-readying the Pod.
	t.Run("a controller answering not-ok is annotated without being downgraded", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer server.Close()

		dep, pod := loopbackDeployment(t, serverPort(t, server))
		mon := NewNodeOperatorMonitor(logr.Discard(), fake.NewSimpleClientset(dep, pod), server.Client())

		report, _, _, _ := mon.BuildNodeOperatorReport(context.Background())
		require.NotNil(t, report)

		deployComp := report[ComponentKarpenterDeployment]
		assert.Equal(t, HealthStatusHealthy, deployComp.Status)
		assert.Equal(t, "1/1 replicas ready (controller healthz=false readyz=false)", deployComp.Message)
		assert.Equal(t, "false", deployComp.Metadata["controller_healthz"])
		assert.Equal(t, "false", deployComp.Metadata["controller_readyz"])
	})

	// The reported symptom, pinned at the report level. Nothing is listening, so
	// nothing is known — the message must stay clean and the metadata must say
	// unknown rather than accusing a healthy controller of failing its checks.
	t.Run("an unreachable controller reports unknown and no message suffix", func(t *testing.T) {
		// Port 1: nothing listens, so the dial is refused.
		dep, pod := loopbackDeployment(t, 1)
		mon := NewNodeOperatorMonitor(logr.Discard(), fake.NewSimpleClientset(dep, pod),
			&http.Client{Timeout: 2 * time.Second})

		report, _, _, _ := mon.BuildNodeOperatorReport(context.Background())
		require.NotNil(t, report)

		deployComp := report[ComponentKarpenterDeployment]
		assert.Equal(t, HealthStatusHealthy, deployComp.Status)
		assert.Equal(t, "1/1 replicas ready", deployComp.Message)
		assert.NotContains(t, deployComp.Message, "healthz")
		assert.Equal(t, "unknown", deployComp.Metadata["controller_healthz"])
		assert.Equal(t, "unknown", deployComp.Metadata["controller_readyz"])
		assert.Equal(t, "1", deployComp.Metadata["probed_pods"])
	})

	// Pod discovery failing is not a health signal either: report what the
	// Deployment says and omit the probe fields entirely rather than defaulting
	// them to a value that reads as a verdict.
	t.Run("undiscoverable pods fall back to deployment status only", func(t *testing.T) {
		dep, _ := loopbackDeployment(t, 1)
		dep.Spec.Selector = nil
		mon := NewNodeOperatorMonitor(logr.Discard(), fake.NewSimpleClientset(dep),
			&http.Client{Timeout: 100 * time.Millisecond})

		report, version, _, _ := mon.BuildNodeOperatorReport(context.Background())
		require.NotNil(t, report)
		assert.Equal(t, "1.7.17", version)
		assert.Len(t, report, 1)

		deployComp := report[ComponentKarpenterDeployment]
		assert.Equal(t, HealthStatusHealthy, deployComp.Status)
		assert.Equal(t, "1/1 replicas ready", deployComp.Message)
		assert.NotContains(t, deployComp.Metadata, "controller_healthz")
		assert.NotContains(t, deployComp.Metadata, "controller_readyz")
	})

	t.Run("returns nil when no devzero karpenter found", func(t *testing.T) {
		clientset := fake.NewSimpleClientset()
		mon := NewNodeOperatorMonitor(logr.Discard(), clientset, &http.Client{})

		report, _, _, _ := mon.BuildNodeOperatorReport(context.Background())
		assert.Nil(t, report)
	})
}

func int32Ptr(i int32) *int32 { return &i }
