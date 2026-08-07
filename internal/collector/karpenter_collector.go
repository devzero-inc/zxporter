// internal/collector/karpenter_collector.go
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
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
)

// karpenterControllerLabelSelector selects the Karpenter controller Deployment.
// See karpenterLabelName in internal/health/node_operator_monitor.go for why
// the release-name label is the only usable selector and why both names are
// accepted — the two constants must stay in sync.
//
// Unlike the health monitor, this collector deliberately does not filter by
// image: it ingests any Karpenter controller, upstream or DevZero-managed.
const karpenterControllerLabelSelector = "app.kubernetes.io/instance in (karpenter,dzkarp)"

// KarpenterResource defines a Karpenter resource to be watched
type KarpenterResource struct {
	GroupVersion schema.GroupVersion
	Resource     string
	Kind         string
}

// nodeClaimLifecycleConditions are the NodeClaim status conditions whose transitions
// are reported as NodeLifecycleTransition resources. Everything else on the NodeClaim
// (Drifted, Expired, Disrupted, ...) is left to the generic Karpenter resource path.
// The names match the Karpenter NodeClaim condition types verbatim, and the dakr-side
// ClickHouse Enum8 values.
var nodeClaimLifecycleConditions = map[string]bool{
	"Launched":    true,
	"Registered":  true,
	"Initialized": true,
	"Ready":       true,
}

// nodeClaimConditionState is the last-observed (status, lastTransitionTime) pair for a
// single NodeClaim condition. A transition is "newly observed" when either field
// differs from what was last seen, which is what keeps informer resyncs — which
// redeliver an unchanged object — from re-emitting the same transition forever.
type nodeClaimConditionState struct {
	status             string
	lastTransitionTime string
}

// KarpenterCollector watches for Karpenter resources
type KarpenterCollector struct {
	dynamicClient     dynamic.Interface
	batchChan         chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan      chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher           *ResourcesBatcher
	stopCh            chan struct{}
	logger            logr.Logger
	telemetryLogger   telemetry_logger.Logger
	informers         map[string]cache.SharedIndexInformer
	informerStopChs   map[string]chan struct{}
	excludedResources map[string]map[string]bool // resourceType -> resourceName -> excluded
	// nodeClaimConditions holds the last-observed lifecycle conditions per NodeClaim,
	// keyed by NodeClaim name then condition type. Entries are dropped when the
	// NodeClaim is deleted so the map cannot grow with cluster churn.
	nodeClaimConditions map[string]map[string]nodeClaimConditionState
	version             string
	mu                  sync.RWMutex

	// chanMu guards the batchChan send/close race. Every sender here runs on an informer
	// callback goroutine — handleKarpenterResourceEvent, emitNodeClaimLifecycleTransitions,
	// sendInstallationMetric — and Stop() closes batchChan with no wait for those goroutines
	// to quiesce first (unlike loopWG-style collectors, nothing here polls on a ticker, so
	// there is no single loop to wait on). Every sender takes the read lock and checks
	// stopped before sending (concurrent sends don't block each other); Stop() takes the
	// write lock to flip stopped and close+nil the channel as one atomic step, so a sender
	// either completes its send before the close or observes stopped=true and returns
	// without touching the channel. Same pattern as cluster_autoscaler_status_collector.go's
	// chanMu, applied here for the same reason: a closed-channel send panics, and closing
	// then nil-ing the field first turns any sender that read the stale non-nil channel
	// into a permanent block on a nil-channel send instead — a silent goroutine leak.
	//
	// HARD INVARIANT this depends on: a sender holds the read lock for as long as its send
	// blocks, so Stop's write-lock acquisition — and therefore the close — cannot proceed
	// while a sender is still waiting for batchChan to have room. The batcher goroutine
	// (started in NewKarpenterCollector) is what drains it, via a `select` with two exit
	// paths: batchChan closing, or its own internal b.stopCh closing (see batcher.go's
	// stop()). Only the FIRST matters here, and this method's call order is what keeps it
	// that way: close(c.batchChan) below runs before c.batcher.stop() (which is what closes
	// b.stopCh), so the batcher is still selecting on batchChan — draining any buffered
	// backlog via its ok-checked receive — for the entire time this write-lock section can
	// possibly be waiting on an in-flight sender. If a future change ever called
	// c.batcher.stop() (or otherwise closed b.stopCh) BEFORE this section runs, the batcher
	// could exit while batchChan still had a sender blocked on a full buffer, and that
	// sender would hold the read lock forever — deadlocking Stop here. Flagged by automated
	// review; verified against the current call order in Stop and batcher.go rather than
	// just documenting the risk.
	chanMu  sync.RWMutex
	stopped bool
}

// NewKarpenterCollector creates a new collector for Karpenter resources
func NewKarpenterCollector(
	dynamicClient dynamic.Interface,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
) *KarpenterCollector {
	// Create channels
	batchChan := make(chan CollectedResource, 100)
	resourceChan := make(chan []CollectedResource, 100)

	// Create the batcher
	batcher := NewResourcesBatcher(
		maxBatchSize,
		maxBatchTime,
		batchChan,
		resourceChan,
		logger,
	)

	return &KarpenterCollector{
		dynamicClient:       dynamicClient,
		batchChan:           batchChan,
		resourceChan:        resourceChan,
		batcher:             batcher,
		stopCh:              make(chan struct{}),
		logger:              logger.WithName("karpenter-collector"),
		telemetryLogger:     telemetryLogger,
		informers:           make(map[string]cache.SharedIndexInformer),
		informerStopChs:     make(map[string]chan struct{}),
		excludedResources:   make(map[string]map[string]bool),
		nodeClaimConditions: make(map[string]map[string]nodeClaimConditionState),
	}
}

