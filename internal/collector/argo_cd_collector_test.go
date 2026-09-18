package collector

import (
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func argoCDApplicationFixture(namespace, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "argoproj.io/v1alpha1",
			"kind":       "Application",
			"metadata": map[string]interface{}{
				"name":            name,
				"namespace":       namespace,
				"uid":             "app-uid-1",
				"resourceVersion": "4211",
			},
			"spec": map[string]interface{}{
				"project": "default",
				"destination": map[string]interface{}{
					"server":    "https://kubernetes.default.svc",
					"namespace": "guestbook",
				},
			},
			"status": map[string]interface{}{
				"sync":   map[string]interface{}{"status": "Synced"},
				"health": map[string]interface{}{"status": "Healthy"},
			},
		},
	}
}

func argoCDApplicationSetFixture(namespace, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "argoproj.io/v1alpha1",
			"kind":       "ApplicationSet",
			"metadata": map[string]interface{}{
				"name":            name,
				"namespace":       namespace,
				"uid":             "appset-uid-1",
				"resourceVersion": "88",
			},
		},
	}
}

// Applications and ApplicationSets must never share a collector instance: the
// send path labels a whole batch with the resource type of its first element,
// so a collector emitting both kinds would ship ApplicationSets to DAKR
// labelled as Applications.
func TestArgoCDCollector_EachKindHasItsOwnResourceType(t *testing.T) {
	apps := NewArgoCDApplicationCollector(nil, nil, nil, 10, time.Second, logr.Discard(), nil)
	appSets := NewArgoCDApplicationSetCollector(nil, nil, nil, 10, time.Second, logr.Discard(), nil)

	assert.Equal(t, ArgoCDApplication, apps.resourceType)
	assert.Equal(t, "applications", apps.gvr.Resource)

	assert.Equal(t, ArgoCDApplicationSet, appSets.resourceType)
	assert.Equal(t, "applicationsets", appSets.gvr.Resource)

	assert.NotEqual(t, apps.resourceType, appSets.resourceType)
	assert.NotEqual(t, apps.informerKey, appSets.informerKey)
}

// GetType is the key the manager stores collectors under and the value the
// disabled-collectors policy is matched against, so it must equal the
// ResourceType string rather than any other spelling.
func TestArgoCDCollector_GetTypeMatchesResourceTypeString(t *testing.T) {
	apps := NewArgoCDApplicationCollector(nil, nil, nil, 10, time.Second, logr.Discard(), nil)
	appSets := NewArgoCDApplicationSetCollector(nil, nil, nil, 10, time.Second, logr.Discard(), nil)

	assert.Equal(t, "argo_cd_application", apps.GetType())
	assert.Equal(t, ArgoCDApplication.String(), apps.GetType())

	assert.Equal(t, "argo_cd_application_set", appSets.GetType())
	assert.Equal(t, ArgoCDApplicationSet.String(), appSets.GetType())
}

