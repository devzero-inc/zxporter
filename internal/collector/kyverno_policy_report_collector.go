// internal/collector/kyverno_policy_report_collector.go
package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
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
	kyvernoPolicyReportGVR = schema.GroupVersionResource{
		Group:    "wgpolicyk8s.io",
		Version:  "v1alpha2",
		Resource: "policyreports",
	}
	kyvernoClusterPolicyReportGVR = schema.GroupVersionResource{
		Group:    "wgpolicyk8s.io",
		Version:  "v1alpha2",
		Resource: "clusterpolicyreports",
	}
)

// maxPolicyReportResults caps how many per-resource results ride each report
// payload; the summary counts always cover the full report.
const maxPolicyReportResults = 50

// KyvernoPolicyReportCollector watches PolicyReport and ClusterPolicyReport
// resources (wgpolicyk8s.io) and emits them as RESOURCE_TYPE_KYVERNO_POLICY_REPORT.
// Reports are churn-heavy: kyverno re-writes them on every scan even when
// nothing changed, so update events whose summary and results are identical to
// the last emission are dropped.
type KyvernoPolicyReportCollector struct {
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

	// lastEmitted maps report key -> fnv hash of the last emitted payload,
	// used to drop no-op update events.
	lastEmitted   map[string]uint64
	lastEmittedMu sync.Mutex
}

// NewKyvernoPolicyReportCollector creates a new collector for Kyverno policy reports.
func NewKyvernoPolicyReportCollector(
	dynamicClient dynamic.Interface,
	namespaces []string,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
) *KyvernoPolicyReportCollector {
	batchChan := make(chan CollectedResource, 100)
	resourceChan := make(chan []CollectedResource, 100)
	batcher := NewResourcesBatcher(maxBatchSize, maxBatchTime, batchChan, resourceChan, logger)

	return &KyvernoPolicyReportCollector{
		dynamicClient:   dynamicClient,
		batchChan:       batchChan,
		resourceChan:    resourceChan,
		batcher:         batcher,
		stopCh:          make(chan struct{}),
		namespaces:      namespaces,
		logger:          logger.WithName("kyverno-policy-report-collector"),
		telemetryLogger: telemetryLogger,
		lastEmitted:     make(map[string]uint64),
	}
}

// Start begins watching PolicyReport and ClusterPolicyReport resources.
func (c *KyvernoPolicyReportCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting Kyverno policy report collector", "namespaces", c.namespaces)

	if !c.IsAvailable(ctx) {
		c.logger.V(4).Info("Policy report CRDs not available, skipping")
		return nil
	}

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
		{"policyreports", namespacedFactory.ForResource(kyvernoPolicyReportGVR).Informer()},
		{"clusterpolicyreports", clusterFactory.ForResource(kyvernoClusterPolicyReportGVR).Informer()},
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
		return fmt.Errorf("timeout waiting for policy report caches to sync")
	}

	c.logger.Info("Kyverno policy report informers started and synced")
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

func (c *KyvernoPolicyReportCollector) eventHandler() cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if u, ok := obj.(*unstructured.Unstructured); ok {
				c.handleReportEvent(u, EventTypeAdd)
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if u, ok := newObj.(*unstructured.Unstructured); ok {
				c.handleReportEvent(u, EventTypeUpdate)
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
				c.handleReportEvent(u, EventTypeDelete)
			}
		},
	}
}

