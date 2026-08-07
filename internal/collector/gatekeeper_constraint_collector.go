// internal/collector/gatekeeper_constraint_collector.go
package collector

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	telemetry_logger "github.com/devzero-inc/zxporter/internal/logger"
	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
)

const (
	gatekeeperConstraintsGroupVersion = "constraints.gatekeeper.sh/v1beta1"

	// gatekeeperConstraintDiscoveryInterval is how often the collector re-runs
	// API discovery to pick up constraint kinds created after startup (each new
	// ConstraintTemplate generates a new CRD). Matches the controller's
	// pending-collector retry cadence.
	gatekeeperConstraintDiscoveryInterval = 5 * time.Minute

	// maxConstraintViolations caps the audit violations carried per constraint
	// payload; gatekeeper itself defaults to 20 via --constraint-violations-limit.
	maxConstraintViolations = 20
)

// GatekeeperConstraintCollector watches all constraint kinds under
// constraints.gatekeeper.sh and emits them as RESOURCE_TYPE_GATEKEEPER_CONSTRAINT.
// Constraint kinds are dynamic (one CRD per ConstraintTemplate), so the
// collector discovers the group's resources at startup and re-discovers on an
// interval, registering an informer per kind. Informers for kinds whose
// template is later deleted keep running until the collector restarts; their
// delete events still flow through before the CRD itself is removed.
type GatekeeperConstraintCollector struct {
	dynamicClient   dynamic.Interface
	discoveryClient discovery.DiscoveryInterface
	factory         dynamicinformer.DynamicSharedInformerFactory
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

	// watchedGVRs tracks which constraint resources already have an informer.
	watchedGVRs   map[schema.GroupVersionResource]bool
	watchedGVRsMu sync.Mutex
}

// NewGatekeeperConstraintCollector creates a new collector for Gatekeeper
// constraint instances.
func NewGatekeeperConstraintCollector(
	dynamicClient dynamic.Interface,
	discoveryClient discovery.DiscoveryInterface,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
) *GatekeeperConstraintCollector {
	batchChan := make(chan CollectedResource, 100)
	resourceChan := make(chan []CollectedResource, 100)
	batcher := NewResourcesBatcher(maxBatchSize, maxBatchTime, batchChan, resourceChan, logger)

	return &GatekeeperConstraintCollector{
		dynamicClient:   dynamicClient,
		discoveryClient: discoveryClient,
		batchChan:       batchChan,
		resourceChan:    resourceChan,
		batcher:         batcher,
		stopCh:          make(chan struct{}),
		logger:          logger.WithName("gatekeeper-constraint-collector"),
		telemetryLogger: telemetryLogger,
		watchedGVRs:     make(map[schema.GroupVersionResource]bool),
	}
}

// Start begins watching constraint resources.
func (c *GatekeeperConstraintCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting Gatekeeper constraint collector")

	if !c.IsAvailable(ctx) {
		c.logger.V(4).Info("Gatekeeper not available, skipping")
		return nil
	}

	c.factory = dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		c.dynamicClient, 0, "", nil,
	)

	// Initial discovery; constraint kinds appearing later are picked up by the
	// refresh loop.
	if err := c.discoverAndWatch(ctx); err != nil {
		return fmt.Errorf("initial constraint discovery: %w", err)
	}

	c.batcher.start()

	stopCh := c.stopCh
	go func() {
		ticker := time.NewTicker(gatekeeperConstraintDiscoveryInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := c.discoverAndWatch(ctx); err != nil {
					c.logger.Error(err, "Constraint re-discovery failed")
				}
			case <-ctx.Done():
				c.Stop() //nolint:errcheck
				return
			case <-stopCh:
				return
			}
		}
	}()

	return nil
}

