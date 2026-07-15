// internal/transport/snapshot_batched.go
package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
)

// ErrSnapshotBatchedUnsupported signals that the dakr backend does not serve
// SendClusterSnapshotBatched yet; callers fall back to the legacy
// SendClusterSnapshotStream path.
var ErrSnapshotBatchedUnsupported = errors.New("dakr does not support SendClusterSnapshotBatched")

// SnapshotBatchStream sends one snapshot's batches to dakr. It stamps the
// snapshot identity (cluster, team, snapshot ID, timestamp) onto every wire
// message so callers only provide per-batch content.
type SnapshotBatchStream interface {
	// SendBatch sends one batch for a resource type; typeComplete marks the
	// last batch of that type.
	SendBatch(rt gen.ResourceType, entries []*gen.SnapshotEntry, typeComplete bool) error
	// Finish sends the snapshot_complete marker and returns the response.
	Finish() (*gen.SendClusterSnapshotBatchedResponse, error)
	// Abort closes the stream without the snapshot_complete marker, telling
	// the receiver to treat the snapshot as incomplete. Idempotent.
	Abort()
}

type snapshotBatchStream struct {
	stream     *connect.ClientStreamForClient[gen.ClusterSnapshotBatch, gen.SendClusterSnapshotBatchedResponse]
	clusterID  string
	teamID     string
	snapshotID string
	timestamp  *timestamppb.Timestamp
	isFull     bool
	closed     bool
}

// contextString reads a string context value set under either a plain string
// key (used by the snapshotter's context plumbing) or the transport-typed key
// (used by DirectDakrSender). Empty values are fine: the backend derives
// cluster and team from the bearer token when the fields are unset.
func contextString(ctx context.Context, key string) string {
	if v, ok := ctx.Value(key).(string); ok && v != "" {
		return v
	}
	if v, ok := ctx.Value(contextKey(key)).(string); ok {
		return v
	}
	return ""
}

// OpenClusterSnapshotBatchStream opens a batched snapshot stream to dakr.
// Opening is lazy: an unsupported backend surfaces as
// ErrSnapshotBatchedUnsupported from SendBatch/Finish, not from here.
func (c *RealDakrClient) OpenClusterSnapshotBatchStream(
	ctx context.Context,
	snapshotID string,
	timestamp time.Time,
	isFullSnapshot bool,
) (SnapshotBatchStream, error) {
	stream := c.client.SendClusterSnapshotBatched(ctx)
	c.clientHeaders.AttachToRequest(stream.RequestHeader())

	return &snapshotBatchStream{
		stream:     stream,
		clusterID:  contextString(ctx, "cluster_id"),
		teamID:     contextString(ctx, "team_id"),
		snapshotID: snapshotID,
		timestamp:  timestamppb.New(timestamp),
		isFull:     isFullSnapshot,
	}, nil
}

func (s *snapshotBatchStream) newMessage() *gen.ClusterSnapshotBatch {
	return &gen.ClusterSnapshotBatch{
		ClusterId:      s.clusterID,
		TeamId:         s.teamID,
		Timestamp:      s.timestamp,
		SnapshotId:     s.snapshotID,
		IsFullSnapshot: s.isFull,
	}
}

func (s *snapshotBatchStream) SendBatch(
	rt gen.ResourceType,
	entries []*gen.SnapshotEntry,
	typeComplete bool,
) error {
	if s.closed {
		return fmt.Errorf("snapshot batch stream %s already closed", s.snapshotID)
	}

	msg := s.newMessage()
	msg.ResourceType = rt
	msg.Entries = entries
	msg.TypeComplete = typeComplete

	if err := s.stream.Send(msg); err != nil {
		// connect reports the server's actual error only on CloseAndReceive;
		// Send just returns io.EOF once the server has closed the stream.
		return s.surfaceStreamError(err)
	}
	return nil
}

func (s *snapshotBatchStream) Finish() (*gen.SendClusterSnapshotBatchedResponse, error) {
	if s.closed {
		return nil, fmt.Errorf("snapshot batch stream %s already closed", s.snapshotID)
	}

	final := s.newMessage()
	final.SnapshotComplete = true
	if err := s.stream.Send(final); err != nil && !errors.Is(err, io.EOF) {
		s.closed = true
		return nil, err
	}

	s.closed = true
	resp, err := s.stream.CloseAndReceive()
	if err != nil {
		return nil, classifyBatchStreamError(err)
	}
	return resp.Msg, nil
}

func (s *snapshotBatchStream) Abort() {
	if s.closed {
		return
	}
	s.closed = true
	// Close without the snapshot_complete marker; the response (an Aborted
	// error from a receiver that saw an incomplete snapshot) is irrelevant.
	_, _ = s.stream.CloseAndReceive()
}

// surfaceStreamError resolves a Send failure into the server's real error.
func (s *snapshotBatchStream) surfaceStreamError(sendErr error) error {
	if !errors.Is(sendErr, io.EOF) {
		s.closed = true
		return sendErr
	}
	s.closed = true
	if _, err := s.stream.CloseAndReceive(); err != nil {
		return classifyBatchStreamError(err)
	}
	return sendErr
}

func classifyBatchStreamError(err error) error {
	if connect.CodeOf(err) == connect.CodeUnimplemented {
		return fmt.Errorf("%w: %v", ErrSnapshotBatchedUnsupported, err)
	}
	return err
}
