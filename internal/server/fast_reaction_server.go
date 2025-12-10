package server

import (
	"fmt"
	"io"
	"sync"
	"time"

	"net"
	"google.golang.org/grpc"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	"github.com/go-logr/logr"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// FastReactionServer implements the gRPC service for fast-reaction streaming
type FastReactionServer struct {
	gen.UnimplementedFastReactionServiceServer
	logger              logr.Logger
	subscriptionManager *SubscriptionManager
	grpcServer          *grpc.Server
}

// NewFastReactionServer creates a new FastReactionServer
func NewFastReactionServer(logger logr.Logger) *FastReactionServer {
	return &FastReactionServer{
		logger:              logger.WithName("fast-reaction-server"),
		subscriptionManager: NewSubscriptionManager(logger),
	}
}

// Start starts the gRPC server on the given port
func (s *FastReactionServer) Start(port int) error {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("failed to listen on port %d: %w", port, err)
	}

	opts := []grpc.ServerOption{}
	s.grpcServer = grpc.NewServer(opts...)
	gen.RegisterFastReactionServiceServer(s.grpcServer, s)

	s.logger.Info("Starting Fast Reaction gRPC server", "port", port)
	go func() {
		if err := s.grpcServer.Serve(lis); err != nil {
			s.logger.Error(err, "Failed to serve gRPC")
		}
	}()
	return nil
}

// Stop gracefully stops the gRPC server
func (s *FastReactionServer) Stop() {
	if s.grpcServer != nil {
		s.logger.Info("Stopping Fast Reaction gRPC server")
		s.grpcServer.GracefulStop()
	}
}

func (s *FastReactionServer) StreamWorkloadMetrics(stream gen.FastReactionService_StreamWorkloadMetricsServer) error {
	// Create a channel for this client's metric updates
	updates := make(chan *gen.ContainerMetricsBatch, 100)
	clientID := s.subscriptionManager.Register(updates)
	defer s.subscriptionManager.Unregister(clientID)

	s.logger.Info("Client connected to metric stream", "clientID", clientID)

	// Start a goroutine to send metrics to the client
	go func() {
		for batch := range updates {
			if err := stream.Send(batch); err != nil {
				s.logger.Error(err, "Failed to send metrics to client", "clientID", clientID)
				return
			}
		}
	}()

	// Read subscriptions from the client
	for {
		sub, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			s.logger.Error(err, "Stream receive error", "clientID", clientID)
			return err
		}

		// Update subscription for this client
		s.subscriptionManager.UpdateSubscription(clientID, sub)
		s.logger.Info("Updated subscription", "clientID", clientID, "workloads_count", len(sub.Workloads))
	}
}

// PublishMetrics is called by the collector to broadcast new metrics
func (s *FastReactionServer) PublishMetrics(metrics map[string]interface{}, timestamp time.Time) {
	s.subscriptionManager.Broadcast(metrics, timestamp)
}

// SubscriptionManager manages active streams and their interests
type SubscriptionManager struct {
	mu      sync.RWMutex
	clients map[string]*ClientSubscription
	logger  logr.Logger
}

type ClientSubscription struct {
	ID        string
	Channel   chan *gen.ContainerMetricsBatch
	Interests []WorkloadInterest
}

type WorkloadInterest struct {
	Namespace string
	Name      string
	Kind      string
	Selector  map[string]string
}

func NewSubscriptionManager(logger logr.Logger) *SubscriptionManager {
	return &SubscriptionManager{
		clients: make(map[string]*ClientSubscription),
		logger:  logger.WithName("sub-manager"),
	}
}

func (sm *SubscriptionManager) Register(ch chan *gen.ContainerMetricsBatch) string {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	id := fmt.Sprintf("%d", time.Now().UnixNano())
	sm.clients[id] = &ClientSubscription{
		ID:        id,
		Channel:   ch,
		Interests: []WorkloadInterest{},
	}
	return id
}

func (sm *SubscriptionManager) Unregister(id string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if client, ok := sm.clients[id]; ok {
		close(client.Channel)
		delete(sm.clients, id)
	}
}

func (sm *SubscriptionManager) UpdateSubscription(clientID string, sub *gen.WorkloadSubscription) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	client, ok := sm.clients[clientID]
	if !ok {
		return
	}

	// Replace current interests
	newInterests := make([]WorkloadInterest, 0, len(sub.Workloads))
	for _, w := range sub.Workloads {
		newInterests = append(newInterests, WorkloadInterest{
			Namespace: w.Namespace,
			Name:      w.Name,
			Kind:      w.Kind,
			Selector:  w.MatchLabels,
		})
	}
	client.Interests = newInterests
}

// Broadcast filters and dispatches metrics to interested clients
func (sm *SubscriptionManager) Broadcast(data map[string]interface{}, timestamp time.Time) {
	// Parse metric data
	ns, _ := data["namespace"].(string)
	pod, _ := data["podName"].(string)
	container, _ := data["containerName"].(string)
	podLabels, _ := data["podLabels"].(map[string]string)

	cpuMillis, _ := getInt64(data["cpuUsageMillis"])
	memBytes, _ := getInt64(data["memoryUsageBytes"])
	oomCount, _ := getInt64(data["oomKillCount"])
	restartCount, _ := getInt64(data["restartCount"])
	lastReason, _ := data["lastTerminationReason"].(string)

	sm.mu.RLock()
	defer sm.mu.RUnlock()

	for _, client := range sm.clients {
		matched := false
		var matchedWorkload WorkloadInterest

		for _, interest := range client.Interests {
			if interest.Namespace != ns {
				continue
			}

			// Match by Labels (Preferred for Deployment -> Pod)
			if len(interest.Selector) > 0 {
				if matchLabels(podLabels, interest.Selector) {
					matched = true
					matchedWorkload = interest
					break
				}
			} else {
				// Fallback: Exact name match (only works if pod name contains workload name safely, which is heuristics)
				// Or if we had workload info in data.
				// For now, assume if no selector, we can't match safely for Deployment.
				// But maybe interest is for a specific Pod?
				if interest.Kind == "Pod" && interest.Name == pod {
					matched = true
					matchedWorkload = interest
					break
				}
			}
		}

		if matched {
			item := &gen.ContainerMetricItem{
				Workload: &gen.FastReactionWorkloadIdentifier{
					Namespace:   matchedWorkload.Namespace,
					Name:        matchedWorkload.Name,
					Kind:        matchedWorkload.Kind,
					MatchLabels: matchedWorkload.Selector,
				},
				PodName:               pod,
				ContainerName:         container,
				Timestamp:             timestamppb.New(timestamp),
				CpuUsageMillis:        cpuMillis,
				MemoryUsageBytes:      memBytes,
				OomKillCount:          oomCount,
				RestartCount:          int32(restartCount),
				LastTerminationReason: lastReason,
			}

			batch := &gen.ContainerMetricsBatch{
				Items: []*gen.ContainerMetricItem{item},
			}

			select {
			case client.Channel <- batch:
			default:
				// Buffer full, drop metric
			}
		}
	}
}

func matchLabels(podLabels, selector map[string]string) bool {
	if podLabels == nil {
		return false
	}
	for k, v := range selector {
		if podLabels[k] != v {
			return false
		}
	}
	return true
}

func getInt64(v interface{}) (int64, bool) {
	switch val := v.(type) {
	case int64:
		return val, true
	case int:
		return int64(val), true
	case float64:
		return int64(val), true
	case float32:
		return int64(val), true
	}
	return 0, false
}
