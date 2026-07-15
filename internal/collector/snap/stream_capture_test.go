package snap

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	metadatafake "k8s.io/client-go/metadata/fake"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	"github.com/devzero-inc/zxporter/internal/collector"
	"github.com/devzero-inc/zxporter/internal/transport"
)

// recordedBatch captures one SendBatch call.
type recordedBatch struct {
	rt           gen.ResourceType
	uids         []string
	typeComplete bool
}

// fakeBatchStream records the emitter's output.
type fakeBatchStream struct {
	mu       sync.Mutex
	batches  []recordedBatch
	sendErr  error
	finished bool
	aborted  bool
	response *gen.SendClusterSnapshotBatchedResponse
}

func (f *fakeBatchStream) SendBatch(rt gen.ResourceType, entries []*gen.SnapshotEntry, typeComplete bool) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var uids []string
	for _, e := range entries {
		uids = append(uids, e.GetUid())
	}
	f.batches = append(f.batches, recordedBatch{rt: rt, uids: uids, typeComplete: typeComplete})
	return nil
}

func (f *fakeBatchStream) Finish() (*gen.SendClusterSnapshotBatchedResponse, error) {
	f.finished = true
	if f.response != nil {
		return f.response, nil
	}
	return &gen.SendClusterSnapshotBatchedResponse{Status: "processed"}, nil
}

func (f *fakeBatchStream) Abort() { f.aborted = true }

func (f *fakeBatchStream) forType(rt gen.ResourceType) []recordedBatch {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recordedBatch
	for _, b := range f.batches {
		if b.rt == rt {
			out = append(out, b)
		}
	}
	return out
}

func staticWalk(uids ...string) func(ctx context.Context, emit func([]*gen.SnapshotEntry) error) error {
	return func(ctx context.Context, emit func([]*gen.SnapshotEntry) error) error {
		var entries []*gen.SnapshotEntry
		for _, uid := range uids {
			entries = append(entries, &gen.SnapshotEntry{Uid: uid, Name: "n-" + uid})
		}
		return emit(entries)
	}
}

func TestStreamSnapshotSources_BatchFlushThreshold(t *testing.T) {
	stream := &fakeBatchStream{}
	var uids []string
	for i := 0; i < 5; i++ {
		uids = append(uids, fmt.Sprintf("u%d", i))
	}
	sources := []snapshotSource{{rt: rtDeploymentWire, name: "deployments", walk: staticWalk(uids...)}}

	_, err := streamSnapshotSources(context.Background(), logr.Discard(), stream, sources, 2)
	require.NoError(t, err)

	got := stream.forType(rtDeploymentWire)
	require.Len(t, got, 3, "5 entries at batch size 2 = 2 full batches + final batch")
	assert.Equal(t, []string{"u0", "u1"}, got[0].uids)
	assert.False(t, got[0].typeComplete)
	assert.Equal(t, []string{"u2", "u3"}, got[1].uids)
	assert.False(t, got[1].typeComplete)
	assert.Equal(t, []string{"u4"}, got[2].uids)
	assert.True(t, got[2].typeComplete, "final batch carries type_complete")
	assert.True(t, stream.finished)
}

func TestStreamSnapshotSources_EmptyTypeStillCompletes(t *testing.T) {
	stream := &fakeBatchStream{}
	sources := []snapshotSource{{rt: rtDeploymentWire, name: "deployments", walk: staticWalk()}}

	_, err := streamSnapshotSources(context.Background(), logr.Discard(), stream, sources, 100)
	require.NoError(t, err)

	got := stream.forType(rtDeploymentWire)
	require.Len(t, got, 1)
	assert.Empty(t, got[0].uids)
	assert.True(t, got[0].typeComplete, "successful empty listing is 'looked, none exist'")
}

