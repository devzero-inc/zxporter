// internal/collector/flagger_resource_collector.go
package collector

import (
	"context"
	"fmt"
	"sync"
	"time"

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

// GVRs for the three Flagger kinds this package collects. All three live in
// one API group and one version, and all three are namespaced.
var (
	// flagger.app/v1beta1
	FlaggerCanaryGVR         = schema.GroupVersionResource{Group: "flagger.app", Version: "v1beta1", Resource: "canaries"}
	FlaggerMetricTemplateGVR = schema.GroupVersionResource{Group: "flagger.app", Version: "v1beta1", Resource: "metrictemplates"}
	FlaggerAlertProviderGVR  = schema.GroupVersionResource{Group: "flagger.app", Version: "v1beta1", Resource: "alertproviders"}
)

// FlaggerResourceCollector watches one Flagger GVR through a dynamic informer
// and emits an identity envelope around the whole object. All three Flagger
// kinds watch, convert and send the same shape, so one parameterized collector
// replaces three near-identical files -- the same consolidation
// FluxResourceCollector makes for the eighteen FluxCD kinds.
//
// If a Flagger kind ever needs zxporter-side field extraction (the way
// KyvernoPolicyCollector.processPolicy lifts ruleCount/background), it gets its
// own collector file at that point rather than a switch in here.
type FlaggerResourceCollector struct {
	dynamicClient   dynamic.Interface
	gvr             schema.GroupVersionResource
	resourceType    ResourceType
	batchChan       chan CollectedResource
	resourceChan    chan []CollectedResource
	batcher         *ResourcesBatcher
	stopCh          chan struct{}
	namespaces      []string
	excluded        map[types.NamespacedName]bool
	logger          logr.Logger
	telemetryLogger telemetry_logger.Logger
	mu              sync.Mutex
	// chanMu guards batchChan against the send-after-close shutdown race:
	// informer callbacks RLock around sends, Stop write-locks before closing.
	chanMu  sync.RWMutex
	stopped bool
}

// NewFlaggerResourceCollector creates a collector for one Flagger kind,
// identified by its GVR and the ResourceType it emits as.
func NewFlaggerResourceCollector(
	dynamicClient dynamic.Interface,
	gvr schema.GroupVersionResource,
	resourceType ResourceType,
	namespaces []string,
	excludedResources []ExcludedFlaggerResource,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
) *FlaggerResourceCollector {
	batchChan := make(chan CollectedResource, 100)
	resourceChan := make(chan []CollectedResource, 100)
	batcher := NewResourcesBatcher(maxBatchSize, maxBatchTime, batchChan, resourceChan, logger)

	excluded := make(map[types.NamespacedName]bool, len(excludedResources))
	for _, e := range excludedResources {
		excluded[types.NamespacedName{Namespace: e.Namespace, Name: e.Name}] = true
	}

	return &FlaggerResourceCollector{
		dynamicClient:   dynamicClient,
		gvr:             gvr,
		resourceType:    resourceType,
		batchChan:       batchChan,
		resourceChan:    resourceChan,
		batcher:         batcher,
		stopCh:          make(chan struct{}),
		namespaces:      namespaces,
		excluded:        excluded,
		logger:          logger.WithName(fmt.Sprintf("%s-collector", resourceType.String())),
		telemetryLogger: telemetryLogger,
	}
}

// Start begins watching this collector's GVR.
func (c *FlaggerResourceCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting Flagger resource collector", "gvr", c.gvr.String(), "namespaces", c.namespaces)

	// Flagger is an optional add-on, so an absent CRD is a live cluster state
	// rather than a failure: skip silently instead of erroring, the same
	// posture FluxResourceCollector and KyvernoPolicyCollector take.
	if !c.IsAvailable(ctx) {
		c.logger.V(4).Info("Flagger CRD not available, skipping", "gvr", c.gvr.String())
		return nil
	}

	targetNamespace := ""
	if len(c.namespaces) == 1 && c.namespaces[0] != "" {
		targetNamespace = c.namespaces[0]
	}
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		c.dynamicClient, 0, targetNamespace, nil,
	)
	informer := factory.ForResource(c.gvr).Informer()

	if err := informer.SetTransform(StripMetadataTransform); err != nil {
		return fmt.Errorf("failed to set informer transform for %s: %w", c.gvr.String(), err)
	}
	if _, err := informer.AddEventHandler(c.eventHandler()); err != nil {
		return fmt.Errorf("failed to add event handler for %s: %w", c.gvr.String(), err)
	}

	stopCh := c.stopCh
	go informer.Run(stopCh)

	syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if !cache.WaitForCacheSync(syncCtx.Done(), informer.HasSynced) {
		return fmt.Errorf("timeout waiting for %s cache to sync", c.gvr.String())
	}

	c.logger.Info("Flagger resource informer started and synced", "gvr", c.gvr.String())
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

// eventHandler builds the shared informer callbacks.
func (c *FlaggerResourceCollector) eventHandler() cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if u, ok := obj.(*unstructured.Unstructured); ok {
				c.handleEvent(u, EventTypeAdd)
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if u, ok := newObj.(*unstructured.Unstructured); ok {
				c.handleEvent(u, EventTypeUpdate)
			}
		},
		DeleteFunc: func(obj interface{}) {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				if tombstone, ok2 := obj.(cache.DeletedFinalStateUnknown); ok2 {
					u, ok = tombstone.Obj.(*unstructured.Unstructured)
				}
			}
			if ok {
				c.handleEvent(u, EventTypeDelete)
			}
		},
	}
}

