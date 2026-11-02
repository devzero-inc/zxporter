// internal/collector/flux_resource_collector.go
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
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
)

// GVRs for the eighteen FluxCD kinds this package collects, grouped by API
// group the way the ledger and common.proto group them.
var (
	// source.toolkit.fluxcd.io/v1
	FluxGitRepositoryGVR    = schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "gitrepositories"}
	FluxOCIRepositoryGVR    = schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "ocirepositories"}
	FluxHelmRepositoryGVR   = schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "helmrepositories"}
	FluxHelmChartGVR        = schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "helmcharts"}
	FluxBucketGVR           = schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "buckets"}
	FluxExternalArtifactGVR = schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "externalartifacts"}

	// kustomize.toolkit.fluxcd.io/v1
	FluxKustomizationGVR = schema.GroupVersionResource{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Resource: "kustomizations"}

	// helm.toolkit.fluxcd.io/v2
	FluxHelmReleaseGVR = schema.GroupVersionResource{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}

	// notification.toolkit.fluxcd.io
	FluxAlertGVR    = schema.GroupVersionResource{Group: "notification.toolkit.fluxcd.io", Version: "v1beta3", Resource: "alerts"}
	FluxProviderGVR = schema.GroupVersionResource{Group: "notification.toolkit.fluxcd.io", Version: "v1beta3", Resource: "providers"}
	FluxReceiverGVR = schema.GroupVersionResource{Group: "notification.toolkit.fluxcd.io", Version: "v1", Resource: "receivers"}

	// fluxcd.controlplane.io/v1 (Flux Operator)
	FluxInstanceGVR                 = schema.GroupVersionResource{Group: "fluxcd.controlplane.io", Version: "v1", Resource: "fluxinstances"}
	FluxReportGVR                   = schema.GroupVersionResource{Group: "fluxcd.controlplane.io", Version: "v1", Resource: "fluxreports"}
	FluxResourceSetInputProviderGVR = schema.GroupVersionResource{Group: "fluxcd.controlplane.io", Version: "v1", Resource: "resourcesetinputproviders"}
	FluxResourceSetGVR              = schema.GroupVersionResource{Group: "fluxcd.controlplane.io", Version: "v1", Resource: "resourcesets"}

	// image.toolkit.fluxcd.io/v1 (image-reflector-controller / image-automation-controller --
	// not deployed on every cluster, hence collector-only + the factory pattern)
	FluxImageRepositoryGVR       = schema.GroupVersionResource{Group: "image.toolkit.fluxcd.io", Version: "v1", Resource: "imagerepositories"}
	FluxImagePolicyGVR           = schema.GroupVersionResource{Group: "image.toolkit.fluxcd.io", Version: "v1", Resource: "imagepolicies"}
	FluxImageUpdateAutomationGVR = schema.GroupVersionResource{Group: "image.toolkit.fluxcd.io", Version: "v1", Resource: "imageupdateautomations"}
)

// FluxResourceCollector is a generic single-GVR collector shared by every
// FluxCD kind. All eighteen kinds emit the same wire shape -- identity
// fields plus the full unstructured object under "raw" -- because dakr's
// per-kind converters do the field extraction from raw, not zxporter (see
// dakr/k8s-gen/k8sconverters/flux_common.go). That leaves nothing per-kind
// for zxporter to do beyond naming the GVR and the ResourceType, so one
// parameterized collector replaces what would otherwise be eighteen
// near-identical ~250-line files (the shape KyvernoPolicyCollector and
// every other collector in this package hand-roll).
//
// If a Flux kind ever needs zxporter-side field extraction (the way
// KyvernoPolicyCollector.processPolicy lifts ruleCount/background), give it
// its own collector file at that point -- this type is for the common case,
// not a mandate to force every future kind through it.
type FluxResourceCollector struct {
	dynamicClient   dynamic.Interface
	gvr             schema.GroupVersionResource
	resourceType    ResourceType
	batchChan       chan CollectedResource
	resourceChan    chan []CollectedResource
	batcher         *ResourcesBatcher
	stopCh          chan struct{}
	namespaces      []string
	logger          logr.Logger
	telemetryLogger telemetry_logger.Logger
	mu              sync.Mutex
	// chanMu guards batchChan against the send-after-close shutdown race:
	// informer callbacks RLock around sends, Stop write-locks before closing.
	chanMu  sync.RWMutex
	stopped bool
}