func TestStreamSnapshotSources_ListFailureSkipsTypeCompleteAndContinues(t *testing.T) {
	stream := &fakeBatchStream{}
	sources := []snapshotSource{
		{rt: rtDeploymentWire, name: "deployments", walk: func(ctx context.Context, emit func([]*gen.SnapshotEntry) error) error {
			if err := emit([]*gen.SnapshotEntry{{Uid: "d1"}}); err != nil {
				return err
			}
			return errors.New("apiserver timeout")
		}},
		{rt: rtServiceWire, name: "services", walk: staticWalk("s1")},
	}

	_, err := streamSnapshotSources(context.Background(), logr.Discard(), stream, sources, 1)
	require.NoError(t, err, "one failed type must not abort the snapshot")

	depl := stream.forType(rtDeploymentWire)
	for _, b := range depl {
		assert.False(t, b.typeComplete, "failed type never gets type_complete")
	}
	svc := stream.forType(rtServiceWire)
	require.NotEmpty(t, svc)
	assert.True(t, svc[len(svc)-1].typeComplete)
	assert.True(t, stream.finished, "snapshot still completes for remaining types")
}

func TestStreamSnapshotSources_SendFailureAborts(t *testing.T) {
	stream := &fakeBatchStream{sendErr: errors.New("stream broken")}
	sources := []snapshotSource{{rt: rtDeploymentWire, name: "deployments", walk: staticWalk("d1")}}

	_, err := streamSnapshotSources(context.Background(), logr.Discard(), stream, sources, 1)
	require.Error(t, err)
	assert.True(t, stream.aborted, "send failure aborts the stream (no snapshot_complete)")
	assert.False(t, stream.finished)
}

// --- production source construction ---

var testScheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	require.NoError(&testing.T{}, metav1.AddMetaToScheme(s))
	require.NoError(&testing.T{}, corev1.AddToScheme(s))
	return s
}()

func partialMeta(gvk schema.GroupVersionKind, ns, name, uid string) *metav1.PartialObjectMetadata {
	return &metav1.PartialObjectMetadata{
		TypeMeta:   metav1.TypeMeta{APIVersion: gvk.GroupVersion().String(), Kind: gvk.Kind},
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID(uid)},
	}
}

func pod(ns, name, uid, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID(uid)},
		Spec:       corev1.PodSpec{NodeName: node},
	}
}

func newTestSnapshotter(t *testing.T, k8s *k8sfake.Clientset, md *metadatafake.FakeMetadataClient, namespaces []string, excludedPods []collector.ExcludedPod, excludedNodes []string) *ClusterSnapshotter {
	t.Helper()
	return NewClusterSnapshotter(
		k8s,
		nil, // keda typed client unused by streaming capture
		nil, // dynamic client unused by streaming capture
		md,
		time.Hour,
		nil,
		nil,
		namespaces,
		excludedPods,
		excludedNodes,
		logr.Discard(),
	)
}

func TestStreamSources_EmissionOrderParentsFirst(t *testing.T) {
	k8s := k8sfake.NewSimpleClientset()
	md := metadatafake.NewSimpleMetadataClient(testScheme)
	c := newTestSnapshotter(t, k8s, md, nil, nil, nil)

	sources := c.streamSources()
	require.NotEmpty(t, sources)

	pos := make(map[gen.ResourceType]int)
	for i, s := range sources {
		pos[s.rt] = i
	}
	assert.Equal(t, 0, pos[gen.ResourceType_RESOURCE_TYPE_NAMESPACE], "namespaces first")
	assert.Equal(t, 1, pos[gen.ResourceType_RESOURCE_TYPE_NODE], "nodes second")
	assert.Less(t, pos[gen.ResourceType_RESOURCE_TYPE_DEPLOYMENT], pos[gen.ResourceType_RESOURCE_TYPE_REPLICA_SET], "owners before replicasets")
	assert.Less(t, pos[gen.ResourceType_RESOURCE_TYPE_CRON_JOB], pos[gen.ResourceType_RESOURCE_TYPE_JOB], "cronjobs before jobs")
	assert.Less(t, pos[gen.ResourceType_RESOURCE_TYPE_REPLICA_SET], pos[gen.ResourceType_RESOURCE_TYPE_POD], "pod owners before pods")
	assert.Less(t, pos[gen.ResourceType_RESOURCE_TYPE_POD], pos[gen.ResourceType_RESOURCE_TYPE_SERVICE], "remaining types after pods")
}

