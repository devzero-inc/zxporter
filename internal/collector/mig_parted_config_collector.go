// internal/collector/mig_parted_config_collector.go
package collector

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	telemetry_logger "github.com/devzero-inc/zxporter/internal/logger"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/yaml"
)

const (
	// DefaultMigPartedConfigMapName is the ConfigMap the NVIDIA GPU operator
	// creates for mig-parted by default.
	DefaultMigPartedConfigMapName = "default-mig-parted-config"

	// DefaultMigPartedConfigMapNamespace is the GPU operator's default namespace.
	DefaultMigPartedConfigMapNamespace = "gpu-operator"

	// migPartedConfigResyncPeriod re-delivers the ConfigMap periodically so dakr
	// keeps a fresh last-seen timestamp even when the config never changes.
	migPartedConfigResyncPeriod = 1 * time.Hour
)

// MigPartedConfigCollector watches the NVIDIA mig-parted ConfigMap, which
// declares the available MIG partitioning profiles, and emits it as
// RESOURCE_TYPE_MIG_PARTED_CONFIG. The applied per-node state rides node
// labels (nvidia.com/mig.config, nvidia.com/mig.config.state) through the
// regular Node collector.
type MigPartedConfigCollector struct {
	client          kubernetes.Interface
	configMapName   string
	namespace       string
	batchChan       chan CollectedResource
	resourceChan    chan []CollectedResource
	batcher         *ResourcesBatcher
	stopCh          chan struct{}
	logger          logr.Logger
	telemetryLogger telemetry_logger.Logger
	mu              sync.Mutex
	// chanMu guards batchChan against the send-after-close shutdown race:
	// informer callbacks RLock around sends, Stop write-locks before closing.
	chanMu  sync.RWMutex
	stopped bool
}

// NewMigPartedConfigCollector creates a new collector for the mig-parted
// ConfigMap. Empty name/namespace fall back to the GPU operator defaults.
func NewMigPartedConfigCollector(
	client kubernetes.Interface,
	configMapName string,
	namespace string,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
) *MigPartedConfigCollector {
	if configMapName == "" {
		configMapName = DefaultMigPartedConfigMapName
	}
	if namespace == "" {
		namespace = DefaultMigPartedConfigMapNamespace
	}

	batchChan := make(chan CollectedResource, 10)
	resourceChan := make(chan []CollectedResource, 10)
	batcher := NewResourcesBatcher(maxBatchSize, maxBatchTime, batchChan, resourceChan, logger)

	return &MigPartedConfigCollector{
		client:          client,
		configMapName:   configMapName,
		namespace:       namespace,
		batchChan:       batchChan,
		resourceChan:    resourceChan,
		batcher:         batcher,
		stopCh:          make(chan struct{}),
		logger:          logger.WithName("mig-parted-config-collector"),
		telemetryLogger: telemetryLogger,
	}
}

// Start begins watching the mig-parted ConfigMap.
func (c *MigPartedConfigCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting mig-parted config collector",
		"configMap", c.configMapName, "namespace", c.namespace)

	factory := informers.NewSharedInformerFactoryWithOptions(
		c.client,
		migPartedConfigResyncPeriod,
		informers.WithNamespace(c.namespace),
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.FieldSelector = fields.OneTermEqualSelector("metadata.name", c.configMapName).String()
		}),
	)
	informer := factory.Core().V1().ConfigMaps().Informer()

	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if cm, ok := obj.(*corev1.ConfigMap); ok {
				c.handleConfigMapEvent(cm, EventTypeAdd)
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if cm, ok := newObj.(*corev1.ConfigMap); ok {
				c.handleConfigMapEvent(cm, EventTypeUpdate)
			}
		},
		DeleteFunc: func(obj interface{}) {
			cm, ok := obj.(*corev1.ConfigMap)
			if !ok {
				if tombstone, ok2 := obj.(cache.DeletedFinalStateUnknown); ok2 {
					cm, ok = tombstone.Obj.(*corev1.ConfigMap)
				}
			}
			if ok {
				c.handleConfigMapEvent(cm, EventTypeDelete)
			}
		},
	})
	if err != nil {
		return fmt.Errorf("add event handler: %w", err)
	}

	stopCh := c.stopCh
	factory.Start(stopCh)

	syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if !cache.WaitForCacheSync(syncCtx.Done(), informer.HasSynced) {
		return fmt.Errorf("timeout waiting for mig-parted ConfigMap cache to sync")
	}

	c.logger.Info("mig-parted config informer started and synced")
	c.batcher.start()

	go func() {
		select {
		case <-ctx.Done():
			c.Stop() //nolint:errcheck
		case <-stopCh:
		}
	}()

	return nil
}

