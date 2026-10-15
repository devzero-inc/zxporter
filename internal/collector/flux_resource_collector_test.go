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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

// fluxKinds is the roster the controller registers one collector per, spelled
// out as literals. The registration site repeats each (GVR, ResourceType) pair
// four times per kind, which is where a copy-paste pairs one kind's GVR with
// another kind's ResourceType — and because all eighteen share one collector
// type and one wire shape, nothing downstream can detect the swap.
var fluxKinds = []struct {
	resourceType ResourceType
	gvr          schema.GroupVersionResource
	wantGroup    string
	wantVersion  string
	wantResource string
	listKind     string
}{
	{FluxGitRepository, FluxGitRepositoryGVR, "source.toolkit.fluxcd.io", "v1", "gitrepositories", "GitRepositoryList"},
	{FluxOCIRepository, FluxOCIRepositoryGVR, "source.toolkit.fluxcd.io", "v1", "ocirepositories", "OCIRepositoryList"},
	{FluxHelmRepository, FluxHelmRepositoryGVR, "source.toolkit.fluxcd.io", "v1", "helmrepositories", "HelmRepositoryList"},
	{FluxHelmChart, FluxHelmChartGVR, "source.toolkit.fluxcd.io", "v1", "helmcharts", "HelmChartList"},
	{FluxBucket, FluxBucketGVR, "source.toolkit.fluxcd.io", "v1", "buckets", "BucketList"},
	{FluxKustomization, FluxKustomizationGVR, "kustomize.toolkit.fluxcd.io", "v1", "kustomizations", "KustomizationList"},
	{FluxHelmRelease, FluxHelmReleaseGVR, "helm.toolkit.fluxcd.io", "v2", "helmreleases", "HelmReleaseList"},
	{FluxExternalArtifact, FluxExternalArtifactGVR, "source.toolkit.fluxcd.io", "v1", "externalartifacts", "ExternalArtifactList"},
	{FluxAlert, FluxAlertGVR, "notification.toolkit.fluxcd.io", "v1beta3", "alerts", "AlertList"},
	{FluxProvider, FluxProviderGVR, "notification.toolkit.fluxcd.io", "v1beta3", "providers", "ProviderList"},
	{FluxReceiver, FluxReceiverGVR, "notification.toolkit.fluxcd.io", "v1", "receivers", "ReceiverList"},
	{FluxInstance, FluxInstanceGVR, "fluxcd.controlplane.io", "v1", "fluxinstances", "FluxInstanceList"},
	{FluxReport, FluxReportGVR, "fluxcd.controlplane.io", "v1", "fluxreports", "FluxReportList"},
	{FluxResourceSetInputProvider, FluxResourceSetInputProviderGVR, "fluxcd.controlplane.io", "v1", "resourcesetinputproviders", "ResourceSetInputProviderList"},
	{FluxResourceSet, FluxResourceSetGVR, "fluxcd.controlplane.io", "v1", "resourcesets", "ResourceSetList"},
	{FluxImageRepository, FluxImageRepositoryGVR, "image.toolkit.fluxcd.io", "v1", "imagerepositories", "ImageRepositoryList"},
	{FluxImagePolicy, FluxImagePolicyGVR, "image.toolkit.fluxcd.io", "v1", "imagepolicies", "ImagePolicyList"},
	{FluxImageUpdateAutomation, FluxImageUpdateAutomationGVR, "image.toolkit.fluxcd.io", "v1", "imageupdateautomations", "ImageUpdateAutomationList"},
}

// newFluxTestCollector builds a collector over a fake API server that serves
// every Flux GVR except those in absent, which answer List with a NotFound the
// way a real API server does for an uninstalled CRD. The fake client panics
// rather than erroring on a GVR it has never heard of, so "absent" has to be
// expressed as a reactor, not as an unregistered list kind.
func newFluxTestCollector(t *testing.T, gvr schema.GroupVersionResource, resourceType ResourceType, absent []schema.GroupVersionResource, objects ...runtime.Object) *FluxResourceCollector {
	t.Helper()

	listKinds := map[schema.GroupVersionResource]string{}
	for _, kind := range fluxKinds {
		listKinds[kind.gvr] = kind.listKind
	}

	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, objects...)
	for _, missing := range absent {
		client.PrependReactor("list", missing.Resource, func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewNotFound(missing.GroupResource(), "")
		})
	}

	return NewFluxResourceCollector(client, gvr, resourceType, nil, 10, time.Second, logr.Discard(), nil)
}

