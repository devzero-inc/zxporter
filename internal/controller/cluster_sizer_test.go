package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSizerNamespace = "devzero-zxporter"

// The chart's default ladder: 256Mi + 256Mi per 8 nodes, capped at 8Gi.
const defaultSizerAnnotation = `{"scaleDownDelay":"1h","containers":{"manager":{"requests":{"memory":{"base":"256Mi","step":"256Mi","nodesPerStep":8,"max":"8Gi"}}}}}`

func TestResourceLadder_Calculate(t *testing.T) {
	q := func(s string) *resource.Quantity { v := resource.MustParse(s); return &v }
	// A ladder with both terms active: 2Gi base, 256Mi per 20 nodes or 480
	// cores, capped at 8Gi.
	both := ResourceLadder{Base: q("2Gi"), Step: q("256Mi"), Max: q("8Gi"), NodesPerStep: 20, CoresPerStep: 480}

	tests := []struct {
		name   string
		ladder ResourceLadder
		res    corev1.ResourceName
		size   ClusterSize
		want   string
	}{
		{"nodes term wins at 47 nodes", both, corev1.ResourceMemory, ClusterSize{Nodes: 47, Cores: 416}, "2816Mi"},
		{"cores term wins on few large nodes", both, corev1.ResourceMemory, ClusterSize{Nodes: 10, Cores: 2000}, "3328Mi"},
		{"capped at max", both, corev1.ResourceMemory, ClusterSize{Nodes: 1000, Cores: 100}, "8Gi"},
		{"partial step rounds up", both, corev1.ResourceMemory, ClusterSize{Nodes: 21, Cores: 1}, "2560Mi"},
		{"cpu in milli-cores", ResourceLadder{Base: q("100m"), Step: q("50m"), NodesPerStep: 10}, corev1.ResourceCPU, ClusterSize{Nodes: 25}, "250m"},
		{"no step terms keeps base", ResourceLadder{Base: q("512Mi")}, corev1.ResourceMemory, ClusterSize{Nodes: 500, Cores: 9000}, "512Mi"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.ladder.Calculate(tt.size, tt.res)
			assert.Zero(t, got.Cmp(resource.MustParse(tt.want)), "got %s, want %s", got.String(), tt.want)
		})
	}
}

func TestClusterSizer_ScalesUpImmediately(t *testing.T) {
	c := newSizerFakeClient(t, sizerDeployment(defaultSizerAnnotation, "2Gi"), sizerNodes(200, "8")...)
	sizer := NewClusterSizer(c, c, testSizerNamespace, nil)

	_, err := sizer.ReconcileOnce(context.Background())
	require.NoError(t, err)

	// 256Mi + ceil(200/8)=25 steps * 256Mi = 6656Mi.
	assert.Equal(t, "6656Mi", managerMemoryRequest(t, c))
}

func TestClusterSizer_ScaleDownWaitsForDelay(t *testing.T) {
	c := newSizerFakeClient(t, sizerDeployment(defaultSizerAnnotation, "2Gi"), sizerNodes(10, "4")...)
	sizer := NewClusterSizer(c, c, testSizerNamespace, nil)
	clock := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	sizer.now = func() time.Time { return clock }

	for _, tick := range []struct {
		advance time.Duration
		want    string
	}{
		{0, "2Gi"},                // first sight of the lower value starts the timer
		{59 * time.Minute, "2Gi"}, // still inside the delay
		{time.Minute, "768Mi"},    // delay elapsed: 256Mi + ceil(10/8)*256Mi
	} {
		clock = clock.Add(tick.advance)
		_, err := sizer.ReconcileOnce(context.Background())
		require.NoError(t, err)
		assert.Equal(t, tick.want, managerMemoryRequest(t, c), "after %s", tick.advance)
	}
}

