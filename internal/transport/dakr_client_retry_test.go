package transport

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/go-logr/logr"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	genconnect "github.com/devzero-inc/zxporter/gen/api/v1/apiv1connect"
	"github.com/devzero-inc/zxporter/internal/collector"
)

// alwaysUnavailableServer fails every call it overrides with a retryable code and
// records how many times each procedure was actually delivered. Everything it does
// not override is inherited from MockMetricsServer.
type alwaysUnavailableServer struct {
	*MockMetricsServer

	mu     sync.Mutex
	counts map[string]int
}

func newAlwaysUnavailableServer() *alwaysUnavailableServer {
	return &alwaysUnavailableServer{
		MockMetricsServer: &MockMetricsServer{},
		counts:            map[string]int{},
	}
}

func (s *alwaysUnavailableServer) bump(procedure string) error {
	s.mu.Lock()
	s.counts[procedure]++
	s.mu.Unlock()
	return connect.NewError(connect.CodeUnavailable, errors.New("dakr is down"))
}

func (s *alwaysUnavailableServer) count(procedure string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[procedure]
}

func (s *alwaysUnavailableServer) SendContainerRedMetrics(
	_ context.Context,
	_ *connect.Request[gen.SendContainerRedMetricsRequest],
) (*connect.Response[gen.SendContainerRedMetricsResponse], error) {
	return nil, s.bump("SendContainerRedMetrics")
}

func (s *alwaysUnavailableServer) SendResourceBatch(
	_ context.Context,
	_ *connect.Request[gen.SendResourceBatchRequest],
) (*connect.Response[gen.SendResourceBatchResponse], error) {
	return nil, s.bump("SendResourceBatch")
}

// TestSendContainerRedMetricsIsNeverRetried pins the invariant that makes the RED
// metrics table trustworthy. The items are per-minute counter deltas written into a
// SummingMergeTree, so a batch delivered twice is added twice and the minute reads as
// busier than it really was. A lost response is indistinguishable from a lost request,
// which is exactly why the client must not guess and send it again.
//
// The contrast case matters as much as the exemption: it proves the client still
// retries everything else, so passing here cannot mean the interceptor was disabled
// wholesale.
func TestSendContainerRedMetricsIsNeverRetried(t *testing.T) {
	server := newAlwaysUnavailableServer()

	mux := http.NewServeMux()
	path, handler := genconnect.NewMetricsCollectorServiceHandler(server)
	mux.Handle(path, handler)

	httpServer := httptest.NewServer(mux)
	defer httpServer.Close()

	client := NewDakrClient(httpServer.URL, "test-token", logr.Discard())
	ctx := context.Background()

	_, err := client.SendContainerRedMetrics(ctx, &gen.SendContainerRedMetricsRequest{
		ClusterId: "test-cluster",
		NodeName:  "node-a",
		Items: []*gen.ContainerRedMetricsItem{{
			PodName:       "pod-a",
			PodNamespace:  "default",
			PodUid:        "uid-a",
			ContainerName: "app",
			RequestCount:  1,
		}},
	})
	if err == nil {
		t.Fatal("expected SendContainerRedMetrics to fail against an unavailable server")
	}

	if got := server.count("SendContainerRedMetrics"); got != 1 {
		t.Errorf(
			"SendContainerRedMetrics was delivered %d times, want exactly 1: a retried flush double counts a minute",
			got,
		)
	}

	// A retryable procedure on the same client must still be retried, otherwise this
	// test would pass just as happily with the retry interceptor removed entirely.
	_, err = client.SendResourceBatch(ctx, []collector.CollectedResource{{
		ResourceType: collector.Pod,
		Object:       createLargeKubernetesResource(0),
		Timestamp:    time.Now(),
		EventType:    collector.EventTypeAdd,
		Key:          "default/pod-a",
	}}, collector.Pod)
	if err == nil {
		t.Fatal("expected SendResourceBatch to fail against an unavailable server")
	}

	if got := server.count("SendResourceBatch"); got != 3 {
		t.Errorf("SendResourceBatch was delivered %d times, want 3 (the retry policy's MaxAttempts)", got)
	}
}
