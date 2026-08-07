// internal/collector/kyverno_policy_collector.go
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

var (
	kyvernoPolicyGVR = schema.GroupVersionResource{
		Group:    "kyverno.io",
		Version:  "v1",
		Resource: "policies",
	}
	kyvernoClusterPolicyGVR = schema.GroupVersionResource{
		Group:    "kyverno.io",
		Version:  "v1",
		Resource: "clusterpolicies",
	}
)

// KyvernoPolicyCollector watches Kyverno Policy (namespaced) and ClusterPolicy
// (cluster-scoped) resources and emits them as RESOURCE_TYPE_KYVERNO_POLICY.
// The payload kind field discriminates the two scopes.
type KyvernoPolicyCollector struct {
	dynamicClient   dynamic.Interface
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

// NewKyvernoPolicyCollector creates a new collector for Kyverno policies.
func NewKyvernoPolicyCollector(
	dynamicClient dynamic.Interface,
	namespaces []string,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
) *KyvernoPolicyCollector {
	batchChan := make(chan CollectedResource, 100)
	resourceChan := make(chan []CollectedResource, 100)
	batcher := NewResourcesBatcher(maxBatchSize, maxBatchTime, batchChan, resourceChan, logger)

	return &KyvernoPolicyCollector{
		dynamicClient:   dynamicClient,
		batchChan:       batchChan,
		resourceChan:    resourceChan,
		batcher:         batcher,
		stopCh:          make(chan struct{}),
		namespaces:      namespaces,
		logger:          logger.WithName("kyverno-policy-collector"),
		telemetryLogger: telemetryLogger,
	}
}

// Start begins watching Kyverno Policy and ClusterPolicy resources.
func (c *KyvernoPolicyCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting Kyverno policy collector", "namespaces", c.namespaces)

	// If the Kyverno CRDs are not installed, skip silently.
	if !c.IsAvailable(ctx) {
		c.logger.V(4).Info("Kyverno CRDs not available, skipping")
		return nil
	}

	// Namespaced policies honor the single-namespace target configuration;
	// cluster policies always use an unfiltered factory.
	targetNamespace := ""
	if len(c.namespaces) == 1 && c.namespaces[0] != "" {
		targetNamespace = c.namespaces[0]
	}
	namespacedFactory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		c.dynamicClient, 0, targetNamespace, nil,
	)
	clusterFactory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		c.dynamicClient, 0, "", nil,
	)

	informersToStart := []struct {
		name     string
		informer cache.SharedIndexInformer
	}{
		{"policies", namespacedFactory.ForResource(kyvernoPolicyGVR).Informer()},
		{"clusterpolicies", clusterFactory.ForResource(kyvernoClusterPolicyGVR).Informer()},
	}

	stopCh := c.stopCh
	syncFuncs := make([]cache.InformerSynced, 0, len(informersToStart))
	for _, entry := range informersToStart {
		if err := entry.informer.SetTransform(StripMetadataTransform); err != nil {
			return fmt.Errorf("failed to set %s informer transform: %w", entry.name, err)
		}
		if _, err := entry.informer.AddEventHandler(c.eventHandler()); err != nil {
			return fmt.Errorf("failed to add event handler for %s: %w", entry.name, err)
		}
		go entry.informer.Run(stopCh)
		syncFuncs = append(syncFuncs, entry.informer.HasSynced)
	}

	syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if !cache.WaitForCacheSync(syncCtx.Done(), syncFuncs...) {
		return fmt.Errorf("timeout waiting for Kyverno policy caches to sync")
	}

	c.logger.Info("Kyverno policy informers started and synced")
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

// eventHandler builds the shared informer callbacks for both policy scopes.
func (c *KyvernoPolicyCollector) eventHandler() cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if u, ok := obj.(*unstructured.Unstructured); ok {
				c.handlePolicyEvent(u, EventTypeAdd)
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if u, ok := newObj.(*unstructured.Unstructured); ok {
				c.handlePolicyEvent(u, EventTypeUpdate)
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
				c.handlePolicyEvent(u, EventTypeDelete)
			}
		},
	}
}

// handlePolicyEvent processes a Kyverno policy event.
func (c *KyvernoPolicyCollector) handlePolicyEvent(obj *unstructured.Unstructured, eventType EventType) {
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
		ResourceType: KyvernoPolicy,
		Object:       c.processPolicy(obj),
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          key,
	}
}

// processPolicy extracts the fields dakr ingests from a Kyverno policy.
func (c *KyvernoPolicyCollector) processPolicy(obj *unstructured.Unstructured) map[string]interface{} {
	kind := obj.GetKind()
	if kind == "" {
		// Informer objects usually carry kind, but derive it from scope as a fallback.
		if obj.GetNamespace() != "" {
			kind = "Policy"
		} else {
			kind = "ClusterPolicy"
		}
	}

	ruleNames := []string{}
	rules, _, _ := unstructured.NestedSlice(obj.Object, "spec", "rules")
	// Kyverno 1.13 moved validationFailureAction from the policy spec to the
	// rule-level validate.failureAction field; read both, rule-level wins.
	failureAction, _, _ := unstructured.NestedString(obj.Object, "spec", "validationFailureAction")
	for _, r := range rules {
		rule, ok := r.(map[string]interface{})
		if !ok {
			continue
		}
		if name, ok := rule["name"].(string); ok {
			ruleNames = append(ruleNames, name)
		}
		if ruleAction, found, _ := unstructured.NestedString(rule, "validate", "failureAction"); found {
			failureAction = ruleAction
		}
	}

	background := true
	if b, found, _ := unstructured.NestedBool(obj.Object, "spec", "background"); found {
		background = b
	}

	return map[string]interface{}{
		"kind":                    kind,
		"name":                    obj.GetName(),
		"namespace":               obj.GetNamespace(),
		"uid":                     string(obj.GetUID()),
		"labels":                  obj.GetLabels(),
		"annotations":             obj.GetAnnotations(),
		"creationTimestamp":       obj.GetCreationTimestamp().Format(time.RFC3339),
		"background":              background,
		"validationFailureAction": failureAction,
		"ruleCount":               len(ruleNames),
		"ruleNames":               ruleNames,
		"raw":                     obj.Object,
	}
}

// Stop shuts down the collector.
func (c *KyvernoPolicyCollector) Stop() error {
	c.logger.Info("Stopping Kyverno policy collector")
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
func (c *KyvernoPolicyCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the string key used to identify this collector.
func (c *KyvernoPolicyCollector) GetType() string {
	return KyvernoPolicy.String()
}

// IsAvailable returns true when the Kyverno ClusterPolicy CRD exists in the cluster.
func (c *KyvernoPolicyCollector) IsAvailable(ctx context.Context) bool {
	_, err := c.dynamicClient.Resource(kyvernoClusterPolicyGVR).List(ctx, metav1.ListOptions{Limit: 1})
	return err == nil
}

// AddResource implements ResourceCollector for manual injection (used in tests).
func (c *KyvernoPolicyCollector) AddResource(resource interface{}) error {
	obj, ok := resource.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("expected *unstructured.Unstructured, got %T", resource)
	}
	c.handlePolicyEvent(obj, EventTypeAdd)
	return nil
}
