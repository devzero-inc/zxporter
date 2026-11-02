package collector

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

// flaggerKinds is the roster the controller registers one collector per,
// spelled out as literals. The registration site repeats each (GVR,
// ResourceType) pair four times per kind, which is where a copy-paste pairs one
// kind's GVR with another kind's ResourceType -- and because all three share
// one collector type and one wire shape, nothing downstream can detect the swap.
var flaggerKinds = []struct {
	resourceType ResourceType
	gvr          schema.GroupVersionResource
	wantResource string
	listKind     string
}{
	{FlaggerCanary, FlaggerCanaryGVR, "canaries", "CanaryList"},
	{FlaggerMetricTemplate, FlaggerMetricTemplateGVR, "metrictemplates", "MetricTemplateList"},
	{FlaggerAlertProvider, FlaggerAlertProviderGVR, "alertproviders", "AlertProviderList"},
}

// newFlaggerTestCollector builds a collector over a fake API server that serves
// every Flagger GVR except those in absent, which answer List with a NotFound
// the way a real API server does for an uninstalled CRD. The fake client panics
// rather than erroring on a GVR it has never heard of, so "absent" has to be
// expressed as a reactor, not as an unregistered list kind.
func newFlaggerTestCollector(t *testing.T, gvr schema.GroupVersionResource, resourceType ResourceType, absent []schema.GroupVersionResource, objects ...runtime.Object) *FlaggerResourceCollector {
	t.Helper()

	listKinds := map[schema.GroupVersionResource]string{}
	for _, kind := range flaggerKinds {
		listKinds[kind.gvr] = kind.listKind
	}

	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, objects...)
	for _, missing := range absent {
		client.PrependReactor("list", missing.Resource, func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewNotFound(missing.GroupResource(), "")
		})
	}

	return NewFlaggerResourceCollector(client, gvr, resourceType, nil, nil, 10, time.Second, logr.Discard(), nil)
}

func flaggerObjectFixture(gvr schema.GroupVersionResource, kind, namespace, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": gvr.GroupVersion().String(),
		"kind":       kind,
		"metadata": map[string]interface{}{
			"name":              name,
			"namespace":         namespace,
			"uid":               "flagger-uid-1",
			"resourceVersion":   "77214",
			"creationTimestamp": "2026-09-01T08:30:00Z",
			"labels":            map[string]interface{}{"app": "podinfo"},
			"annotations":       map[string]interface{}{"flagger.app/primary": "podinfo-primary"},
		},
		"spec": map[string]interface{}{
			"provider": "istio",
			"analysis": map[string]interface{}{"interval": "1m", "threshold": int64(5)},
		},
		"status": map[string]interface{}{
			"phase":        "Progressing",
			"canaryWeight": int64(10),
			"failedChecks": int64(0),
		},
	}}
}

// One collector type serves all three kinds, so the GVR and the ResourceType
// are the only things that distinguish two instances. Pairing them wrongly
// ships one kind's objects to DAKR under another kind's label, which the send
// path cannot detect: both are the same wire shape.
func TestFlaggerResourceCollector_EachKindKeepsItsOwnGVRAndResourceType(t *testing.T) {
	require.Len(t, flaggerKinds, 3, "three Flagger kinds are collected; this roster drifted")

	seenGVR := map[schema.GroupVersionResource]ResourceType{}
	seenType := map[ResourceType]schema.GroupVersionResource{}

	for _, kind := range flaggerKinds {
		t.Run(kind.resourceType.String(), func(t *testing.T) {
			c := NewFlaggerResourceCollector(nil, kind.gvr, kind.resourceType, nil, nil, 10, time.Second, logr.Discard(), nil)

			assert.Equal(t, kind.gvr, c.gvr)
			assert.Equal(t, kind.resourceType, c.resourceType)
			assert.Equal(t, kind.resourceType.String(), c.GetType(),
				"GetType is the key the manager stores the collector under and the disabled-collectors policy matches on")
			assert.True(t, strings.HasPrefix(c.GetType(), "flagger_"))

			assert.Equal(t, "flagger.app", kind.gvr.Group)
			assert.Equal(t, "v1beta1", kind.gvr.Version)
			assert.Equal(t, kind.wantResource, kind.gvr.Resource)
		})

		if previous, ok := seenGVR[kind.gvr]; ok {
			t.Fatalf("GVR %s is shared by %s and %s", kind.gvr, previous, kind.resourceType)
		}
		seenGVR[kind.gvr] = kind.resourceType

		if previous, ok := seenType[kind.resourceType]; ok {
			t.Fatalf("ResourceType %s is shared by %s and %s", kind.resourceType, previous, kind.gvr)
		}
		seenType[kind.resourceType] = kind.gvr
	}
}