// sendBatchResource puts one resource on batchChan, or drops it silently if Stop has
// already run. See chanMu's doc comment for why this check-then-send must be atomic with
// Stop's close.
func (c *KarpenterCollector) sendBatchResource(resource CollectedResource) {
	c.chanMu.RLock()
	defer c.chanMu.RUnlock()
	if c.stopped {
		return
	}
	c.batchChan <- resource
}

// Start begins the Karpenter resources collection process
func (c *KarpenterCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting Karpenter collector")

	// Get Karpenter deployment for installation metric
	gvr := schema.GroupVersionResource{
		Group:    "apps",
		Version:  "v1",
		Resource: "deployments",
	}
	labelSelector := karpenterControllerLabelSelector

	deployments, err := c.dynamicClient.Resource(gvr).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err == nil && len(deployments.Items) > 0 {
		for _, d := range deployments.Items {
			status, found, _ := unstructured.NestedMap(d.Object, "status")
			if found {
				readyReplicas, found, _ := unstructured.NestedInt64(status, "readyReplicas")
				if found && readyReplicas > 0 {
					c.detectKarpenterVersion(&d)
					c.sendInstallationMetric(&d)
					break
				}
			}
		}
	}

	// Define all Karpenter resources to watch
	resources := []KarpenterResource{
		// v1alpha5 resources
		{
			GroupVersion: schema.GroupVersion{Group: "karpenter.sh", Version: "v1alpha5"},
			Resource:     "provisioners",
			Kind:         "Provisioner",
		},
		{
			GroupVersion: schema.GroupVersion{Group: "karpenter.sh", Version: "v1alpha5"},
			Resource:     "machines",
			Kind:         "Machine",
		},

		// v1alpha2 resources
		{
			GroupVersion: schema.GroupVersion{Group: "karpenter.azure.com", Version: "v1alpha2"},
			Resource:     "aksnodeclasses",
			Kind:         "AKSNodeClass",
			// https://github.com/Azure/karpenter-provider-azure/blob/main/pkg/apis/crds/karpenter.azure.com_aksnodeclasses.yaml
			// https://github.com/Azure/karpenter-provider-azure/tree/main/pkg/apis
		},

		// v1alpha1 resources
		{
			GroupVersion: schema.GroupVersion{Group: "karpenter.k8s.aws", Version: "v1alpha1"},
			Resource:     "awsnodetemplates",
			Kind:         "AWSNodeTemplate",
		},
		{
			GroupVersion: schema.GroupVersion{Group: "karpenter.sh", Version: "v1alpha1"},
			Resource:     "nodeoverlays",
			Kind:         "NodeOverlay",
			// https://github.com/kubernetes-sigs/karpenter/blob/main/pkg/apis/crds/karpenter.sh_nodeoverlays.yaml
			// https://karpenter.sh/docs/concepts/nodeoverlays/
		},
		{
			GroupVersion: schema.GroupVersion{Group: "karpenter.k8s.oracle", Version: "v1alpha1"},
			Resource:     "ocinodeclasses",
			Kind:         "OciNodeClass",
			// https://github.com/zoom/karpenter-oci/blob/main/pkg/apis/crds/karpenter.k8s.oracle_ocinodeclasses.yaml
			// https://github.com/zoom/karpenter-oci/tree/main/pkg/apis
		},
		{
			GroupVersion: schema.GroupVersion{Group: "karpenter.k8s.gcp", Version: "v1alpha1"},
			Resource:     "gcenodeclasses",
			Kind:         "GCENodeClass",
			// https://github.com/cloudpilot-ai/karpenter-provider-gcp/blob/main/charts/karpenter/crds/karpenter.k8s.gcp_gcenodeclasses.yaml
			// https://github.com/cloudpilot-ai/karpenter-provider-gcp/tree/main/pkg/apis
		},

		// v1beta1 resources
		{
			GroupVersion: schema.GroupVersion{Group: "karpenter.sh", Version: "v1beta1"},
			Resource:     "nodepools",
			Kind:         "NodePool",
		},
		{
			GroupVersion: schema.GroupVersion{Group: "karpenter.sh", Version: "v1beta1"},
			Resource:     "nodeclaims",
			Kind:         "NodeClaim",
		},
		{
			GroupVersion: schema.GroupVersion{Group: "karpenter.k8s.aws", Version: "v1beta1"},
			Resource:     "ec2nodeclasses",
			Kind:         "EC2NodeClass",
		},
		{
			GroupVersion: schema.GroupVersion{Group: "karpenter.azure.com", Version: "v1beta1"},
			Resource:     "aksnodeclasses",
			Kind:         "AKSNodeClass",
			// https://github.com/Azure/karpenter-provider-azure/blob/main/pkg/apis/crds/karpenter.azure.com_aksnodeclasses.yaml
			// https://github.com/Azure/karpenter-provider-azure/tree/main/pkg/apis
		},

		// v1 resources
		{
			GroupVersion: schema.GroupVersion{Group: "karpenter.sh", Version: "v1"},
			Resource:     "nodepools",
			Kind:         "NodePool",
		},
		{
			GroupVersion: schema.GroupVersion{Group: "karpenter.sh", Version: "v1"},
			Resource:     "nodeclaims",
			Kind:         "NodeClaim",
		},
		{
			GroupVersion: schema.GroupVersion{Group: "karpenter.k8s.aws", Version: "v1"},
			Resource:     "ec2nodeclasses",
			Kind:         "EC2NodeClass",
		},
	}

	// Create informers for each resource type
	var syncErrors []string
	for _, res := range resources {
		if err := c.startResourceInformer(ctx, res); err != nil {
			syncErrors = append(syncErrors, fmt.Sprintf("%s.%s/%s: %v",
				res.GroupVersion.Group, res.GroupVersion.Version, res.Resource, err))
			continue
		} else {
			c.logger.Info("Successfully started informer for Karpenter resource",
				"group", res.GroupVersion.Group,
				"version", res.GroupVersion.Version,
				"resource", res.Resource)
		}
	}
	// Check if all informers failed to sync
	if len(syncErrors) == len(resources) {
		return fmt.Errorf(
			"failed to sync any Karpenter resources. Errors: %s",
			strings.Join(syncErrors, "; "),
		)
	}

	// Start the batcher since at least one informer synced
	c.logger.Info("Starting resources batcher for Karpenter resources")
	c.batcher.start()

	// Keep this goroutine alive until context cancellation or stop
	stopCh := c.stopCh
	go func() {
		select {
		case <-ctx.Done():
			c.Stop()
		case <-stopCh:
			// Channel was closed by Stop() method
		}
	}()

	return nil
}