func TestClusterSizer_ScaleDownTimerResetsWhenClusterGrowsBack(t *testing.T) {
	c := newSizerFakeClient(t, sizerDeployment(defaultSizerAnnotation, "768Mi"), sizerNodes(10, "4")...)
	sizer := NewClusterSizer(c, c, testSizerNamespace, nil)
	clock := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	sizer.now = func() time.Time { return clock }
	ctx := context.Background()

	// Shrink to 2 nodes: 512Mi wanted, timer starts.
	deleteSizerNodes(t, c, 2, 10)
	_, err := sizer.ReconcileOnce(ctx)
	require.NoError(t, err)

	// Grow back before the delay: the wanted value matches again, timer clears.
	clock = clock.Add(30 * time.Minute)
	createSizerNodes(t, c, 2, 10, "4")
	_, err = sizer.ReconcileOnce(ctx)
	require.NoError(t, err)

	// Shrink again: a fresh hour must pass, so 40 minutes later nothing moves.
	deleteSizerNodes(t, c, 2, 10)
	clock = clock.Add(time.Minute)
	_, err = sizer.ReconcileOnce(ctx)
	require.NoError(t, err)
	clock = clock.Add(40 * time.Minute)
	_, err = sizer.ReconcileOnce(ctx)
	require.NoError(t, err)

	assert.Equal(t, "768Mi", managerMemoryRequest(t, c))
}

func TestClusterSizer_LeavesDeploymentAloneWhen(t *testing.T) {
	tests := []struct {
		name       string
		annotation string
		nodes      int
		cordoned   bool
		wantErr    string
	}{
		{name: "annotation is absent", annotation: "", nodes: 200},
		{name: "node list is empty", annotation: defaultSizerAnnotation, nodes: 0},
		{name: "config is not JSON", annotation: "{", nodes: 200, wantErr: "parse devzero.io/cluster-sizer"},
		{name: "config names a missing container", annotation: `{"containers":{"sidecar":{"requests":{"memory":{"base":"1Gi"}}}}}`, nodes: 200, wantErr: `container "sidecar"`},
		{name: "step has no per-step divisor", annotation: `{"containers":{"manager":{"requests":{"memory":{"base":"1Gi","step":"1Gi"}}}}}`, nodes: 200, wantErr: "neither nodesPerStep nor coresPerStep"},
		{name: "ladder has neither base nor step", annotation: `{"containers":{"manager":{"requests":{"memory":{"nodesPerStep":2}}}}}`, nodes: 200, wantErr: "neither base nor step"},
		{name: "maxNodePercent is above 100", annotation: `{"maxNodePercent":150,"containers":{"manager":{"requests":{"memory":{"base":"1Gi"}}}}}`, nodes: 200, wantErr: "maxNodePercent"},
		{name: "a container has no ladders", annotation: `{"containers":{"manager":null}}`, nodes: 200, wantErr: "has no requests or limits"},
		{name: "no node the pod can schedule on", annotation: defaultSizerAnnotation, nodes: 200, cordoned: true, wantErr: "no schedulable node reports allocatable memory"},
		{name: "max is below base", annotation: `{"containers":{"manager":{"requests":{"memory":{"base":"2Gi","max":"1Gi"}}}}}`, nodes: 200, wantErr: "below base"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodes := sizerNodes(tt.nodes, "8")
			for _, n := range nodes {
				n.(*corev1.Node).Spec.Unschedulable = tt.cordoned
			}
			c := newSizerFakeClient(t, sizerDeployment(tt.annotation, "2Gi"), nodes...)
			_, err := NewClusterSizer(c, c, testSizerNamespace, nil).ReconcileOnce(context.Background())
			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tt.wantErr)
			}
			assert.Equal(t, "2Gi", managerMemoryRequest(t, c))
		})
	}
}

