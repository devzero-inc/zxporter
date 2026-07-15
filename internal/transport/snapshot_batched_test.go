package transport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	genconnect "github.com/devzero-inc/zxporter/gen/api/v1/apiv1connect"
)

// batchRecordingServer records every ClusterSnapshotBatch it receives.
type batchRecordingServer struct {
	genconnect.UnimplementedMetricsCollectorServiceHandler

	mu       sync.Mutex
	received []*gen.ClusterSnapshotBatch
	response *gen.SendClusterSnapshotBatchedResponse
}

func (s *batchRecordingServer) SendClusterSnapshotBatched(
	ctx context.Context,
	stream *connect.ClientStream[gen.ClusterSnapshotBatch],
) (*connect.Response[gen.SendClusterSnapshotBatchedResponse], error) {
	for stream.Receive() {
		s.mu.Lock()
		s.received = append(s.received, proto.Clone(stream.Msg()).(*gen.ClusterSnapshotBatch))
		s.mu.Unlock()
	}
	if err := stream.Err(); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	resp := s.response
	if resp == nil {
		resp = &gen.SendClusterSnapshotBatchedResponse{Status: "processed"}
	}
	return connect.NewResponse(resp), nil
}

func (s *batchRecordingServer) batches() []*gen.ClusterSnapshotBatch {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*gen.ClusterSnapshotBatch(nil), s.received...)
}

func newBatchTestClient(t *testing.T, handler genconnect.MetricsCollectorServiceHandler) *RealDakrClient {
	t.Helper()
	mux := http.NewServeMux()
	path, h := genconnect.NewMetricsCollectorServiceHandler(handler)
	mux.Handle(path, h)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return &RealDakrClient{
		logger:        logr.Discard(),
		client:        genconnect.NewMetricsCollectorServiceClient(&http.Client{Timeout: 30 * time.Second}, server.URL),
		clientHeaders: NewClientHeaders("test-token"),
	}
}

func snapshotCtx() context.Context {
	ctx := context.WithValue(context.Background(), clusterIDContextKey, "cluster-123")
	return context.WithValue(ctx, teamIDContextKey, "team-456")
}

func TestSnapshotBatchStream_StampsIdentityOnEveryMessage(t *testing.T) {
	server := &batchRecordingServer{response: &gen.SendClusterSnapshotBatchedResponse{
		Status:                    "processed",
		MissingResources:          []*gen.MissingResource{{Uid: "m-1", Name: "missing", ResourceType: gen.ResourceType_RESOURCE_TYPE_DEPLOYMENT}},
		MissingResourcesTruncated: true,
	}}
	client := newBatchTestClient(t, server)

	ts := time.Now().UTC().Truncate(time.Second)
	stream, err := client.OpenClusterSnapshotBatchStream(snapshotCtx(), "snap-42", ts, true)
	require.NoError(t, err)

	require.NoError(t, stream.SendBatch(gen.ResourceType_RESOURCE_TYPE_DEPLOYMENT,
		[]*gen.SnapshotEntry{{Uid: "d1", Name: "web", Namespace: "prod"}}, false))
	require.NoError(t, stream.SendBatch(gen.ResourceType_RESOURCE_TYPE_DEPLOYMENT, nil, true))

	resp, err := stream.Finish()
	require.NoError(t, err)

	require.Len(t, resp.MissingResources, 1)
	assert.Equal(t, "m-1", resp.MissingResources[0].Uid)
	assert.True(t, resp.MissingResourcesTruncated)

	got := server.batches()
	require.Len(t, got, 3, "two batches plus the snapshot_complete marker")
	for i, b := range got {
		assert.Equal(t, "cluster-123", b.GetClusterId(), "batch %d", i)
		assert.Equal(t, "team-456", b.GetTeamId(), "batch %d", i)
		assert.Equal(t, "snap-42", b.GetSnapshotId(), "batch %d", i)
		assert.True(t, b.GetIsFullSnapshot(), "batch %d", i)
		assert.True(t, ts.Equal(b.GetTimestamp().AsTime()), "batch %d timestamp", i)
	}
	assert.Equal(t, []string{"d1"}, []string{got[0].Entries[0].GetUid()})
	assert.False(t, got[0].GetTypeComplete())
	assert.True(t, got[1].GetTypeComplete())
	assert.False(t, got[1].GetSnapshotComplete())
	assert.True(t, got[2].GetSnapshotComplete())
}

func TestSnapshotBatchStream_UnimplementedBackendYieldsSentinel(t *testing.T) {
	client := newBatchTestClient(t, genconnect.UnimplementedMetricsCollectorServiceHandler{})

	stream, err := client.OpenClusterSnapshotBatchStream(snapshotCtx(), "snap-42", time.Now(), true)
	require.NoError(t, err, "opening is lazy; unsupported surfaces on send/finish")

	err = stream.SendBatch(gen.ResourceType_RESOURCE_TYPE_DEPLOYMENT,
		[]*gen.SnapshotEntry{{Uid: "d1"}}, true)
	if err == nil {
		_, err = stream.Finish()
	}
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSnapshotBatchedUnsupported)
}

func TestSnapshotBatchStream_AbortDoesNotSendSnapshotComplete(t *testing.T) {
	server := &batchRecordingServer{}
	client := newBatchTestClient(t, server)

	stream, err := client.OpenClusterSnapshotBatchStream(snapshotCtx(), "snap-42", time.Now(), true)
	require.NoError(t, err)
	require.NoError(t, stream.SendBatch(gen.ResourceType_RESOURCE_TYPE_DEPLOYMENT,
		[]*gen.SnapshotEntry{{Uid: "d1"}}, false))

	stream.Abort()
	stream.Abort() // idempotent

	for _, b := range server.batches() {
		assert.False(t, b.GetSnapshotComplete(), "abort must not emit the snapshot_complete marker")
	}
}
