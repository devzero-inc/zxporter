package collector

import (
	"context"
	"fmt"
	"sync"
	"time"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	telemetry_logger "github.com/devzero-inc/zxporter/internal/logger"
	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
)

var cnpgGVR = schema.GroupVersionResource{
	Group:    "postgresql.cnpg.io",
	Version:  "v1",
	Resource: "clusters",
}

// CNPGCollector watches for CloudNativePG Cluster CRD resources
type CNPGCollector struct {
	dynamicClient    dynamic.Interface
	batchChan        chan CollectedResource
	resourceChan     chan []CollectedResource
	batcher          *ResourcesBatcher
	stopCh           chan struct{}
	informers        map[string]cache.SharedIndexInformer
	informerStopChs  map[string]chan struct{}
	namespaces       []string
	excludedClusters map[types.NamespacedName]bool
	logger           logr.Logger
	telemetryLogger  telemetry_logger.Logger
	mu               sync.RWMutex
}

// NewCNPGCollector creates a new collector for CloudNativePG Cluster resources
func NewCNPGCollector(
	dynamicClient dynamic.Interface,
	namespaces []string,
	excludedClusters []ExcludedCNPGCluster,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
) *CNPGCollector {
	excludedMap := make(map[types.NamespacedName]bool)
	for _, c := range excludedClusters {
		excludedMap[types.NamespacedName{Namespace: c.Namespace, Name: c.Name}] = true
	}

	batchChan := make(chan CollectedResource, 100)
	resourceChan := make(chan []CollectedResource, 100)
	batcher := NewResourcesBatcher(maxBatchSize, maxBatchTime, batchChan, resourceChan, logger)

	return &CNPGCollector{
		dynamicClient:    dynamicClient,
		batchChan:        batchChan,
		resourceChan:     resourceChan,
		batcher:          batcher,
		stopCh:           make(chan struct{}),
		informers:        make(map[string]cache.SharedIndexInformer),
		informerStopChs:  make(map[string]chan struct{}),
		namespaces:       namespaces,
		excludedClusters: excludedMap,
		logger:           logger.WithName("cnpg-collector"),
		telemetryLogger:  telemetryLogger,
	}
}

// Start begins watching CloudNativePG Cluster resources
func (c *CNPGCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting CNPG collector", "namespaces", c.namespaces)

	var factory dynamicinformer.DynamicSharedInformerFactory
	if len(c.namespaces) == 1 && c.namespaces[0] != "" {
		factory = dynamicinformer.NewFilteredDynamicSharedInformerFactory(
			c.dynamicClient, 0, c.namespaces[0], nil,
		)
	} else {
		factory = dynamicinformer.NewFilteredDynamicSharedInformerFactory(
			c.dynamicClient, 0, "", nil,
		)
	}

	informer := factory.ForResource(cnpgGVR).Informer()

	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				c.logger.Error(nil, "Failed to convert object to unstructured")
				c.telemetryLogger.Report(gen.LogLevel_LOG_LEVEL_ERROR, "CNPGCollector_AddFunc",
					"Failed to convert object to unstructured", fmt.Errorf("type assertion failed"),
					map[string]string{"object_type": fmt.Sprintf("%T", obj)})
				return
			}
			c.handleClusterEvent(u, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			_, ok := oldObj.(*unstructured.Unstructured)
			if !ok {
				c.logger.Error(nil, "Failed to convert old object to unstructured")
				c.telemetryLogger.Report(gen.LogLevel_LOG_LEVEL_ERROR, "CNPGCollector_UpdateFunc",
					"Failed to convert old object to unstructured", fmt.Errorf("type assertion failed"),
					map[string]string{"object_type": fmt.Sprintf("%T", oldObj)})
				return
			}
			newU, ok := newObj.(*unstructured.Unstructured)
			if !ok {
				c.logger.Error(nil, "Failed to convert new object to unstructured")
				c.telemetryLogger.Report(gen.LogLevel_LOG_LEVEL_ERROR, "CNPGCollector_UpdateFunc",
					"Failed to convert new object to unstructured", fmt.Errorf("type assertion failed"),
					map[string]string{"object_type": fmt.Sprintf("%T", newObj)})
				return
			}
			c.handleClusterEvent(newU, EventTypeUpdate)
		},
		DeleteFunc: func(obj interface{}) {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
					if u, ok = tombstone.Obj.(*unstructured.Unstructured); ok {
						c.handleClusterEvent(u, EventTypeDelete)
						return
					}
				}
				c.logger.Error(nil, "Failed to convert deleted object")
				c.telemetryLogger.Report(gen.LogLevel_LOG_LEVEL_ERROR, "CNPGCollector_DeleteFunc",
					"Failed to convert deleted object", fmt.Errorf("type assertion failed"),
					map[string]string{"object_type": fmt.Sprintf("%T", obj)})
				return
			}
			c.handleClusterEvent(u, EventTypeDelete)
		},
	})
	if err != nil {
		c.telemetryLogger.Report(gen.LogLevel_LOG_LEVEL_ERROR, "CNPGCollector_Start",
			"Failed to add event handler to informer", err,
			map[string]string{"resource": "clusters"})
		return fmt.Errorf("failed to add event handler to informer for CNPG clusters: %w", err)
	}

	key := "cnpg-clusters"
	c.informers[key] = informer
	c.informerStopChs[key] = make(chan struct{})

	go informer.Run(c.informerStopChs[key])

	syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if !cache.WaitForCacheSync(syncCtx.Done(), informer.HasSynced) {
		c.telemetryLogger.Report(gen.LogLevel_LOG_LEVEL_ERROR, "CNPGCollector_Start",
			"Timeout waiting for cache to sync", fmt.Errorf("cache sync timeout"),
			map[string]string{"resource": "clusters", "timeout": "30s"})
		if stopCh, ok := c.informerStopChs[key]; ok {
			close(stopCh)
			delete(c.informerStopChs, key)
			delete(c.informers, key)
		}
		return fmt.Errorf("timeout waiting for CNPG clusters cache to sync")
	}

	c.logger.Info("Successfully started informer for CNPG clusters")
	c.batcher.start()

	stopCh := c.stopCh
	go func() {
		select {
		case <-ctx.Done():
			if err := c.Stop(); err != nil {
				c.logger.Error(err, "Error stopping CNPG collector")
			}
		case <-stopCh:
		}
	}()

	return nil
}

