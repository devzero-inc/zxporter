package controller

import (
	"testing"

	kedaclient "github.com/kedacore/keda/v2/pkg/generated/clientset/versioned"
	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
	metricsv1 "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/go-logr/logr"
)

// newSpecTestReconciler builds a reconciler whose clients point at an address nothing
// listens on. resourceCollectorSpecs only constructs collectors; none of them is started,
// so no request is ever made.
func newSpecTestReconciler(t *testing.T) (*CollectionPolicyReconciler, *metricsv1.Clientset) {
	t.Helper()
	cfg := &rest.Config{Host: "http://127.0.0.1:1"}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("building client: %v", err)
		}
	}

	k8s, err := kubernetes.NewForConfig(cfg)
	must(err)
	dyn, err := dynamic.NewForConfig(cfg)
	must(err)
	meta, err := metadata.NewForConfig(cfg)
	must(err)
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	must(err)
	apiext, err := apiextensionsclientset.NewForConfig(cfg)
	must(err)
	keda, err := kedaclient.NewForConfig(cfg)
	must(err)
	metrics, err := metricsv1.NewForConfig(cfg)
	must(err)

	return &CollectionPolicyReconciler{
		Log:               logr.Discard(),
		K8sClient:         k8s,
		DynamicClient:     dyn,
		MetadataClient:    meta,
		DiscoveryClient:   disc,
		ApiExtensions:     apiext,
		KEDAClient:        keda,
		TelemetryLogger:   noopTelemetryLogger{},
		TelemetryMetrics:  getTestTelemetryMetrics(),
		CollectionManager: newTestCollectionManager(),
	}, metrics
}

// TestResourceCollectorSpecs_TypeMatchesName pins, for every collector the reconciler
// registers, that the key the CollectionManager stores it under (GetType) is the
// ResourceType name the reconciler later starts, restarts and disables it by. When the
// two drift, lookups by name miss: the KEDA ScaledJob/ScaledObject collectors returned
// "scaledjob"/"scaledobject", so their deferred start after the KEDA CRDs appeared failed
// as "not registered" — the path that leaked the manager mutex on 2026-09-25 — and
// DISABLE_COLLECTORS=keda_scaled_job could never deregister them at runtime.
//
// Factory-built collectors are checked separately because the deferred retry registers
// the factory's output, not the eagerly built instance.
func TestResourceCollectorSpecs_TypeMatchesName(t *testing.T) {
	r, metricsClient := newSpecTestReconciler(t)
	specs := r.resourceCollectorSpecs(logr.Discard(), &PolicyConfig{}, metricsClient)
	if len(specs) == 0 {
		t.Fatal("no collector specs returned")
	}

	seen := make(map[string]bool, len(specs))
	for _, spec := range specs {
		name := spec.name.String()
		t.Run(name, func(t *testing.T) {
			if seen[name] {
				t.Errorf("ResourceType %q is listed twice", name)
			}
			seen[name] = true

			if got := spec.collector.GetType(); got != name {
				t.Errorf("collector.GetType() = %q, want %q (the name it is started and disabled by)", got, name)
			}
			if spec.factory != nil {
				if got := spec.factory().GetType(); got != name {
					t.Errorf("factory().GetType() = %q, want %q (retryPendingCollectors registers the factory's output)",
						got, name)
				}
			}
		})
	}
}