// startResourceInformer creates and starts an informer for a specific Karpenter resource
func (c *KarpenterCollector) startResourceInformer(
	ctx context.Context,
	res KarpenterResource,
) error {
	// Create a resource-specific GVR
	gvr := schema.GroupVersionResource{
		Group:    res.GroupVersion.Group,
		Version:  res.GroupVersion.Version,
		Resource: res.Resource,
	}

	// Create a unique key for this resource
	resKey := fmt.Sprintf(
		"%s.%s.%s",
		res.GroupVersion.Group,
		res.GroupVersion.Version,
		res.Resource,
	)

	// First check if the resource exists in the cluster
	_, err := c.dynamicClient.Resource(gvr).List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil {
		// Resource doesn't exist - log at debug level and return without error
		c.logger.V(4).Info("Resource not available in cluster, skipping",
			"group", res.GroupVersion.Group,
			"version", res.GroupVersion.Version,
			"resource", res.Resource)
		return nil
	}

	// Create a dynamic informer factory
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		c.dynamicClient,
		0,  // No resync period
		"", // All namespaces
		nil,
	)

	// Create an informer for this resource
	informer := factory.ForResource(gvr).Informer()

	// Strip managedFields + last-applied-configuration from cached objects.
	if err := informer.SetTransform(StripMetadataTransform); err != nil {
		return fmt.Errorf("failed to set informer transform: %w", err)
	}

	// Add event handlers
	_, err = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				c.logger.Error(nil, "Failed to convert object to unstructured", "resource", resKey)
				return
			}
			c.handleKarpenterResourceEvent(u, res, EventTypeAdd)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			_, ok := oldObj.(*unstructured.Unstructured)
			if !ok {
				c.logger.Error(
					nil,
					"Failed to convert old object to unstructured",
					"resource",
					resKey,
				)
				return
			}

			newU, ok := newObj.(*unstructured.Unstructured)
			if !ok {
				c.logger.Error(
					nil,
					"Failed to convert new object to unstructured",
					"resource",
					resKey,
				)
				return
			}

			c.handleKarpenterResourceEvent(newU, res, EventTypeUpdate)
		},
		DeleteFunc: func(obj interface{}) {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				// Try to handle DeletedFinalStateUnknown
				if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
					if u, ok = tombstone.Obj.(*unstructured.Unstructured); ok {
						c.handleKarpenterResourceEvent(u, res, EventTypeDelete)
						return
					}
				}
				c.logger.Error(nil, "Failed to convert deleted object", "resource", resKey)
				return
			}
			c.handleKarpenterResourceEvent(u, res, EventTypeDelete)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add event handler to informer for %s: %w", resKey, err)
	}

	// Create a stop channel for this informer
	stopCh := make(chan struct{})

	// Store the informer under c.mu: NodeClaimForNode reads this map from the event
	// collector's informer callback, on a different goroutine to this one.
	c.mu.Lock()
	c.informerStopChs[resKey] = stopCh
	c.informers[resKey] = informer
	c.mu.Unlock()

	// Start the informer
	go informer.Run(stopCh)

	// Wait for cache sync with timeout
	syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if !cache.WaitForCacheSync(syncCtx.Done(), informer.HasSynced) {
		return fmt.Errorf("timeout waiting for %s cache to sync", resKey)
	}

	c.logger.Info("Successfully started informer",
		"group", res.GroupVersion.Group,
		"version", res.GroupVersion.Version,
		"resource", res.Resource)

	return nil
}