// handleReportEvent processes a policy report event, dropping no-op updates.
func (c *KyvernoPolicyReportCollector) handleReportEvent(obj *unstructured.Unstructured, eventType EventType) {
	key := obj.GetName()
	if ns := obj.GetNamespace(); ns != "" {
		key = fmt.Sprintf("%s/%s", ns, obj.GetName())
	}

	if eventType == EventTypeDelete {
		c.lastEmittedMu.Lock()
		delete(c.lastEmitted, key)
		c.lastEmittedMu.Unlock()
	}

	processed := c.processReport(obj)

	if eventType == EventTypeAdd || eventType == EventTypeUpdate {
		hash := payloadHash(processed)
		c.lastEmittedMu.Lock()
		previous, seen := c.lastEmitted[key]
		c.lastEmitted[key] = hash
		c.lastEmittedMu.Unlock()
		if eventType == EventTypeUpdate && seen && previous == hash {
			return
		}
	}

	c.chanMu.RLock()
	defer c.chanMu.RUnlock()
	if c.stopped {
		return
	}

	c.batchChan <- CollectedResource{
		ResourceType: KyvernoPolicyReport,
		Object:       processed,
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          key,
	}
}

// processReport extracts summary counts and a capped result list from a report.
// The raw object is intentionally omitted: reports are the highest-churn
// resource kyverno produces and the results list already carries everything
// dakr stores.
func (c *KyvernoPolicyReportCollector) processReport(obj *unstructured.Unstructured) map[string]interface{} {
	kind := obj.GetKind()
	if kind == "" {
		if obj.GetNamespace() != "" {
			kind = "PolicyReport"
		} else {
			kind = "ClusterPolicyReport"
		}
	}

	summary, _, _ := unstructured.NestedMap(obj.Object, "summary")
	scope, _, _ := unstructured.NestedMap(obj.Object, "scope")

	rawResults, _, _ := unstructured.NestedSlice(obj.Object, "results")
	totalResults := len(rawResults)
	if len(rawResults) > maxPolicyReportResults {
		rawResults = rawResults[:maxPolicyReportResults]
	}
	results := make([]interface{}, 0, len(rawResults))
	for _, r := range rawResults {
		result, ok := r.(map[string]interface{})
		if !ok {
			continue
		}
		entry := map[string]interface{}{
			"policy":   result["policy"],
			"rule":     result["rule"],
			"result":   result["result"],
			"severity": result["severity"],
			"message":  result["message"],
		}
		if ts, found, _ := unstructured.NestedMap(result, "timestamp"); found {
			entry["timestamp"] = ts
		}
		if resources, found, _ := unstructured.NestedSlice(result, "resources"); found {
			entry["resources"] = resources
		}
		results = append(results, entry)
	}

	return map[string]interface{}{
		"kind":              kind,
		"name":              obj.GetName(),
		"namespace":         obj.GetNamespace(),
		"uid":               string(obj.GetUID()),
		"labels":            obj.GetLabels(),
		"creationTimestamp": obj.GetCreationTimestamp().Format(time.RFC3339),
		"scope":             scope,
		"summary":           summary,
		"results":           results,
		"totalResults":      totalResults,
	}
}

// payloadHash returns a stable fnv-64 hash of a processed payload.
func payloadHash(payload map[string]interface{}) uint64 {
	data, err := json.Marshal(payload)
	if err != nil {
		return 0
	}
	h := fnv.New64a()
	h.Write(data) //nolint:errcheck
	return h.Sum64()
}

// Stop shuts down the collector.
func (c *KyvernoPolicyReportCollector) Stop() error {
	c.logger.Info("Stopping Kyverno policy report collector")
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
func (c *KyvernoPolicyReportCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the string key used to identify this collector.
func (c *KyvernoPolicyReportCollector) GetType() string {
	return KyvernoPolicyReport.String()
}

// IsAvailable returns true when the PolicyReport CRDs exist in the cluster.
func (c *KyvernoPolicyReportCollector) IsAvailable(ctx context.Context) bool {
	_, err := c.dynamicClient.Resource(kyvernoClusterPolicyReportGVR).List(ctx, metav1.ListOptions{Limit: 1})
	return err == nil
}

// AddResource implements ResourceCollector for manual injection (used in tests).
func (c *KyvernoPolicyReportCollector) AddResource(resource interface{}) error {
	obj, ok := resource.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("expected *unstructured.Unstructured, got %T", resource)
	}
	c.handleReportEvent(obj, EventTypeAdd)
	return nil
}
