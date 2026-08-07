package collector

import (
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// migPartedConfigYAML is a trimmed version of the GPU operator's
// default-mig-parted-config payload.
const migPartedConfigYAML = `version: v1
mig-configs:
  all-disabled:
    - devices: all
      mig-enabled: false
  all-1g.5gb:
    - devices: all
      mig-enabled: true
      mig-devices:
        1g.5gb: 7
  custom-config:
    - devices: [0, 1]
      mig-enabled: true
      mig-devices:
        1g.5gb: 2
        2g.10gb: 1
        3g.20gb: 1
`

func newTestMigPartedConfigCollector() (*MigPartedConfigCollector, chan CollectedResource) {
	batchChan := make(chan CollectedResource, 10)
	return &MigPartedConfigCollector{
		configMapName: DefaultMigPartedConfigMapName,
		namespace:     DefaultMigPartedConfigMapNamespace,
		batchChan:     batchChan,
		logger:        logr.Discard(),
	}, batchChan
}

func migPartedConfigMapFixture() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      DefaultMigPartedConfigMapName,
			Namespace: DefaultMigPartedConfigMapNamespace,
			UID:       types.UID("cm-uid-1"),
		},
		Data: map[string]string{
			"config.yaml": migPartedConfigYAML,
		},
	}
}

func TestMigPartedConfigCollector_ProcessConfigMap(t *testing.T) {
	c, _ := newTestMigPartedConfigCollector()

	processed := c.processConfigMap(migPartedConfigMapFixture())

	assert.Equal(t, DefaultMigPartedConfigMapName, processed["name"])
	assert.Equal(t, DefaultMigPartedConfigMapNamespace, processed["namespace"])
	assert.Equal(t, "cm-uid-1", processed["uid"])
	assert.Equal(t, "config.yaml", processed["configKey"])
	assert.Equal(t, "v1", processed["version"])
	assert.Equal(t, migPartedConfigYAML, processed["raw"])

	migConfigs, ok := processed["migConfigs"].(map[string]interface{})
	require.True(t, ok, "migConfigs should parse into a map, got %T", processed["migConfigs"])
	assert.Len(t, migConfigs, 3)
	assert.Contains(t, migConfigs, "all-1g.5gb")
	assert.Contains(t, migConfigs, "all-disabled")
	assert.Contains(t, migConfigs, "custom-config")

	_, hasParseError := processed["parseError"]
	assert.False(t, hasParseError)
}

func TestMigPartedConfigCollector_ProcessConfigMap_AlternateKey(t *testing.T) {
	c, _ := newTestMigPartedConfigCollector()

	cm := migPartedConfigMapFixture()
	cm.Data = map[string]string{"custom-key.yaml": migPartedConfigYAML}

	processed := c.processConfigMap(cm)

	assert.Equal(t, "custom-key.yaml", processed["configKey"])
	assert.Equal(t, "v1", processed["version"])
}

func TestMigPartedConfigCollector_ProcessConfigMap_MultiKeyFallbackIsDeterministic(t *testing.T) {
	c, _ := newTestMigPartedConfigCollector()

	// Map iteration order is randomized; with several data keys the fallback
	// must always prefer the lexicographically first yaml-suffixed key rather
	// than whatever iteration yields (e.g. a checksum or README entry).
	cm := migPartedConfigMapFixture()
	cm.Data = map[string]string{
		"README.md":     "not yaml",
		"checksum":      "abc123",
		"custom.yaml":   migPartedConfigYAML,
		"z-config.yaml": "version: v9",
	}

	for i := 0; i < 20; i++ {
		processed := c.processConfigMap(cm)
		assert.Equal(t, "custom.yaml", processed["configKey"])
		assert.Equal(t, "v1", processed["version"])
	}
}

func TestMigPartedConfigCollector_ProcessConfigMap_ParseError(t *testing.T) {
	c, _ := newTestMigPartedConfigCollector()

	cm := migPartedConfigMapFixture()
	cm.Data = map[string]string{"config.yaml": "{not: valid: yaml: ["}

	processed := c.processConfigMap(cm)

	// The payload still ships with the raw content so dakr can store it.
	assert.NotEmpty(t, processed["parseError"])
	assert.Equal(t, "{not: valid: yaml: [", processed["raw"])
	assert.Nil(t, processed["migConfigs"])
}

func TestMigPartedConfigCollector_HandleConfigMapEvent(t *testing.T) {
	c, batchChan := newTestMigPartedConfigCollector()

	c.handleConfigMapEvent(migPartedConfigMapFixture(), EventTypeAdd)

	select {
	case res := <-batchChan:
		assert.Equal(t, MigPartedConfig, res.ResourceType)
		assert.Equal(t, EventTypeAdd, res.EventType)
		assert.Equal(t, "gpu-operator/default-mig-parted-config", res.Key)
	case <-time.After(time.Second):
		t.Fatal("expected a collected resource on the batch channel")
	}
}

func TestMigPartedConfigCollector_DefaultsAppliedByConstructor(t *testing.T) {
	c := NewMigPartedConfigCollector(nil, "", "", 10, time.Second, logr.Discard(), nil)
	assert.Equal(t, DefaultMigPartedConfigMapName, c.configMapName)
	assert.Equal(t, DefaultMigPartedConfigMapNamespace, c.namespace)

	custom := NewMigPartedConfigCollector(nil, "my-config", "my-ns", 10, time.Second, logr.Discard(), nil)
	assert.Equal(t, "my-config", custom.configMapName)
	assert.Equal(t, "my-ns", custom.namespace)
}