// handleKarpenterResourceEvent processes Karpenter resource events
func (c *KarpenterCollector) handleKarpenterResourceEvent(
	obj *unstructured.Unstructured,
	resource KarpenterResource,
	eventType EventType,
) {
	name := obj.GetName()
	namespace := obj.GetNamespace()

	// Check if this resource should be excluded
	if c.isExcluded(resource.Resource, namespace, name) {
		return
	}

	// Create a resource-specific key
	var key string
	if namespace != "" {
		key = fmt.Sprintf("%s/%s/%s", resource.Resource, namespace, name)
	} else {
		key = fmt.Sprintf("%s/%s", resource.Resource, name)
	}

	// Process resource based on its kind
	var processedObj map[string]interface{}

	switch resource.Kind {
	case "Provisioner":
		processedObj = c.processProvisioner(obj)
	case "Machine":
		processedObj = c.processMachine(obj)
	case "NodePool":
		processedObj = c.processNodePool(obj)
	case "NodeClaim":
		processedObj = c.processNodeClaim(obj, eventType)
	case "AWSNodeTemplate":
		processedObj = c.processAWSNodeTemplate(obj)
	case "EC2NodeClass":
		processedObj = c.processEC2NodeClass(obj)
	default:
		// Generic processing for unknown types
		processedObj = c.processGenericResource(obj)
	}

	// Send the Karpenter resource to the batch channel
	c.sendBatchResource(CollectedResource{
		ResourceType: Karpenter,
		Object:       processedObj,
		Timestamp:    time.Now(),
		EventType:    eventType,
		Key:          key,
	})
}

// processProvisioner extracts relevant fields from Provisioner objects
func (c *KarpenterCollector) processProvisioner(
	obj *unstructured.Unstructured,
) map[string]interface{} {
	result := c.extractCommonFields(obj)

	// Extract provisioner-specific fields
	limits, found, _ := unstructured.NestedMap(obj.Object, "spec", "limits")
	if found {
		result["limits"] = limits
	}

	requirements, found, _ := unstructured.NestedSlice(obj.Object, "spec", "requirements")
	if found {
		result["requirements"] = requirements
	}

	// Add any status information
	status, found, _ := unstructured.NestedMap(obj.Object, "status")
	if found {
		result["status"] = status
	}

	return result
}

// processMachine extracts relevant fields from Machine objects
func (c *KarpenterCollector) processMachine(obj *unstructured.Unstructured) map[string]interface{} {
	result := c.extractCommonFields(obj)

	// Extract machine-specific fields
	machineClass, found, _ := unstructured.NestedString(obj.Object, "spec", "machineClass")
	if found {
		result["machineClass"] = machineClass
	}

	// Get node name if assigned
	nodeName, found, _ := unstructured.NestedString(obj.Object, "status", "nodeName")
	if found {
		result["nodeName"] = nodeName
	}

	// Get phase
	phase, found, _ := unstructured.NestedString(obj.Object, "status", "phase")
	if found {
		result["phase"] = phase
	}

	// Get conditions
	conditions, found, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if found {
		result["conditions"] = conditions
	}

	return result
}

// processNodePool extracts relevant fields from NodePool objects
func (c *KarpenterCollector) processNodePool(
	obj *unstructured.Unstructured,
) map[string]interface{} {
	result := c.extractCommonFields(obj)

	// Extract nodepool-specific fields
	limits, found, _ := unstructured.NestedMap(obj.Object, "spec", "limits")
	if found {
		result["limits"] = limits
	}

	disruption, found, _ := unstructured.NestedMap(obj.Object, "spec", "disruption")
	if found {
		result["disruption"] = disruption
	}

	template, found, _ := unstructured.NestedMap(obj.Object, "spec", "template")
	if found {
		result["template"] = template
	}

	// Status information
	status, found, _ := unstructured.NestedMap(obj.Object, "status")
	if found {
		result["status"] = status
	}

	return result
}

// processNodeClaim extracts relevant fields from NodeClaim objects. It also emits one
// NodeLifecycleTransition per newly-observed lifecycle-condition transition, which is
// the signal behind the node time-to-Ready report.
func (c *KarpenterCollector) processNodeClaim(
	obj *unstructured.Unstructured,
	eventType EventType,
) map[string]interface{} {
	// A delete carries the object's last-known conditions, which by definition are the
	// ones already recorded — emitting them again would be duplicate work, so just drop
	// the tracked state instead.
	if eventType == EventTypeDelete {
		c.forgetNodeClaimLifecycle(obj.GetName())
	} else {
		c.emitNodeClaimLifecycleTransitions(obj)
	}

	result := c.extractCommonFields(obj)

	// Extract nodeclaim-specific fields
	requirements, found, _ := unstructured.NestedSlice(obj.Object, "spec", "requirements")
	if found {
		result["requirements"] = requirements
	}

	resources, found, _ := unstructured.NestedMap(obj.Object, "spec", "resources")
	if found {
		result["resources"] = resources
	}

	// Status information
	status, found, _ := unstructured.NestedMap(obj.Object, "status")
	if found {
		result["status"] = status
	}

	// Get node name if assigned
	nodeName, found, _ := unstructured.NestedString(obj.Object, "status", "nodeName")
	if found {
		result["nodeName"] = nodeName
	}

	// Get phase
	phase, found, _ := unstructured.NestedString(obj.Object, "status", "phase")
	if found {
		result["phase"] = phase
	}

	return result
}