// NewFluxResourceCollector creates a collector for one FluxCD kind, identified
// by its GVR and the ResourceType it emits as.
func NewFluxResourceCollector(
	dynamicClient dynamic.Interface,
	gvr schema.GroupVersionResource,
	resourceType ResourceType,
	namespaces []string,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
) *FluxResourceCollector {
	batchChan := make(chan CollectedResource, 100)
	resourceChan := make(chan []CollectedResource, 100)
	batcher := NewResourcesBatcher(maxBatchSize, maxBatchTime, batchChan, resourceChan, logger)

	return &FluxResourceCollector{
		dynamicClient:   dynamicClient,
		gvr:             gvr,
		resourceType:    resourceType,
		batchChan:       batchChan,
		resourceChan:    resourceChan,
		batcher:         batcher,
		stopCh:          make(chan struct{}),
		namespaces:      namespaces,
		logger:          logger.WithName(fmt.Sprintf("%s-collector", resourceType.String())),
		telemetryLogger: telemetryLogger,
	}
}

// Start begins watching this collector's GVR.
func (c *FluxResourceCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting Flux resource collector", "gvr", c.gvr.String(), "namespaces", c.namespaces)

	// If the CRD is not installed (e.g. the image automation controllers are
	// not deployed on this cluster), skip silently rather than error -- the
	// same posture KyvernoPolicyCollector takes for the Kyverno CRDs.
	if !c.IsAvailable(ctx) {
		c.logger.V(4).Info("Flux CRD not available, skipping", "gvr", c.gvr.String())
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

	c.logger.Info("Flux resource informer started and synced", "gvr", c.gvr.String())
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
func (c *FluxResourceCollector) eventHandler() cache.ResourceEventHandlerFuncs {
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

// handleEvent processes a Flux object event.
func (c *FluxResourceCollector) handleEvent(obj *unstructured.Unstructured, eventType EventType) {
	key := obj.GetName()
	if ns := obj.GetNamespace(); ns != "" {
		key = fmt.Sprintf("%s/%s", ns, obj.GetName())
	}

	c.chanMu.RLock()
	defer c.chanMu.RUnlock()
	if c.stopped {
		return
	}

	c.batchChan <- CollectedResource{
		ResourceType: c.resourceType,
		Object:       processFluxObject(obj),
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          key,
	}
}

// processFluxObject extracts the identity fields dakr's Flux converters
// expect (see k8smodels.ProcessedFluxGitRepository et al.): every Flux kind
// is a thin identity envelope around the full object, with all field
// extraction happening in dakr, not here.
func processFluxObject(obj *unstructured.Unstructured) map[string]interface{} {
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
func (c *FluxResourceCollector) Stop() error {
	c.logger.Info("Stopping Flux resource collector", "gvr", c.gvr.String())
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
func (c *FluxResourceCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the string key used to identify this collector.
func (c *FluxResourceCollector) GetType() string {
	return c.resourceType.String()
}

// IsAvailable returns true when this collector's GVR is served by the
// cluster's API server.
func (c *FluxResourceCollector) IsAvailable(ctx context.Context) bool {
	_, err := c.dynamicClient.Resource(c.gvr).List(ctx, metav1.ListOptions{Limit: 1})
	return err == nil
}

// AddResource implements ResourceCollector for manual injection (used in tests).
func (c *FluxResourceCollector) AddResource(resource interface{}) error {
	obj, ok := resource.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("expected *unstructured.Unstructured, got %T", resource)
	}
	c.handleEvent(obj, EventTypeAdd)
	return nil
}
