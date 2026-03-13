package collector

import (
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func newTestDeploymentCollector(client *fake.Clientset, namespaces []string, excluded []ExcludedDeployment) *DeploymentCollector {
	return NewDeploymentCollector(
		client,
		namespaces,
		excluded,
		50,
		100*time.Millisecond,
		logr.Discard(),
		&noopTelemetryLogger{},
	)
}

// startDeploymentCollector starts the collector (via t.Context()) and waits for Start() to return.
// It fails the test immediately if Start() returns an error or doesn't return within 5 s.
func startDeploymentCollector(t *testing.T, c *DeploymentCollector) {
	t.Helper()
	errCh := make(chan error, 1)
	go func() { errCh <- c.Start(t.Context()) }()
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("collector.Start() timed out after 5s")
	}
}

func makeBaseDeployment(name, namespace, uid, rv string) *appsv1.Deployment {
	replicas := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			UID:             types.UID("test-deploy-uid-" + uid),
			ResourceVersion: rv,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": name},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx:1.0"}}},
			},
		},
	}
}

func TestDeploymentCollector_HandleAdd(t *testing.T) {
	dep := makeBaseDeployment("test-dep", "default", "1", "1")
	fakeClient := fake.NewClientset(dep)
	c := newTestDeploymentCollector(fakeClient, []string{""}, nil)
	defer func() { _ = c.Stop() }()

	startDeploymentCollector(t, c)

	assert.Eventually(t, func() bool {
		select {
		case resources := <-c.GetResourceChannel():
			for _, r := range resources {
				if r.EventType == EventTypeAdd && r.ResourceType == Deployment {
					return true
				}
			}
		default:
		}
		return false
	}, 3*time.Second, 50*time.Millisecond)
}

func TestDeploymentCollector_HandleUpdate_MeaningfulChange(t *testing.T) {
	dep := makeBaseDeployment("test-dep", "default", "2", "1")
	fakeClient := fake.NewClientset(dep)
	c := newTestDeploymentCollector(fakeClient, []string{""}, nil)
	defer func() { _ = c.Stop() }()

	startDeploymentCollector(t, c)

	// Drain the initial Add event reliably before triggering the update.
	drainInitialAddEvent(t, c.GetResourceChannel())

	updatedDep := dep.DeepCopy()
	updatedDep.ResourceVersion = "2"
	updatedDep.Spec.Template.Spec.Containers[0].Image = "nginx:2.0"
	_, err := fakeClient.AppsV1().Deployments("default").Update(t.Context(), updatedDep, metav1.UpdateOptions{})
	require.NoError(t, err)

	assert.Eventually(t, func() bool {
		select {
		case resources := <-c.GetResourceChannel():
			for _, r := range resources {
				if r.EventType == EventTypeUpdate && r.ResourceType == Deployment {
					return true
				}
			}
		default:
		}
		return false
	}, 3*time.Second, 50*time.Millisecond)
}

// TestDeploymentCollector_HandleUpdate_NoChange verifies that when ResourceVersion changes
// (the "UnknownChanges" path after our bug-fix) but spec and status are identical,
// no Update event is emitted.
//
// Note: in real Kubernetes, ResourceVersion always increments on every write, even when
// content is unchanged. This test simulates that realistic scenario — it tests the
// UnknownChanges → field-check fall-through path, not the IgnoreChanges short-circuit.
func TestDeploymentCollector_HandleUpdate_NoChange(t *testing.T) {
	dep := makeBaseDeployment("test-dep", "default", "3", "1")
	fakeClient := fake.NewClientset(dep)
	c := newTestDeploymentCollector(fakeClient, []string{""}, nil)
	defer func() { _ = c.Stop() }()

	startDeploymentCollector(t, c)

	// Drain the initial Add event reliably.
	drainInitialAddEvent(t, c.GetResourceChannel())

	// Increment ResourceVersion (as Kubernetes always does on any write),
	// but keep spec and status identical → UnknownChanges → all field checks pass
	// without change → deploymentChanged returns false → no Update event.
	sameDep := dep.DeepCopy()
	sameDep.ResourceVersion = "2"
	_, err := fakeClient.AppsV1().Deployments("default").Update(t.Context(), sameDep, metav1.UpdateOptions{})
	require.NoError(t, err)

	assert.Never(t, func() bool {
		select {
		case resources := <-c.GetResourceChannel():
			for _, r := range resources {
				if r.EventType == EventTypeUpdate {
					return true
				}
			}
		default:
		}
		return false
	}, 500*time.Millisecond, 50*time.Millisecond)
}