// emitNodeClaimLifecycleTransitions sends one NodeLifecycleTransition per NodeClaim
// lifecycle condition whose status or lastTransitionTime differs from the last value
// observed for that NodeClaim. The first time a NodeClaim is seen every one of its
// lifecycle conditions counts as a transition, which is what seeds a node's history on
// informer startup; dakr's table is keyed on
// (cluster_id, node_claim_name, condition) so a re-seed after a restart replaces rows
// rather than duplicating them.
func (c *KarpenterCollector) emitNodeClaimLifecycleTransitions(obj *unstructured.Unstructured) {
	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return
	}

	name := obj.GetName()
	labels := obj.GetLabels()
	// Karpenter stamps the resolved instance type and capacity type onto the NodeClaim
	// as labels once the cloud provider has launched it. Both are absent on an
	// unlaunched NodeClaim, which is why the dakr-side columns are nullable.
	instanceType := labels["node.kubernetes.io/instance-type"]
	reservationType := labels["karpenter.sh/capacity-type"]

	nodeName, _, _ := unstructured.NestedString(obj.Object, "status", "nodeName")

	observedAt := time.Now()

	for _, raw := range conditions {
		condition, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}

		condType, _ := condition["type"].(string)
		if !nodeClaimLifecycleConditions[condType] {
			continue
		}

		status, _ := condition["status"].(string)
		lastTransitionTime, _ := condition["lastTransitionTime"].(string)
		// Without a lastTransitionTime there is no timestamp to measure a phase
		// duration against, so the row would be useless to the read path.
		if lastTransitionTime == "" {
			continue
		}

		if !c.recordNodeClaimCondition(name, condType, nodeClaimConditionState{
			status:             status,
			lastTransitionTime: lastTransitionTime,
		}) {
			continue
		}

		object := map[string]interface{}{
			"node_claim_name":      name,
			"condition":            condType,
			"status":               status,
			"last_transition_time": lastTransitionTime,
		}
		// Omit rather than send empty strings: dakr maps a missing key to a NULL
		// column, and "unknown yet" is meaningfully different from "".
		if nodeName != "" {
			object["node_name"] = nodeName
		}
		if instanceType != "" {
			object["instance_type"] = instanceType
		}
		if reservationType != "" {
			object["reservation_type"] = reservationType
		}

		c.sendBatchResource(CollectedResource{
			ResourceType: NodeLifecycleTransition,
			Object:       object,
			Timestamp:    observedAt,
			EventType:    EventTypeAdd,
			Key:          fmt.Sprintf("nodeclaim-lifecycle/%s/%s", name, condType),
		})
	}
}

// recordNodeClaimCondition stores the observed state for one NodeClaim condition and
// reports whether it changed (and therefore should be emitted). The read and the write
// are a single critical section so two concurrent informer callbacks for the same
// NodeClaim cannot both decide to emit the same transition.
func (c *KarpenterCollector) recordNodeClaimCondition(
	nodeClaimName, conditionType string,
	state nodeClaimConditionState,
) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	conditions, ok := c.nodeClaimConditions[nodeClaimName]
	if !ok {
		conditions = make(map[string]nodeClaimConditionState)
		c.nodeClaimConditions[nodeClaimName] = conditions
	}

	if previous, seen := conditions[conditionType]; seen && previous == state {
		return false
	}

	conditions[conditionType] = state
	return true
}

// forgetNodeClaimLifecycle drops the tracked conditions for a deleted NodeClaim.
func (c *KarpenterCollector) forgetNodeClaimLifecycle(nodeClaimName string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.nodeClaimConditions, nodeClaimName)
}

// nodeClaimInformerKeys are the c.informers keys for the NodeClaim CRD, newest API
// version first. A cluster only ever has one of them registered — startResourceInformer
// skips a GVR the API server does not serve — but which one depends on the Karpenter
// version the customer runs, so both are tried.
var nodeClaimInformerKeys = []string{
	"karpenter.sh.v1.nodeclaims",
	"karpenter.sh.v1beta1.nodeclaims",
}

// nodeClaimInformer returns the running NodeClaim informer, or nil when the Karpenter
// CRDs are absent from the cluster or the collector has not started.
func (c *KarpenterCollector) nodeClaimInformer() cache.SharedIndexInformer {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, key := range nodeClaimInformerKeys {
		if informer, ok := c.informers[key]; ok {
			return informer
		}
	}
	return nil
}

// NodeClaimByName returns a NodeClaim by name, or nil if it is not in the informer's
// store. NodeClaims are cluster-scoped, so the name is the whole store key.
//
// The returned object is the informer's cached object and MUST be treated as read-only.
func (c *KarpenterCollector) NodeClaimByName(name string) *unstructured.Unstructured {
	if name == "" {
		return nil
	}

	informer := c.nodeClaimInformer()
	if informer == nil {
		return nil
	}

	obj, exists, err := informer.GetStore().GetByKey(name)
	if err != nil || !exists {
		return nil
	}
	nodeClaim, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil
	}
	return nodeClaim
}

// NodeClaimForNode returns the NodeClaim currently bound to a Node, or nil if the
// Karpenter CRDs are absent, the informer has not started, or no NodeClaim claims that
// Node (an unmanaged node, or one whose NodeClaim has not registered yet).
//
// The returned object is the informer's cached object and MUST be treated as read-only.
//
// This is a linear scan of the NodeClaim store rather than an index lookup. NodeClaims
// are per-node objects, so the store is bounded by cluster size, and the only caller
// runs on Karpenter DisruptionBlocked events — rare enough that adding a status.nodeName
// indexer would cost more in permanent memory than it saves.
func (c *KarpenterCollector) NodeClaimForNode(nodeName string) *unstructured.Unstructured {
	if nodeName == "" {
		return nil
	}

	informer := c.nodeClaimInformer()
	if informer == nil {
		return nil
	}

	for _, obj := range informer.GetStore().List() {
		nodeClaim, ok := obj.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		if claimed, _, _ := unstructured.NestedString(nodeClaim.Object, "status", "nodeName"); claimed == nodeName {
			return nodeClaim
		}
	}
	return nil
}

