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
	"k8s.io/client-go/kubernetes/fake"
)

// newTestDaemonSetCollector creates a DaemonSetCollector for testing.
func newTestDaemonSetCollector(client *fake.Clientset, namespaces []string, excluded []ExcludedDaemonSet) *DaemonSetCollector {
	return NewDaemonSetCollector(
		client,
		namespaces,
		excluded,
		50,
		100*time.Millisecond,
		logr.Discard(),
		&noopTelemetryLogger{},
	)
}

// startDSCollector starts the collector (via t.Context()) and waits for Start() to return.
// It fails the test immediately if Start() returns an error or doesn't return within 5 s.
func startDSCollector(t *testing.T, c *DaemonSetCollector) {
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

// drainInitialAddEvent waits for and discards the first Add event from the resource channel.
// This clears the initial snapshot batch so subsequent update/delete tests start clean.
// Using assert.Eventually is more reliable than time.Sleep in CI environments.
func drainInitialAddEvent(t *testing.T, ch <-chan []CollectedResource) {
	t.Helper()
	assert.Eventually(t, func() bool {
		select {
		case batch := <-ch:
			for _, r := range batch {
				if r.EventType == EventTypeAdd {
					return true
				}
			}
		default:
		}
		return false
	}, 3*time.Second, 50*time.Millisecond, "timed out waiting for initial Add event during drain")
}

func TestDaemonSetCollector_HandleAdd(t *testing.T) {
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "test-ds",
			Namespace:       "default",
			UID:             "test-uid-1",
			ResourceVersion: "1",
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx"}}},
			},
		},
	}

	fakeClient := fake.NewClientset(ds)
	c := newTestDaemonSetCollector(fakeClient, []string{""}, nil)
	defer func() { _ = c.Stop() }()

	startDSCollector(t, c)

	assert.Eventually(t, func() bool {
		select {
		case resources := <-c.GetResourceChannel():
			for _, r := range resources {
				if r.EventType == EventTypeAdd && r.ResourceType == DaemonSet {
					return true
				}
			}
		default:
		}
		return false
	}, 3*time.Second, 50*time.Millisecond)
}

func TestDaemonSetCollector_HandleUpdate_MeaningfulChange(t *testing.T) {
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "test-ds",
			Namespace:       "default",
			UID:             "test-uid-2",
			ResourceVersion: "1",
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx:1.0"}}},
			},
		},
	}

	fakeClient := fake.NewClientset(ds)
	c := newTestDaemonSetCollector(fakeClient, []string{""}, nil)
	defer func() { _ = c.Stop() }()

	startDSCollector(t, c)

	// Drain the initial Add event reliably before triggering the update.
	drainInitialAddEvent(t, c.GetResourceChannel())

	// Update with changed image and incremented ResourceVersion.
	updatedDS := ds.DeepCopy()
	updatedDS.ResourceVersion = "2"
	updatedDS.Spec.Template.Spec.Containers[0].Image = "nginx:2.0"
	_, err := fakeClient.AppsV1().DaemonSets("default").Update(t.Context(), updatedDS, metav1.UpdateOptions{})
	require.NoError(t, err)

	assert.Eventually(t, func() bool {
		select {
		case resources := <-c.GetResourceChannel():
			for _, r := range resources {
				if r.EventType == EventTypeUpdate && r.ResourceType == DaemonSet {
					return true
				}
			}
		default:
		}
		return false
	}, 3*time.Second, 50*time.Millisecond)
}

// TestDaemonSetCollector_HandleUpdate_NoChange verifies that when ResourceVersion changes
// (the "UnknownChanges" path after our bug-fix) but spec and status are identical,
// no Update event is emitted.
//
// Note: in real Kubernetes, ResourceVersion always increments on every write, even when
// content is unchanged. This test simulates that realistic scenario — it tests the
// UnknownChanges → field-check fall-through path, not the IgnoreChanges short-circuit.
func TestDaemonSetCollector_HandleUpdate_NoChange(t *testing.T) {
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "test-ds",
			Namespace:       "default",
			UID:             "test-uid-3",
			ResourceVersion: "1",
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx"}}},
			},
		},
		Status: appsv1.DaemonSetStatus{NumberReady: 3, DesiredNumberScheduled: 3},
	}

	fakeClient := fake.NewClientset(ds)
	c := newTestDaemonSetCollector(fakeClient, []string{""}, nil)
	defer func() { _ = c.Stop() }()

	startDSCollector(t, c)
	drainInitialAddEvent(t, c.GetResourceChannel())

	// Increment ResourceVersion (as Kubernetes always does on any write),
	// but keep spec and status identical → UnknownChanges → all field checks pass
	// without change → daemonSetChanged returns false → no Update event.
	sameDS := ds.DeepCopy()
	sameDS.ResourceVersion = "2"
	_, err := fakeClient.AppsV1().DaemonSets("default").Update(t.Context(), sameDS, metav1.UpdateOptions{})
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