func TestArgoCDCollector_HandleEventEmitsCollectorResourceType(t *testing.T) {
	tests := []struct {
		name         string
		collector    *ArgoCDCollector
		obj          *unstructured.Unstructured
		wantType     ResourceType
		wantKey      string
		wantUID      string
		wantRevision string
	}{
		{
			name:         "application",
			collector:    NewArgoCDApplicationCollector(nil, nil, nil, 10, time.Second, logr.Discard(), nil),
			obj:          argoCDApplicationFixture("argocd", "guestbook"),
			wantType:     ArgoCDApplication,
			wantKey:      "argocd/guestbook",
			wantUID:      "app-uid-1",
			wantRevision: "4211",
		},
		{
			name:         "applicationset",
			collector:    NewArgoCDApplicationSetCollector(nil, nil, nil, 10, time.Second, logr.Discard(), nil),
			obj:          argoCDApplicationSetFixture("argocd", "guestbook-set"),
			wantType:     ArgoCDApplicationSet,
			wantKey:      "argocd/guestbook-set",
			wantUID:      "appset-uid-1",
			wantRevision: "88",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.collector.handleArgoCDEvent(tt.obj, EventTypeUpdate)

			select {
			case res := <-tt.collector.batchChan:
				assert.Equal(t, tt.wantType, res.ResourceType)
				assert.Equal(t, EventTypeUpdate, res.EventType)
				assert.Equal(t, tt.wantKey, res.Key)

				payload, ok := res.Object.(map[string]interface{})
				require.True(t, ok, "payload should be a map, got %T", res.Object)
				assert.Equal(t, tt.obj.GetName(), payload["name"])
				assert.Equal(t, "argocd", payload["namespace"])
				assert.Equal(t, tt.wantUID, payload["uid"])
				assert.Equal(t, tt.wantRevision, payload["resourceVersion"])
				// DAKR derives every stored column from raw, so the whole
				// object has to survive the trip.
				assert.Equal(t, tt.obj, payload["raw"])
			case <-time.After(time.Second):
				t.Fatal("expected a collected resource on the batch channel")
			}
		})
	}
}

func TestArgoCDCollector_DropsExcludedApplication(t *testing.T) {
	c := NewArgoCDApplicationCollector(
		nil, nil,
		[]ExcludedArgoCDApplication{{Namespace: "argocd", Name: "noisy-app"}},
		10, time.Second, logr.Discard(), nil,
	)

	c.handleArgoCDEvent(argoCDApplicationFixture("argocd", "noisy-app"), EventTypeAdd)
	assert.Empty(t, c.batchChan, "an excluded application must not be collected")

	c.handleArgoCDEvent(argoCDApplicationFixture("argocd", "guestbook"), EventTypeAdd)
	assert.Len(t, c.batchChan, 1, "a non-excluded application must still be collected")
}

// Each constructor builds the exclusion map from its own exclusion type; the
// ApplicationSet one is easy to leave empty, and doing so silently disables
// ApplicationSet exclusions.
func TestArgoCDCollector_DropsExcludedApplicationSet(t *testing.T) {
	c := NewArgoCDApplicationSetCollector(
		nil, nil,
		[]ExcludedArgoCDApplicationSet{{Namespace: "argocd", Name: "noisy-set"}},
		10, time.Second, logr.Discard(), nil,
	)

	c.handleArgoCDEvent(argoCDApplicationSetFixture("argocd", "noisy-set"), EventTypeAdd)
	assert.Empty(t, c.batchChan, "an excluded applicationset must not be collected")

	c.handleArgoCDEvent(argoCDApplicationSetFixture("argocd", "wanted-set"), EventTypeAdd)
	assert.Len(t, c.batchChan, 1, "a non-excluded applicationset must still be collected")
}

func TestArgoCDCollector_DropsResourceOutsideTargetNamespaces(t *testing.T) {
	c := NewArgoCDApplicationCollector(
		nil, []string{"argocd"}, nil, 10, time.Second, logr.Discard(), nil,
	)

	c.handleArgoCDEvent(argoCDApplicationFixture("other-ns", "guestbook"), EventTypeAdd)
	assert.Empty(t, c.batchChan, "an application outside the target namespaces must not be collected")

	c.handleArgoCDEvent(argoCDApplicationFixture("argocd", "guestbook"), EventTypeAdd)
	assert.Len(t, c.batchChan, 1, "an application inside the target namespaces must be collected")
}

// An empty target-namespace entry means cluster-wide collection, which is how
// the controller expresses "no namespace restriction".
func TestArgoCDCollector_EmptyNamespaceMeansClusterWide(t *testing.T) {
	c := NewArgoCDApplicationCollector(
		nil, []string{""}, nil, 10, time.Second, logr.Discard(), nil,
	)

	c.handleArgoCDEvent(argoCDApplicationFixture("any-namespace", "guestbook"), EventTypeAdd)
	assert.Len(t, c.batchChan, 1)
}