// handleClusterEvent processes CNPG Cluster CRD events
func (c *CNPGCollector) handleClusterEvent(obj *unstructured.Unstructured, eventType EventType) {
	name := obj.GetName()
	namespace := obj.GetNamespace()

	if c.isExcluded(namespace, name) {
		return
	}

	key := fmt.Sprintf("%s/%s", namespace, name)

	phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
	currentPrimary, _, _ := unstructured.NestedString(obj.Object, "status", "currentPrimary")
	instances, _, _ := unstructured.NestedInt64(obj.Object, "spec", "instances")
	readyInstances, _, _ := unstructured.NestedInt64(obj.Object, "status", "readyInstances")
	uid := obj.GetUID()

	c.logger.Info("Collected CNPG cluster resource",
		"key", key,
		"eventType", eventType,
		"uid", uid,
		"phase", phase,
		"currentPrimary", currentPrimary,
		"instances", instances,
		"readyInstances", readyInstances,
	)

	c.batchChan <- CollectedResource{
		ResourceType: CNPGCluster,
		Object:       obj.Object, // raw map — matches cnpgv1.Cluster JSON structure
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          key,
	}
}

// isExcluded checks if a CNPG cluster should be skipped
func (c *CNPGCollector) isExcluded(namespace, name string) bool {
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		found := false
		for _, ns := range c.namespaces {
			if ns == namespace {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.excludedClusters[types.NamespacedName{Namespace: namespace, Name: name}]
}

// Stop gracefully shuts down the CNPG collector
func (c *CNPGCollector) Stop() error {
	c.logger.Info("Stopping CNPG collector")

	for key, stopCh := range c.informerStopChs {
		c.logger.Info("Stopping informer", "resource", key)
		close(stopCh)
	}
	c.informers = make(map[string]cache.SharedIndexInformer)
	c.informerStopChs = make(map[string]chan struct{})

	select {
	case <-c.stopCh:
		c.logger.Info("CNPG collector stop channel already closed")
	default:
		close(c.stopCh)
	}

	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
	}

	if c.batcher != nil {
		c.batcher.stop()
	}

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *CNPGCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *CNPGCollector) GetType() string {
	return "cnpg_cluster"
}

// IsAvailable checks if CNPG Cluster CRDs exist in the cluster
func (c *CNPGCollector) IsAvailable(ctx context.Context) bool {
	_, err := c.dynamicClient.Resource(cnpgGVR).List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil {
		c.logger.Info("CNPG Cluster resources not available in the cluster", "error", err.Error())
		c.telemetryLogger.Report(gen.LogLevel_LOG_LEVEL_WARN, "CNPGCollector_IsAvailable",
			"CNPG Cluster resources not available in the cluster", err,
			map[string]string{"resource": "clusters"})
		return false
	}
	return true
}

// AddResource manually adds a CNPG Cluster resource to be processed
func (c *CNPGCollector) AddResource(resource interface{}) error {
	u, ok := resource.(*unstructured.Unstructured)
	if !ok {
		err := fmt.Errorf("expected *unstructured.Unstructured, got %T", resource)
		c.telemetryLogger.Report(gen.LogLevel_LOG_LEVEL_ERROR, "CNPGCollector_AddResource",
			"Invalid resource type", err,
			map[string]string{"expected_type": "*unstructured.Unstructured", "actual_type": fmt.Sprintf("%T", resource)})
		return err
	}
	c.handleClusterEvent(u, EventTypeAdd)
	return nil
}