func TestDaemonSetCollector_HandleDelete(t *testing.T) {
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "test-ds",
			Namespace:       "default",
			UID:             "test-uid-4",
			ResourceVersion: "1",
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx"}}},
			},
		},
	}

	fakeClient := fake.NewClientset(ds)
	c := newTestDaemonSetCollector(fakeClient, []string{""}, nil)
	defer func() { _ = c.Stop() }()

	startDSCollector(t, c)
	drainInitialAddEvent(t, c.GetResourceChannel())

	err := fakeClient.AppsV1().DaemonSets("default").Delete(t.Context(), "test-ds", metav1.DeleteOptions{})
	require.NoError(t, err)

	assert.Eventually(t, func() bool {
		select {
		case resources := <-c.GetResourceChannel():
			for _, r := range resources {
				if r.EventType == EventTypeDelete && r.ResourceType == DaemonSet {
					return true
				}
			}
		default:
		}
		return false
	}, 3*time.Second, 50*time.Millisecond)
}

func TestDaemonSetCollector_ExcludedResource_IsNotCollected(t *testing.T) {
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "excluded-ds",
			Namespace:       "default",
			UID:             "test-uid-5",
			ResourceVersion: "1",
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx"}}},
			},
		},
	}

	fakeClient := fake.NewClientset(ds)
	excluded := []ExcludedDaemonSet{{Namespace: "default", Name: "excluded-ds"}}
	c := newTestDaemonSetCollector(fakeClient, []string{""}, excluded)
	defer func() { _ = c.Stop() }()

	startDSCollector(t, c)

	assert.Never(t, func() bool {
		select {
		case resources := <-c.GetResourceChannel():
			for _, r := range resources {
				if r.Key == "default/excluded-ds" {
					return true
				}
			}
		default:
		}
		return false
	}, 500*time.Millisecond, 50*time.Millisecond)
}

func TestDaemonSetCollector_NamespaceFiltering(t *testing.T) {
	dsInNamespace := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "ds-in-ns",
			Namespace:       "monitored",
			UID:             "test-uid-6",
			ResourceVersion: "1",
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx"}}},
			},
		},
	}
	dsOutNamespace := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "ds-out-ns",
			Namespace:       "other",
			UID:             "test-uid-7",
			ResourceVersion: "1",
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx"}}},
			},
		},
	}

	fakeClient := fake.NewClientset(dsInNamespace, dsOutNamespace)
	// Single-namespace mode: only "monitored" namespace is watched via the informer factory.
	c := newTestDaemonSetCollector(fakeClient, []string{"monitored"}, nil)
	defer func() { _ = c.Stop() }()

	startDSCollector(t, c)

	assert.Eventually(t, func() bool {
		select {
		case resources := <-c.GetResourceChannel():
			for _, r := range resources {
				if r.Key == "monitored/ds-in-ns" {
					return true
				}
			}
		default:
		}
		return false
	}, 3*time.Second, 50*time.Millisecond)

	// Ensure the out-of-namespace DaemonSet was never emitted.
	assert.Never(t, func() bool {
		select {
		case resources := <-c.GetResourceChannel():
			for _, r := range resources {
				if r.Key == "other/ds-out-ns" {
					return true
				}
			}
		default:
		}
		return false
	}, 300*time.Millisecond, 50*time.Millisecond)
}

// --- daemonSetChanged unit tests ---
//
// These tests call daemonSetChanged() directly — no informer, no goroutines.

func makeTestDaemonSetCollector() *DaemonSetCollector {
	batchChan := make(chan CollectedResource, 100)
	resourceChan := make(chan []CollectedResource, 100)
	logger := logr.Discard()
	batcher := NewResourcesBatcher(50, 100*time.Millisecond, batchChan, resourceChan, logger)
	return &DaemonSetCollector{
		batchChan:    batchChan,
		resourceChan: resourceChan,
		batcher:      batcher,
		stopCh:       make(chan struct{}),
		logger:       logger,
		cDHelper:     ChangeDetectionHelper{logger: logger},
	}
}

func TestDaemonSetChanged_SpecChange_ContainerImage_Detected(t *testing.T) {
	c := makeTestDaemonSetCollector()

	old := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ds", ResourceVersion: "1"},
		Spec: appsv1.DaemonSetSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx:1.0"}}},
			},
		},
	}
	newDS := old.DeepCopy()
	newDS.ResourceVersion = "2"
	newDS.Spec.Template.Spec.Containers[0].Image = "nginx:2.0"

	assert.True(t, c.daemonSetChanged(old, newDS), "spec image change should be detected")
}

