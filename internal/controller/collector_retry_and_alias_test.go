package controller

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	monitoringv1 "github.com/devzero-inc/zxporter/api/v1"
	"github.com/devzero-inc/zxporter/internal/collector"
	"github.com/go-logr/logr"
)

// flakyCollector fails Start while failStarts is positive, then succeeds.
type flakyCollector struct {
	keyedCollector
	failStarts *atomic.Int32
}

func (c *flakyCollector) Start(context.Context) error {
	if c.failStarts.Add(-1) >= 0 {
		return errors.New("transient start failure")
	}
	return nil
}

// TestRetryPendingCollectors_RetriesAfterFailedStart: a pending collector whose first
// Start fails must be retried on the next reconcile. The failed instance used to stay
// registered while its pending entry was dropped, so the next attempt could not register
// a fresh one ("already registered") and the collector stayed off until the pod
// restarted.
func TestRetryPendingCollectors_RetriesAfterFailedStart(t *testing.T) {
	mgr := newTestCollectionManager()
	r := &CollectionPolicyReconciler{Log: logr.Discard(), CollectionManager: mgr}
	failStarts := &atomic.Int32{}
	failStarts.Store(1)
	r.pendingCollectors = []pendingCollector{{
		name: collector.KedaScaledJob,
		factory: func() collector.ResourceCollector {
			return &flakyCollector{
				keyedCollector: keyedCollector{
					collectorType: collector.KedaScaledJob.String(),
					resourceCh:    make(chan []collector.CollectedResource, 1),
				},
				failStarts: failStarts,
			}
		},
	}}
	key := collector.KedaScaledJob.String()

	r.retryPendingCollectors(context.Background())
	if mgr.IsCollectorRunning(key) {
		t.Fatal("precondition: the first Start was meant to fail")
	}
	if len(r.pendingCollectors) != 1 {
		t.Fatalf("a failed start must stay pending, got %d entries", len(r.pendingCollectors))
	}

	r.retryPendingCollectors(context.Background())
	if !mgr.IsCollectorRunning(key) {
		t.Fatal("the retry after a failed start did not register and start the collector")
	}
	if len(r.pendingCollectors) != 0 {
		t.Errorf("a started collector must leave the pending list, got %d entries", len(r.pendingCollectors))
	}
	_ = mgr.StopCollector(key)
}

// writeTestKubeconfig points KUBECONFIG at an unreachable cluster, because
// handleDisabledCollectorsChange calls ctrl.GetConfigOrDie, which exits the process when
// no configuration can be found.
func writeTestKubeconfig(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	const kubeconfig = `apiVersion: v1
kind: Config
clusters:
- cluster: {server: "http://127.0.0.1:1"}
  name: test
contexts:
- context: {cluster: test, user: test}
  name: test
current-context: test
users:
- name: test
  user: {}
`
	if err := os.WriteFile(path, []byte(kubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", path)
}

// TestDisabledCollectors_LegacySpellingsStillDisable pins that every spelling older
// releases accepted (the API doc advertised "statefulset" and "daemonset"; the others
// were those collectors' registry keys) still disables its collector, both at startup
// (registerResourceCollectors compares the parsed list with the canonical name) and when
// the list changes at runtime (handleDisabledCollectorsChange compares it with the
// registry key).
func TestDisabledCollectors_LegacySpellingsStillDisable(t *testing.T) {
	writeTestKubeconfig(t)

	cases := map[string]collector.ResourceType{
		"statefulset":           collector.StatefulSet,
		"daemonset":             collector.DaemonSet,
		"persistentvolume":      collector.PersistentVolume,
		"persistentvolumeclaim": collector.PersistentVolumeClaim,
		"crd":                   collector.CustomResourceDefinition,
		"scaledjob":             collector.KedaScaledJob,
		"scaledobject":          collector.KedaScaledObject,
	}
	for legacy, resourceType := range cases {
		canonical := resourceType.String()
		t.Run(legacy, func(t *testing.T) {
			r := &CollectionPolicyReconciler{
				Log:               logr.Discard(),
				CollectionManager: newTestCollectionManager(),
				TelemetryLogger:   noopTelemetryLogger{},
			}
			spec := &monitoringv1.CollectionPolicySpec{
				Policies: monitoringv1.Policies{DisabledCollectors: []string{legacy}},
			}
			newConfig, _ := r.createNewConfig(spec, logr.Discard())

			if len(newConfig.DisabledCollectors) != 1 || newConfig.DisabledCollectors[0] != canonical {
				t.Fatalf("DisabledCollectors = %v, want [%q]", newConfig.DisabledCollectors, canonical)
			}

			running := &keyedCollector{collectorType: canonical, resourceCh: make(chan []collector.CollectedResource, 1)}
			if err := r.CollectionManager.RegisterCollector(running); err != nil {
				t.Fatal(err)
			}
			if err := r.handleDisabledCollectorsChange(context.Background(), logr.Discard(), &PolicyConfig{}, newConfig); err != nil {
				t.Fatal(err)
			}
			if r.CollectionManager.GetCollector(canonical) != nil {
				t.Errorf("disabledCollectors: [%q] did not deregister the %q collector", legacy, canonical)
			}
		})
	}
}
