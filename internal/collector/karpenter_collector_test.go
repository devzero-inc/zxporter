package collector

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

// newTestKarpenterCollector builds a minimal KarpenterCollector sufficient to exercise
// processNodeClaim's lifecycle-transition emission in isolation, without the informer /
// dynamic-client / batcher dependency graph NewKarpenterCollector needs.
func newTestKarpenterCollector() (*KarpenterCollector, chan CollectedResource) {
	batchChan := make(chan CollectedResource, 32)
	return &KarpenterCollector{
		batchChan:           batchChan,
		logger:              logr.Discard(),
		nodeClaimConditions: make(map[string]map[string]nodeClaimConditionState),
	}, batchChan
}

// nodeClaimCondition is a NodeClaim status condition as it arrives from the dynamic
// informer: an untyped map, not a typed Go struct.
func nodeClaimCondition(condType, status, lastTransitionTime string) map[string]interface{} {
	return map[string]interface{}{
		"type":               condType,
		"status":             status,
		"lastTransitionTime": lastTransitionTime,
	}
}

// nodeClaim builds an unstructured Karpenter NodeClaim with the given conditions.
// nodeName is omitted when empty, matching a NodeClaim that has not yet bound a Node.
func nodeClaim(name, nodeName string, conditions ...map[string]interface{}) *unstructured.Unstructured {
	raw := make([]interface{}, 0, len(conditions))
	for _, c := range conditions {
		raw = append(raw, c)
	}

	status := map[string]interface{}{"conditions": raw}
	if nodeName != "" {
		status["nodeName"] = nodeName
	}

	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "karpenter.sh/v1",
			"kind":       "NodeClaim",
			"metadata": map[string]interface{}{
				"name": name,
				"labels": map[string]interface{}{
					"node.kubernetes.io/instance-type": "m7g.large",
					"karpenter.sh/capacity-type":       "spot",
				},
			},
			"status": status,
		},
	}
}

// drainTransitions returns every NodeLifecycleTransition currently buffered on the
// batch channel, keyed by condition type. Non-lifecycle resources (the NodeClaim's own
// Karpenter row) are ignored — those are emitted by handleKarpenterResourceEvent, not
// by the code under test.
func drainTransitions(t *testing.T, batchChan chan CollectedResource) map[string]map[string]interface{} {
	t.Helper()

	transitions := make(map[string]map[string]interface{})
	for {
		select {
		case resource := <-batchChan:
			if resource.ResourceType != NodeLifecycleTransition {
				continue
			}
			object, ok := resource.Object.(map[string]interface{})
			require.True(t, ok, "transition object should be a map, got %T", resource.Object)
			condition, ok := object["condition"].(string)
			require.True(t, ok, "transition should carry a condition")
			_, duplicate := transitions[condition]
			require.False(t, duplicate, "condition %q emitted more than once", condition)
			transitions[condition] = object
		default:
			return transitions
		}
	}
}

