package controller

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/devzero-inc/zxporter/internal/health"
)

const (
	gpuDaemonSetName          = "zxporter-nodemon-gpu"
	gpuRuntimeModeAnnotation  = "devzero.io/gpu-runtime-mode"
	gpuRuntimeClassAnnotation = "devzero.io/gpu-runtime-class"
)

// GPURuntimeResolver keeps the managed GPU DaemonSet's RuntimeClass in sync
// with the RuntimeClass API when the DaemonSet opts into auto mode.
type GPURuntimeResolver struct {
	client        client.Client
	apiReader     client.Reader
	namespace     string
	interval      time.Duration
	healthManager *health.HealthManager
}

// NewGPURuntimeResolver builds the resolver. apiReader must be an uncached
// reader (mgr.GetAPIReader()): the RuntimeClass read is served by a direct GET,
// which the narrow get-on-resourceName RBAC allows. The manager's cached client
// would instead back the read with a cluster-wide LIST+WATCH informer, which the
// resolver's RBAC intentionally forbids, so a cached read blocks forever on a
// cache that can never sync. client is used for the DaemonSet patch (and its
// already-permitted cached read).
func NewGPURuntimeResolver(
	c client.Client,
	apiReader client.Reader,
	namespace string,
	interval time.Duration,
	healthManager *health.HealthManager,
) *GPURuntimeResolver {
	return &GPURuntimeResolver{
		client:        c,
		apiReader:     apiReader,
		namespace:     namespace,
		interval:      interval,
		healthManager: healthManager,
	}
}

// Start implements the controller-manager Runnable interface.
func (r *GPURuntimeResolver) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("gpu-runtime-resolver")
	reconcile := func() {
		if err := r.ReconcileOnce(ctx); err != nil {
			logger.Error(err, "failed to reconcile GPU runtime")
		}
	}

	reconcile()
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			reconcile()
		case <-ctx.Done():
			return nil
		}
	}
}

// NeedLeaderElection makes the resolver run only on the elected manager.
func (r *GPURuntimeResolver) NeedLeaderElection() bool { return true }

// ReconcileOnce resolves and, when necessary, merge-patches the managed GPU
// DaemonSet. Conflicts retry the entire transaction so every attempt computes
// desired state from a fresh DaemonSet read.
func (r *GPURuntimeResolver) ReconcileOnce(ctx context.Context) error {
	var mode, candidate, lookupResult, result string

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		lookupResult = ""
		var ds appsv1.DaemonSet
		if err := r.client.Get(ctx, client.ObjectKey{
			Name:      gpuDaemonSetName,
			Namespace: r.namespace,
		}, &ds); err != nil {
			if apierrors.IsNotFound(err) {
				mode = ""
				candidate = ""
				result = "daemonset_missing"
				return nil
			}
			return err
		}

		mode = ds.Annotations[gpuRuntimeModeAnnotation]
		candidate = ds.Annotations[gpuRuntimeClassAnnotation]
		switch mode {
		case "auto":
		case "default", "explicit":
			result = "disabled"
			return nil
		default:
			return fmt.Errorf("%s mode must be one of auto, default, or explicit; got %q", gpuRuntimeModeAnnotation, mode)
		}
		if candidate == "" {
			return fmt.Errorf("%s candidate must be set in auto mode", gpuRuntimeClassAnnotation)
		}

		var runtimeClass nodev1.RuntimeClass
		// Uncached direct GET: satisfied by get-on-resourceName RBAC. A cached
		// read would require a forbidden cluster-wide RuntimeClass watch.
		err := r.apiReader.Get(ctx, client.ObjectKey{Name: candidate}, &runtimeClass)
		desired := candidate
		if apierrors.IsNotFound(err) {
			desired = ""
			lookupResult = "runtimeclass_missing"
		} else if err != nil {
			return err
		} else {
			lookupResult = "runtimeclass_found"
		}

		current := ptr.Deref(ds.Spec.Template.Spec.RuntimeClassName, "")
		if current == desired {
			result = "unchanged"
			return nil
		}

		base := ds.DeepCopy()
		ds.Spec.Template.Spec.RuntimeClassName = nil
		if desired != "" {
			ds.Spec.Template.Spec.RuntimeClassName = ptr.To(desired)
		}
		if err := r.client.Patch(ctx, &ds, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		result = "patched"
		return nil
	})
	if err != nil {
		r.updateHealth(health.HealthStatusDegraded, mode, candidate, lookupResult, "error", err)
		return err
	}

	// Report the RuntimeClass lookup outcome (found/missing) and the action
	// taken (patched/unchanged/...) in a single status write. HealthManager
	// keeps one status per component, so two sequential updates would make the
	// second clobber the first and drop the lookup outcome entirely.
	r.updateHealth(health.HealthStatusHealthy, mode, candidate, lookupResult, result, nil)
	return nil
}

func (r *GPURuntimeResolver) updateHealth(
	status health.HealthStatus,
	mode string,
	candidate string,
	lookupResult string,
	result string,
	err error,
) {
	if r.healthManager == nil {
		return
	}
	errorMessage := ""
	message := result
	if err != nil {
		errorMessage = err.Error()
		message = errorMessage
	}
	r.healthManager.UpdateStatus(
		health.ComponentGPURuntimeResolver,
		status,
		message,
		map[string]string{
			"mode":          mode,
			"candidate":     candidate,
			"lookup_result": lookupResult,
			"result":        result,
			"error":         errorMessage,
		},
	)
}