func TestStreamClusterState_PodsFlatWithExclusions(t *testing.T) {
	k8s := k8sfake.NewSimpleClientset(
		pod("prod", "keep-scheduled", "p1", "node-a"),
		pod("prod", "keep-unscheduled", "p2", ""),
		pod("prod", "excluded-by-name", "p3", "node-a"),
		pod("prod", "excluded-by-node", "p4", "node-bad"),
	)
	md := metadatafake.NewSimpleMetadataClient(testScheme)
	c := newTestSnapshotter(t, k8s, md, nil,
		[]collector.ExcludedPod{{Namespace: "prod", Name: "excluded-by-name"}},
		[]string{"node-bad"})

	stream := &fakeBatchStream{}
	_, err := c.streamClusterState(context.Background(), stream)
	require.NoError(t, err)

	var podUIDs []string
	for _, b := range stream.forType(gen.ResourceType_RESOURCE_TYPE_POD) {
		podUIDs = append(podUIDs, b.uids...)
	}
	assert.ElementsMatch(t, []string{"p1", "p2"}, podUIDs,
		"scheduled + unscheduled pods flat; name- and node-excluded pods dropped")
}

func TestStreamClusterState_NodeExclusionAndMetadataTypes(t *testing.T) {
	k8s := k8sfake.NewSimpleClientset()
	md := metadatafake.NewSimpleMetadataClient(testScheme,
		partialMeta(schema.GroupVersionKind{Version: "v1", Kind: "Node"}, "", "node-a", "n1"),
		partialMeta(schema.GroupVersionKind{Version: "v1", Kind: "Node"}, "", "node-bad", "n2"),
		partialMeta(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, "prod", "web", "d1"),
	)
	c := newTestSnapshotter(t, k8s, md, nil, nil, []string{"node-bad"})

	stream := &fakeBatchStream{}
	_, err := c.streamClusterState(context.Background(), stream)
	require.NoError(t, err)

	var nodeUIDs []string
	for _, b := range stream.forType(gen.ResourceType_RESOURCE_TYPE_NODE) {
		nodeUIDs = append(nodeUIDs, b.uids...)
	}
	assert.ElementsMatch(t, []string{"n1"}, nodeUIDs, "excluded node dropped")

	var deplBatches []recordedBatch
	deplBatches = stream.forType(gen.ResourceType_RESOURCE_TYPE_DEPLOYMENT)
	require.NotEmpty(t, deplBatches)
	var deplUIDs []string
	for _, b := range deplBatches {
		deplUIDs = append(deplUIDs, b.uids...)
	}
	assert.ElementsMatch(t, []string{"d1"}, deplUIDs)
	assert.True(t, stream.finished)
}

func TestStreamClusterState_TargetNamespaceScoping(t *testing.T) {
	k8s := k8sfake.NewSimpleClientset(
		pod("prod", "in-scope", "p1", "node-a"),
		pod("other", "out-of-scope", "p2", "node-a"),
	)
	md := metadatafake.NewSimpleMetadataClient(testScheme,
		partialMeta(schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}, "", "prod", "ns1"),
		partialMeta(schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}, "", "other", "ns2"),
		partialMeta(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, "prod", "web", "d1"),
		partialMeta(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, "other", "api", "d2"),
	)
	c := newTestSnapshotter(t, k8s, md, []string{"prod"}, nil, nil)

	stream := &fakeBatchStream{}
	_, err := c.streamClusterState(context.Background(), stream)
	require.NoError(t, err)

	collect := func(rt gen.ResourceType) []string {
		var uids []string
		for _, b := range stream.forType(rt) {
			uids = append(uids, b.uids...)
		}
		return uids
	}
	assert.ElementsMatch(t, []string{"ns1"}, collect(gen.ResourceType_RESOURCE_TYPE_NAMESPACE), "only target namespaces")
	assert.ElementsMatch(t, []string{"d1"}, collect(gen.ResourceType_RESOURCE_TYPE_DEPLOYMENT), "namespaced types scoped to targets")
	assert.ElementsMatch(t, []string{"p1"}, collect(gen.ResourceType_RESOURCE_TYPE_POD), "pods scoped to targets")
}

// --- takeSnapshot routing / fallback ---

