// internal/collector/argo_cd_collector.go
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

// Argo CD GVRs. These are Argo CD resources, NOT Argo Rollouts: both projects
// publish under the argoproj.io group, but Rollouts is a separate controller
// and ArgoRolloutsCollector continues to own it.
var (
	argoCDApplicationGVR = schema.GroupVersionResource{
		Group:    "argoproj.io",
		Version:  "v1alpha1",
		Resource: "applications",
	}

	argoCDApplicationSetGVR = schema.GroupVersionResource{
		Group:    "argoproj.io",
		Version:  "v1alpha1",
		Resource: "applicationsets",
	}
)

// ArgoCDCollector watches one Argo CD kind through a dynamic informer.
//
// Applications and ApplicationSets share this implementation but NOT an
// instance: each collector emits exactly one ResourceType, because the send
// path labels a whole batch with the resource type of its first element
// (see CollectionManager.processCollectorChannel and
// CollectionPolicyReconciler.processCollectedResources). A single collector
// feeding both kinds into one batcher would ship ApplicationSets to DAKR
// labelled as Applications. Construct with NewArgoCDApplicationCollector or
// NewArgoCDApplicationSetCollector.
type ArgoCDCollector struct {
	dynamicClient   dynamic.Interface
	gvr             schema.GroupVersionResource
	resourceType    ResourceType
	informerKey     string
	batchChan       chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan    chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher         *ResourcesBatcher
	stopCh          chan struct{}
	informers       map[string]cache.SharedIndexInformer
	informerStopChs map[string]chan struct{}
	namespaces      []string
	excludedApps    map[types.NamespacedName]bool
	logger          logr.Logger
	telemetryLogger telemetry_logger.Logger

	// mu guards the informer maps and stopped and, with it, the lifetime of
	// batchChan: senders hold the read lock, Stop takes the write lock before
	// closing the channel.
	mu      sync.RWMutex
	stopped bool

	// stopOnce makes Stop idempotent. StopAll cancels the collector context --
	// waking the watcher goroutine in Start, which calls Stop -- and then calls
	// Stop itself, so two Stops genuinely race.
	stopOnce sync.Once
}

// NewArgoCDApplicationCollector creates a collector for Argo CD Applications.
func NewArgoCDApplicationCollector(
	dynamicClient dynamic.Interface,
	namespaces []string,
	excludedApplications []ExcludedArgoCDApplication,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
) *ArgoCDCollector {
	excluded := make(map[types.NamespacedName]bool, len(excludedApplications))
	for _, a := range excludedApplications {
		excluded[types.NamespacedName{Namespace: a.Namespace, Name: a.Name}] = true
	}

	return newArgoCDCollector(
		dynamicClient,
		argoCDApplicationGVR,
		ArgoCDApplication,
		"argo-cd-applications",
		namespaces,
		excluded,
		maxBatchSize,
		maxBatchTime,
		logger.WithName("argo-cd-application-collector"),
		telemetryLogger,
	)
}

// NewArgoCDApplicationSetCollector creates a collector for Argo CD
// ApplicationSets.
func NewArgoCDApplicationSetCollector(
	dynamicClient dynamic.Interface,
	namespaces []string,
	excludedApplicationSets []ExcludedArgoCDApplicationSet,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
) *ArgoCDCollector {
	excluded := make(map[types.NamespacedName]bool, len(excludedApplicationSets))
	for _, a := range excludedApplicationSets {
		excluded[types.NamespacedName{Namespace: a.Namespace, Name: a.Name}] = true
	}

	return newArgoCDCollector(
		dynamicClient,
		argoCDApplicationSetGVR,
		ArgoCDApplicationSet,
		"argo-cd-applicationsets",
		namespaces,
		excluded,
		maxBatchSize,
		maxBatchTime,
		logger.WithName("argo-cd-applicationset-collector"),
		telemetryLogger,
	)
}

func newArgoCDCollector(
	dynamicClient dynamic.Interface,
	gvr schema.GroupVersionResource,
	resourceType ResourceType,
	informerKey string,
	namespaces []string,
	excludedApps map[types.NamespacedName]bool,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
) *ArgoCDCollector {
	batchChan := make(chan CollectedResource, 100)
	resourceChan := make(chan []CollectedResource, 100)
	batcher := NewResourcesBatcher(maxBatchSize, maxBatchTime, batchChan, resourceChan, logger)

	return &ArgoCDCollector{
		dynamicClient:   dynamicClient,
		gvr:             gvr,
		resourceType:    resourceType,
		informerKey:     informerKey,
		batchChan:       batchChan,
		resourceChan:    resourceChan,
		batcher:         batcher,
		stopCh:          make(chan struct{}),
		informers:       make(map[string]cache.SharedIndexInformer),
		informerStopChs: make(map[string]chan struct{}),
		namespaces:      namespaces,
		excludedApps:    excludedApps,
		logger:          logger,
		telemetryLogger: telemetryLogger,
	}
}

