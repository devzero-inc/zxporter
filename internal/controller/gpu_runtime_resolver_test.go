package controller

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/devzero-inc/zxporter/internal/health"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testGPUNamespace = "zxporter-system"

type gpuRuntimeRecordingClient struct {
	client.Client
	runtimeClassErr      error
	daemonSetGets        int
	runtimeClassGets     int
	patchCalls           int
	patchErrs            []error
	onPatchError         func(context.Context) error
	concurrentModeChange bool
}

type gpuRuntimeLifecycleClient struct {
	client.Client
	daemonSetGets  atomic.Int32
	failFirstGets  int32
	daemonSetGetCh chan struct{}
}

func (c *gpuRuntimeLifecycleClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*appsv1.DaemonSet); ok {
		call := c.daemonSetGets.Add(1)
		select {
		case c.daemonSetGetCh <- struct{}{}:
		default:
		}
		if call <= c.failFirstGets {
			return errors.New("injected daemonset get failure")
		}
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func TestGPURuntimeResolver_StartReconcilesImmediatelyAndStopsOnCancellation(t *testing.T) {
	recordingClient := &gpuRuntimeLifecycleClient{
		Client:         newGPURuntimeFakeClient(t, gpuDaemonSet("default", "nvidia", "")),
		daemonSetGetCh: make(chan struct{}, 4),
	}
	resolver := NewGPURuntimeResolver(recordingClient, recordingClient, testGPUNamespace, time.Hour, nil)
	ctx, cancel := context.WithCancel(context.Background())
	startErr := make(chan error, 1)
	go func() {
		startErr <- resolver.Start(ctx)
	}()

	select {
	case <-recordingClient.daemonSetGetCh:
	case <-time.After(time.Second):
		t.Fatal("Start did not reconcile immediately")
	}

	cancel()
	select {
	case err := <-startErr:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Start did not stop after context cancellation")
	}

	getsAfterStop := recordingClient.daemonSetGets.Load()
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, getsAfterStop, recordingClient.daemonSetGets.Load())
}

func TestGPURuntimeResolver_StartRepeatsAtInjectedInterval(t *testing.T) {
	recordingClient := &gpuRuntimeLifecycleClient{
		Client:         newGPURuntimeFakeClient(t, gpuDaemonSet("default", "nvidia", "")),
		daemonSetGetCh: make(chan struct{}, 4),
	}
	resolver := NewGPURuntimeResolver(recordingClient, recordingClient, testGPUNamespace, 10*time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())
	startErr := make(chan error, 1)
	go func() {
		startErr <- resolver.Start(ctx)
	}()

	for reconciliation := 1; reconciliation <= 2; reconciliation++ {
		select {
		case <-recordingClient.daemonSetGetCh:
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for reconciliation %d", reconciliation)
		}
	}

	cancel()
	require.NoError(t, <-startErr)
}

func TestGPURuntimeResolver_StartContinuesAfterReconciliationError(t *testing.T) {
	recordingClient := &gpuRuntimeLifecycleClient{
		Client:         newGPURuntimeFakeClient(t, gpuDaemonSet("default", "nvidia", "")),
		failFirstGets:  1,
		daemonSetGetCh: make(chan struct{}, 4),
	}
	healthManager := health.NewHealthManager()
	healthManager.Register(health.ComponentGPURuntimeResolver)
	healthTransitions := make(chan health.HealthStatus, 2)
	healthManager.SetTransitionObserver(func(_ string, _, newStatus health.HealthStatus, _ string, _ map[string]string) {
		healthTransitions <- newStatus
	})
	resolver := NewGPURuntimeResolver(recordingClient, recordingClient, testGPUNamespace, 10*time.Millisecond, healthManager)
	ctx, cancel := context.WithCancel(context.Background())
	startErr := make(chan error, 1)
	go func() {
		startErr <- resolver.Start(ctx)
	}()

	for _, want := range []health.HealthStatus{health.HealthStatusDegraded, health.HealthStatusHealthy} {
		select {
		case got := <-healthTransitions:
			assert.Equal(t, want, got)
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for health transition to %v", want)
		}
	}

	cancel()
	require.NoError(t, <-startErr)
	assert.GreaterOrEqual(t, recordingClient.daemonSetGets.Load(), int32(2))
}