// fakeStreamingSender implements transport.DirectSender plus the batched
// snapshot capability.
type fakeStreamingSender struct {
	stream        *fakeBatchStream
	openErr       error
	opened        int
	legacyStreams int
}

func (f *fakeStreamingSender) SendBatch(ctx context.Context, resource []collector.CollectedResource, resourceType collector.ResourceType) (string, error) {
	return "", nil
}

func (f *fakeStreamingSender) Send(ctx context.Context, resource collector.CollectedResource) (string, error) {
	return "", nil
}

func (f *fakeStreamingSender) SendClusterSnapshotStream(ctx context.Context, snapshot *gen.ClusterSnapshot, snapshotID string, timestamp time.Time) (string, *gen.ClusterSnapshot, error) {
	f.legacyStreams++
	return "cluster-1", nil, nil
}

func (f *fakeStreamingSender) SendTelemetryLogs(ctx context.Context, in *gen.SendTelemetryLogsRequest) (*gen.SendTelemetryLogsResponse, error) {
	return &gen.SendTelemetryLogsResponse{}, nil
}

func (f *fakeStreamingSender) OpenClusterSnapshotBatchStream(ctx context.Context, snapshotID string, timestamp time.Time, isFullSnapshot bool) (transport.SnapshotBatchStream, error) {
	f.opened++
	if f.openErr != nil {
		return nil, f.openErr
	}
	return f.stream, nil
}

// unsupportedStream fails the first send with the unsupported sentinel,
// mimicking an old backend behind a working connection.
type unsupportedStream struct{ fakeBatchStream }

func (u *unsupportedStream) SendBatch(rt gen.ResourceType, entries []*gen.SnapshotEntry, typeComplete bool) error {
	return fmt.Errorf("wrapped: %w", transport.ErrSnapshotBatchedUnsupported)
}

func newRoutingSnapshotter(t *testing.T, sender transport.DirectSender) *ClusterSnapshotter {
	t.Helper()
	k8s := k8sfake.NewSimpleClientset()
	md := metadatafake.NewSimpleMetadataClient(testScheme)
	return NewClusterSnapshotter(
		k8s, nil, nil, md,
		time.Hour,
		sender,
		nil,
		nil, nil, nil,
		logr.Discard(),
	)
}

func TestTakeSnapshot_UsesStreamingWhenSupported(t *testing.T) {
	sender := &fakeStreamingSender{stream: &fakeBatchStream{}}
	c := newRoutingSnapshotter(t, sender)

	c.takeSnapshot(context.Background())

	assert.Equal(t, 1, sender.opened)
	assert.True(t, sender.stream.finished)
	assert.Equal(t, 0, sender.legacyStreams, "no legacy snapshot when streaming works")
}

func TestTakeSnapshot_KillSwitchForcesLegacy(t *testing.T) {
	t.Setenv("SNAPSHOT_STREAMING_DISABLED", "true")
	sender := &fakeStreamingSender{stream: &fakeBatchStream{}}
	c := newRoutingSnapshotter(t, sender)

	c.takeSnapshot(context.Background())

	assert.Equal(t, 0, sender.opened)
	assert.Equal(t, 1, sender.legacyStreams)
}

func TestTakeSnapshot_UnsupportedBackendFallsBackAndRemembers(t *testing.T) {
	// Sender whose streams always report the backend as unsupported.
	s := &fakeStreamingSender{}
	cs := newRoutingSnapshotter(t, &unsupportedSender{inner: s})

	cs.takeSnapshot(context.Background())
	assert.Equal(t, 1, s.opened, "first cycle probes the new path")
	assert.Equal(t, 1, s.legacyStreams, "falls back to legacy in the same cycle")

	cs.takeSnapshot(context.Background())
	assert.Equal(t, 1, s.opened, "unsupported backend is remembered; no immediate re-probe")
	assert.Equal(t, 2, s.legacyStreams)
}

// unsupportedSender opens streams that always fail with the unsupported sentinel.
type unsupportedSender struct{ inner *fakeStreamingSender }

func (u *unsupportedSender) SendBatch(ctx context.Context, resource []collector.CollectedResource, resourceType collector.ResourceType) (string, error) {
	return u.inner.SendBatch(ctx, resource, resourceType)
}