func TestDeploymentCollector_HandleDelete(t *testing.T) {
	dep := makeBaseDeployment("test-dep", "default", "4", "1")
	fakeClient := fake.NewClientset(dep)
	c := newTestDeploymentCollector(fakeClient, []string{""}, nil)
	defer func() { _ = c.Stop() }()

	startDeploymentCollector(t, c)

	// Drain the initial Add event reliably.
	drainInitialAddEvent(t, c.GetResourceChannel())

	err := fakeClient.AppsV1().Deployments("default").Delete(t.Context(), "test-dep", metav1.DeleteOptions{})
	require.NoError(t, err)

	assert.Eventually(t, func() bool {
		select {
		case resources := <-c.GetResourceChannel():
			for _, r := range resources {
				if r.EventType == EventTypeDelete && r.ResourceType == Deployment {
					return true
				}
			}
		default:
		}
		return false
	}, 3*time.Second, 50*time.Millisecond)
}

func TestDeploymentCollector_ExcludedResource_IsNotCollected(t *testing.T) {
	dep := makeBaseDeployment("excluded-dep", "default", "5", "1")
	fakeClient := fake.NewClientset(dep)
	excluded := []ExcludedDeployment{{Namespace: "default", Name: "excluded-dep"}}
	c := newTestDeploymentCollector(fakeClient, []string{""}, excluded)
	defer func() { _ = c.Stop() }()

	startDeploymentCollector(t, c)

	assert.Never(t, func() bool {
		select {
		case resources := <-c.GetResourceChannel():
			for _, r := range resources {
				if r.Key == "default/excluded-dep" {
					return true
				}
			}
		default:
		}
		return false
	}, 500*time.Millisecond, 50*time.Millisecond)
}

func TestDeploymentCollector_NamespaceFiltering(t *testing.T) {
	depInNS := makeBaseDeployment("dep-in-ns", "monitored", "6", "1")
	depOutNS := makeBaseDeployment("dep-out-ns", "other", "7", "1")

	fakeClient := fake.NewClientset(depInNS, depOutNS)
	c := newTestDeploymentCollector(fakeClient, []string{"monitored"}, nil)
	defer func() { _ = c.Stop() }()

	startDeploymentCollector(t, c)

	assert.Eventually(t, func() bool {
		select {
		case resources := <-c.GetResourceChannel():
			for _, r := range resources {
				if r.Key == "monitored/dep-in-ns" {
					return true
				}
			}
		default:
		}
		return false
	}, 3*time.Second, 50*time.Millisecond)
}

// --- deploymentChanged unit tests ---
//
// These tests call deploymentChanged() directly — no informer, no goroutines.

func makeTestDeploymentCollector() *DeploymentCollector {
	batchChan := make(chan CollectedResource, 100)
	resourceChan := make(chan []CollectedResource, 100)
	logger := logr.Discard()
	batcher := NewResourcesBatcher(50, 100*time.Millisecond, batchChan, resourceChan, logger)
	return &DeploymentCollector{
		batchChan:    batchChan,
		resourceChan: resourceChan,
		batcher:      batcher,
		stopCh:       make(chan struct{}),
		logger:       logger,
		cDHelper:     ChangeDetectionHelper{logger: logger},
	}
}

func TestDeploymentChanged_SpecChange_ContainerImage_Detected(t *testing.T) {
	c := makeTestDeploymentCollector()
	replicas := int32(1)

	old := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "dep", ResourceVersion: "1"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx:1.0"}}},
			},
		},
	}
	newDep := old.DeepCopy()
	newDep.ResourceVersion = "2"
	newDep.Spec.Template.Spec.Containers[0].Image = "nginx:2.0"

	assert.True(t, c.deploymentChanged(old, newDep))
}

func TestDeploymentChanged_StatusChange_ReadyReplicas_Detected(t *testing.T) {
	c := makeTestDeploymentCollector()
	replicas := int32(2)

	old := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "dep", ResourceVersion: "1"},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 1},
	}
	newDep := old.DeepCopy()
	newDep.ResourceVersion = "2"
	newDep.Status.ReadyReplicas = 2

	assert.True(t, c.deploymentChanged(old, newDep))
}

