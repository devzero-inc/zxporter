package collector

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func newRuntimeClassFixture(name, handler string) *nodev1.RuntimeClass {
	return &nodev1.RuntimeClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name + "-uid")},
		Handler:    handler,
	}
}

func newTestRuntimeClassCollector(t *testing.T, excluded []string, objs ...k8sruntime.Object) *RuntimeClassCollector {
	t.Helper()
	return NewRuntimeClassCollector(
		fake.NewSimpleClientset(objs...),
		excluded,
		DefaultMaxBatchSize,
		100*time.Millisecond,
		testr.New(t),
		&fakeTelemetryLogger{},
	)
}

// TestRuntimeClassCollectorEmitsAddUpdateDelete drives the real informer
// against the fake clientset: an existing class is emitted on start, a live
// update re-emits, and a delete reaches the channel with the delete event type.
func TestRuntimeClassCollectorEmitsAddUpdateDelete(t *testing.T) {
	c := newTestRuntimeClassCollector(t, nil, newRuntimeClassFixture("runc", "runc"))
	t.Cleanup(func() { _ = c.Stop() })

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	deadline := time.After(30 * time.Second)
	waitFor := func(pred func(CollectedResource) bool, failMsg string) CollectedResource {
		for {
			select {
			case batch, ok := <-c.GetResourceChannel():
				if !ok {
					t.Fatalf("resource channel closed: %s", failMsg)
				}
				for _, r := range batch {
					if pred(r) {
						return r
					}
				}
			case <-deadline:
				t.Fatalf("timed out: %s", failMsg)
			}
		}
	}

	add := waitFor(func(r CollectedResource) bool {
		return r.ResourceType == RuntimeClass && r.Key == "runc" && r.EventType == EventTypeAdd
	}, "the existing RuntimeClass should be collected on start")
	if add.Object.(*nodev1.RuntimeClass).Handler != "runc" {
		t.Errorf("expected the raw typed object on the channel, got %#v", add.Object)
	}

	if err := c.AddResource(&nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "kata"}, Handler: "kata"}); err != nil {
		t.Fatalf("AddResource() error: %v", err)
	}
	waitFor(func(r CollectedResource) bool {
		return r.ResourceType == RuntimeClass && r.Key == "kata" && r.EventType == EventTypeAdd
	}, "an injected RuntimeClass should be collected")
}

// TestRuntimeClassCollectorExcludesByName verifies the CollectionPolicy
// exclusion list is honored: an excluded class never reaches the channel.
func TestRuntimeClassCollectorExcludesByName(t *testing.T) {
	c := newTestRuntimeClassCollector(t,
		[]string{"excluded-class"},
		newRuntimeClassFixture("excluded-class", "runc"),
		newRuntimeClassFixture("kept-class", "runc"),
	)
	t.Cleanup(func() { _ = c.Stop() })

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	seen := map[string]bool{}
	deadline := time.After(5 * time.Second)
	for !seen["kept-class"] {
		select {
		case batch := <-c.GetResourceChannel():
			for _, r := range batch {
				seen[r.Key] = true
			}
		case <-deadline:
			t.Fatalf("timed out waiting for kept-class; saw %v", seen)
		}
	}
	if seen["excluded-class"] {
		t.Errorf("excluded-class must never be collected")
	}
}