func (u *unsupportedSender) Send(ctx context.Context, resource collector.CollectedResource) (string, error) {
	return u.inner.Send(ctx, resource)
}

func (u *unsupportedSender) SendClusterSnapshotStream(ctx context.Context, snapshot *gen.ClusterSnapshot, snapshotID string, timestamp time.Time) (string, *gen.ClusterSnapshot, error) {
	return u.inner.SendClusterSnapshotStream(ctx, snapshot, snapshotID, timestamp)
}

func (u *unsupportedSender) SendTelemetryLogs(ctx context.Context, in *gen.SendTelemetryLogsRequest) (*gen.SendTelemetryLogsResponse, error) {
	return u.inner.SendTelemetryLogs(ctx, in)
}

func (u *unsupportedSender) OpenClusterSnapshotBatchStream(ctx context.Context, snapshotID string, timestamp time.Time, isFullSnapshot bool) (transport.SnapshotBatchStream, error) {
	u.inner.opened++
	return &unsupportedStream{}, nil
}

// flippableSender serves unsupported streams until flipped, then working ones.
type flippableSender struct {
	fakeStreamingSender
	supported bool
}

func (f *flippableSender) OpenClusterSnapshotBatchStream(ctx context.Context, snapshotID string, timestamp time.Time, isFullSnapshot bool) (transport.SnapshotBatchStream, error) {
	f.opened++
	if !f.supported {
		return &unsupportedStream{}, nil
	}
	f.stream = &fakeBatchStream{}
	return f.stream, nil
}

func TestTakeSnapshot_SuccessfulReprobeRestoresStreaming(t *testing.T) {
	sender := &flippableSender{}
	c := newRoutingSnapshotter(t, sender)

	// Cycle 1: backend is old → probe fails, remembered, legacy fallback.
	c.takeSnapshot(context.Background())
	require.Equal(t, 1, sender.opened)
	require.Equal(t, 1, sender.legacyStreams)

	// Backend upgrades before the re-probe cycle.
	sender.supported = true

	// Cycles 2-7: no probing while the backend is remembered as unsupported.
	for i := 0; i < 6; i++ {
		c.takeSnapshot(context.Background())
	}
	require.Equal(t, 1, sender.opened)
	require.Equal(t, 7, sender.legacyStreams)

	// Cycle 8: re-probe succeeds.
	c.takeSnapshot(context.Background())
	require.Equal(t, 2, sender.opened)
	require.Equal(t, 7, sender.legacyStreams)

	// Cycle 9: a successful re-probe must clear the flag — streaming resumes
	// every cycle instead of 1-in-8.
	c.takeSnapshot(context.Background())
	assert.Equal(t, 3, sender.opened, "successful re-probe restores full streaming")
	assert.Equal(t, 7, sender.legacyStreams, "no further legacy fallbacks after recovery")
}

// gatedStream blocks its first SendBatch until released, so tests can hold a
// streaming snapshot mid-send.
type gatedStream struct {
	fakeBatchStream
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gatedStream) SendBatch(rt gen.ResourceType, entries []*gen.SnapshotEntry, typeComplete bool) error {
	g.once.Do(func() { close(g.started) })
	<-g.release
	return g.fakeBatchStream.SendBatch(rt, entries, typeComplete)
}

// gatedSender hands out gated streams and counts opens.
type gatedSender struct {
	fakeStreamingSender
	mu      sync.Mutex
	streams []*gatedStream
}

func (g *gatedSender) OpenClusterSnapshotBatchStream(ctx context.Context, snapshotID string, timestamp time.Time, isFullSnapshot bool) (transport.SnapshotBatchStream, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := &gatedStream{started: make(chan struct{}), release: make(chan struct{})}
	g.streams = append(g.streams, s)
	return s, nil
}

func (g *gatedSender) openCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.streams)
}