// TestProcessNodeClaim_EmitsOneTransitionPerNewlyObservedCondition walks a NodeClaim
// through the sequence a real one takes — first observation, a new condition appearing,
// an existing condition flipping, and a resync that changes nothing — asserting each
// step emits exactly the transitions that are actually new. The unchanged-resync case
// is the important one: informers redeliver the same object indefinitely, and
// re-emitting every condition each time would flood the table with rows that all claim
// to be the same transition.
func TestProcessNodeClaim_EmitsOneTransitionPerNewlyObservedCondition(t *testing.T) {
	c, batchChan := newTestKarpenterCollector()

	launchedAt := "2026-08-03T10:00:00Z"
	registeredAt := "2026-08-03T10:00:42.5Z"
	readyAt := "2026-08-03T10:01:07Z"

	// First observation: both present conditions are new.
	firstSeen := nodeClaim("nodeclaim-abc", "",
		nodeClaimCondition("Launched", "True", launchedAt),
		nodeClaimCondition("Ready", "Unknown", launchedAt),
	)
	c.processNodeClaim(firstSeen, EventTypeAdd)

	transitions := drainTransitions(t, batchChan)
	require.Len(t, transitions, 2)
	assert.Equal(t, "True", transitions["Launched"]["status"])
	assert.Equal(t, launchedAt, transitions["Launched"]["last_transition_time"])
	assert.Equal(t, "nodeclaim-abc", transitions["Launched"]["node_claim_name"])
	assert.Equal(t, "m7g.large", transitions["Launched"]["instance_type"])
	assert.Equal(t, "spot", transitions["Launched"]["reservation_type"])
	// No Node bound yet, so node_name is absent rather than empty — dakr maps a
	// missing key to a NULL column.
	assert.NotContains(t, transitions["Launched"], "node_name")
	assert.Equal(t, "Unknown", transitions["Ready"]["status"])

	// A Node binds and Registered appears; Launched is unchanged and must not repeat.
	registered := nodeClaim("nodeclaim-abc", "ip-10-0-1-5.ec2.internal",
		nodeClaimCondition("Launched", "True", launchedAt),
		nodeClaimCondition("Registered", "True", registeredAt),
		nodeClaimCondition("Ready", "Unknown", launchedAt),
	)
	c.processNodeClaim(registered, EventTypeUpdate)

	transitions = drainTransitions(t, batchChan)
	require.Len(t, transitions, 1)
	assert.Equal(t, registeredAt, transitions["Registered"]["last_transition_time"])
	assert.Equal(t, "ip-10-0-1-5.ec2.internal", transitions["Registered"]["node_name"])

	// Ready flips Unknown -> True with a new timestamp: one transition, for Ready only.
	ready := nodeClaim("nodeclaim-abc", "ip-10-0-1-5.ec2.internal",
		nodeClaimCondition("Launched", "True", launchedAt),
		nodeClaimCondition("Registered", "True", registeredAt),
		nodeClaimCondition("Ready", "True", readyAt),
	)
	c.processNodeClaim(ready, EventTypeUpdate)

	transitions = drainTransitions(t, batchChan)
	require.Len(t, transitions, 1)
	assert.Equal(t, "True", transitions["Ready"]["status"])
	assert.Equal(t, readyAt, transitions["Ready"]["last_transition_time"])

	// A resync redelivering the identical object emits nothing.
	c.processNodeClaim(ready, EventTypeUpdate)
	assert.Empty(t, drainTransitions(t, batchChan))
}

// TestProcessNodeClaim_IgnoresNonLifecycleAndTimestamplessConditions asserts the
// filtering, since a NodeClaim carries conditions this signal has no column for
// (Drifted, Expired, ...) and dakr's Enum8 would reject them at insert time.
func TestProcessNodeClaim_IgnoresNonLifecycleAndTimestamplessConditions(t *testing.T) {
	c, batchChan := newTestKarpenterCollector()

	claim := nodeClaim("nodeclaim-def", "",
		nodeClaimCondition("Drifted", "True", "2026-08-03T10:00:00Z"),
		nodeClaimCondition("Expired", "False", "2026-08-03T10:00:00Z"),
		// A lifecycle condition with no lastTransitionTime has no timestamp to
		// measure a phase duration against.
		nodeClaimCondition("Initialized", "True", ""),
		nodeClaimCondition("Launched", "True", "2026-08-03T10:00:00Z"),
	)
	c.processNodeClaim(claim, EventTypeAdd)

	transitions := drainTransitions(t, batchChan)
	require.Len(t, transitions, 1)
	assert.Contains(t, transitions, "Launched")
}