func TestClusterSizer_CapsRequestAtNodeFit(t *testing.T) {
	// 300 nodes put the default ladder at its 8Gi max; every row adds one
	// 64Gi node and decides whether the pod could land on it.
	bigNode := func(mutate func(*corev1.Node)) *corev1.Node {
		n := fitNode(300, "64Gi")
		n.Labels = map[string]string{"pool": "big"}
		mutate(n)
		return n
	}
	tainted := func(n *corev1.Node) {
		n.Spec.Taints = []corev1.Taint{{Key: "gpu", Value: "true", Effect: corev1.TaintEffectNoSchedule}}
	}
	tests := []struct {
		name       string
		annotation string
		big        *corev1.Node
		tolerate   bool
		selector   map[string]string
		want       string
	}{
		{name: "half the largest node by default", annotation: defaultSizerAnnotation, want: "4Gi"},
		{name: "maxNodePercent from the annotation", annotation: `{"maxNodePercent":75,"containers":{"manager":{"requests":{"memory":{"base":"256Mi","step":"256Mi","nodesPerStep":8,"max":"8Gi"}}}}}`, want: "6Gi"},
		{name: "a large node the pod can reach lifts the cap", annotation: defaultSizerAnnotation, big: bigNode(func(*corev1.Node) {}), want: "8Gi"},
		{name: "a tainted large node is ignored", annotation: defaultSizerAnnotation, big: bigNode(tainted), want: "4Gi"},
		{name: "a tolerated taint counts", annotation: defaultSizerAnnotation, big: bigNode(tainted), tolerate: true, want: "8Gi"},
		{name: "a cordoned large node is ignored", annotation: defaultSizerAnnotation, big: bigNode(func(n *corev1.Node) { n.Spec.Unschedulable = true }), want: "4Gi"},
		{name: "a large node outside the nodeSelector is ignored", annotation: defaultSizerAnnotation, big: bigNode(func(*corev1.Node) {}), selector: map[string]string{"pool": "small"}, want: "4Gi"},
		{name: "a large node missing a label selected with an empty value is ignored", annotation: defaultSizerAnnotation, big: bigNode(func(*corev1.Node) {}), selector: map[string]string{"dedicated": ""}, want: "4Gi"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deploy := sizerDeployment(tt.annotation, "2Gi")
			pod := &deploy.Spec.Template.Spec
			pod.NodeSelector = tt.selector
			if tt.tolerate {
				pod.Tolerations = []corev1.Toleration{{Key: "gpu", Operator: corev1.TolerationOpExists}}
			}
			nodes := make([]client.Object, 0, 301)
			for i := range 300 {
				n := fitNode(i, "8Gi")
				n.Labels = map[string]string{"pool": "small", "dedicated": ""}
				nodes = append(nodes, n)
			}
			if tt.big != nil {
				nodes = append(nodes, tt.big)
			}
			c := newSizerFakeClient(t, deploy, nodes...)
			_, err := NewClusterSizer(c, c, testSizerNamespace, nil).ReconcileOnce(context.Background())
			require.NoError(t, err)
			assert.Equal(t, tt.want, managerMemoryRequest(t, c))
		})
	}
}

func TestClusterSizer_KeepsRequestsWithinLimits(t *testing.T) {
	tests := []struct {
		name       string
		annotation string
		request    string
		limit      string
		wantReq    string
		wantLim    string
	}{
		// 200 nodes ask for 6656Mi, above the container's 2Gi limit.
		{name: "a sized request stops at the limit", annotation: defaultSizerAnnotation, request: "1Gi", limit: "2Gi", wantReq: "2Gi", wantLim: "2Gi"},
		// The limit ladder wants 1Gi, below the unsized 2Gi request.
		{name: "a sized limit stops at an unsized request", annotation: `{"scaleDownDelay":"1m","containers":{"manager":{"limits":{"memory":{"base":"1Gi"}}}}}`, request: "2Gi", limit: "4Gi", wantReq: "2Gi", wantLim: "2Gi"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deploy := sizerDeployment(tt.annotation, tt.request)
			deploy.Spec.Template.Spec.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(tt.limit)}
			c := newSizerFakeClient(t, deploy, sizerNodes(200, "8")...)
			sizer := NewClusterSizer(c, c, testSizerNamespace, nil)
			clock := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
			sizer.now = func() time.Time { return clock }
			for range 2 {
				_, err := sizer.ReconcileOnce(context.Background())
				require.NoError(t, err)
				clock = clock.Add(2 * time.Minute)
			}
			assert.Equal(t, tt.wantReq, managerMemoryRequest(t, c))
			assert.Equal(t, tt.wantLim, managerMemoryLimit(t, c))
		})
	}
}