// Start begins watching the collector's Argo CD kind.
func (c *ArgoCDCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting Argo CD collector",
		"resource", c.gvr.Resource,
		"namespaces", c.namespaces)

	// A stopped collector's channels are already closed, so restarting this
	// instance would double-close them. Callers build a fresh collector instead.
	c.mu.RLock()
	stopped := c.stopped
	c.mu.RUnlock()
	if stopped {
		return fmt.Errorf("Argo CD collector for %s has been stopped and cannot be restarted", c.gvr.Resource)
	}

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

	informer := factory.ForResource(c.gvr).Informer()

	// Strip managedFields + last-applied-configuration from cached objects.
	// Argo CD writes a large last-applied-configuration onto Applications it
	// manages, so this is a meaningful memory saving here.
	if err := informer.SetTransform(StripMetadataTransform); err != nil {
		return fmt.Errorf("failed to set %s informer transform: %w", c.gvr.Resource, err)
	}

	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				c.logger.Error(nil, "Failed to convert object to unstructured")
				c.telemetryLogger.Report(gen.LogLevel_LOG_LEVEL_ERROR, "ArgoCDCollector_AddFunc",
					"Failed to convert object to unstructured", fmt.Errorf("type assertion failed"),
					map[string]string{"object_type": fmt.Sprintf("%T", obj), "resource": c.gvr.Resource})
				return
			}
			c.handleArgoCDEvent(u, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			newU, ok := newObj.(*unstructured.Unstructured)
			if !ok {
				c.logger.Error(nil, "Failed to convert new object to unstructured")
				c.telemetryLogger.Report(gen.LogLevel_LOG_LEVEL_ERROR, "ArgoCDCollector_UpdateFunc",
					"Failed to convert new object to unstructured", fmt.Errorf("type assertion failed"),
					map[string]string{"object_type": fmt.Sprintf("%T", newObj), "resource": c.gvr.Resource})
				return
			}
			c.handleArgoCDEvent(newU, EventTypeUpdate)
		},
		DeleteFunc: func(obj interface{}) {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
					if u, ok = tombstone.Obj.(*unstructured.Unstructured); ok {
						c.handleArgoCDEvent(u, EventTypeDelete)
						return
					}
				}
				c.logger.Error(nil, "Failed to convert deleted object")
				c.telemetryLogger.Report(gen.LogLevel_LOG_LEVEL_ERROR, "ArgoCDCollector_DeleteFunc",
					"Failed to convert deleted object", fmt.Errorf("type assertion failed"),
					map[string]string{"object_type": fmt.Sprintf("%T", obj), "resource": c.gvr.Resource})
				return
			}
			c.handleArgoCDEvent(u, EventTypeDelete)
		},
	})
	if err != nil {
		c.telemetryLogger.Report(gen.LogLevel_LOG_LEVEL_ERROR, "ArgoCDCollector_Start",
			"Failed to add event handler to informer", err,
			map[string]string{"resource": c.gvr.Resource})
		return fmt.Errorf("failed to add event handler to informer for %s: %w", c.gvr.Resource, err)
	}

	c.mu.Lock()
	c.informers[c.informerKey] = informer
	c.informerStopChs[c.informerKey] = make(chan struct{})
	informerStopCh := c.informerStopChs[c.informerKey]
	c.mu.Unlock()

	go informer.Run(informerStopCh)

	syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if !cache.WaitForCacheSync(syncCtx.Done(), informer.HasSynced) {
		c.telemetryLogger.Report(gen.LogLevel_LOG_LEVEL_ERROR, "ArgoCDCollector_Start",
			"Timeout waiting for cache to sync", fmt.Errorf("cache sync timeout"),
			map[string]string{"resource": c.gvr.Resource, "timeout": "30s"})

		c.mu.Lock()
		if stopCh, ok := c.informerStopChs[c.informerKey]; ok {
			close(stopCh)
			delete(c.informerStopChs, c.informerKey)
			delete(c.informers, c.informerKey)
		}
		c.mu.Unlock()

		return fmt.Errorf("timeout waiting for %s cache to sync", c.gvr.Resource)
	}

	c.logger.Info("Successfully started informer", "resource", c.gvr.Resource)
	c.batcher.start()

	stopCh := c.stopCh
	go func() {
		select {
		case <-ctx.Done():
			if err := c.Stop(); err != nil {
				c.logger.Error(err, "Error stopping Argo CD collector")
			}
		case <-stopCh:
			// Channel was closed by Stop()
		}
	}()

	return nil
}