// TestProcessNodeClaim_ReemitsAfterDeleteAndRecreate covers NodeClaim name reuse: once
// a NodeClaim is deleted its tracked state is dropped, so a later NodeClaim with the
// same name is a genuinely new node whose conditions must all be reported again.
func TestProcessNodeClaim_ReemitsAfterDeleteAndRecreate(t *testing.T) {
	c, batchChan := newTestKarpenterCollector()

	claim := nodeClaim("nodeclaim-ghi", "",
		nodeClaimCondition("Launched", "True", "2026-08-03T10:00:00Z"),
	)
	c.processNodeClaim(claim, EventTypeAdd)
	require.Len(t, drainTransitions(t, batchChan), 1)

	// The delete itself reports nothing — its conditions are already recorded.
	c.processNodeClaim(claim, EventTypeDelete)
	require.Empty(t, drainTransitions(t, batchChan))

	c.processNodeClaim(claim, EventTypeAdd)
	assert.Len(t, drainTransitions(t, batchChan), 1)
}

// TestNodeLifecycleTransitionProtoType pins the ResourceType -> proto mapping, which is
// what routes these rows to dakr's node_lifecycle_transitions converter.
func TestNodeLifecycleTransitionProtoType(t *testing.T) {
	assert.Equal(
		t,
		"RESOURCE_TYPE_NODE_LIFECYCLE_TRANSITION",
		NodeLifecycleTransition.ProtoType().String(),
	)
	assert.Equal(t, "node_lifecycle_transition", NodeLifecycleTransition.String())
}

// --- availability and controller discovery ----------------------------------
//
// karpenterTestGVRs is the collector's watch roster spelled out as literals,
// mirroring flaggerKinds: IsAvailable's contract is defined against this list,
// so a drift between the production roster and the one the availability probe
// walks should surface here.
var karpenterTestGVRs = []schema.GroupVersionResource{
	{Group: "karpenter.sh", Version: "v1alpha5", Resource: "provisioners"},
	{Group: "karpenter.sh", Version: "v1alpha5", Resource: "machines"},
	{Group: "karpenter.azure.com", Version: "v1alpha2", Resource: "aksnodeclasses"},
	{Group: "karpenter.k8s.aws", Version: "v1alpha1", Resource: "awsnodetemplates"},
	{Group: "karpenter.sh", Version: "v1alpha1", Resource: "nodeoverlays"},
	{Group: "karpenter.k8s.oracle", Version: "v1alpha1", Resource: "ocinodeclasses"},
	{Group: "karpenter.k8s.gcp", Version: "v1alpha1", Resource: "gcenodeclasses"},
	{Group: "karpenter.sh", Version: "v1beta1", Resource: "nodepools"},
	{Group: "karpenter.sh", Version: "v1beta1", Resource: "nodeclaims"},
	{Group: "karpenter.k8s.aws", Version: "v1beta1", Resource: "ec2nodeclasses"},
	{Group: "karpenter.azure.com", Version: "v1beta1", Resource: "aksnodeclasses"},
	{Group: "karpenter.sh", Version: "v1", Resource: "nodepools"},
	{Group: "karpenter.sh", Version: "v1", Resource: "nodeclaims"},
	{Group: "karpenter.k8s.aws", Version: "v1", Resource: "ec2nodeclasses"},
	// Oracle's official KPO provider serves its node class from its own API
	// group — a different group than zoom's fork for the same concept.
	{Group: "oci.oraclecloud.com", Version: "v1beta1", Resource: "ocinodeclasses"},
}

// karpenterControllerDeployment builds an unstructured Karpenter controller
// Deployment the way a provider chart renders one: chart-name label
// app.kubernetes.io/name=karpenter, release-name label
// app.kubernetes.io/instance=<releaseName>, a ready-replica count, and the
// container image detectKarpenterVersion parses the version out of.
func karpenterControllerDeployment(name, namespace, releaseName string, readyReplicas int64, image string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": namespace,
			"uid":       "controller-uid-1",
			"labels": map[string]interface{}{
				"app.kubernetes.io/name":     "karpenter",
				"app.kubernetes.io/instance": releaseName,
				"helm.sh/chart":              "karpenter-1.4.1",
			},
		},
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{map[string]interface{}{
						"name":  "controller",
						"image": image,
					}},
				},
			},
		},
		"status": map[string]interface{}{"readyReplicas": readyReplicas},
	}}
}