func TestTakeSnapshot_ConcurrentCallsDoNotStreamInParallel(t *testing.T) {
	sender := &gatedSender{}
	c := newRoutingSnapshotter(t, sender)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); c.takeSnapshot(context.Background()) }()

	// Wait until the first snapshot is mid-send (holding the send lock).
	var first *gatedStream
	require.Eventually(t, func() bool {
		if sender.openCount() == 0 {
			return false
		}
		sender.mu.Lock()
		first = sender.streams[0]
		sender.mu.Unlock()
		select {
		case <-first.started:
			return true
		default:
			return false
		}
	}, 2*time.Second, time.Millisecond)

	go func() { defer wg.Done(); c.takeSnapshot(context.Background()) }()

	// The second snapshot must not open a stream while the first is in flight
	// ("don't send multiple snapshots at once" — same invariant as the legacy
	// sendSnapshot mutex).
	assert.Never(t, func() bool { return sender.openCount() > 1 }, 150*time.Millisecond, 10*time.Millisecond,
		"second snapshot streamed concurrently with the first")

	close(first.release)
	// Release any stream the second snapshot opens after the first finishes.
	go func() {
		for {
			sender.mu.Lock()
			for _, s := range sender.streams[1:] {
				select {
				case <-s.release:
				default:
					close(s.release)
				}
			}
			done := len(sender.streams) >= 2
			sender.mu.Unlock()
			if done {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	wg.Wait()

	assert.Equal(t, 2, sender.openCount(), "both snapshots eventually stream, one after the other")
}

func TestOrderMissingResources_ParentsFirst(t *testing.T) {
	missing := []*gen.MissingResource{
		{ResourceType: gen.ResourceType_RESOURCE_TYPE_SERVICE, Uid: "svc"},
		{ResourceType: gen.ResourceType_RESOURCE_TYPE_REPLICA_SET, Uid: "rs"},
		{ResourceType: gen.ResourceType_RESOURCE_TYPE_DEPLOYMENT, Uid: "deploy"},
		{ResourceType: gen.ResourceType_RESOURCE_TYPE_JOB, Uid: "job"},
		{ResourceType: gen.ResourceType_RESOURCE_TYPE_CRON_JOB, Uid: "cron"},
	}

	ordered := orderMissingResources(missing)

	var uids []string
	for _, m := range ordered {
		uids = append(uids, m.Uid)
	}
	assert.Equal(t, []string{"deploy", "cron", "rs", "job", "svc"}, uids,
		"workload owners, then replicasets/jobs, then the rest; stable within a rank")
}

func TestMissingResourceHandlerKey_MapsWireTypesToRefreshHandlers(t *testing.T) {
	cases := []struct {
		rt         gen.ResourceType
		key        string
		namespaced bool
		ok         bool
	}{
		{gen.ResourceType_RESOURCE_TYPE_DEPLOYMENT, "deployment", true, true},
		{gen.ResourceType_RESOURCE_TYPE_REPLICA_SET, "replica_set", true, true},
		{gen.ResourceType_RESOURCE_TYPE_HORIZONTAL_POD_AUTOSCALER, "horizontal_pod_autoscaler", true, true},
		{gen.ResourceType_RESOURCE_TYPE_PERSISTENT_VOLUME, "persistent_volume", false, true},
		{gen.ResourceType_RESOURCE_TYPE_VOLUME_ATTACHMENT, "volume_attachment", false, true},
		// no refresh handler exists for secrets (parity with the legacy refresh maps)
		{gen.ResourceType_RESOURCE_TYPE_SECRET, "", false, false},
	}
	for _, tc := range cases {
		key, namespaced, ok := missingResourceHandlerKey(tc.rt)
		assert.Equal(t, tc.ok, ok, "%s ok", tc.rt)
		if tc.ok {
			assert.Equal(t, tc.key, key, "%s key", tc.rt)
			assert.Equal(t, tc.namespaced, namespaced, "%s namespaced", tc.rt)
		}
	}
}

func TestTakeSnapshot_TransientStreamErrorDoesNotFallBack(t *testing.T) {
	sender := &fakeStreamingSender{stream: &fakeBatchStream{sendErr: errors.New("connection reset")}}
	c := newRoutingSnapshotter(t, sender)

	c.takeSnapshot(context.Background())

	assert.Equal(t, 1, sender.opened)
	assert.Equal(t, 0, sender.legacyStreams,
		"transient stream failures wait for the next cycle instead of double-sending via legacy")
	assert.True(t, sender.stream.aborted)
}