func TestDaemonSetChanged_StatusChange_NumberReady_Detected(t *testing.T) {
	c := makeTestDaemonSetCollector()

	old := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ds", ResourceVersion: "1"},
		Status:     appsv1.DaemonSetStatus{NumberReady: 1},
	}
	newDS := old.DeepCopy()
	newDS.ResourceVersion = "2"
	newDS.Status.NumberReady = 2

	assert.True(t, c.daemonSetChanged(old, newDS), "status NumberReady change should be detected")
}

func TestDaemonSetChanged_GenerationChange_Detected(t *testing.T) {
	c := makeTestDaemonSetCollector()

	old := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ds", ResourceVersion: "1", Generation: 1},
	}
	newDS := old.DeepCopy()
	newDS.ResourceVersion = "2"
	newDS.Generation = 2

	assert.True(t, c.daemonSetChanged(old, newDS), "generation change should be detected")
}

// TestDaemonSetChanged_NoChange_Returns_False tests the IgnoreChanges short-circuit:
// when ResourceVersion is identical, objectMetaChanged returns IgnoreChanges and the
// function returns false immediately without running any field checks.
func TestDaemonSetChanged_NoChange_Returns_False(t *testing.T) {
	c := makeTestDaemonSetCollector()

	old := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ds", ResourceVersion: "1"},
		Spec: appsv1.DaemonSetSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx"}}},
			},
		},
		Status: appsv1.DaemonSetStatus{NumberReady: 1},
	}
	newDS := old.DeepCopy() // identical ResourceVersion "1"

	assert.False(t, c.daemonSetChanged(old, newDS), "same ResourceVersion → IgnoreChanges → false")
}

// TestDaemonSetChanged_UnknownChanges_NoFieldChange_ReturnsFalse validates the critical
// bug-fix path: when ResourceVersion differs (UnknownChanges) but spec, status, generation,
// and conditions are all identical, daemonSetChanged must fall through to field checks and
// ultimately return false — rather than short-circuiting to false without checking, which
// was the bug that caused updates to be silently dropped.
func TestDaemonSetChanged_UnknownChanges_NoFieldChange_ReturnsFalse(t *testing.T) {
	c := makeTestDaemonSetCollector()

	old := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ds", ResourceVersion: "1"},
		Spec: appsv1.DaemonSetSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx"}}},
			},
		},
		Status: appsv1.DaemonSetStatus{
			NumberReady:            3,
			DesiredNumberScheduled: 3,
		},
	}
	// Kubernetes always increments ResourceVersion on any API write, even when content
	// is unchanged (e.g. a metadata-only patch that races with our cache snapshot).
	// objectMetaChanged → UnknownChanges → field checks run → all identical → false.
	newDS := old.DeepCopy()
	newDS.ResourceVersion = "2"

	assert.False(t, c.daemonSetChanged(old, newDS),
		"RV incremented but spec/status identical: UnknownChanges path should return false")
}

func TestDaemonSetChanged_LabelChange_Detected(t *testing.T) {
	c := makeTestDaemonSetCollector()

	old := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ds", ResourceVersion: "1", Labels: map[string]string{"a": "1"}},
	}
	newDS := old.DeepCopy()
	newDS.ResourceVersion = "2"
	newDS.Labels = map[string]string{"a": "2"}

	assert.True(t, c.daemonSetChanged(old, newDS), "label change should be detected via objectMetaChanged")
}

func TestDaemonSetChanged_AnnotationChange_Detected(t *testing.T) {
	c := makeTestDaemonSetCollector()

	old := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ds", ResourceVersion: "1", Annotations: map[string]string{"x": "y"}},
	}
	newDS := old.DeepCopy()
	newDS.ResourceVersion = "2"
	newDS.Annotations = map[string]string{"x": "z"}

	assert.True(t, c.daemonSetChanged(old, newDS), "annotation change should be detected via objectMetaChanged")
}

// TestDaemonSetChanged_SpecDeepEqual_CatchAll verifies the reflect.DeepEqual(Spec) catch-all
// detects spec changes that are not covered by the explicit field-by-field checks above it
// (e.g. MinReadySeconds, node selector, tolerations, etc.).
func TestDaemonSetChanged_SpecDeepEqual_CatchAll(t *testing.T) {
	c := makeTestDaemonSetCollector()

	old := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ds", ResourceVersion: "1"},
		Spec: appsv1.DaemonSetSpec{
			MinReadySeconds: 10,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx"}}},
			},
		},
	}
	newDS := old.DeepCopy()
	newDS.ResourceVersion = "2"
	// MinReadySeconds is not checked explicitly — only caught by DeepEqual.
	newDS.Spec.MinReadySeconds = 20

	assert.True(t, c.daemonSetChanged(old, newDS), "catch-all DeepEqual should detect MinReadySeconds change")
}