// newKarpenterTestCollector builds a collector over a fake API server that
// serves every GVR in karpenterTestGVRs plus the controller Deployments handed
// in controllers. The fake panics on a GVR with no registered list kind, so
// every GVR is registered and "the CRD is not installed" is expressed as a
// NotFound reactor per resource (same shape as newFlaggerTestCollector). Note
// the fake matches reactors by resource name only, so a NotFound reactor on
// "nodeclaims" covers both karpenter.sh/v1 and /v1beta1 — precise enough here,
// where an absent resource name means absent in every group/version that
// serves it.
//
// Deployments are served through a dedicated reactor because the fake ignores
// label selectors on List: without faithful selector matching, a Deployment
// whose labels do NOT satisfy the collector's selector would still be
// returned, and "the selector missed it" — the production failure — would be
// indistinguishable from a match.
func newKarpenterTestCollector(t *testing.T, absent []schema.GroupVersionResource, controllers []*unstructured.Unstructured, objects ...runtime.Object) *KarpenterCollector {
	t.Helper()

	listKinds := map[schema.GroupVersionResource]string{}
	for _, gvr := range karpenterTestGVRs {
		listKinds[gvr] = strings.TrimSuffix(gvr.Resource, "s") + "List"
	}
	listKinds[schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}] = "DeploymentList"

	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, objects...)
	client.PrependReactor("list", "deployments", func(action clienttesting.Action) (bool, runtime.Object, error) {
		selector := action.(clienttesting.ListAction).GetListRestrictions().Labels
		list := &unstructured.UnstructuredList{}
		for i := range controllers {
			if selector.Matches(labels.Set(controllers[i].GetLabels())) {
				list.Items = append(list.Items, *controllers[i])
			}
		}
		return true, list, nil
	})
	for _, missing := range absent {
		client.PrependReactor("list", missing.Resource, func(action clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewNotFound(action.GetResource().GroupResource(), "")
		})
	}

	return NewKarpenterCollector(client, 10, 10*time.Millisecond, logr.Discard(), nil)
}

// drainCollectedBatches empties the collector's batched-output channel without
// blocking, tolerating a closed channel.
func drainCollectedBatches(ch <-chan []CollectedResource) []CollectedResource {
	var all []CollectedResource
	for {
		select {
		case batch, ok := <-ch:
			if !ok {
				return all
			}
			all = append(all, batch...)
		default:
			return all
		}
	}
}