func TestClusterSizer_LimitDelayDoesNotCarryTheRequestPastItsOwn(t *testing.T) {
	// The request ladder is 1Gi + 1Gi per node; the limit ladder a flat 2Gi.
	annotation := `{"scaleDownDelay":"1h","containers":{"manager":{` +
		`"requests":{"memory":{"base":"1Gi","step":"1Gi","nodesPerStep":1}},` +
		`"limits":{"memory":{"base":"2Gi"}}}}}`
	deploy := sizerDeployment(annotation, "4Gi")
	deploy.Spec.Template.Spec.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("8Gi")}
	c := newSizerFakeClient(t, deploy, sizerNodes(3, "8")...)
	sizer := NewClusterSizer(c, c, testSizerNamespace, nil)
	clock := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	sizer.now = func() time.Time { return clock }
	ctx := context.Background()

	for _, step := range []struct {
		advance          time.Duration
		shrinkTo1        bool
		wantReq, wantLim string
	}{
		{0, false, "4Gi", "8Gi"},                // request settled at 4Gi; limit's 1h delay starts
		{61 * time.Minute, true, "4Gi", "4Gi"},  // limit due, request's delay only starts: limit held at 4Gi
		{61 * time.Minute, false, "2Gi", "2Gi"}, // both due
	} {
		clock = clock.Add(step.advance)
		if step.shrinkTo1 {
			deleteSizerNodes(t, c, 1, 3)
		}
		_, err := sizer.ReconcileOnce(ctx)
		require.NoError(t, err)
		assert.Equal(t, step.wantReq, managerMemoryRequest(t, c), "request after %s", step.advance)
		assert.Equal(t, step.wantLim, managerMemoryLimit(t, c), "limit after %s", step.advance)
	}
}

func TestClusterSizer_CapsALimitThatStandsInForTheRequest(t *testing.T) {
	// No memory request: the API server would default it to the limit, so
	// the limit is capped like a request (half of 8Gi).
	annotation := `{"containers":{"manager":{"limits":{"memory":{"base":"8Gi"}}}}}`
	deploy := sizerDeployment(annotation, "2Gi")
	delete(deploy.Spec.Template.Spec.Containers[0].Resources.Requests, corev1.ResourceMemory)
	c := newSizerFakeClient(t, deploy, fitNode(0, "8Gi"), fitNode(1, "8Gi"))

	_, err := NewClusterSizer(c, c, testSizerNamespace, nil).ReconcileOnce(context.Background())

	require.NoError(t, err)
	assert.Equal(t, "4Gi", managerMemoryLimit(t, c))
}

func TestClusterSizer_CountsNativeSidecarsInThePodRequest(t *testing.T) {
	// A restartable init container runs beside the manager: 2Gi + 3Gi is
	// more than the 4Gi node, though each alone fits.
	deploy := sizerDeployment(`{"containers":{"manager":{"requests":{"memory":{"base":"2Gi"}}}}}`, "1Gi")
	always := corev1.ContainerRestartPolicyAlways
	deploy.Spec.Template.Spec.InitContainers = []corev1.Container{{
		Name:          "sidecar",
		RestartPolicy: &always,
		Resources:     corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("3Gi")}},
	}}
	c := newSizerFakeClient(t, deploy, fitNode(0, "4Gi"))

	_, err := NewClusterSizer(c, c, testSizerNamespace, nil).ReconcileOnce(context.Background())

	require.ErrorContains(t, err, "fit no schedulable node")
	assert.Equal(t, "1Gi", managerMemoryRequest(t, c))
}

func TestClusterSizer_RefusesWhenNoSingleNodeFitsThePod(t *testing.T) {
	// Each resource fits the largest node for it, but those are different
	// nodes: 16 cores fit only the first and 16Gi only the second.
	wide := sizerNode(0, "32")
	wide.Status.Allocatable[corev1.ResourceMemory] = resource.MustParse("2Gi")
	tall := sizerNode(1, "2")
	tall.Status.Allocatable[corev1.ResourceMemory] = resource.MustParse("32Gi")
	annotation := `{"containers":{"manager":{"requests":{"cpu":{"base":"16"},"memory":{"base":"16Gi"}}}}}`
	c := newSizerFakeClient(t, sizerDeployment(annotation, "2Gi"), wide, tall)

	_, err := NewClusterSizer(c, c, testSizerNamespace, nil).ReconcileOnce(context.Background())

	require.ErrorContains(t, err, "fit no schedulable node")
	assert.Equal(t, "2Gi", managerMemoryRequest(t, c))
}

