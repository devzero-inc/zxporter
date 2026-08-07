package collector

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/cache"
)

// crdFixtures builds n distinct CRDs for the fake clientset.
func crdFixtures(n int) []k8sruntime.Object {
	objs := make([]k8sruntime.Object, 0, n)
	for i := range n {
		objs = append(objs, newCRDFixture(fmt.Sprintf("thing%03d", i)))
	}
	return objs
}

func newCRDFixture(plural string) *apiextv1.CustomResourceDefinition {
	return &apiextv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: plural + ".example.com"},
		Spec: apiextv1.CustomResourceDefinitionSpec{
			Group: "example.com",
			Names: apiextv1.CustomResourceDefinitionNames{Plural: plural, Kind: plural},
			Scope: apiextv1.ClusterScoped,
		},
	}
}

func newTestCRDCollector(t *testing.T, objs ...k8sruntime.Object) *CRDCollector {
	t.Helper()
	return newTestCRDCollectorWith(t, DefaultMaxBatchSize, DefaultMaxBatchTime, objs...)
}

func newTestCRDCollectorWith(
	t *testing.T,
	maxBatchSize int,
	maxBatchTime time.Duration,
	objs ...k8sruntime.Object,
) *CRDCollector {
	t.Helper()
	return NewCRDCollector(
		apiextfake.NewSimpleClientset(objs...),
		maxBatchSize,
		maxBatchTime,
		testr.New(t),
		&fakeTelemetryLogger{},
	)
}

// TestCRDCollectorStartReturnsRegardlessOfCRDCount is the regression test for the
// startup deadlock: Start used to re-emit every CRD from the informer store
// before starting the batcher, its only consumer. batchChan buffers 100 and the
// informer already emits one Add per existing CRD, so any cluster with more than
// 50 CRDs wedged Start forever and CollectionManager.StartAll logged
// "Timed out starting collector" {"type":"crd"} — permanently, with no retry.
func TestCRDCollectorStartReturnsRegardlessOfCRDCount(t *testing.T) {
	// 51 is the old cliff (2 x 51 > 100); the rest are realistic cluster sizes.
	for _, n := range []int{0, 1, 50, 51, 120, 400} {
		t.Run(fmt.Sprintf("crds=%d", n), func(t *testing.T) {
			c := newTestCRDCollector(t, crdFixtures(n)...)
			t.Cleanup(func() { _ = c.Stop() })

			done := make(chan error, 1)
			go func() { done <- c.Start(context.Background()) }()

			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("Start() returned error: %v", err)
				}
			case <-time.After(30 * time.Second):
				t.Fatalf("Start() did not return for %d CRDs; batchChan len=%d cap=%d",
					n, len(c.batchChan), cap(c.batchChan))
			}
		})
	}
}

// TestCRDCollectorEmitsEveryCRDExactlyOnce guards both halves of the fix: every
// existing CRD still reaches the pipeline (the deleted re-emit loop was
// redundant, not load-bearing), and none is emitted twice.
func TestCRDCollectorEmitsEveryCRDExactlyOnce(t *testing.T) {
	const n = 120

	// A short batch interval keeps this from waiting out the default 5s tick.
	c := newTestCRDCollectorWith(t, DefaultMaxBatchSize, 100*time.Millisecond, crdFixtures(n)...)
	t.Cleanup(func() { _ = c.Stop() })

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	seen := map[string]int{}
	deadline := time.After(30 * time.Second)
	for len(seen) < n {
		select {
		case batch, ok := <-c.GetResourceChannel():
			if !ok {
				t.Fatalf("resource channel closed after %d/%d CRDs", len(seen), n)
			}
			for _, r := range batch {
				if r.ResourceType != CustomResourceDefinition {
					t.Errorf("unexpected resource type %v", r.ResourceType)
				}
				if r.EventType != EventTypeAdd {
					t.Errorf("%s: expected Add, got %v", r.Key, r.EventType)
				}
				seen[r.Key]++
			}
		case <-deadline:
			t.Fatalf("only received %d/%d CRDs", len(seen), n)
		}
	}

	for key, count := range seen {
		if count != 1 {
			t.Errorf("CRD %s emitted %d times, want exactly 1", key, count)
		}
	}
}