// awaitCollectedKeys polls the batched-output channel until every key in
// wantKeys has arrived or the deadline passes, then returns everything seen.
// The batcher flushes on a timer and informer handlers dispatch on their own
// goroutines, so a fixed sleep would be a flake waiting to happen.
func awaitCollectedKeys(t *testing.T, ch <-chan []CollectedResource, wantKeys ...string) []CollectedResource {
	t.Helper()

	var all []CollectedResource
	deadline := time.Now().Add(2 * time.Second)
	for {
		all = append(all, drainCollectedBatches(ch)...)

		have := make(map[string]bool)
		for _, resource := range all {
			have[resource.Key] = true
		}
		complete := true
		for _, key := range wantKeys {
			if !have[key] {
				complete = false
			}
		}
		if complete || time.Now().After(deadline) {
			return all
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestKarpenterCollector_IsAvailableGatesOnServedCRDsNotControllerLabels pins
// the production incident of 2026-11-02: cluster apm1.dp.navi-tech.in (OKE,
// zoom/karpenter-oci fork) served karpenter.sh/v1 NodePools/NodeClaims and
// karpenter.k8s.oracle/v1alpha1 OciNodeClasses with 451 Karpenter-owned nodes,
// but the collector never started — its availability gate matched the
// controller Deployment only by Helm release name
// (`app.kubernetes.io/instance in (karpenter,dzkarp)`), and the fork was
// released as "karpenter-oci". No karpenter resource was ever collected, and
// the gate's failure mode logged at V(1) locally, invisibly.
//
// The contract this pins: availability is the API server serving a watched
// CRD — what the collector collects — never how the controller was deployed.
func TestKarpenterCollector_IsAvailableGatesOnServedCRDsNotControllerLabels(t *testing.T) {
	ctx := context.Background()

	t.Run("karpenter CRDs served, controller released under a nonstandard name", func(t *testing.T) {
		incidentController := karpenterControllerDeployment(
			"karpenter-apm1-dp-navi-tech", "kube-system", "karpenter-oci", 2,
			"public.ecr.aws/karpenter/controller:0.37.7",
		)
		c := newKarpenterTestCollector(t, nil, []*unstructured.Unstructured{incidentController}, nodeClaim("nodeclaim-abc", ""))

		assert.True(t, c.IsAvailable(ctx),
			"a cluster serving karpenter.sh/v1 NodeClaims must be available whatever the controller Deployment's release name says")
	})

	t.Run("no karpenter CRDs served even with a ready labeled controller", func(t *testing.T) {
		classicController := karpenterControllerDeployment(
			"karpenter", "karpenter", "karpenter", 1,
			"public.ecr.aws/karpenter/controller:0.37.7",
		)
		c := newKarpenterTestCollector(t, karpenterTestGVRs, []*unstructured.Unstructured{classicController})

		assert.False(t, c.IsAvailable(ctx),
			"a cluster serving none of the watched GVRs must not run the collector, whatever the controller Deployment says")
	})
}

// TestKarpenterCollector_StartCollectsUnderNonstandardReleaseName walks the
// full Start path over the incident shape: the controller Deployment is found
// through the chart-name label (not the release name), so its installation
// metric carries the detected version, and the served NodeClaim is collected
// through the informer. On the production cluster the collection half of this
// never happened because IsAvailable never let the collector start; the
// discovery half failed too, so even the installation metric was absent from
// k8s_karpenter_resources.
func TestKarpenterCollector_StartCollectsUnderNonstandardReleaseName(t *testing.T) {
	ctx := context.Background()

	incidentController := karpenterControllerDeployment(
		"karpenter-apm1-dp-navi-tech", "kube-system", "karpenter-oci", 2,
		"public.ecr.aws/karpenter/controller:0.37.7",
	)
	c := newKarpenterTestCollector(t, nil, []*unstructured.Unstructured{incidentController}, nodeClaim("nodeclaim-abc", ""))

	require.NoError(t, c.Start(ctx))
	t.Cleanup(func() {
		_ = c.Stop()
	})

	resources := awaitCollectedKeys(t, c.GetResourceChannel(),
		"karpenter/installation/controller-uid-1",
		"nodeclaims/nodeclaim-abc",
	)

	var installation, nodeClaimRow *CollectedResource
	for i := range resources {
		switch resources[i].Key {
		case "karpenter/installation/controller-uid-1":
			installation = &resources[i]
		case "nodeclaims/nodeclaim-abc":
			nodeClaimRow = &resources[i]
		}
	}

	require.NotNil(t, installation,
		"the controller found through the chart-name label must be reported as the Karpenter installation")
	installObj, ok := installation.Object.(map[string]interface{})
	require.True(t, ok, "installation object should be a map, got %T", installation.Object)
	assert.Equal(t, "0.37.7", installObj["karpenterVersion"])
	assert.Equal(t, "karpenter-apm1-dp-navi-tech", installObj["name"])
	assert.Equal(t, Karpenter, installation.ResourceType)

	require.NotNil(t, nodeClaimRow,
		"the served NodeClaim must be collected — on the incident cluster none of this ever arrived")
	claimObj, ok := nodeClaimRow.Object.(map[string]interface{})
	require.True(t, ok, "node claim object should be a map, got %T", nodeClaimRow.Object)
	assert.Equal(t, "NodeClaim", claimObj["kind"])
	assert.Equal(t, "nodeclaim-abc", claimObj["name"])
	assert.Equal(t, Karpenter, nodeClaimRow.ResourceType)
}

// oracleOciNodeClass builds an unstructured OCINodeClass from Oracle's
// official Karpenter provider (KPO): apiVersion oci.oraclecloud.com/v1beta1,
// kind OCINodeClass — a different API group and Kind spelling than zoom's
// fork (karpenter.k8s.oracle/v1alpha1, OciNodeClass) for the same concept.
func oracleOciNodeClass(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "oci.oraclecloud.com/v1beta1",
		"kind":       "OCINodeClass",
		"metadata": map[string]interface{}{
			"name": name,
			"uid":  "kpo-nodeclass-uid-1",
		},
		"spec": map[string]interface{}{
			"shapeConfigs": []interface{}{map[string]interface{}{
				"ocpus":       "4",
				"memoryInGbs": "16",
			}},
		},
	}}
}

// TestKarpenterWatchedResources_CoversBothOciProviders pins that the watch
// roster carries both OCI providers' node classes: zoom's karpenter-oci fork
// (karpenter.k8s.oracle/v1alpha1) and Oracle's official KPO
// (oci.oraclecloud.com/v1beta1). A cluster can serve either — and during a
// fork-to-KPO migration, both — so a roster that only knows one silently
// drops the other's node classes from inventory.
func TestKarpenterWatchedResources_CoversBothOciProviders(t *testing.T) {
	roster := karpenterWatchedResources()

	byGVR := map[string]KarpenterResource{}
	for _, res := range roster {
		byGVR[res.GroupVersion.String()+" "+res.Resource] = res
	}

	zoom := byGVR["karpenter.k8s.oracle/v1alpha1 ocinodeclasses"]
	require.NotEmpty(t, zoom.Kind, "zoom karpenter-oci fork's OciNodeClass must be watched")
	assert.Equal(t, "OciNodeClass", zoom.Kind)

	kpo, ok := byGVR["oci.oraclecloud.com/v1beta1 ocinodeclasses"]
	require.True(t, ok, "Oracle KPO's OCINodeClass must be watched — it was the one kind the roster was missing")
	// The KPO kind is OCINodeClass — capital OCI — where zoom's is OciNodeClass;
	// the spelling difference is exactly the kind switch in
	// handleKarpenterResourceEvent keys on.
	assert.Equal(t, "OCINodeClass", kpo.Kind)
}

// TestKarpenterCollector_StartCollectsOracleKpoNodeClass walks a full Start
// over a cluster serving only Oracle KPO's node-class CRD alongside the core
// karpenter.sh CRDs: the informer must exist for the KPO GVR and the class
// must arrive as a Karpenter resource. Before the roster carried the GVR the
// object sat in the API server uncollected — the same silent gap as the
// availability-gate incident, one layer down.
func TestKarpenterCollector_StartCollectsOracleKpoNodeClass(t *testing.T) {
	ctx := context.Background()

	c := newKarpenterTestCollector(t, nil, nil, oracleOciNodeClass("mirror-node-class"))

	require.NoError(t, c.Start(ctx))
	t.Cleanup(func() {
		_ = c.Stop()
	})

	resources := awaitCollectedKeys(t, c.GetResourceChannel(),
		"ocinodeclasses/mirror-node-class")

	var classRow *CollectedResource
	for i := range resources {
		if resources[i].Key == "ocinodeclasses/mirror-node-class" {
			classRow = &resources[i]
		}
	}

	require.NotNil(t, classRow,
		"Oracle KPO's OCINodeClass must be collected — the roster was missing its GVR")
	classObj, ok := classRow.Object.(map[string]interface{})
	require.True(t, ok, "node class object should be a map, got %T", classRow.Object)
	assert.Equal(t, "OCINodeClass", classObj["kind"])
	assert.Equal(t, "mirror-node-class", classObj["name"])
	assert.Equal(t, Karpenter, classRow.ResourceType)
}