// discoverAndWatch lists the resources in constraints.gatekeeper.sh/v1beta1 and
// registers an informer for any constraint kind not yet watched.
func (c *GatekeeperConstraintCollector) discoverAndWatch(ctx context.Context) error {
	resourceList, err := c.discoveryClient.ServerResourcesForGroupVersion(gatekeeperConstraintsGroupVersion)
	if err != nil {
		// The group vanishes entirely when the last template is deleted; that
		// is not an error worth surfacing on every tick.
		c.logger.V(4).Info("Constraint group discovery returned no resources", "error", err)
		return nil
	}

	newSyncFuncs := []cache.InformerSynced{}
	for _, resource := range resourceList.APIResources {
		// Skip subresources (e.g. <kind>/status) and anything not watchable.
		if strings.Contains(resource.Name, "/") {
			continue
		}
		gvr := schema.GroupVersionResource{
			Group:    "constraints.gatekeeper.sh",
			Version:  "v1beta1",
			Resource: resource.Name,
		}

		c.watchedGVRsMu.Lock()
		alreadyWatched := c.watchedGVRs[gvr]
		c.watchedGVRsMu.Unlock()
		if alreadyWatched {
			continue
		}

		informer := c.factory.ForResource(gvr).Informer()
		if err := informer.SetTransform(StripMetadataTransform); err != nil {
			return fmt.Errorf("failed to set %s informer transform: %w", gvr.Resource, err)
		}
		if _, err := informer.AddEventHandler(c.eventHandler()); err != nil {
			return fmt.Errorf("failed to add event handler for %s: %w", gvr.Resource, err)
		}
		// Mark watched only after the informer is fully wired: an error above
		// leaves the GVR unmarked so the re-discovery loop retries it instead
		// of skipping the kind until the collector restarts.
		c.watchedGVRsMu.Lock()
		c.watchedGVRs[gvr] = true
		c.watchedGVRsMu.Unlock()
		newSyncFuncs = append(newSyncFuncs, informer.HasSynced)
		c.logger.Info("Watching Gatekeeper constraint kind", "resource", resource.Name)
	}

	if len(newSyncFuncs) == 0 {
		return nil
	}

	// Start is idempotent: it only launches informers not yet running.
	c.factory.Start(c.stopCh)

	syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if !cache.WaitForCacheSync(syncCtx.Done(), newSyncFuncs...) {
		return fmt.Errorf("timeout waiting for constraint caches to sync")
	}
	return nil
}

func (c *GatekeeperConstraintCollector) eventHandler() cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if u, ok := obj.(*unstructured.Unstructured); ok {
				c.handleConstraintEvent(u, EventTypeAdd)
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if u, ok := newObj.(*unstructured.Unstructured); ok {
				c.handleConstraintEvent(u, EventTypeUpdate)
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
				c.handleConstraintEvent(u, EventTypeDelete)
			}
		},
	}
}

// handleConstraintEvent processes a constraint event.
func (c *GatekeeperConstraintCollector) handleConstraintEvent(obj *unstructured.Unstructured, eventType EventType) {
	c.chanMu.RLock()
	defer c.chanMu.RUnlock()
	if c.stopped {
		return
	}

	c.batchChan <- CollectedResource{
		ResourceType: GatekeeperConstraint,
		Object:       c.processConstraint(obj),
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          fmt.Sprintf("%s/%s", strings.ToLower(obj.GetKind()), obj.GetName()),
	}
}

// processConstraint extracts the fields dakr ingests from a constraint.
func (c *GatekeeperConstraintCollector) processConstraint(obj *unstructured.Unstructured) map[string]interface{} {
	enforcementAction := "deny" // gatekeeper's default when spec.enforcementAction is unset
	if action, found, _ := unstructured.NestedString(obj.Object, "spec", "enforcementAction"); found {
		enforcementAction = action
	}

	match, _, _ := unstructured.NestedMap(obj.Object, "spec", "match")
	parameters, _, _ := unstructured.NestedMap(obj.Object, "spec", "parameters")

	totalViolations, _, _ := unstructured.NestedInt64(obj.Object, "status", "totalViolations")
	auditTimestamp, _, _ := unstructured.NestedString(obj.Object, "status", "auditTimestamp")

	violations, _, _ := unstructured.NestedSlice(obj.Object, "status", "violations")
	if len(violations) > maxConstraintViolations {
		violations = violations[:maxConstraintViolations]
	}

	return map[string]interface{}{
		"kind":              obj.GetKind(),
		"name":              obj.GetName(),
		"uid":               string(obj.GetUID()),
		"labels":            obj.GetLabels(),
		"annotations":       obj.GetAnnotations(),
		"creationTimestamp": obj.GetCreationTimestamp().Format(time.RFC3339),
		"enforcementAction": enforcementAction,
		"match":             match,
		"parameters":        parameters,
		"totalViolations":   totalViolations,
		"auditTimestamp":    auditTimestamp,
		"violations":        violations,
	}
}

// Stop shuts down the collector.
func (c *GatekeeperConstraintCollector) Stop() error {
	c.logger.Info("Stopping Gatekeeper constraint collector")
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
func (c *GatekeeperConstraintCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the string key used to identify this collector.
func (c *GatekeeperConstraintCollector) GetType() string {
	return GatekeeperConstraint.String()
}

// IsAvailable returns true when Gatekeeper is installed, probed via the
// ConstraintTemplate CRD (the constraints group only exists once at least one
// template has been created, so it cannot be the availability signal).
func (c *GatekeeperConstraintCollector) IsAvailable(ctx context.Context) bool {
	_, err := c.dynamicClient.Resource(gatekeeperConstraintTemplateGVR).List(ctx, metav1.ListOptions{Limit: 1})
	return err == nil
}

// AddResource implements ResourceCollector for manual injection (used in tests).
func (c *GatekeeperConstraintCollector) AddResource(resource interface{}) error {
	obj, ok := resource.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("expected *unstructured.Unstructured, got %T", resource)
	}
	c.handleConstraintEvent(obj, EventTypeAdd)
	return nil
}
