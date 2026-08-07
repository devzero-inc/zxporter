package collector

import (
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// newTestKyvernoPolicyCollector builds a minimal collector sufficient to
// exercise processPolicy and handlePolicyEvent without informers.
func newTestKyvernoPolicyCollector() (*KyvernoPolicyCollector, chan CollectedResource) {
	batchChan := make(chan CollectedResource, 10)
	return &KyvernoPolicyCollector{
		batchChan: batchChan,
		logger:    logr.Discard(),
	}, batchChan
}

func clusterPolicyFixture() *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "kyverno.io/v1",
			"kind":       "ClusterPolicy",
			"metadata": map[string]interface{}{
				"name": "require-labels",
				"uid":  "cp-uid-1",
				"labels": map[string]interface{}{
					"app": "kyverno",
				},
			},
			"spec": map[string]interface{}{
				"validationFailureAction": "Audit",
				"background":              true,
				"rules": []interface{}{
					map[string]interface{}{
						"name": "check-team-label",
						"validate": map[string]interface{}{
							"message": "label team is required",
						},
					},
					map[string]interface{}{
						"name": "check-owner-label",
					},
				},
			},
		},
	}
}

func TestKyvernoPolicyCollector_ProcessPolicy(t *testing.T) {
	c, _ := newTestKyvernoPolicyCollector()

	processed := c.processPolicy(clusterPolicyFixture())

	assert.Equal(t, "ClusterPolicy", processed["kind"])
	assert.Equal(t, "require-labels", processed["name"])
	assert.Equal(t, "", processed["namespace"])
	assert.Equal(t, "cp-uid-1", processed["uid"])
	assert.Equal(t, "Audit", processed["validationFailureAction"])
	assert.Equal(t, true, processed["background"])
	assert.Equal(t, 2, processed["ruleCount"])
	assert.Equal(t, []string{"check-team-label", "check-owner-label"}, processed["ruleNames"])
	assert.NotNil(t, processed["raw"])
}

func TestKyvernoPolicyCollector_ProcessPolicy_RuleLevelFailureAction(t *testing.T) {
	c, _ := newTestKyvernoPolicyCollector()

	// Kyverno 1.13+ style: no spec-level validationFailureAction, the rule
	// carries validate.failureAction instead.
	policy := clusterPolicyFixture()
	spec := policy.Object["spec"].(map[string]interface{})
	delete(spec, "validationFailureAction")
	rules := spec["rules"].([]interface{})
	rules[0].(map[string]interface{})["validate"] = map[string]interface{}{
		"failureAction": "Enforce",
	}

	processed := c.processPolicy(policy)

	assert.Equal(t, "Enforce", processed["validationFailureAction"])
}

func TestKyvernoPolicyCollector_ProcessPolicy_DefaultsAndKindFallback(t *testing.T) {
	c, _ := newTestKyvernoPolicyCollector()

	policy := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"metadata": map[string]interface{}{
				"name":      "ns-policy",
				"namespace": "team-a",
				"uid":       "p-uid-1",
			},
			"spec": map[string]interface{}{},
		},
	}

	processed := c.processPolicy(policy)

	// Namespaced object without kind falls back to Policy; background
	// defaults to true when unset.
	assert.Equal(t, "Policy", processed["kind"])
	assert.Equal(t, "team-a", processed["namespace"])
	assert.Equal(t, true, processed["background"])
	assert.Equal(t, 0, processed["ruleCount"])
}

func TestKyvernoPolicyCollector_HandlePolicyEvent(t *testing.T) {
	c, batchChan := newTestKyvernoPolicyCollector()

	c.handlePolicyEvent(clusterPolicyFixture(), EventTypeAdd)

	select {
	case res := <-batchChan:
		assert.Equal(t, KyvernoPolicy, res.ResourceType)
		assert.Equal(t, EventTypeAdd, res.EventType)
		assert.Equal(t, "require-labels", res.Key)
	case <-time.After(time.Second):
		t.Fatal("expected a collected resource on the batch channel")
	}

	// Namespaced policies get namespace/name keys.
	nsPolicy := clusterPolicyFixture()
	nsPolicy.SetKind("Policy")
	nsPolicy.SetNamespace("team-a")
	c.handlePolicyEvent(nsPolicy, EventTypeUpdate)

	res := <-batchChan
	require.Equal(t, "team-a/require-labels", res.Key)
}