// Flagger is an optional add-on, so most clusters serve none of these GVRs.
// This is the live case behind "Collector not available, skipping registration",
// which is what puts the kind on the controller's pending-retry list rather
// than failing startup.
func TestFlaggerResourceCollector_IsAvailableFollowsTheServedGVRs(t *testing.T) {
	t.Run("a served GVR is available", func(t *testing.T) {
		c := newFlaggerTestCollector(t, FlaggerCanaryGVR, FlaggerCanary, nil)
		assert.True(t, c.IsAvailable(context.Background()))
	})

	t.Run("a cluster without Flagger installed serves none of them", func(t *testing.T) {
		allAbsent := []schema.GroupVersionResource{FlaggerCanaryGVR, FlaggerMetricTemplateGVR, FlaggerAlertProviderGVR}
		for _, kind := range flaggerKinds {
			c := newFlaggerTestCollector(t, kind.gvr, kind.resourceType, allAbsent)
			assert.False(t, c.IsAvailable(context.Background()), "%s", kind.resourceType)
		}
	})
}

// A missing CRD must leave Start a no-op that returns nil: the manager treats a
// non-nil error as a startup failure, so erroring here would take zxporter down
// on every cluster that has not installed Flagger -- which is most of them.
func TestFlaggerResourceCollector_StartOnAnAbsentCRDIsANoOpNotAnError(t *testing.T) {
	c := newFlaggerTestCollector(t, FlaggerCanaryGVR, FlaggerCanary, []schema.GroupVersionResource{FlaggerCanaryGVR})

	require.NoError(t, c.Start(context.Background()))
	assert.Empty(t, c.batchChan, "a skipped collector must not emit resources")

	select {
	case batch := <-c.resourceChan:
		t.Fatalf("a skipped collector emitted a batch: %v", batch)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestFlaggerResourceCollector_StartOnAServedCRDDeliversExistingObjects(t *testing.T) {
	existing := flaggerObjectFixture(FlaggerCanaryGVR, "Canary", "test", "podinfo")
	c := newFlaggerTestCollector(t, FlaggerCanaryGVR, FlaggerCanary, nil, existing)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, c.Start(ctx))
	defer c.Stop() //nolint:errcheck

	select {
	case batch := <-c.GetResourceChannel():
		require.Len(t, batch, 1)
		assert.Equal(t, FlaggerCanary, batch[0].ResourceType)
		assert.Equal(t, EventTypeAdd, batch[0].EventType)
		assert.Equal(t, "test/podinfo", batch[0].Key)
	case <-time.After(10 * time.Second):
		t.Fatal("expected the informer's initial list to reach the resource channel")
	}
}

// DAKR's Flagger converter unmarshals the raw object into the upstream typed
// struct, so the envelope has to carry the whole thing alongside the identity
// fields -- dropping raw would leave every typed column empty with no error
// anywhere, and DAKR rejects a nil raw with InvalidArgument.
func TestFlaggerResourceCollector_HandleEventEmitsTheIdentityEnvelope(t *testing.T) {
	obj := flaggerObjectFixture(FlaggerCanaryGVR, "Canary", "test", "podinfo")
	c := NewFlaggerResourceCollector(nil, FlaggerCanaryGVR, FlaggerCanary, nil, nil, 10, time.Second, logr.Discard(), nil)

	c.handleEvent(obj, EventTypeUpdate)

	select {
	case res := <-c.batchChan:
		assert.Equal(t, FlaggerCanary, res.ResourceType)
		assert.Equal(t, EventTypeUpdate, res.EventType)
		assert.Equal(t, "test/podinfo", res.Key)

		payload, ok := res.Object.(map[string]interface{})
		require.True(t, ok, "payload should be a map, got %T", res.Object)
		assert.Equal(t, "Canary", payload["kind"])
		assert.Equal(t, "podinfo", payload["name"])
		assert.Equal(t, "test", payload["namespace"])
		assert.Equal(t, "flagger-uid-1", payload["uid"])
		assert.Equal(t, map[string]string{"app": "podinfo"}, payload["labels"])
		assert.Equal(t, map[string]string{"flagger.app/primary": "podinfo-primary"}, payload["annotations"])
		// metav1.Time decodes to the pod's local zone, so the envelope carries
		// whatever offset that is. dakr re-parses it as RFC3339, so the instant
		// is what has to survive -- not the spelling.
		created, err := time.Parse(time.RFC3339, payload["creationTimestamp"].(string))
		require.NoError(t, err)
		assert.Equal(t, time.Date(2026, 9, 1, 8, 30, 0, 0, time.UTC), created.UTC())
		assert.Equal(t, obj.Object, payload["raw"])
	case <-time.After(time.Second):
		t.Fatal("expected a collected resource on the batch channel")
	}
}

func TestFlaggerResourceCollector_ExcludedObjectIsDropped(t *testing.T) {
	c := NewFlaggerResourceCollector(
		nil, FlaggerCanaryGVR, FlaggerCanary, nil,
		[]ExcludedFlaggerResource{{Namespace: "test", Name: "noisy"}},
		10, time.Second, logr.Discard(), nil,
	)

	c.handleEvent(flaggerObjectFixture(FlaggerCanaryGVR, "Canary", "test", "noisy"), EventTypeAdd)
	assert.Empty(t, c.batchChan, "an excluded object must not reach the batch channel")

	// Same name in another namespace is a different object.
	c.handleEvent(flaggerObjectFixture(FlaggerCanaryGVR, "Canary", "prod", "noisy"), EventTypeAdd)
	assert.Len(t, c.batchChan, 1)
}

// Informer callbacks can still be in flight when the manager stops a collector,
// so an event arriving after Stop has to be dropped rather than sent on a
// closed channel. Stop itself is called from both the manager and the
// context-cancel goroutine in Start, so it has to tolerate being called twice.
func TestFlaggerResourceCollector_StopIsIdempotentAndDropsLaterEvents(t *testing.T) {
	c := NewFlaggerResourceCollector(nil, FlaggerAlertProviderGVR, FlaggerAlertProvider, nil, nil, 10, time.Second, logr.Discard(), nil)

	require.NoError(t, c.Stop())
	require.NoError(t, c.Stop())

	assert.NotPanics(t, func() {
		c.handleEvent(flaggerObjectFixture(FlaggerAlertProviderGVR, "AlertProvider", "test", "on-call"), EventTypeAdd)
	})
}

// Run with -race: senders that win the read lock just before Stop takes the
// write lock must not send on the closed channel, and a sender parked on a full
// batchChan must fall out through stopCh rather than hold the read lock Stop is
// waiting on.
func TestFlaggerResourceCollector_StopIsSafeAgainstInFlightEvents(t *testing.T) {
	c := NewFlaggerResourceCollector(nil, FlaggerMetricTemplateGVR, FlaggerMetricTemplate, nil, nil, 10, time.Second, logr.Discard(), nil)
	obj := flaggerObjectFixture(FlaggerMetricTemplateGVR, "MetricTemplate", "test", "latency")

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 50; j++ {
				c.handleEvent(obj, EventTypeUpdate)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		assert.NoError(t, c.Stop())
	}()
	close(start)
	wg.Wait()
}

func TestFlaggerResourceCollector_AddResourceRejectsAForeignType(t *testing.T) {
	c := NewFlaggerResourceCollector(nil, FlaggerCanaryGVR, FlaggerCanary, nil, nil, 10, time.Second, logr.Discard(), nil)

	require.Error(t, c.AddResource("not an unstructured object"))
	assert.Empty(t, c.batchChan)

	require.NoError(t, c.AddResource(flaggerObjectFixture(FlaggerCanaryGVR, "Canary", "test", "podinfo")))
	assert.Len(t, c.batchChan, 1)
}
