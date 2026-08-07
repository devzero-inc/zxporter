package collector

import (
	"fmt"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func newTestKyvernoPolicyReportCollector() (*KyvernoPolicyReportCollector, chan CollectedResource) {
	batchChan := make(chan CollectedResource, 10)
	return &KyvernoPolicyReportCollector{
		batchChan:   batchChan,
		logger:      logr.Discard(),
		lastEmitted: make(map[string]uint64),
	}, batchChan
}

func policyReportFixture(failCount int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "wgpolicyk8s.io/v1alpha2",
			"kind":       "PolicyReport",
			"metadata": map[string]interface{}{
				"name":      "polr-pod-nginx",
				"namespace": "default",
				"uid":       "pr-uid-1",
			},
			"scope": map[string]interface{}{
				"kind":      "Pod",
				"name":      "nginx",
				"namespace": "default",
			},
			"summary": map[string]interface{}{
				"pass":  int64(2),
				"fail":  failCount,
				"warn":  int64(0),
				"error": int64(0),
				"skip":  int64(1),
			},
			"results": []interface{}{
				map[string]interface{}{
					"policy":   "require-labels",
					"rule":     "check-team-label",
					"result":   "fail",
					"severity": "medium",
					"message":  "label team is required",
					"resources": []interface{}{
						map[string]interface{}{
							"kind": "Pod", "name": "nginx", "namespace": "default",
						},
					},
				},
			},
		},
	}
}

func TestKyvernoPolicyReportCollector_ProcessReport(t *testing.T) {
	c, _ := newTestKyvernoPolicyReportCollector()

	processed := c.processReport(policyReportFixture(1))

	assert.Equal(t, "PolicyReport", processed["kind"])
	assert.Equal(t, "polr-pod-nginx", processed["name"])
	assert.Equal(t, "default", processed["namespace"])
	summary := processed["summary"].(map[string]interface{})
	assert.Equal(t, int64(1), summary["fail"])
	scope := processed["scope"].(map[string]interface{})
	assert.Equal(t, "Pod", scope["kind"])
	results := processed["results"].([]interface{})
	require.Len(t, results, 1)
	first := results[0].(map[string]interface{})
	assert.Equal(t, "require-labels", first["policy"])
	assert.Equal(t, "fail", first["result"])
	assert.Equal(t, 1, processed["totalResults"])

	// Raw object must not ride report payloads (churn-heavy resource).
	_, hasRaw := processed["raw"]
	assert.False(t, hasRaw)
}

func TestKyvernoPolicyReportCollector_ProcessReport_CapsResults(t *testing.T) {
	c, _ := newTestKyvernoPolicyReportCollector()

	report := policyReportFixture(60)
	results := make([]interface{}, 0, 60)
	for i := 0; i < 60; i++ {
		results = append(results, map[string]interface{}{
			"policy": "require-labels",
			"rule":   fmt.Sprintf("rule-%d", i),
			"result": "fail",
		})
	}
	report.Object["results"] = results

	processed := c.processReport(report)

	assert.Len(t, processed["results"], maxPolicyReportResults)
	assert.Equal(t, 60, processed["totalResults"])
}

func TestKyvernoPolicyReportCollector_DropsNoOpUpdates(t *testing.T) {
	c, batchChan := newTestKyvernoPolicyReportCollector()

	c.handleReportEvent(policyReportFixture(1), EventTypeAdd)
	require.Len(t, batchChan, 1)
	<-batchChan

	// Kyverno rewrites reports on every scan; identical content must not re-emit.
	c.handleReportEvent(policyReportFixture(1), EventTypeUpdate)
	assert.Len(t, batchChan, 0)

	// A real change emits again.
	c.handleReportEvent(policyReportFixture(3), EventTypeUpdate)
	require.Len(t, batchChan, 1)
	res := <-batchChan
	assert.Equal(t, KyvernoPolicyReport, res.ResourceType)
	assert.Equal(t, EventTypeUpdate, res.EventType)
	assert.Equal(t, "default/polr-pod-nginx", res.Key)

	// Delete always emits and clears the dedup entry, so a re-created report
	// with identical content emits its add.
	c.handleReportEvent(policyReportFixture(3), EventTypeDelete)
	require.Len(t, batchChan, 1)
	<-batchChan
	c.handleReportEvent(policyReportFixture(3), EventTypeAdd)
	assert.Len(t, batchChan, 1)
}
