// internal/collector/gatekeeper_constraint_template_collector.go
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

var gatekeeperConstraintTemplateGVR = schema.GroupVersionResource{
	Group:    "templates.gatekeeper.sh",
	Version:  "v1",
	Resource: "constrainttemplates",
}

// GatekeeperConstraintTemplateCollector watches Gatekeeper ConstraintTemplate
// resources and emits them as RESOURCE_TYPE_GATEKEEPER_CONSTRAINT_TEMPLATE.
type GatekeeperConstraintTemplateCollector struct {
	dynamicClient   dynamic.Interface
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

// NewGatekeeperConstraintTemplateCollector creates a new collector for
// Gatekeeper constraint templates.
func NewGatekeeperConstraintTemplateCollector(
	dynamicClient dynamic.Interface,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
) *GatekeeperConstraintTemplateCollector {
	batchChan := make(chan CollectedResource, 50)
	resourceChan := make(chan []CollectedResource, 50)
	batcher := NewResourcesBatcher(maxBatchSize, maxBatchTime, batchChan, resourceChan, logger)

	return &GatekeeperConstraintTemplateCollector{
		dynamicClient:   dynamicClient,
		batchChan:       batchChan,
		resourceChan:    resourceChan,
		batcher:         batcher,
		stopCh:          make(chan struct{}),
		logger:          logger.WithName("gatekeeper-constraint-template-collector"),
		telemetryLogger: telemetryLogger,
	}
}

// Start begins watching ConstraintTemplate resources.
func (c *GatekeeperConstraintTemplateCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting Gatekeeper constraint template collector")

	if !c.IsAvailable(ctx) {
		c.logger.V(4).Info("Gatekeeper ConstraintTemplate CRD not available, skipping")
		return nil
	}

	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		c.dynamicClient, 0, "", nil,
	)
	informer := factory.ForResource(gatekeeperConstraintTemplateGVR).Informer()

	if err := informer.SetTransform(StripMetadataTransform); err != nil {
		return fmt.Errorf("failed to set informer transform: %w", err)
	}

	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if u, ok := obj.(*unstructured.Unstructured); ok {
				c.handleTemplateEvent(u, EventTypeAdd)
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if u, ok := newObj.(*unstructured.Unstructured); ok {
				c.handleTemplateEvent(u, EventTypeUpdate)
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
				c.handleTemplateEvent(u, EventTypeDelete)
			}
		},
	})
	if err != nil {
		return fmt.Errorf("add event handler: %w", err)
	}

	stopCh := c.stopCh
	go informer.Run(stopCh)

	syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if !cache.WaitForCacheSync(syncCtx.Done(), informer.HasSynced) {
		return fmt.Errorf("timeout waiting for ConstraintTemplate cache to sync")
	}

	c.logger.Info("Gatekeeper constraint template informer started and synced")
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

// handleTemplateEvent processes a ConstraintTemplate event.
func (c *GatekeeperConstraintTemplateCollector) handleTemplateEvent(obj *unstructured.Unstructured, eventType EventType) {
	c.chanMu.RLock()
	defer c.chanMu.RUnlock()
	if c.stopped {
		return
	}

	c.batchChan <- CollectedResource{
		ResourceType: GatekeeperConstraintTemplate,
		Object:       c.processTemplate(obj),
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("constrainttemplates/%s", obj.GetName()),
	}
}

// processTemplate extracts the fields dakr ingests from a ConstraintTemplate.
func (c *GatekeeperConstraintTemplateCollector) processTemplate(obj *unstructured.Unstructured) map[string]interface{} {
	// The kind of the constraint CRD this template generates
	// (spec.crd.spec.names.kind), e.g. K8sRequiredLabels.
	crdKind, _, _ := unstructured.NestedString(obj.Object, "spec", "crd", "spec", "names", "kind")

	targets := []string{}
	rawTargets, _, _ := unstructured.NestedSlice(obj.Object, "spec", "targets")
	for _, t := range rawTargets {
		target, ok := t.(map[string]interface{})
		if !ok {
			continue
		}
		if name, ok := target["target"].(string); ok {
			targets = append(targets, name)
		}
	}

	created := false
	if b, found, _ := unstructured.NestedBool(obj.Object, "status", "created"); found {
		created = b
	}

	return map[string]interface{}{
		"name":              obj.GetName(),
		"uid":               string(obj.GetUID()),
		"labels":            obj.GetLabels(),
		"annotations":       obj.GetAnnotations(),
		"creationTimestamp": obj.GetCreationTimestamp().Format(time.RFC3339),
		"crdKind":           crdKind,
		"targets":           targets,
		"created":           created,
		"raw":               obj.Object,
	}
}

// Stop shuts down the collector.
func (c *GatekeeperConstraintTemplateCollector) Stop() error {
	c.logger.Info("Stopping Gatekeeper constraint template collector")
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
func (c *GatekeeperConstraintTemplateCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the string key used to identify this collector.
func (c *GatekeeperConstraintTemplateCollector) GetType() string {
	return GatekeeperConstraintTemplate.String()
}

// IsAvailable returns true when the ConstraintTemplate CRD exists in the cluster.
func (c *GatekeeperConstraintTemplateCollector) IsAvailable(ctx context.Context) bool {
	_, err := c.dynamicClient.Resource(gatekeeperConstraintTemplateGVR).List(ctx, metav1.ListOptions{Limit: 1})
	return err == nil
}

// AddResource implements ResourceCollector for manual injection (used in tests).
func (c *GatekeeperConstraintTemplateCollector) AddResource(resource interface{}) error {
	obj, ok := resource.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("expected *unstructured.Unstructured, got %T", resource)
	}
	c.handleTemplateEvent(obj, EventTypeAdd)
	return nil
}