// processAWSNodeTemplate extracts relevant fields from AWSNodeTemplate objects
func (c *KarpenterCollector) processAWSNodeTemplate(
	obj *unstructured.Unstructured,
) map[string]interface{} {
	result := c.extractCommonFields(obj)

	// Extract AWS-specific fields
	instanceTypes, found, _ := unstructured.NestedSlice(obj.Object, "spec", "instanceTypes")
	if found {
		result["instanceTypes"] = instanceTypes
	}

	subnetSelector, found, _ := unstructured.NestedMap(obj.Object, "spec", "subnetSelector")
	if found {
		result["subnetSelector"] = subnetSelector
	}

	securityGroupSelector, found, _ := unstructured.NestedMap(
		obj.Object,
		"spec",
		"securityGroupSelector",
	)
	if found {
		result["securityGroupSelector"] = securityGroupSelector
	}

	amiFamilies, found, _ := unstructured.NestedSlice(obj.Object, "spec", "amiFamilies")
	if found {
		result["amiFamilies"] = amiFamilies
	}

	return result
}

// processEC2NodeClass extracts relevant fields from EC2NodeClass objects
func (c *KarpenterCollector) processEC2NodeClass(
	obj *unstructured.Unstructured,
) map[string]interface{} {
	result := c.extractCommonFields(obj)

	// Extract EC2NodeClass-specific fields
	instanceTypes, found, _ := unstructured.NestedSlice(obj.Object, "spec", "instanceTypes")
	if found {
		result["instanceTypes"] = instanceTypes
	}

	subnetSelectorTerms, found, _ := unstructured.NestedSlice(
		obj.Object,
		"spec",
		"subnetSelectorTerms",
	)
	if found {
		result["subnetSelectorTerms"] = subnetSelectorTerms
	}

	securityGroupSelectorTerms, found, _ := unstructured.NestedSlice(
		obj.Object,
		"spec",
		"securityGroupSelectorTerms",
	)
	if found {
		result["securityGroupSelectorTerms"] = securityGroupSelectorTerms
	}

	amiSelectorTerms, found, _ := unstructured.NestedSlice(obj.Object, "spec", "amiSelectorTerms")
	if found {
		result["amiSelectorTerms"] = amiSelectorTerms
	}

	userData, found, _ := unstructured.NestedString(obj.Object, "spec", "userData")
	if found && userData != "" {
		result["hasUserData"] = true
	}

	role, found, _ := unstructured.NestedString(obj.Object, "spec", "role")
	if found {
		result["role"] = role
	}

	// Check status
	status, found, _ := unstructured.NestedMap(obj.Object, "status")
	if found {
		result["status"] = status
	}

	return result
}

// processGenericResource provides basic processing for unknown resource types
func (c *KarpenterCollector) processGenericResource(
	obj *unstructured.Unstructured,
) map[string]interface{} {
	return c.extractCommonFields(obj)
}

// extractCommonFields gets fields common to all Karpenter resources
func (c *KarpenterCollector) extractCommonFields(
	obj *unstructured.Unstructured,
) map[string]interface{} {
	// Basic metadata
	result := map[string]interface{}{
		"name":              obj.GetName(),
		"karpenterVersion":  c.version,
		"namespace":         obj.GetNamespace(),
		"uid":               string(obj.GetUID()),
		"kind":              obj.GetKind(),
		"apiVersion":        obj.GetAPIVersion(),
		"labels":            obj.GetLabels(),
		"annotations":       obj.GetAnnotations(),
		"creationTimestamp": obj.GetCreationTimestamp().Unix(),
		"resourceVersion":   obj.GetResourceVersion(),
		"raw":               obj,
	}

	// Get owner references if any
	if owners := obj.GetOwnerReferences(); len(owners) > 0 {
		ownerRefs := make([]map[string]interface{}, 0, len(owners))
		for _, owner := range owners {
			ownerRefs = append(ownerRefs, map[string]interface{}{
				"apiVersion": owner.APIVersion,
				"kind":       owner.Kind,
				"name":       owner.Name,
				"uid":        string(owner.UID),
			})
		}
		result["ownerReferences"] = ownerRefs
	}

	// Include finalizers if present
	if finalizers := obj.GetFinalizers(); len(finalizers) > 0 {
		result["finalizers"] = finalizers
	}

	// if status exists, pick it up
	status, found, _ := unstructured.NestedMap(obj.Object, "status")
	if found {
		result["status"] = status
	}

	// if spec exists, pick it up
	spec, found, _ := unstructured.NestedMap(obj.Object, "spec")
	if found {
		result["spec"] = spec
	}

	return result
}

// isExcluded checks if a resource should be excluded
func (c *KarpenterCollector) isExcluded(resourceType, namespace, name string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// Check if there are exclusions for this resource type
	excludedNames, exists := c.excludedResources[resourceType]
	if !exists {
		return false
	}

	// For namespaced resources, use namespace/name as the key
	key := name
	if namespace != "" {
		key = fmt.Sprintf("%s/%s", namespace, name)
	}

	return excludedNames[key]
}

// ExcludeResource adds a resource to the exclusion list
func (c *KarpenterCollector) ExcludeResource(resourceType, namespace, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Initialize the exclusion map for this resource type if needed
	if _, exists := c.excludedResources[resourceType]; !exists {
		c.excludedResources[resourceType] = make(map[string]bool)
	}

	// For namespaced resources, use namespace/name as the key
	key := name
	if namespace != "" {
		key = fmt.Sprintf("%s/%s", namespace, name)
	}

	c.excludedResources[resourceType][key] = true
}