func fluxObjectFixture(gvr schema.GroupVersionResource, kind, namespace, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": gvr.GroupVersion().String(),
		"kind":       kind,
		"metadata": map[string]interface{}{
			"name":              name,
			"namespace":         namespace,
			"uid":               "flux-uid-1",
			"resourceVersion":   "884213",
			"creationTimestamp": "2026-09-01T08:30:00Z",
			"labels":            map[string]interface{}{"kustomize.toolkit.fluxcd.io/name": "flux-system"},
			"annotations":       map[string]interface{}{"reconcile.fluxcd.io/requestedAt": "2026-09-15T10:00:00Z"},
		},
		"spec": map[string]interface{}{"interval": "10m0s"},
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{"type": "Ready", "status": "True", "reason": "Succeeded"},
			},
		},
	}}
}

// One collector type serves all eighteen kinds, so the GVR and the ResourceType
// are the only things that distinguish two instances. Pairing them wrongly
// ships one kind's objects to DAKR under another kind's label, which the send
// path cannot detect: both are the same wire shape.
func TestFluxResourceCollector_EachKindKeepsItsOwnGVRAndResourceType(t *testing.T) {
	require.Len(t, fluxKinds, 18, "eighteen Flux kinds are collected; this roster drifted")

	seenGVR := map[schema.GroupVersionResource]ResourceType{}
	seenType := map[ResourceType]schema.GroupVersionResource{}

	for _, kind := range fluxKinds {
		t.Run(kind.resourceType.String(), func(t *testing.T) {
			c := NewFluxResourceCollector(nil, kind.gvr, kind.resourceType, nil, 10, time.Second, logr.Discard(), nil)

			assert.Equal(t, kind.gvr, c.gvr)
			assert.Equal(t, kind.resourceType, c.resourceType)
			assert.Equal(t, kind.resourceType.String(), c.GetType(),
				"GetType is the key the manager stores the collector under and the disabled-collectors policy matches on")
			assert.True(t, strings.HasPrefix(c.GetType(), "flux_"))

			assert.Equal(t, kind.wantGroup, kind.gvr.Group)
			assert.Equal(t, kind.wantVersion, kind.gvr.Version)
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

// The image-automation and Flux Operator CRDs are optional add-ons, so a
// cluster running core Flux serves some of these GVRs and not others. This is
// the live case behind "Collector not available, skipping registration".
func TestFluxResourceCollector_IsAvailableFollowsTheServedGVRs(t *testing.T) {
	t.Run("a served GVR is available", func(t *testing.T) {
		c := newFluxTestCollector(t, FluxKustomizationGVR, FluxKustomization, nil)
		assert.True(t, c.IsAvailable(context.Background()))
	})

	t.Run("a GVR the cluster does not serve is unavailable", func(t *testing.T) {
		// Core Flux without the image-reflector-controller: kustomizations
		// resolve, imagepolicies do not.
		c := newFluxTestCollector(t, FluxImagePolicyGVR, FluxImagePolicy, []schema.GroupVersionResource{FluxImagePolicyGVR})
		assert.False(t, c.IsAvailable(context.Background()))
	})
}

// A missing CRD must leave Start a no-op that returns nil: the manager treats a
// non-nil error as a startup failure, so erroring here would take zxporter down
// on any cluster that has not installed the optional Flux controllers.
func TestFluxResourceCollector_StartOnAnAbsentCRDIsANoOpNotAnError(t *testing.T) {
	c := newFluxTestCollector(t, FluxImageUpdateAutomationGVR, FluxImageUpdateAutomation, []schema.GroupVersionResource{FluxImageUpdateAutomationGVR})

	require.NoError(t, c.Start(context.Background()))
	assert.Empty(t, c.batchChan, "a skipped collector must not emit resources")

	select {
	case batch := <-c.resourceChan:
		t.Fatalf("a skipped collector emitted a batch: %v", batch)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestFluxResourceCollector_StartOnAServedCRDDeliversExistingObjects(t *testing.T) {
	existing := fluxObjectFixture(FluxKustomizationGVR, "Kustomization", "flux-system", "apps")
	c := newFluxTestCollector(t, FluxKustomizationGVR, FluxKustomization, nil, existing)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, c.Start(ctx))
	defer c.Stop() //nolint:errcheck

	select {
	case batch := <-c.GetResourceChannel():
		require.Len(t, batch, 1)
		assert.Equal(t, FluxKustomization, batch[0].ResourceType)
		assert.Equal(t, EventTypeAdd, batch[0].EventType)
		assert.Equal(t, "flux-system/apps", batch[0].Key)
	case <-time.After(10 * time.Second):
		t.Fatal("expected the informer's initial list to reach the resource channel")
	}
}

// DAKR's Flux converters read every stored column out of the raw object, so the
// envelope has to carry the whole thing alongside the identity fields --
// dropping raw would leave every typed column empty with no error anywhere.
func TestFluxResourceCollector_HandleEventEmitsTheIdentityEnvelope(t *testing.T) {
	obj := fluxObjectFixture(FluxGitRepositoryGVR, "GitRepository", "flux-system", "fleet-infra")
	c := NewFluxResourceCollector(nil, FluxGitRepositoryGVR, FluxGitRepository, nil, 10, time.Second, logr.Discard(), nil)

	c.handleEvent(obj, EventTypeUpdate)

	select {
	case res := <-c.batchChan:
		assert.Equal(t, FluxGitRepository, res.ResourceType)
		assert.Equal(t, EventTypeUpdate, res.EventType)
		assert.Equal(t, "flux-system/fleet-infra", res.Key)

		payload, ok := res.Object.(map[string]interface{})
		require.True(t, ok, "payload should be a map, got %T", res.Object)
		assert.Equal(t, "GitRepository", payload["kind"])
		assert.Equal(t, "fleet-infra", payload["name"])
		assert.Equal(t, "flux-system", payload["namespace"])
		assert.Equal(t, "flux-uid-1", payload["uid"])
		assert.Equal(t, map[string]string{"kustomize.toolkit.fluxcd.io/name": "flux-system"}, payload["labels"])
		assert.Equal(t, map[string]string{"reconcile.fluxcd.io/requestedAt": "2026-09-15T10:00:00Z"}, payload["annotations"])
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

// FluxInstance and FluxReport are cluster-scoped, so their key is the bare name.
// A "/name" key would not match the key any other event for the same object
// produces, breaking dedup downstream.
func TestFluxResourceCollector_ClusterScopedKindKeyIsTheBareName(t *testing.T) {
	obj := fluxObjectFixture(FluxInstanceGVR, "FluxInstance", "", "flux")
	unstructured.RemoveNestedField(obj.Object, "metadata", "namespace")
	c := NewFluxResourceCollector(nil, FluxInstanceGVR, FluxInstance, nil, 10, time.Second, logr.Discard(), nil)

	c.handleEvent(obj, EventTypeAdd)

	res := <-c.batchChan
	assert.Equal(t, "flux", res.Key)
	payload := res.Object.(map[string]interface{})
	assert.Empty(t, payload["namespace"])
}

// Informer callbacks can still be in flight when the manager stops a collector,
// so an event arriving after Stop has to be dropped rather than sent on a
// closed channel. Stop itself is called from both the manager and the
// context-cancel goroutine in Start, so it has to tolerate being called twice.
func TestFluxResourceCollector_StopIsIdempotentAndDropsLaterEvents(t *testing.T) {
	c := NewFluxResourceCollector(nil, FluxAlertGVR, FluxAlert, nil, 10, time.Second, logr.Discard(), nil)

	require.NoError(t, c.Stop())
	require.NoError(t, c.Stop())

	assert.NotPanics(t, func() {
		c.handleEvent(fluxObjectFixture(FluxAlertGVR, "Alert", "flux-system", "on-call"), EventTypeAdd)
	})
}

func TestFluxResourceCollector_AddResourceRejectsAForeignType(t *testing.T) {
	c := NewFluxResourceCollector(nil, FluxBucketGVR, FluxBucket, nil, 10, time.Second, logr.Discard(), nil)

	require.Error(t, c.AddResource("not an unstructured object"))
	assert.Empty(t, c.batchChan)

	require.NoError(t, c.AddResource(fluxObjectFixture(FluxBucketGVR, "Bucket", "flux-system", "manifests")))
	assert.Len(t, c.batchChan, 1)
}