func TestGPURuntimeResolver_NeedLeaderElection(t *testing.T) {
	resolver := NewGPURuntimeResolver(nil, nil, testGPUNamespace, time.Minute, nil)
	assert.True(t, resolver.NeedLeaderElection())
}

// TestGPURuntimeResolver_ReadsRuntimeClassViaAPIReader guards the RBAC/caching
// fix: the RuntimeClass must be read through the uncached apiReader (a direct
// GET the narrow get-on-resourceName RBAC allows), never through the cached
// client (which would need a forbidden cluster-wide RuntimeClass watch). Passing
// distinct recorders proves which handle serves the read.
func TestGPURuntimeResolver_ReadsRuntimeClassViaAPIReader(t *testing.T) {
	ctx := context.Background()
	baseClient := newGPURuntimeFakeClient(t, gpuDaemonSet("auto", "nvidia", ""), gpuRuntimeClass())
	cachedClient := &gpuRuntimeRecordingClient{Client: baseClient}
	apiReader := &gpuRuntimeRecordingClient{Client: baseClient}
	resolver := NewGPURuntimeResolver(cachedClient, apiReader, testGPUNamespace, time.Minute, nil)

	require.NoError(t, resolver.ReconcileOnce(ctx))

	assert.Equal(t, 1, apiReader.runtimeClassGets, "RuntimeClass must be read via the uncached apiReader")
	assert.Equal(t, 0, cachedClient.runtimeClassGets, "RuntimeClass must never be read via the cached client")
	assert.GreaterOrEqual(t, cachedClient.daemonSetGets, 1, "DaemonSet is still read via the (cached) client")
	assert.Equal(t, 0, apiReader.daemonSetGets, "DaemonSet must not be read via the apiReader")
}

func (c *gpuRuntimeRecordingClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	switch obj.(type) {
	case *appsv1.DaemonSet:
		c.daemonSetGets++
	case *nodev1.RuntimeClass:
		c.runtimeClassGets++
		if c.runtimeClassErr != nil {
			return c.runtimeClassErr
		}
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *gpuRuntimeRecordingClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.patchCalls++
	if c.concurrentModeChange {
		c.concurrentModeChange = false
		var current appsv1.DaemonSet
		key := client.ObjectKey{Name: gpuDaemonSetName, Namespace: testGPUNamespace}
		if err := c.Client.Get(ctx, key, &current); err != nil {
			return err
		}
		base := current.DeepCopy()
		current.Annotations[gpuRuntimeModeAnnotation] = "default"
		if err := c.Client.Patch(ctx, &current, client.MergeFrom(base)); err != nil {
			return err
		}
		data, err := patch.Data(obj)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(`"resourceVersion"`)) {
			return apierrors.NewConflict(
				schema.GroupResource{Group: appsv1.GroupName, Resource: "daemonsets"},
				gpuDaemonSetName,
				errors.New("concurrent mode change"),
			)
		}
	}
	if len(c.patchErrs) >= c.patchCalls && c.patchErrs[c.patchCalls-1] != nil {
		if c.onPatchError != nil {
			if err := c.onPatchError(ctx); err != nil {
				return err
			}
		}
		return c.patchErrs[c.patchCalls-1]
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func TestGPURuntimeResolver_ConcurrentModeChangeSuppressesStalePatch(t *testing.T) {
	ctx := context.Background()
	ds := gpuDaemonSet("auto", "nvidia", "")
	ds.ResourceVersion = "1"
	baseClient := newGPURuntimeFakeClient(t, ds, gpuRuntimeClass())
	recordingClient := &gpuRuntimeRecordingClient{
		Client:               baseClient,
		concurrentModeChange: true,
	}
	healthManager := health.NewHealthManager()
	healthManager.Register(health.ComponentGPURuntimeResolver)
	resolver := NewGPURuntimeResolver(recordingClient, recordingClient, testGPUNamespace, time.Minute, healthManager)

	require.NoError(t, resolver.ReconcileOnce(ctx))
	assert.Equal(t, 2, recordingClient.daemonSetGets, "optimistic-lock conflict must restart from a fresh read")
	assert.Equal(t, 1, recordingClient.patchCalls)

	var current appsv1.DaemonSet
	require.NoError(t, baseClient.Get(ctx, client.ObjectKey{Name: gpuDaemonSetName, Namespace: testGPUNamespace}, &current))
	assert.Equal(t, "default", current.Annotations[gpuRuntimeModeAnnotation])
	assert.Empty(t, ptr.Deref(current.Spec.Template.Spec.RuntimeClassName, ""), "stale auto decision must not patch after mode changes")
	status, ok := healthManager.GetStatus(health.ComponentGPURuntimeResolver)
	require.True(t, ok)
	assert.Equal(t, "disabled", status.Metadata["result"])
}

func TestGPURuntimeResolver_PatchFailureDoesNotPublishHealthyTransition(t *testing.T) {
	ctx := context.Background()
	baseClient := newGPURuntimeFakeClient(t, gpuDaemonSet("auto", "nvidia", ""), gpuRuntimeClass())
	patchForbidden := apierrors.NewForbidden(
		schema.GroupResource{Group: appsv1.GroupName, Resource: "daemonsets"},
		gpuDaemonSetName,
		errors.New("patch denied"),
	)
	recordingClient := &gpuRuntimeRecordingClient{
		Client:    baseClient,
		patchErrs: []error{patchForbidden},
	}
	healthManager := health.NewHealthManager()
	healthManager.Register(health.ComponentGPURuntimeResolver)
	var transitions []health.HealthStatus
	healthManager.SetTransitionObserver(func(_ string, _, newStatus health.HealthStatus, _ string, _ map[string]string) {
		transitions = append(transitions, newStatus)
	})
	resolver := NewGPURuntimeResolver(recordingClient, recordingClient, testGPUNamespace, time.Minute, healthManager)

	require.Error(t, resolver.ReconcileOnce(ctx))
	assert.Equal(t, []health.HealthStatus{health.HealthStatusDegraded}, transitions)
}

func TestGPURuntimeResolver_ReconcileOnce(t *testing.T) {
	forbidden := apierrors.NewForbidden(
		schema.GroupResource{Group: nodev1.GroupName, Resource: "runtimeclasses"},
		"nvidia",
		errors.New("access denied"),
	)

	tests := []struct {
		name                   string
		objects                []client.Object
		runtimeClassErr        error
		reconciliations        int
		wantRuntimeClass       string
		wantManagedDaemonSet   bool
		wantRuntimeClassReads  int
		wantPatchCalls         int
		wantError              bool
		wantHealthStatus       health.HealthStatus
		wantHealthResult       string
		wantHealthLookupResult string
		wantHealthMode         string
		wantHealthCandidate    string
		wantHealthError        string
	}{
		{
			name:                   "auto adds an existing runtime class",
			objects:                []client.Object{gpuDaemonSet("auto", "nvidia", ""), gpuRuntimeClass()},
			reconciliations:        1,
			wantRuntimeClass:       "nvidia",
			wantManagedDaemonSet:   true,
			wantRuntimeClassReads:  1,
			wantPatchCalls:         1,
			wantHealthStatus:       health.HealthStatusHealthy,
			wantHealthResult:       "patched",
			wantHealthLookupResult: "runtimeclass_found",
			wantHealthMode:         "auto",
			wantHealthCandidate:    "nvidia",
		},
		{
			name:                   "auto removes a missing runtime class",
			objects:                []client.Object{gpuDaemonSet("auto", "nvidia", "nvidia")},
			reconciliations:        1,
			wantManagedDaemonSet:   true,
			wantRuntimeClassReads:  1,
			wantPatchCalls:         1,
			wantHealthStatus:       health.HealthStatusHealthy,
			wantHealthResult:       "patched",
			wantHealthLookupResult: "runtimeclass_missing",
			wantHealthMode:         "auto",
			wantHealthCandidate:    "nvidia",
		},
		{
			name:                   "auto leaves the desired runtime class unchanged",
			objects:                []client.Object{gpuDaemonSet("auto", "nvidia", "nvidia"), gpuRuntimeClass()},
			reconciliations:        1,
			wantRuntimeClass:       "nvidia",
			wantManagedDaemonSet:   true,
			wantRuntimeClassReads:  1,
			wantPatchCalls:         0,
			wantHealthStatus:       health.HealthStatusHealthy,
			wantHealthResult:       "unchanged",
			wantHealthLookupResult: "runtimeclass_found",
			wantHealthMode:         "auto",
			wantHealthCandidate:    "nvidia",
		},
		{
			name:                  "default mode skips runtime class lookup and patch",
			objects:               []client.Object{gpuDaemonSet("default", "nvidia", "")},
			reconciliations:       1,
			wantManagedDaemonSet:  true,
			wantRuntimeClassReads: 0,
			wantPatchCalls:        0,
			wantHealthStatus:      health.HealthStatusHealthy,
			wantHealthResult:      "disabled",
			wantHealthMode:        "default",
			wantHealthCandidate:   "nvidia",
		},
		{
			name:                  "explicit mode skips runtime class lookup and patch",
			objects:               []client.Object{gpuDaemonSet("explicit", "nvidia", "nvidia")},
			reconciliations:       1,
			wantRuntimeClass:      "nvidia",
			wantManagedDaemonSet:  true,
			wantRuntimeClassReads: 0,
			wantPatchCalls:        0,
			wantHealthStatus:      health.HealthStatusHealthy,
			wantHealthResult:      "disabled",
			wantHealthMode:        "explicit",
			wantHealthCandidate:   "nvidia",
		},
		{
			name:                  "missing mode is a degraded configuration error",
			objects:               []client.Object{gpuDaemonSetWithoutMode("nvidia", "nvidia")},
			reconciliations:       1,
			wantRuntimeClass:      "nvidia",
			wantManagedDaemonSet:  true,
			wantRuntimeClassReads: 0,
			wantPatchCalls:        0,
			wantError:             true,
			wantHealthStatus:      health.HealthStatusDegraded,
			wantHealthResult:      "error",
			wantHealthCandidate:   "nvidia",
			wantHealthError:       "mode",
		},
		{
			name:                  "unknown mode is a degraded configuration error",
			objects:               []client.Object{gpuDaemonSet("legacy", "nvidia", "nvidia")},
			reconciliations:       1,
			wantRuntimeClass:      "nvidia",
			wantManagedDaemonSet:  true,
			wantRuntimeClassReads: 0,
			wantPatchCalls:        0,
			wantError:             true,
			wantHealthStatus:      health.HealthStatusDegraded,
			wantHealthResult:      "error",
			wantHealthMode:        "legacy",
			wantHealthCandidate:   "nvidia",
			wantHealthError:       "mode",
		},
		{
			name:                  "missing managed daemonset is a successful no-op",
			reconciliations:       1,
			wantManagedDaemonSet:  false,
			wantRuntimeClassReads: 0,
			wantPatchCalls:        0,
			wantHealthStatus:      health.HealthStatusHealthy,
			wantHealthResult:      "daemonset_missing",
		},
		{
			name:                  "runtime class API error retains the current field",
			objects:               []client.Object{gpuDaemonSet("auto", "nvidia", "nvidia")},
			runtimeClassErr:       forbidden,
			reconciliations:       1,
			wantRuntimeClass:      "nvidia",
			wantManagedDaemonSet:  true,
			wantRuntimeClassReads: 1,
			wantPatchCalls:        0,
			wantError:             true,
			wantHealthStatus:      health.HealthStatusDegraded,
			wantHealthResult:      "error",
			wantHealthMode:        "auto",
			wantHealthCandidate:   "nvidia",
			wantHealthError:       "forbidden",
		},
		{
			name: "unrelated daemonset is untouched",
			objects: []client.Object{
				unrelatedDaemonSet("other-daemonset", "other-runtime"),
			},
			reconciliations:       1,
			wantManagedDaemonSet:  false,
			wantRuntimeClassReads: 0,
			wantPatchCalls:        0,
			wantHealthStatus:      health.HealthStatusHealthy,
			wantHealthResult:      "daemonset_missing",
		},
		{
			name:                   "repeated reconciliation is idempotent",
			objects:                []client.Object{gpuDaemonSet("auto", "nvidia", ""), gpuRuntimeClass()},
			reconciliations:        2,
			wantRuntimeClass:       "nvidia",
			wantManagedDaemonSet:   true,
			wantRuntimeClassReads:  2,
			wantPatchCalls:         1,
			wantHealthStatus:       health.HealthStatusHealthy,
			wantHealthResult:       "unchanged",
			wantHealthLookupResult: "runtimeclass_found",
			wantHealthMode:         "auto",
			wantHealthCandidate:    "nvidia",
		},
		{
			name:                  "auto without a candidate is degraded",
			objects:               []client.Object{gpuDaemonSet("auto", "", "nvidia")},
			reconciliations:       1,
			wantRuntimeClass:      "nvidia",
			wantManagedDaemonSet:  true,
			wantRuntimeClassReads: 0,
			wantPatchCalls:        0,
			wantError:             true,
			wantHealthStatus:      health.HealthStatusDegraded,
			wantHealthResult:      "error",
			wantHealthMode:        "auto",
			wantHealthError:       "candidate",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			baseClient := newGPURuntimeFakeClient(t, tt.objects...)
			recordingClient := &gpuRuntimeRecordingClient{
				Client:          baseClient,
				runtimeClassErr: tt.runtimeClassErr,
			}
			healthManager := health.NewHealthManager()
			healthManager.Register(health.ComponentGPURuntimeResolver)
			resolver := NewGPURuntimeResolver(recordingClient, recordingClient, testGPUNamespace, time.Minute, healthManager)

			var reconcileErr error
			for range tt.reconciliations {
				reconcileErr = resolver.ReconcileOnce(ctx)
				if reconcileErr != nil {
					break
				}
			}
			if tt.wantError {
				require.Error(t, reconcileErr)
			} else {
				require.NoError(t, reconcileErr)
			}

			assert.Equal(t, tt.wantRuntimeClassReads, recordingClient.runtimeClassGets)
			assert.Equal(t, tt.wantPatchCalls, recordingClient.patchCalls)

			var managed appsv1.DaemonSet
			err := baseClient.Get(ctx, client.ObjectKey{Name: gpuDaemonSetName, Namespace: testGPUNamespace}, &managed)
			if tt.wantManagedDaemonSet {
				require.NoError(t, err)
				assert.Equal(t, tt.wantRuntimeClass, ptr.Deref(managed.Spec.Template.Spec.RuntimeClassName, ""))
			} else {
				require.True(t, apierrors.IsNotFound(err))
			}

			if tt.name == "unrelated daemonset is untouched" {
				var unrelated appsv1.DaemonSet
				require.NoError(t, baseClient.Get(ctx, client.ObjectKey{Name: "other-daemonset", Namespace: testGPUNamespace}, &unrelated))
				assert.Equal(t, "other-runtime", ptr.Deref(unrelated.Spec.Template.Spec.RuntimeClassName, ""))
			}

			status, ok := healthManager.GetStatus(health.ComponentGPURuntimeResolver)
			require.True(t, ok)
			assert.Equal(t, tt.wantHealthStatus, status.Status)
			assert.Equal(t, tt.wantHealthResult, status.Metadata["result"])
			assert.Equal(t, tt.wantHealthLookupResult, status.Metadata["lookup_result"])
			assert.Equal(t, tt.wantHealthMode, status.Metadata["mode"])
			assert.Equal(t, tt.wantHealthCandidate, status.Metadata["candidate"])
			if tt.wantHealthError == "" {
				assert.Empty(t, status.Metadata["error"])
			} else {
				assert.Contains(t, strings.ToLower(status.Metadata["error"]), tt.wantHealthError)
			}
		})
	}
}

func TestGPURuntimeResolver_ConflictRetriesFromFreshRead(t *testing.T) {
	ctx := context.Background()
	baseClient := newGPURuntimeFakeClient(t, gpuDaemonSet("auto", "nvidia", ""), gpuRuntimeClass())
	conflict := apierrors.NewConflict(
		schema.GroupResource{Group: appsv1.GroupName, Resource: "daemonsets"},
		gpuDaemonSetName,
		errors.New("conflict"),
	)
	recordingClient := &gpuRuntimeRecordingClient{
		Client:    baseClient,
		patchErrs: []error{conflict},
	}
	recordingClient.onPatchError = func(ctx context.Context) error {
		var current appsv1.DaemonSet
		key := client.ObjectKey{Name: gpuDaemonSetName, Namespace: testGPUNamespace}
		if err := baseClient.Get(ctx, key, &current); err != nil {
			return err
		}
		base := current.DeepCopy()
		current.Spec.Template.Spec.RuntimeClassName = ptr.To("nvidia")
		return baseClient.Patch(ctx, &current, client.MergeFrom(base))
	}
	healthManager := health.NewHealthManager()
	healthManager.Register(health.ComponentGPURuntimeResolver)
	resolver := NewGPURuntimeResolver(recordingClient, recordingClient, testGPUNamespace, time.Minute, healthManager)

	require.NoError(t, resolver.ReconcileOnce(ctx))
	assert.Equal(t, 2, recordingClient.daemonSetGets, "conflict retry must re-read the DaemonSet")
	assert.Equal(t, 2, recordingClient.runtimeClassGets, "fresh reconciliation must repeat desired-state computation")
	assert.Equal(t, 1, recordingClient.patchCalls, "fresh read should observe the externally applied desired state")
	status, ok := healthManager.GetStatus(health.ComponentGPURuntimeResolver)
	require.True(t, ok)
	assert.Equal(t, "unchanged", status.Metadata["result"])
}

func TestGPURuntimeResolver_PersistentConflictIsSurfaced(t *testing.T) {
	ctx := context.Background()
	baseClient := newGPURuntimeFakeClient(t, gpuDaemonSet("auto", "nvidia", ""), gpuRuntimeClass())
	conflict := apierrors.NewConflict(
		schema.GroupResource{Group: appsv1.GroupName, Resource: "daemonsets"},
		gpuDaemonSetName,
		errors.New("persistent conflict"),
	)
	recordingClient := &gpuRuntimeRecordingClient{
		Client:    baseClient,
		patchErrs: []error{conflict, conflict, conflict, conflict, conflict, conflict, conflict, conflict},
	}
	healthManager := health.NewHealthManager()
	healthManager.Register(health.ComponentGPURuntimeResolver)
	resolver := NewGPURuntimeResolver(recordingClient, recordingClient, testGPUNamespace, time.Minute, healthManager)

	err := resolver.ReconcileOnce(ctx)
	require.Error(t, err)
	assert.True(t, apierrors.IsConflict(err))
	assert.Greater(t, recordingClient.patchCalls, 1)
	assert.Equal(t, recordingClient.patchCalls, recordingClient.daemonSetGets, "each conflict retry must start with a fresh DaemonSet read")
	status, ok := healthManager.GetStatus(health.ComponentGPURuntimeResolver)
	require.True(t, ok)
	assert.Equal(t, health.HealthStatusDegraded, status.Status)
	assert.Equal(t, "error", status.Metadata["result"])
	assert.Contains(t, status.Metadata["error"], "persistent conflict")
}

func newGPURuntimeFakeClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, nodev1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func gpuDaemonSet(mode, candidate, runtimeClassName string) *appsv1.DaemonSet {
	annotations := map[string]string{
		gpuRuntimeModeAnnotation:  mode,
		gpuRuntimeClassAnnotation: candidate,
	}
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:        gpuDaemonSetName,
			Namespace:   testGPUNamespace,
			Annotations: annotations,
		},
	}
	if runtimeClassName != "" {
		ds.Spec.Template.Spec.RuntimeClassName = ptr.To(runtimeClassName)
	}
	return ds
}

func gpuDaemonSetWithoutMode(candidate, runtimeClassName string) *appsv1.DaemonSet {
	ds := gpuDaemonSet("", candidate, runtimeClassName)
	delete(ds.Annotations, gpuRuntimeModeAnnotation)
	return ds
}

func unrelatedDaemonSet(name, runtimeClassName string) *appsv1.DaemonSet {
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testGPUNamespace},
	}
	if runtimeClassName != "" {
		ds.Spec.Template.Spec.RuntimeClassName = ptr.To(runtimeClassName)
	}
	return ds
}

func gpuRuntimeClass() *nodev1.RuntimeClass {
	return &nodev1.RuntimeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "nvidia"},
		Handler:    "nvidia",
	}
}
