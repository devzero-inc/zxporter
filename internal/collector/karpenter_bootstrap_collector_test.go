package collector_test

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	"github.com/devzero-inc/zxporter/internal/collector"
)

// bootstrapTestTelemetryLogger is a no-op telemetry logger used by the
// bootstrap collector tests. It implements the zxporter telemetry_logger
// Logger interface.
type bootstrapTestTelemetryLogger struct{}

func (bootstrapTestTelemetryLogger) Report(
	_ gen.LogLevel,
	_ string,
	_ string,
	_ error,
	_ map[string]string,
) {
}
func (bootstrapTestTelemetryLogger) Stop() {}

func newBootstrapTestCollector(
	objs ...runtime.Object,
) *collector.KarpenterBootstrapCollector {
	client := fake.NewSimpleClientset(objs...)
	return collector.NewKarpenterBootstrapCollector(
		client,
		10,
		100*time.Millisecond,
		logr.Discard(),
		bootstrapTestTelemetryLogger{},
	)
}

func TestKarpenterBootstrapCollector_LabeledConfigMapEmitted(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "karpenter-settings-bootstrap",
			Namespace: "kube-system",
			Labels: map[string]string{
				"devzero.io/karpenter-bootstrap": "true",
			},
		},
		Data: map[string]string{"bootstrap.yaml": "LogLevel: info\n"},
	}

	c := newBootstrapTestCollector(cm)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		if err := c.Stop(); err != nil {
			t.Logf("stop: %v", err)
		}
	}()

	select {
	case batch := <-c.GetResourceChannel():
		if len(batch) != 1 {
			t.Fatalf("expected 1 resource, got %d", len(batch))
		}
		got, ok := batch[0].Object.(*corev1.ConfigMap)
		if !ok {
			t.Fatalf("expected *corev1.ConfigMap, got %T", batch[0].Object)
		}
		if got.Name != "karpenter-settings-bootstrap" {
			t.Fatalf("got wrong ConfigMap: %s", got.Name)
		}
		if batch[0].ResourceType != collector.ConfigMap {
			t.Fatalf(
				"expected ResourceType ConfigMap, got %s",
				batch[0].ResourceType.String(),
			)
		}
		if batch[0].Key != "kube-system/karpenter-settings-bootstrap" {
			t.Fatalf("unexpected key: %s", batch[0].Key)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for emit")
	}
}

func TestKarpenterBootstrapCollector_UnlabeledConfigMapIgnored(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unrelated",
			Namespace: "kube-system",
		},
		Data: map[string]string{"hello": "world"},
	}

	c := newBootstrapTestCollector(cm)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := c.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		if err := c.Stop(); err != nil {
			t.Logf("stop: %v", err)
		}
	}()

	select {
	case batch := <-c.GetResourceChannel():
		t.Fatalf("expected no emit, got %d resources", len(batch))
	case <-time.After(1 * time.Second):
		// success — unlabeled ConfigMap was filtered out by the label selector.
	}
}
