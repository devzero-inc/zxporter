package transport

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
)

func TestDirectDakrSender_OpenClusterSnapshotBatchStream_InjectsIdentity(t *testing.T) {
	server := &batchRecordingServer{}
	client := newBatchTestClient(t, server)

	sender := &DirectDakrSender{
		dakrClient: client,
		logger:     logr.Discard(),
		clusterID:  "cluster-from-sender",
		teamID:     "team-from-sender",
	}

	stream, err := sender.OpenClusterSnapshotBatchStream(context.Background(), "snap-7", time.Now(), true)
	require.NoError(t, err)
	require.NoError(t, stream.SendBatch(gen.ResourceType_RESOURCE_TYPE_DEPLOYMENT, nil, true))
	_, err = stream.Finish()
	require.NoError(t, err)

	got := server.batches()
	require.NotEmpty(t, got)
	for _, b := range got {
		assert.Equal(t, "cluster-from-sender", b.GetClusterId())
		assert.Equal(t, "team-from-sender", b.GetTeamId())
	}
}

func TestSimpleDakrClient_OpenClusterSnapshotBatchStream_Unsupported(t *testing.T) {
	client := NewSimpleDakrClient(logr.Discard())
	_, err := client.OpenClusterSnapshotBatchStream(context.Background(), "snap-7", time.Now(), true)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSnapshotBatchedUnsupported)
}

func TestDirectSenderImpl_ForwardsOpenClusterSnapshotBatchStream(t *testing.T) {
	server := &batchRecordingServer{}
	client := newBatchTestClient(t, server)
	sender := NewDirectSender(client, logr.Discard())

	streamer, ok := sender.(interface {
		OpenClusterSnapshotBatchStream(ctx context.Context, snapshotID string, timestamp time.Time, isFullSnapshot bool) (SnapshotBatchStream, error)
	})
	require.True(t, ok, "directSenderImpl must expose the batched snapshot capability")

	stream, err := streamer.OpenClusterSnapshotBatchStream(context.Background(), "snap-8", time.Now(), true)
	require.NoError(t, err)
	_, err = stream.Finish()
	require.NoError(t, err)
	require.Len(t, server.batches(), 1)
}