// handleEvent processes a Flagger object event.
func (c *FlaggerResourceCollector) handleEvent(obj *unstructured.Unstructured, eventType EventType) {
	if c.excluded[types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}] {
		return
	}

	key := obj.GetName()
	if ns := obj.GetNamespace(); ns != "" {
		key = fmt.Sprintf("%s/%s", ns, obj.GetName())
	}

	c.chanMu.RLock()
	defer c.chanMu.RUnlock()
	if c.stopped {
		return
	}

	select {
	case c.batchChan <- CollectedResource{
		ResourceType: c.resourceType,
		Object:       processFlaggerObject(obj),
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          key,
	}:
	case <-c.stopCh:
		// Stop is closing us down and the batcher has stopped draining;
		// parking on a full channel here would hold the read lock Stop is
		// waiting on.
	}
}

// processFlaggerObject extracts the identity fields dakr's Flagger converters
// expect: every Flagger kind is a thin identity envelope around the full
// object, with all field extraction happening in dakr, not here.
func processFlaggerObject(obj *unstructured.Unstructured) map[string]interface{} {
	return map[string]interface{}{
		"kind":              obj.GetKind(),
		"name":              obj.GetName(),
		"namespace":         obj.GetNamespace(),
		"uid":               string(obj.GetUID()),
		"labels":            obj.GetLabels(),
		"annotations":       obj.GetAnnotations(),
		"creationTimestamp": obj.GetCreationTimestamp().Format(time.RFC3339),
		"raw":               obj.Object,
	}
}

// Stop shuts down the collector.
func (c *FlaggerResourceCollector) Stop() error {
	c.logger.Info("Stopping Flagger resource collector", "gvr", c.gvr.String())
	c.mu.Lock()
	defer c.mu.Unlock()

	// stopCh closes before the write lock is taken: a sender parked on a full
	// batchChan holds the read lock, and only the stopCh arm of its select can
	// release it. Taking the write lock first would deadlock against it.
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

// GetResourceChannel returns the channel of batched resources.
func (c *FlaggerResourceCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the string key used to identify this collector.
func (c *FlaggerResourceCollector) GetType() string {
	return c.resourceType.String()
}

// IsAvailable reports whether this collector's GVR is served by the cluster's
// API server.
func (c *FlaggerResourceCollector) IsAvailable(ctx context.Context) bool {
	_, err := c.dynamicClient.Resource(c.gvr).List(ctx, metav1.ListOptions{Limit: 1})
	return err == nil
}

// AddResource implements ResourceCollector for manual injection (used in tests).
func (c *FlaggerResourceCollector) AddResource(resource interface{}) error {
	obj, ok := resource.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("expected *unstructured.Unstructured for %s, got %T", c.gvr.String(), resource)
	}
	c.handleEvent(obj, EventTypeAdd)
	return nil
}