func TestDeploymentChanged_ReplicaChange_Detected(t *testing.T) {
	c := makeTestDeploymentCollector()
	r1 := int32(1)
	r2 := int32(3)

	old := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "dep", ResourceVersion: "1"},
		Spec:       appsv1.DeploymentSpec{Replicas: &r1},
	}
	newDep := old.DeepCopy()
	newDep.ResourceVersion = "2"
	newDep.Spec.Replicas = &r2

	assert.True(t, c.deploymentChanged(old, newDep))
}

func TestDeploymentChanged_GenerationChange_Detected(t *testing.T) {
	c := makeTestDeploymentCollector()
	replicas := int32(1)

	old := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "dep", ResourceVersion: "1", Generation: 1},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
	}
	newDep := old.DeepCopy()
	newDep.ResourceVersion = "2"
	newDep.Generation = 2

	assert.True(t, c.deploymentChanged(old, newDep))
}

// TestDeploymentChanged_NoChange_Returns_False tests the IgnoreChanges short-circuit:
// when ResourceVersion is identical, objectMetaChanged returns IgnoreChanges and the
// function returns false immediately without running any field checks.
func TestDeploymentChanged_NoChange_Returns_False(t *testing.T) {
	c := makeTestDeploymentCollector()
	replicas := int32(1)

	old := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "dep", ResourceVersion: "1"},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
	}
	// Same ResourceVersion → IgnoreChanges → false
	newDep := old.DeepCopy()

	assert.False(t, c.deploymentChanged(old, newDep))
}

// TestDeploymentChanged_UnknownChanges_NoFieldChange_ReturnsFalse validates the critical
// bug-fix path: when ResourceVersion differs (UnknownChanges) but spec, status, generation,
// and conditions are all identical, deploymentChanged must fall through to field checks and
// ultimately return false — rather than short-circuiting to false without checking, which
// was the bug that caused updates to be silently dropped.
func TestDeploymentChanged_UnknownChanges_NoFieldChange_ReturnsFalse(t *testing.T) {
	c := makeTestDeploymentCollector()
	replicas := int32(1)

	old := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "dep", ResourceVersion: "1"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx"}}},
			},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 1, Replicas: 1},
	}
	// Kubernetes always increments ResourceVersion on any API write, even when content
	// is unchanged. objectMetaChanged → UnknownChanges → field checks run → all identical → false.
	newDep := old.DeepCopy()
	newDep.ResourceVersion = "2"

	assert.False(t, c.deploymentChanged(old, newDep),
		"RV incremented but spec/status identical: UnknownChanges path should return false")
}

func TestDeploymentChanged_LabelChange_Detected(t *testing.T) {
	c := makeTestDeploymentCollector()
	replicas := int32(1)

	old := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "dep", ResourceVersion: "1", Labels: map[string]string{"v": "1"}},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
	}
	newDep := old.DeepCopy()
	newDep.ResourceVersion = "2"
	newDep.Labels = map[string]string{"v": "2"}

	assert.True(t, c.deploymentChanged(old, newDep))
}

func TestDeploymentChanged_AnnotationChange_Detected(t *testing.T) {
	c := makeTestDeploymentCollector()
	replicas := int32(1)

	old := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "dep", ResourceVersion: "1", Annotations: map[string]string{"k": "v1"}},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
	}
	newDep := old.DeepCopy()
	newDep.ResourceVersion = "2"
	newDep.Annotations = map[string]string{"k": "v2"}

	assert.True(t, c.deploymentChanged(old, newDep))
}

func TestDeploymentChanged_SpecDeepEqual_CatchAll(t *testing.T) {
	c := makeTestDeploymentCollector()
	replicas := int32(1)

	old := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "dep", ResourceVersion: "1"},
		Spec: appsv1.DeploymentSpec{
			Replicas:        &replicas,
			MinReadySeconds: 10,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx"}}},
			},
		},
	}
	newDep := old.DeepCopy()
	newDep.ResourceVersion = "2"
	// Change something in spec not explicitly checked
	newDep.Spec.MinReadySeconds = 30

	assert.True(t, c.deploymentChanged(old, newDep), "catch-all DeepEqual on spec should detect MinReadySeconds change")
}