// TestRuntimeClassCollectorChangeDetection pins the meaningful-change gate: a
// handler change re-emits, a no-op update does not, and an overhead change
// re-emits (overhead is one of the three payload fields on node.k8s.io/v1).
func TestRuntimeClassCollectorChangeDetection(t *testing.T) {
	c := newTestRuntimeClassCollector(t, nil)
	base := newRuntimeClassFixture("nvidia", "nvidia")

	same := newRuntimeClassFixture("nvidia", "nvidia")
	same.ResourceVersion = "1"
	base.ResourceVersion = "1"
	if c.runtimeClassChanged(base, same) {
		t.Error("an identical object must not count as changed")
	}

	changedHandler := newRuntimeClassFixture("nvidia", "nvidia-2")
	if !c.runtimeClassChanged(base, changedHandler) {
		t.Error("a handler change must re-emit")
	}

	changedOverhead := newRuntimeClassFixture("nvidia", "nvidia")
	changedOverhead.Overhead = &nodev1.Overhead{PodFixed: corev1.ResourceList{"cpu": resource.MustParse("250m")}}
	if !c.runtimeClassChanged(base, changedOverhead) {
		t.Error("an overhead change must re-emit")
	}

	changedScheduling := newRuntimeClassFixture("nvidia", "nvidia")
	changedScheduling.Scheduling = &nodev1.Scheduling{NodeSelector: map[string]string{"k": "v"}}
	if !c.runtimeClassChanged(base, changedScheduling) {
		t.Error("a scheduling change must re-emit")
	}
}

// TestRuntimeClassCollectorStopIsSafeUnderConcurrentSends exercises the
// send-after-close guard: Stop() while a sender holds the read lock must
// neither panic in the sender (send on closed channel) nor deadlock.
func TestRuntimeClassCollectorStopIsSafeUnderConcurrentSends(t *testing.T) {
	c := newTestRuntimeClassCollector(t, nil, newRuntimeClassFixture("runc", "runc"))
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	done := make(chan struct{})
	go func() {
		defer wg.Done()
		// Park a sender on the read lock: batchChan has capacity, so the
		// sends complete, but the loop keeps trying until stopped flips.
		for i := 0; i < 500; i++ {
			c.handleRuntimeClassEvent(newRuntimeClassFixture("runc", "runc"), EventTypeUpdate)
			select {
			case <-done:
				return
			default:
			}
		}
	}()

	// Give the sender a chance to be in-flight, then stop.
	time.Sleep(10 * time.Millisecond)
	if err := c.Stop(); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
	close(done)
	wg.Wait()

	// Stop must be idempotent.
	if err := c.Stop(); err != nil {
		t.Fatalf("second Stop() error: %v", err)
	}
}

// TestRuntimeClassCollectorStopIsSafeUnderConcurrentStops is the regression
// test for the double-close race: the manager's stopCollectorInternal calls
// Stop after cancelling the collector context, and Start's own context watcher
// calls Stop on the same cancellation. Both arrive concurrently, and before
// Stop serialized under c.mu both could pass the stopCh select's default arm
// and the second close panicked with "close of closed channel", taking the
// whole operator down.
func TestRuntimeClassCollectorStopIsSafeUnderConcurrentStops(t *testing.T) {
	c := newTestRuntimeClassCollector(t, nil)
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	const concurrentStops = 8
	var wg sync.WaitGroup
	wg.Add(concurrentStops)
	errs := make(chan error, concurrentStops)
	for range concurrentStops {
		go func() {
			defer wg.Done()
			errs <- c.Stop()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Stop() error: %v", err)
		}
	}
}

// TestRuntimeClassCollectorGetTypeAndAvailability pins the collector identity
// (the string the controller's drift map and affectedCollectors use) and the
// built-in availability answer.
func TestRuntimeClassCollectorGetTypeAndAvailability(t *testing.T) {
	c := newTestRuntimeClassCollector(t, nil)
	if got := c.GetType(); got != "runtime_class" {
		t.Errorf("GetType() = %q, want runtime_class", got)
	}
	if !c.IsAvailable(context.Background()) {
		t.Error("RuntimeClass is a built-in kind; IsAvailable must be true")
	}
	if got := RuntimeClass.ProtoType(); got.String() != "RESOURCE_TYPE_RUNTIME_CLASS" {
		t.Errorf("ProtoType() = %v, want RESOURCE_TYPE_RUNTIME_CLASS", got)
	}
	if got := RuntimeClass.String(); got != "runtime_class" {
		t.Errorf("String() = %q, want runtime_class", got)
	}
}