// Stop gracefully shuts down all informers
func (c *KarpenterCollector) Stop() error {
	c.logger.Info("Stopping Karpenter collector")

	// Stop all informers, then clear the maps. Held under c.mu because NodeClaimForNode
	// reads c.informers from another goroutine.
	c.mu.Lock()
	for key, stopCh := range c.informerStopChs {
		c.logger.Info("Stopping informer", "resource", key)
		close(stopCh)
	}
	c.informers = make(map[string]cache.SharedIndexInformer)
	c.informerStopChs = make(map[string]chan struct{})
	c.mu.Unlock()

	// Close the main stop channel (signals informers to stop)
	select {
	case <-c.stopCh:
		c.logger.Info("Karpenter collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed Karpenter collector stop channel")
	}

	// Close the batchChan (input to the batcher) — see chanMu's doc comment. Closing the
	// informers' stopChs above only stops them from delivering NEW events; a callback
	// already in flight can still be between sendBatchResource's stopped-check and its
	// send when this runs, which is exactly what chanMu makes safe.
	c.chanMu.Lock()
	c.stopped = true
	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed Karpenter collector batch input channel")
	}
	c.chanMu.Unlock()

	// Stop the batcher (waits for completion).
	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("Karpenter collector batcher stopped")
	}
	// resourceChan is closed by the batcher's defer func.

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *KarpenterCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *KarpenterCollector) GetType() string {
	return "karpenter"
}

// detectKarpenterVersion detects the version of Karpenter from the deployment object
func (c *KarpenterCollector) detectKarpenterVersion(obj *unstructured.Unstructured) {
	containers, found, _ := unstructured.NestedSlice(
		obj.Object,
		"spec",
		"template",
		"spec",
		"containers",
	)
	if !found {
		return
	}

	for _, container := range containers {
		containerMap, ok := container.(map[string]interface{})
		if !ok {
			continue
		}

		name, found, _ := unstructured.NestedString(containerMap, "name")
		if !found || name != "controller" {
			continue
		}

		image, found, _ := unstructured.NestedString(containerMap, "image")
		if !found {
			continue
		}

		// Image format: public.ecr.aws/karpenter/controller:0.37.7@sha256:...
		imageParts := strings.Split(image, "@")[0]
		versionParts := strings.Split(imageParts, ":")
		if len(versionParts) != 2 {
			c.logger.V(4).Info("Invalid image format", "image", image)
			continue
		}

		version := versionParts[1]
		c.mu.Lock()
		c.version = version
		c.mu.Unlock()
		c.logger.Info("Detected Karpenter version", "version", version)
		return
	}

	c.logger.V(4).Info("Could not detect Karpenter version from deployment")
}

// Update IsAvailable to detect version
func (c *KarpenterCollector) IsAvailable(ctx context.Context) bool {
	gvr := schema.GroupVersionResource{
		Group:    "apps",
		Version:  "v1",
		Resource: "deployments",
	}

	labelSelector := karpenterControllerLabelSelector

	deployments, err := c.dynamicClient.Resource(gvr).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		c.logger.Error(err, "Failed to list deployments")
		return false
	}

	if len(deployments.Items) == 0 {
		c.logger.V(1).Info("No Karpenter deployment found")
		return false
	}

	// Check if at least one deployment is ready
	for _, d := range deployments.Items {
		status, found, _ := unstructured.NestedMap(d.Object, "status")
		if found {
			readyReplicas, found, _ := unstructured.NestedInt64(status, "readyReplicas")
			if found && readyReplicas > 0 {
				c.detectKarpenterVersion(&d)
				return true
			}
		}
	}

	c.logger.V(1).Info("No ready Karpenter deployment found")
	return false
}