// handleConfigMapEvent processes a mig-parted ConfigMap event.
func (c *MigPartedConfigCollector) handleConfigMapEvent(cm *corev1.ConfigMap, eventType EventType) {
	c.chanMu.RLock()
	defer c.chanMu.RUnlock()
	if c.stopped {
		return
	}

	c.batchChan <- CollectedResource{
		ResourceType: MigPartedConfig,
		Object:       c.processConfigMap(cm),
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", cm.Namespace, cm.Name),
	}
}

// processConfigMap parses the mig-parted config.yaml into a structured payload.
func (c *MigPartedConfigCollector) processConfigMap(cm *corev1.ConfigMap) map[string]interface{} {
	payload := map[string]interface{}{
		"name":              cm.Name,
		"namespace":         cm.Namespace,
		"uid":               string(cm.UID),
		"labels":            cm.Labels,
		"annotations":       cm.Annotations,
		"creationTimestamp": cm.CreationTimestamp.Format(time.RFC3339),
	}

	rawConfig, ok := cm.Data["config.yaml"]
	if ok {
		payload["configKey"] = "config.yaml"
	} else {
		// Some installs use a different key. Map iteration order is
		// randomized, so pick deterministically: the lexicographically first
		// yaml-suffixed key, falling back to the first key of any name.
		keys := make([]string, 0, len(cm.Data))
		for key := range cm.Data {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		selected := ""
		for _, key := range keys {
			if strings.HasSuffix(key, ".yaml") || strings.HasSuffix(key, ".yml") {
				selected = key
				break
			}
		}
		if selected == "" && len(keys) > 0 {
			selected = keys[0]
		}
		if selected != "" {
			rawConfig = cm.Data[selected]
			payload["configKey"] = selected
		}
	}
	payload["raw"] = rawConfig

	if rawConfig != "" {
		var parsed map[string]interface{}
		if err := yaml.Unmarshal([]byte(rawConfig), &parsed); err != nil {
			c.logger.Error(err, "Failed to parse mig-parted config yaml",
				"configMap", cm.Name)
			payload["parseError"] = err.Error()
		} else {
			// The mig-parted schema keys the profile definitions as mig-configs:
			// {profile-name: [{devices, mig-enabled, mig-devices}, ...]}.
			payload["version"] = parsed["version"]
			payload["migConfigs"] = parsed["mig-configs"]
		}
	}

	return payload
}

// Stop shuts down the collector.
func (c *MigPartedConfigCollector) Stop() error {
	c.logger.Info("Stopping mig-parted config collector")
	c.mu.Lock()
	defer c.mu.Unlock()

	select {
	case <-c.stopCh:
	default:
		close(c.stopCh)
	}
	c.chanMu.Lock()
	if !c.stopped {
		c.stopped = true
		close(c.batchChan)
	}
	c.chanMu.Unlock()
	if c.batcher != nil {
		c.batcher.stop()
	}
	return nil
}

// GetResourceChannel returns the output channel for batched resources.
func (c *MigPartedConfigCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the string key used to identify this collector.
func (c *MigPartedConfigCollector) GetType() string {
	return MigPartedConfig.String()
}

// IsAvailable returns true; ConfigMaps always exist as an API. When the
// mig-parted ConfigMap is absent the informer simply never fires, which keeps
// GPU-less clusters quiet without a bespoke probe.
func (c *MigPartedConfigCollector) IsAvailable(_ context.Context) bool {
	return true
}

// AddResource implements ResourceCollector for manual injection (used in tests).
func (c *MigPartedConfigCollector) AddResource(resource interface{}) error {
	cm, ok := resource.(*corev1.ConfigMap)
	if !ok {
		return fmt.Errorf("expected *corev1.ConfigMap, got %T", resource)
	}
	c.handleConfigMapEvent(cm, EventTypeAdd)
	return nil
}