// handleArgoCDEvent processes an Argo CD Application or ApplicationSet event.
func (c *ArgoCDCollector) handleArgoCDEvent(obj *unstructured.Unstructured, eventType EventType) {
	name := obj.GetName()
	namespace := obj.GetNamespace()

	if c.isExcluded(namespace, name) {
		return
	}

	key := fmt.Sprintf("%s/%s", namespace, name)

	resource := CollectedResource{
		ResourceType: c.resourceType,
		Object:       c.processArgoCDObject(obj),
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          key,
	}

	// The read lock keeps Stop from closing batchChan underneath an in-flight
	// send; the stopCh arm releases a send that is blocked on a full buffer.
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.stopped {
		return
	}

	select {
	case c.batchChan <- resource:
	case <-c.stopCh:
	}
}

// processArgoCDObject builds the payload DAKR reads.
//
// DAKR unmarshals this into a ProcessedArgoApplication or
// ProcessedArgoApplicationSet, which read only name, namespace and raw. The
// whole object goes out under raw because every derived column DAKR stores --
// sync status, health, the managed-resource list -- is computed server-side
// from the typed Argo CD API rather than re-extracted here, which keeps one
// definition of those fields instead of two that can drift.
func (c *ArgoCDCollector) processArgoCDObject(obj *unstructured.Unstructured) map[string]interface{} {
	return map[string]interface{}{
		"name":            obj.GetName(),
		"namespace":       obj.GetNamespace(),
		"uid":             string(obj.GetUID()),
		"resourceVersion": obj.GetResourceVersion(),
		"raw":             obj,
	}
}

// isExcluded reports whether a resource should be skipped.
func (c *ArgoCDCollector) isExcluded(namespace, name string) bool {
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
	return c.excludedApps[types.NamespacedName{Namespace: namespace, Name: name}]
}

// Stop gracefully shuts down the collector.
func (c *ArgoCDCollector) Stop() error {
	c.logger.Info("Stopping Argo CD collector", "resource", c.gvr.Resource)

	// Concurrent callers block here until the first Stop has fully finished, so
	// every close below happens exactly once.
	c.stopOnce.Do(func() {
		// 1. Release any handler goroutine parked on a full batchChan first.
		//    This has to happen before the write lock below: a parked sender
		//    holds the read lock, so taking the write lock while it is still
		//    blocked would deadlock.
		close(c.stopCh)
		c.logger.Info("Closed Argo CD collector stop channel", "resource", c.gvr.Resource)

		// 2. Taking the write lock waits for every in-flight handleArgoCDEvent
		//    to leave its send, so closing batchChan cannot panic a live
		//    sender. Later events see stopped and return without sending.
		c.mu.Lock()
		c.stopped = true
		for key, stopCh := range c.informerStopChs {
			c.logger.Info("Stopping informer", "resource", key)
			close(stopCh)
		}
		c.informers = make(map[string]cache.SharedIndexInformer)
		c.informerStopChs = make(map[string]chan struct{})
		close(c.batchChan)
		c.mu.Unlock()
		c.logger.Info("Closed Argo CD collector batch input channel", "resource", c.gvr.Resource)

		// 3. The batcher drains the closed batchChan and closes resourceChan.
		if c.batcher != nil {
			c.batcher.stop()
			c.logger.Info("Argo CD collector batcher stopped", "resource", c.gvr.Resource)
		}
	})

	return nil
}

// GetResourceChannel returns the channel of batched resources.
func (c *ArgoCDCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the collector's type key. It must equal the ResourceType
// string, because the manager keys collectors by this value and the
// disabled-collectors policy is matched against ResourceType.String().
func (c *ArgoCDCollector) GetType() string {
	return c.resourceType.String()
}

// IsAvailable reports whether this collector's Argo CD CRD is present.
//
// Each kind gates on its own CRD: a very old Argo CD install can predate the
// bundled ApplicationSet controller and serve applications but not
// applicationsets, in which case only the ApplicationSet collector is skipped
// (and retried later through the pending-collector factory).
func (c *ArgoCDCollector) IsAvailable(ctx context.Context) bool {
	_, err := c.dynamicClient.Resource(c.gvr).List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil {
		c.logger.V(1).Info("Argo CD resources not available in the cluster",
			"resource", c.gvr.Resource, "error", err.Error())
		c.telemetryLogger.Report(gen.LogLevel_LOG_LEVEL_WARN, "ArgoCDCollector_IsAvailable",
			"Argo CD resources not available in the cluster", err,
			map[string]string{"resource": c.gvr.Resource})
		return false
	}
	return true
}

// AddResource manually adds an Argo CD resource to be processed.
func (c *ArgoCDCollector) AddResource(resource interface{}) error {
	u, ok := resource.(*unstructured.Unstructured)
	if !ok {
		err := fmt.Errorf("expected *unstructured.Unstructured, got %T", resource)
		c.telemetryLogger.Report(gen.LogLevel_LOG_LEVEL_ERROR, "ArgoCDCollector_AddResource",
			"Invalid resource type", err,
			map[string]string{
				"expected_type": "*unstructured.Unstructured",
				"actual_type":   fmt.Sprintf("%T", resource),
				"resource":      c.gvr.Resource,
			})
		return err
	}
	c.handleArgoCDEvent(u, EventTypeAdd)
	return nil
}