// TestCRDCollectorStopIsSafeUnderBackpressure covers the crash this bug exposed:
// Stop closed batchChan while informer handlers were blocked sending on it, so
// StopAll panicked with "send on closed channel" and took the pod down. Nothing
// drains resourceChan here, so the batcher blocks and batchChan fills up,
// leaving real senders parked exactly as they were in production.
func TestCRDCollectorStopIsSafeUnderBackpressure(t *testing.T) {
	// A batch size of 1 makes the batcher forward every item straight to
	// resourceChan, which nothing here drains. Once that fills, the batcher
	// stops consuming and batchChan backs up until informer handlers are parked
	// mid-send — the exact state in which Stop used to close it under them.
	c := newTestCRDCollectorWith(t, 1, DefaultMaxBatchTime, crdFixtures(400)...)

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	// Wait for real backpressure instead of sleeping a fixed interval, and fail
	// loudly if it never arrives: a test that stops an idle collector would pass
	// while proving nothing.
	deadline := time.Now().Add(30 * time.Second)
	for len(c.batchChan) < cap(c.batchChan) {
		if time.Now().After(deadline) {
			t.Fatalf("batchChan never filled (len=%d cap=%d); senders are not parked, so this test cannot exercise the race",
				len(c.batchChan), cap(c.batchChan))
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Two concurrent Stops: StopAll cancels the context (waking the watcher
	// goroutine, which calls Stop) and then calls Stop itself.
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.Stop(); err != nil {
				t.Errorf("Stop() error: %v", err)
			}
		}()
	}

	stopped := make(chan struct{})
	go func() { wg.Wait(); close(stopped) }()

	// Keep draining resourceChan while Stop runs. CollectionManager.processCollectorChannel
	// plays this role in production; without a reader the batcher stays blocked
	// on its own output and batcher.stop() could never return.
	giveUp := time.After(30 * time.Second)
drain:
	for {
		select {
		case <-stopped:
			break drain
		case <-c.GetResourceChannel():
		case <-giveUp:
			t.Fatal("Stop() deadlocked with senders blocked on batchChan")
		}
	}

	// A late event must not panic on the closed channel either.
	c.handleCRDEvent(newCRDFixture("late"), EventTypeAdd)
}

// TestCRDCollectorStartAfterStop pins the restart contract. This is a contract
// test, not a regression test: the old code also refused a restart, but only
// incidentally, by failing the cache-sync check with a misleading "timed out"
// error. The explicit guard keeps a future reordering from reaching
// batcher.start() twice and double-closing resourceChan.
func TestCRDCollectorStartAfterStop(t *testing.T) {
	c := newTestCRDCollector(t, crdFixtures(2)...)

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	if err := c.Stop(); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
	if err := c.Start(context.Background()); err == nil {
		t.Fatal("Start() after Stop() should return an error, got nil")
	}
}

// TestCRDCollectorDeleteTombstone covers the unchecked type assertion in
// DeleteFunc: a missed delete arrives as cache.DeletedFinalStateUnknown, which
// StripMetadataTransform deliberately passes through, so the old
// obj.(*CustomResourceDefinition) panicked on the informer's handler goroutine.
func TestCRDCollectorDeleteTombstone(t *testing.T) {
	crd := newCRDFixture("thing")

	for _, tc := range []struct {
		name string
		obj  interface{}
		want bool
	}{
		{"plain object", crd, true},
		{"tombstone", cache.DeletedFinalStateUnknown{Key: crd.Name, Obj: crd}, true},
		{"tombstone with junk payload", cache.DeletedFinalStateUnknown{Key: "x", Obj: "nope"}, false},
		{"unexpected type", "not a crd", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestCRDCollector(t)
			t.Cleanup(func() { _ = c.Stop() })

			c.handleCRDDelete(tc.obj) // must never panic

			if tc.want {
				select {
				case got := <-c.batchChan:
					if got.Key != crd.Name {
						t.Errorf("got key %q, want %q", got.Key, crd.Name)
					}
					if got.EventType != EventTypeDelete {
						t.Errorf("got event %v, want Delete", got.EventType)
					}
				default:
					t.Error("expected a delete event on batchChan, got none")
				}
				return
			}

			select {
			case got := <-c.batchChan:
				t.Errorf("expected no event, got %+v", got)
			default:
			}
		})
	}
}