func TestClusterSizer_NeverWritesARequestTheCapRoundsToZero(t *testing.T) {
	// 50% of 1m allocatable CPU is 0m.
	annotation := `{"scaleDownDelay":"1m","containers":{"manager":{"requests":{"cpu":{"base":"100m"}}}}}`
	c := newSizerFakeClient(t, sizerDeployment(annotation, "2Gi"), sizerNode(0, "1m"))
	sizer := NewClusterSizer(c, c, testSizerNamespace, nil)
	clock := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	sizer.now = func() time.Time { return clock }

	for range 2 {
		_, err := sizer.ReconcileOnce(context.Background())
		require.NoError(t, err)
		clock = clock.Add(2 * time.Minute)
	}
	// managerMemoryRequest fails unless the CPU request is still 200m.
	assert.Equal(t, "2Gi", managerMemoryRequest(t, c))
}

func TestClusterSizer_PollIntervalComesFromAnnotation(t *testing.T) {
	for _, tt := range []struct {
		annotation string
		want       time.Duration
	}{
		{defaultSizerAnnotation, defaultClusterSizerPollInterval},
		{`{"pollInterval":"10m","containers":{"manager":{"requests":{"memory":{"base":"1Gi"}}}}}`, 10 * time.Minute},
		{`{"pollInterval":"1s","containers":{"manager":{"requests":{"memory":{"base":"1Gi"}}}}}`, minClusterSizerPollInterval},
	} {
		c := newSizerFakeClient(t, sizerDeployment(tt.annotation, "2Gi"), sizerNodes(3, "2")...)
		next, err := NewClusterSizer(c, c, testSizerNamespace, nil).ReconcileOnce(context.Background())
		require.NoError(t, err)
		assert.Equal(t, tt.want, next, tt.annotation)
	}
}

func newSizerFakeClient(t *testing.T, deploy *appsv1.Deployment, nodes ...client.Object) client.Client {
	t.Helper()
	objects := append([]client.Object{deploy}, nodes...)
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func sizerDeployment(annotation, memoryRequest string) *appsv1.Deployment {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: ClusterSizerDeploymentName, Namespace: testSizerNamespace},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "manager",
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("200m"),
				corev1.ResourceMemory: resource.MustParse(memoryRequest),
			}},
		}}}}},
	}
	if annotation != "" {
		d.Annotations = map[string]string{ClusterSizerAnnotation: annotation}
	}
	return d
}

func sizerNode(i int, cpu string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("node-%d", i)},
		Status: corev1.NodeStatus{
			Capacity: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(cpu),
				corev1.ResourceMemory: resource.MustParse("64Gi"),
			},
		},
	}
}

func fitNode(i int, allocatable string) *corev1.Node {
	n := sizerNode(i, "4")
	n.Status.Allocatable[corev1.ResourceMemory] = resource.MustParse(allocatable)
	return n
}

func sizerNodes(n int, cpu string) []client.Object {
	out := make([]client.Object, 0, n)
	for i := range n {
		out = append(out, sizerNode(i, cpu))
	}
	return out
}

func createSizerNodes(t *testing.T, c client.Client, from, to int, cpu string) {
	t.Helper()
	for i := from; i < to; i++ {
		require.NoError(t, c.Create(context.Background(), sizerNode(i, cpu)))
	}
}

func deleteSizerNodes(t *testing.T, c client.Client, from, to int) {
	t.Helper()
	for i := from; i < to; i++ {
		require.NoError(t, c.Delete(context.Background(), sizerNode(i, "1")))
	}
}

func managerMemoryRequest(t *testing.T, c client.Client) string {
	t.Helper()
	var d appsv1.Deployment
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: ClusterSizerDeploymentName, Namespace: testSizerNamespace}, &d))
	// The sizer must never touch a resource its config does not list.
	cpu := d.Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
	require.Equal(t, "200m", cpu.String())
	mem := d.Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory]
	return mem.String()
}

func managerMemoryLimit(t *testing.T, c client.Client) string {
	t.Helper()
	var d appsv1.Deployment
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: ClusterSizerDeploymentName, Namespace: testSizerNamespace}, &d))
	lim := d.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory]
	return lim.String()
}
