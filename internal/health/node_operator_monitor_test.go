package health

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestNodeOperatorMonitor_ProbeHealth(t *testing.T) {
	t.Run("healthy endpoint returns healthy", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		mon := NewNodeOperatorMonitor(logr.Discard(), nil, &http.Client{Timeout: 2 * time.Second})

		addr := strings.TrimPrefix(server.URL, "http://")
		result := mon.probePodHealth(context.Background(), addr)
		assert.True(t, result.healthzOK)
		assert.True(t, result.readyzOK)
	})

	t.Run("unhealthy endpoint returns unhealthy", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer server.Close()

		mon := NewNodeOperatorMonitor(logr.Discard(), nil, &http.Client{Timeout: 2 * time.Second})

		addr := strings.TrimPrefix(server.URL, "http://")
		result := mon.probePodHealth(context.Background(), addr)
		assert.False(t, result.healthzOK)
		assert.False(t, result.readyzOK)
	})

	t.Run("unreachable endpoint returns unhealthy", func(t *testing.T) {
		mon := NewNodeOperatorMonitor(logr.Discard(), nil, &http.Client{Timeout: 100 * time.Millisecond})

		result := mon.probePodHealth(context.Background(), "127.0.0.1:1")
		assert.False(t, result.healthzOK)
		assert.False(t, result.readyzOK)
	})
}

func TestNodeOperatorMonitor_AggregateStatus(t *testing.T) {
	tests := []struct {
		name           string
		probes         []podProbeResult
		expectedHealth HealthStatus
		expectedDeploy HealthStatus
		replicas       int32
		readyReplicas  int32
	}{
		{
			name: "all pods healthy, deployment full",
			probes: []podProbeResult{
				{healthzOK: true, readyzOK: true},
				{healthzOK: true, readyzOK: true},
			},
			replicas: 2, readyReplicas: 2,
			expectedHealth: HealthStatusHealthy,
			expectedDeploy: HealthStatusHealthy,
		},
		{
			name: "one pod unhealthy",
			probes: []podProbeResult{
				{healthzOK: true, readyzOK: true},
				{healthzOK: false, readyzOK: false},
			},
			replicas: 2, readyReplicas: 1,
			expectedHealth: HealthStatusDegraded,
			expectedDeploy: HealthStatusDegraded,
		},
		{
			name:           "no pods reachable",
			probes:         []podProbeResult{{healthzOK: false, readyzOK: false}},
			replicas:       1,
			readyReplicas:  0,
			expectedHealth: HealthStatusUnhealthy,
			expectedDeploy: HealthStatusUnhealthy,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			healthStatus, healthMsg, healthMeta := aggregateProbeStatus(tt.probes)
			assert.Equal(t, tt.expectedHealth, healthStatus)
			assert.NotEmpty(t, healthMsg)
			assert.NotNil(t, healthMeta)

			deployStatus, deployMsg, deployMeta := aggregateDeploymentStatus(tt.replicas, tt.readyReplicas, tt.replicas)
			assert.Equal(t, tt.expectedDeploy, deployStatus)
			assert.NotEmpty(t, deployMsg)
			assert.NotNil(t, deployMeta)
		})
	}
}

func TestNodeOperatorMonitor_DiscoverDeployment(t *testing.T) {
	t.Run("finds devzero-managed karpenter deployment", func(t *testing.T) {
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "karpenter",
				Namespace: "kube-system",
				Labels: map[string]string{
					"app.kubernetes.io/name":    "karpenter",
					"dakr.devzero.io/managed":   "true",
					"app.kubernetes.io/version": "1.7.8",
				},
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: int32Ptr(2),
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app.kubernetes.io/name": "karpenter",
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

	t.Run("ignores non-devzero karpenter", func(t *testing.T) {
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "karpenter",
				Namespace: "karpenter",
				Labels: map[string]string{
					"app.kubernetes.io/name": "karpenter",
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

func TestNodeOperatorMonitor_DiscoverPods(t *testing.T) {
	t.Run("finds pods matching deployment labels", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "karpenter-abc123",
				Namespace: "kube-system",
				Labels: map[string]string{
					"app.kubernetes.io/name":     "karpenter",
					"app.kubernetes.io/instance": "karpenter",
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				PodIP: "10.0.0.1",
			},
		}
		clientset := fake.NewSimpleClientset(pod)
		mon := NewNodeOperatorMonitor(logr.Discard(), clientset, &http.Client{})

		pods, err := mon.discoverPods(context.Background(), "kube-system", map[string]string{
			"app.kubernetes.io/name": "karpenter",
		})
		require.NoError(t, err)
		require.Len(t, pods, 1)
		assert.Equal(t, "10.0.0.1", pods[0].Status.PodIP)
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

func TestNodeOperatorMonitor_BuildReport(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	addr := strings.TrimPrefix(server.URL, "http://")
	host := strings.Split(addr, ":")[0]
	port := strings.Split(addr, ":")[1]

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "karpenter-abc123",
			Namespace: "kube-system",
			Labels: map[string]string{
				"app.kubernetes.io/name":     "karpenter",
				"app.kubernetes.io/instance": "karpenter",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: host,
		},
	}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "karpenter",
			Namespace: "kube-system",
			Labels: map[string]string{
				"app.kubernetes.io/name":    "karpenter",
				"dakr.devzero.io/managed":   "true",
				"app.kubernetes.io/version": "1.7.8",
			},
			CreationTimestamp: metav1.Now(),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/name": "karpenter",
				},
			},
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "controller", Image: "controller:abc123@sha256:def"},
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

	clientset := fake.NewSimpleClientset(dep, pod)
	mon := NewNodeOperatorMonitor(logr.Discard(), clientset, &http.Client{Timeout: 2 * time.Second})
	mon.healthPort = port

	report, version, commit, uptimeSince := mon.BuildNodeOperatorReport(context.Background())
	require.NotNil(t, report, "report should not be nil when dzKarp is found")
	assert.Equal(t, "1.7.8", version)
	assert.Len(t, report, 2)

	healthComp, ok := report[ComponentKarpenterHealth]
	require.True(t, ok)
	assert.Equal(t, HealthStatusHealthy, healthComp.Status)

	deployComp, ok := report[ComponentKarpenterDeployment]
	require.True(t, ok)
	assert.Equal(t, HealthStatusHealthy, deployComp.Status)

	_ = commit
	_ = uptimeSince
	_ = fmt.Sprintf("port=%s", port)
}

func int32Ptr(i int32) *int32 { return &i }
