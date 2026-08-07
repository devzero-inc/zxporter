package collector

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	discoveryfake "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/dynamic/dynamicinformer"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

func newTestGatekeeperConstraintCollector() (*GatekeeperConstraintCollector, chan CollectedResource) {
	batchChan := make(chan CollectedResource, 30)
	return &GatekeeperConstraintCollector{
		batchChan:   batchChan,
		logger:      logr.Discard(),
		stopCh:      make(chan struct{}),
		watchedGVRs: make(map[schema.GroupVersionResource]bool),
	}, batchChan
}

func constraintFixture(violations int) *unstructured.Unstructured {
	violationList := make([]interface{}, 0, violations)
	for i := 0; i < violations; i++ {
		violationList = append(violationList, map[string]interface{}{
			"kind":              "Namespace",
			"name":              "ns-bad",
			"message":           "you must provide labels",
			"enforcementAction": "deny",
		})
	}
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "constraints.gatekeeper.sh/v1beta1",
			"kind":       "K8sRequiredLabels",
			"metadata": map[string]interface{}{
				"name": "ns-must-have-team",
				"uid":  "c-uid-1",
			},
			"spec": map[string]interface{}{
				"match": map[string]interface{}{
					"kinds": []interface{}{
						map[string]interface{}{
							"apiGroups": []interface{}{""},
							"kinds":     []interface{}{"Namespace"},
						},
					},
				},
				"parameters": map[string]interface{}{
					"labels": []interface{}{"team"},
				},
			},
			"status": map[string]interface{}{
				"auditTimestamp":  "2026-08-06T00:00:00Z",
				"totalViolations": int64(violations),
				"violations":      violationList,
			},
		},
	}
}

func TestGatekeeperConstraintCollector_ProcessConstraint(t *testing.T) {
	c, _ := newTestGatekeeperConstraintCollector()

	processed := c.processConstraint(constraintFixture(2))

	assert.Equal(t, "K8sRequiredLabels", processed["kind"])
	assert.Equal(t, "ns-must-have-team", processed["name"])
	// Gatekeeper defaults enforcementAction to deny when the spec omits it.
	assert.Equal(t, "deny", processed["enforcementAction"])
	assert.Equal(t, int64(2), processed["totalViolations"])
	assert.Equal(t, "2026-08-06T00:00:00Z", processed["auditTimestamp"])
	assert.Len(t, processed["violations"], 2)
	assert.NotNil(t, processed["match"])
	assert.NotNil(t, processed["parameters"])
}

func TestGatekeeperConstraintCollector_ProcessConstraint_CapsViolations(t *testing.T) {
	c, _ := newTestGatekeeperConstraintCollector()

	processed := c.processConstraint(constraintFixture(maxConstraintViolations + 10))

	assert.Len(t, processed["violations"], maxConstraintViolations)
	assert.Equal(t, int64(maxConstraintViolations+10), processed["totalViolations"])
}

func TestGatekeeperConstraintCollector_HandleConstraintEvent(t *testing.T) {
	c, batchChan := newTestGatekeeperConstraintCollector()

	c.handleConstraintEvent(constraintFixture(1), EventTypeUpdate)

	select {
	case res := <-batchChan:
		assert.Equal(t, GatekeeperConstraint, res.ResourceType)
		assert.Equal(t, EventTypeUpdate, res.EventType)
		assert.Equal(t, "k8srequiredlabels/ns-must-have-team", res.Key)
	case <-time.After(time.Second):
		t.Fatal("expected a collected resource on the batch channel")
	}
}

// TestGatekeeperConstraintCollector_DiscoverAndWatch_PicksUpNewKinds verifies
// that re-running discovery registers informers for constraint kinds created
// after startup (each new ConstraintTemplate creates a new CRD) and skips
// subresources and already-watched kinds.
func TestGatekeeperConstraintCollector_DiscoverAndWatch_PicksUpNewKinds(t *testing.T) {
	requiredLabelsGVR := schema.GroupVersionResource{
		Group: "constraints.gatekeeper.sh", Version: "v1beta1", Resource: "k8srequiredlabels",
	}
	allowedReposGVR := schema.GroupVersionResource{
		Group: "constraints.gatekeeper.sh", Version: "v1beta1", Resource: "k8sallowedrepos",
	}

	scheme := runtime.NewScheme()
	dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			requiredLabelsGVR: "K8sRequiredLabelsList",
			allowedReposGVR:   "K8sAllowedReposList",
		},
	)

	discoveryClient := &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{}}
	discoveryClient.Resources = []*metav1.APIResourceList{
		{
			GroupVersion: gatekeeperConstraintsGroupVersion,
			APIResources: []metav1.APIResource{
				{Name: "k8srequiredlabels", Kind: "K8sRequiredLabels", Namespaced: false},
				{Name: "k8srequiredlabels/status", Kind: "K8sRequiredLabels", Namespaced: false},
			},
		},
	}

	c, _ := newTestGatekeeperConstraintCollector()
	c.dynamicClient = dynamicClient
	c.discoveryClient = discoveryClient
	c.factory = dynamicinformer.NewFilteredDynamicSharedInformerFactory(dynamicClient, 0, "", nil)
	defer close(c.stopCh)

	ctx := context.Background()
	require.NoError(t, c.discoverAndWatch(ctx))

	c.watchedGVRsMu.Lock()
	assert.Equal(t, map[schema.GroupVersionResource]bool{requiredLabelsGVR: true}, c.watchedGVRs)
	c.watchedGVRsMu.Unlock()

	// A new template appears: its constraint kind joins the discovery list.
	discoveryClient.Resources[0].APIResources = append(discoveryClient.Resources[0].APIResources,
		metav1.APIResource{Name: "k8sallowedrepos", Kind: "K8sAllowedRepos", Namespaced: false},
	)

	require.NoError(t, c.discoverAndWatch(ctx))

	c.watchedGVRsMu.Lock()
	assert.Len(t, c.watchedGVRs, 2)
	assert.True(t, c.watchedGVRs[allowedReposGVR])
	c.watchedGVRsMu.Unlock()

	// Re-running discovery with no changes stays stable.
	require.NoError(t, c.discoverAndWatch(ctx))
	c.watchedGVRsMu.Lock()
	assert.Len(t, c.watchedGVRs, 2)
	c.watchedGVRsMu.Unlock()
}
