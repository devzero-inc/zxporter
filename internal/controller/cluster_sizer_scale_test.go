package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestClusterSizer_Scale drives the sizer against a real kube-apiserver with
// node counts taken from the production distribution (p50 10, p95 27, p99 86,
// largest cluster 1820), so node paging, the ladder cap and rollout counting
// run against real API semantics. It needs the envtest binaries and is opt-in:
//
//	make envtest && ZXPORTER_SIZER_SCALE=1 go test ./internal/controller/ -run TestClusterSizer_Scale -v
func TestClusterSizer_Scale(t *testing.T) {
	if os.Getenv("ZXPORTER_SIZER_SCALE") == "" {
		t.Skip("set ZXPORTER_SIZER_SCALE=1 to run against envtest")
	}
	env := &envtest.Environment{
		BinaryAssetsDirectory: filepath.Join("..", "..", "bin", "k8s",
			fmt.Sprintf("1.31.0-%s-%s", runtime.GOOS, runtime.GOARCH)),
	}
	cfg, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { _ = env.Stop() })

	c, err := client.New(cfg, client.Options{Scheme: clientgoscheme.Scheme})
	require.NoError(t, err)
	ctx := context.Background()

	require.NoError(t, c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testSizerNamespace}}))
	deploy := sizerDeployment(
		`{"scaleDownDelay":"2s","containers":{"manager":{"requests":{"memory":{"base":"256Mi","step":"256Mi","nodesPerStep":8,"max":"8Gi"}}}}}`,
		// Settled at the 10-node value, so every step below is a scale-up;
		// the chart's 2Gi start is covered by ScaleDownWaitsForDelay.
		"768Mi")
	deploy.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": "zxporter"}}
	deploy.Spec.Template.Labels = map[string]string{"app": "zxporter"}
	deploy.Spec.Template.Spec.Containers[0].Image = "zxporter"
	// The apiserver taints every new node not-ready, and with no kubelet here
	// nothing clears it; tolerate it so the nodes count toward the node fit.
	deploy.Spec.Template.Spec.Tolerations = []corev1.Toleration{{
		Key: corev1.TaintNodeNotReady, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule,
	}}
	require.NoError(t, c.Create(ctx, deploy))

	// No apiserver-side default page size applies to a direct reader, so
	// this is the same uncached path the manager's APIReader takes.
	sizer := NewClusterSizer(c, c, testSizerNamespace, nil)

	generation := func() int64 {
		var d appsv1.Deployment
		require.NoError(t, c.Get(ctx, client.ObjectKey{Name: ClusterSizerDeploymentName, Namespace: testSizerNamespace}, &d))
		return d.Generation
	}

	t.Logf("%6s %7s %10s %10s %9s %8s", "nodes", "cores", "expected", "request", "rollouts", "pass")
	created := 0
	for _, target := range []int{10, 27, 86, 150, 300, 600, 1000, 2000} {
		createScaleNodes(t, c, created, target)
		created = target

		size, err := sizer.clusterSize(ctx, &deploy.Spec.Template.Spec)
		require.NoError(t, err)
		require.Equal(t, target, size.Nodes, "every node page must be counted")
		require.Equal(t, target*16, size.Cores)

		before := generation()
		start := time.Now()
		_, err = sizer.ReconcileOnce(ctx)
		took := time.Since(start)
		require.NoError(t, err)
		// A second pass at the same size must be a no-op.
		_, err = sizer.ReconcileOnce(ctx)
		require.NoError(t, err)

		want := defaultLadderAt(target)
		got := managerMemoryRequest(t, c)
		rollouts := generation() - before
		t.Logf("%6d %7d %10s %10s %9d %8s", target, size.Cores, want, got, rollouts, took.Round(time.Millisecond))
		assert.Equal(t, want, got, "at %d nodes", target)
		assert.LessOrEqual(t, rollouts, int64(1), "at most one rollout per size change")
	}

	// The cluster collapses from the largest size to the median one in one
	// go: nothing moves inside the delay, then exactly one rollout lands.
	require.NoError(t, c.DeleteAllOf(ctx, &corev1.Node{}, client.MatchingLabels{"zxporter-scale-test": "true"}))
	createScaleNodes(t, c, 0, 10)
	before := generation()
	_, err = sizer.ReconcileOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, "8Gi", managerMemoryRequest(t, c), "scale-down must wait for the delay")
	time.Sleep(2100 * time.Millisecond)
	for range 3 {
		_, err = sizer.ReconcileOnce(ctx)
		require.NoError(t, err)
	}
	t.Logf("drop 2000 -> 10: request %s after delay, rollouts %d", managerMemoryRequest(t, c), generation()-before)
	assert.Equal(t, "768Mi", managerMemoryRequest(t, c))
	assert.Equal(t, int64(1), generation()-before)
}

// defaultLadderAt is the chart's default ladder, written out independently of
// ResourceLadder.Calculate.
func defaultLadderAt(nodes int) string {
	mi := min(256+256*((nodes+7)/8), 8192)
	return resource.NewQuantity(int64(mi)<<20, resource.BinarySI).String()
}

func createScaleNodes(t *testing.T, c client.Client, from, to int) {
	t.Helper()
	const workers = 16
	jobs := make(chan int)
	errs := make(chan error, to-from)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				n := sizerNode(i, "16")
				n.Labels = map[string]string{"zxporter-scale-test": "true"}
				status := n.Status
				if err := c.Create(context.Background(), n); err != nil {
					errs <- err
					continue
				}
				// The apiserver drops status on create; capacity goes
				// through the status subresource as the kubelet does it.
				n.Status = status
				if err := c.Status().Update(context.Background(), n); err != nil {
					errs <- err
				}
			}
		}()
	}
	for i := from; i < to; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}
