package collector

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/cache"
)

func TestStripMetadataTransform_TypedObject(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web",
			Namespace: "default",
			Labels:    map[string]string{"app": "web"},
			Annotations: map[string]string{
				lastAppliedConfigAnnotation: `{"kind":"Pod","spec":{}}`,
				"keep-me":                   "yes",
			},
			ManagedFields: []metav1.ManagedFieldsEntry{
				{Manager: "kubectl", Operation: metav1.ManagedFieldsOperationApply},
			},
		},
	}

	out, err := StripMetadataTransform(pod)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, ok := out.(*corev1.Pod)
	if !ok {
		t.Fatalf("expected *corev1.Pod, got %T", out)
	}

	if got.ManagedFields != nil {
		t.Errorf("managedFields should be nil, got %v", got.ManagedFields)
	}
	if _, exists := got.Annotations[lastAppliedConfigAnnotation]; exists {
		t.Errorf("last-applied-configuration annotation should be stripped")
	}
	if got.Annotations["keep-me"] != "yes" {
		t.Errorf("other annotations must be preserved, got %v", got.Annotations)
	}
	if got.Labels["app"] != "web" {
		t.Errorf("labels must be preserved, got %v", got.Labels)
	}
}

func TestStripMetadataTransform_Unstructured(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]interface{}{
			"name":          "web",
			"managedFields": []interface{}{map[string]interface{}{"manager": "kubectl"}},
			"annotations": map[string]interface{}{
				lastAppliedConfigAnnotation: "{}",
				"keep-me":                   "yes",
			},
		},
	}}

	out, err := StripMetadataTransform(u)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := out.(*unstructured.Unstructured)

	if len(got.GetManagedFields()) != 0 {
		t.Errorf("managedFields should be cleared, got %v", got.GetManagedFields())
	}
	ann := got.GetAnnotations()
	if _, exists := ann[lastAppliedConfigAnnotation]; exists {
		t.Errorf("last-applied-configuration annotation should be stripped")
	}
	if ann["keep-me"] != "yes" {
		t.Errorf("other annotations must be preserved, got %v", ann)
	}
}

func TestStripMetadataTransform_NilAnnotations(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web"}}
	out, err := StripMetadataTransform(pod)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.(*corev1.Pod).ManagedFields != nil {
		t.Errorf("managedFields should be nil")
	}
}

func TestStripMetadataTransform_Tombstone(t *testing.T) {
	tomb := cache.DeletedFinalStateUnknown{
		Key: "default/web",
		Obj: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web"}},
	}
	out, err := StripMetadataTransform(tomb)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := out.(cache.DeletedFinalStateUnknown); !ok {
		t.Errorf("tombstone should be returned unchanged, got %T", out)
	}
}

func TestStripMetadataTransform_NonObject(t *testing.T) {
	// A value that is not a Kubernetes object must be returned untouched.
	out, err := StripMetadataTransform("not-an-object")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "not-an-object" {
		t.Errorf("non-object should be returned unchanged, got %v", out)
	}
}