// determineKarpenterResourceType determines the KarpenterResource type from an unstructured object
func (c *KarpenterCollector) determineKarpenterResourceType(
	obj *unstructured.Unstructured,
) (KarpenterResource, error) {
	kind := obj.GetKind()
	apiVersion := obj.GetAPIVersion()

	switch {
	// old stuff
	case kind == "Provisioner" && strings.Contains(apiVersion, "karpenter.sh/v1alpha5"):
		return KarpenterResource{
			GroupVersion: schema.GroupVersion{Group: "karpenter.sh", Version: "v1alpha5"},
			Resource:     "provisioners",
			Kind:         "Provisioner",
		}, nil
	case kind == "Machine" && strings.Contains(apiVersion, "karpenter.sh/v1alpha5"):
		return KarpenterResource{
			GroupVersion: schema.GroupVersion{Group: "karpenter.sh", Version: "v1alpha5"},
			Resource:     "machines",
			Kind:         "Machine",
		}, nil

	// default types
	case kind == "NodeClaim" && strings.Contains(apiVersion, "karpenter.sh/v1alpha5"):
		return KarpenterResource{
			GroupVersion: schema.GroupVersion{Group: "karpenter.sh", Version: "v1alpha5"},
			Resource:     "nodeclaims",
			Kind:         "NodeClaim",
		}, nil
	case kind == "NodeClaim" && strings.Contains(apiVersion, "karpenter.sh/v1"):
		return KarpenterResource{
			GroupVersion: schema.GroupVersion{Group: "karpenter.sh", Version: "v1"},
			Resource:     "nodeclaims",
			Kind:         "NodeClaim",
		}, nil
	case kind == "NodeOverlay" && strings.Contains(apiVersion, "karpenter.sh/v1alpha1"):
		return KarpenterResource{
			GroupVersion: schema.GroupVersion{Group: "karpenter.sh", Version: "v1alpha1"},
			Resource:     "nodeoverlays",
			Kind:         "NodeOverlay",
		}, nil
	case kind == "NodePool" && strings.Contains(apiVersion, "karpenter.sh/v1beta1"):
		return KarpenterResource{
			GroupVersion: schema.GroupVersion{Group: "karpenter.sh", Version: "v1beta1"},
			Resource:     "nodepools",
			Kind:         "NodePool",
		}, nil
	case kind == "NodePool" && strings.Contains(apiVersion, "karpenter.sh/v1"):
		return KarpenterResource{
			GroupVersion: schema.GroupVersion{Group: "karpenter.sh", Version: "v1"},
			Resource:     "nodepools",
			Kind:         "NodePool",
		}, nil

	// aws specific
	case kind == "AWSNodeTemplate" && strings.Contains(apiVersion, "karpenter.k8s.aws/v1alpha1"):
		return KarpenterResource{
			GroupVersion: schema.GroupVersion{Group: "karpenter.k8s.aws", Version: "v1alpha1"},
			Resource:     "awsnodetemplates",
			Kind:         "AWSNodeTemplate",
		}, nil
	case kind == "EC2NodeClass" && strings.Contains(apiVersion, "karpenter.k8s.aws/v1beta1"):
		return KarpenterResource{
			GroupVersion: schema.GroupVersion{Group: "karpenter.k8s.aws", Version: "v1beta1"},
			Resource:     "ec2nodeclasses",
			Kind:         "EC2NodeClass",
		}, nil
	case kind == "EC2NodeClass" && strings.Contains(apiVersion, "karpenter.k8s.aws/v1"):
		return KarpenterResource{
			GroupVersion: schema.GroupVersion{Group: "karpenter.k8s.aws", Version: "v1"},
			Resource:     "ec2nodeclasses",
			Kind:         "EC2NodeClass",
		}, nil

	// azure specific
	case kind == "AKSNodeClass" && strings.Contains(apiVersion, "karpenter.azure.com/v1alpha2"):
		return KarpenterResource{
			GroupVersion: schema.GroupVersion{Group: "karpenter.azure.com", Version: "v1alpha2"},
			Resource:     "aksnodeclasses",
			Kind:         "AKSNodeClass",
		}, nil

	// oracle specific
	case kind == "OciNodeClass" && strings.Contains(apiVersion, "karpenter.k8s.oracle/v1alpha1"):
		return KarpenterResource{
			GroupVersion: schema.GroupVersion{Group: "karpenter.k8s.oracle", Version: "v1alpha1"},
			Resource:     "ocinodeclasses",
			Kind:         "OciNodeClass",
		}, nil

	// gcp specific
	case kind == "GCENodeClass" && strings.Contains(apiVersion, "karpenter.k8s.gcp/v1alpha1"):
		return KarpenterResource{
			GroupVersion: schema.GroupVersion{Group: "karpenter.k8s.gcp", Version: "v1alpha1"},
			Resource:     "gcenodeclasses",
			Kind:         "GCENodeClass",
		}, nil

	default:
		return KarpenterResource{}, fmt.Errorf(
			"unsupported Karpenter resource: kind=%s, apiVersion=%s",
			kind,
			apiVersion,
		)
	}
}

func (c *KarpenterCollector) sendInstallationMetric(obj *unstructured.Unstructured) {
	// Extract required fields from the deployment object
	uid := string(obj.GetUID())
	name := obj.GetName()
	namespace := obj.GetNamespace()
	apiVersion := obj.GetAPIVersion()
	resourceVersion := obj.GetResourceVersion()
	creationTimestamp := obj.GetCreationTimestamp().Unix()

	// Get spec and status
	spec, found, _ := unstructured.NestedMap(obj.Object, "spec")
	if !found {
		spec = make(map[string]interface{})
	}

	status, found, _ := unstructured.NestedMap(obj.Object, "status")
	if !found {
		status = make(map[string]interface{})
	}

	// Create installation status object matching the model structure
	installStatus := map[string]interface{}{
		"kind":              obj.GetKind(),
		"apiVersion":        apiVersion,
		"name":              name,
		"namespace":         namespace,
		"uid":               uid,
		"resourceVersion":   resourceVersion,
		"creationTimestamp": creationTimestamp,
		"labels":            obj.GetLabels(),
		"annotations":       obj.GetAnnotations(),
		"spec":              spec,
		"status":            status,
		"raw":               obj.Object,
		"karpenterVersion":  c.version,
	}

	if owners := obj.GetOwnerReferences(); len(owners) > 0 {
		ownerRefs := make([]map[string]interface{}, 0, len(owners))
		for _, owner := range owners {
			ownerRefs = append(ownerRefs, map[string]interface{}{
				"apiVersion": owner.APIVersion,
				"kind":       owner.Kind,
				"name":       owner.Name,
				"uid":        string(owner.UID),
			})
		}
		installStatus["ownerReferences"] = ownerRefs
	}

	c.sendBatchResource(CollectedResource{
		ResourceType: Karpenter,
		Object:       installStatus,
		Timestamp:    time.Now(),
		EventType:    EventTypeAdd,
		Key:          fmt.Sprintf("karpenter/installation/%s", uid),
	})

	c.logger.Info("Sent Karpenter installation metric",
		"Object", installStatus)
}

// AddResource manually adds a Karpenter resource to be processed by the collector
func (c *KarpenterCollector) AddResource(resource interface{}) error {
	obj, ok := resource.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("expected *unstructured.Unstructured, got %T", resource)
	}

	// Use helper method to determine the resource type
	karpenterResource, err := c.determineKarpenterResourceType(obj)
	if err != nil {
		return err
	}

	c.handleKarpenterResourceEvent(obj, karpenterResource, EventTypeAdd)
	return nil
}
