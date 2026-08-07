package collector

import (
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func newTestGatekeeperConstraintTemplateCollector() (*GatekeeperConstraintTemplateCollector, chan CollectedResource) {
	batchChan := make(chan CollectedResource, 10)
	return &GatekeeperConstraintTemplateCollector{
		batchChan: batchChan,
		logger:    logr.Discard(),
	}, batchChan
}

func constraintTemplateFixture() *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "templates.gatekeeper.sh/v1",
			"kind":       "ConstraintTemplate",
			"metadata": map[string]interface{}{
				"name": "k8srequiredlabels",
				"uid":  "ct-uid-1",
			},
			"spec": map[string]interface{}{
				"crd": map[string]interface{}{
					"spec": map[string]interface{}{
						"names": map[string]interface{}{
							"kind": "K8sRequiredLabels",
						},
					},
				},
				"targets": []interface{}{
					map[string]interface{}{
						"target": "admission.k8s.gatekeeper.sh",
						"rego":   "package k8srequiredlabels ...",
					},
				},
			},
			"status": map[string]interface{}{
				"created": true,
			},
		},
	}
}

func TestGatekeeperConstraintTemplateCollector_ProcessTemplate(t *testing.T) {
	c, _ := newTestGatekeeperConstraintTemplateCollector()

	processed := c.processTemplate(constraintTemplateFixture())

	assert.Equal(t, "k8srequiredlabels", processed["name"])
	assert.Equal(t, "ct-uid-1", processed["uid"])
	assert.Equal(t, "K8sRequiredLabels", processed["crdKind"])
	assert.Equal(t, []string{"admission.k8s.gatekeeper.sh"}, processed["targets"])
	assert.Equal(t, true, processed["created"])
	assert.NotNil(t, processed["raw"])
}

func TestGatekeeperConstraintTemplateCollector_HandleTemplateEvent(t *testing.T) {
	c, batchChan := newTestGatekeeperConstraintTemplateCollector()

	c.handleTemplateEvent(constraintTemplateFixture(), EventTypeAdd)

	select {
	case res := <-batchChan:
		assert.Equal(t, GatekeeperConstraintTemplate, res.ResourceType)
		assert.Equal(t, EventTypeAdd, res.EventType)
		assert.Equal(t, "constrainttemplates/k8srequiredlabels", res.Key)
	case <-time.After(time.Second):
		t.Fatal("expected a collected resource on the batch channel")
	}
}
