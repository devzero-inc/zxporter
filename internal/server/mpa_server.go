package server

import (
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	"github.com/devzero-inc/zxporter/internal/collector"
	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// MpaServer implements the gRPC service for in-cluster MPA metric streaming
type MpaServer struct {
	gen.UnimplementedMpaServiceServer
	logger              logr.Logger
	subscriptionManager *SubscriptionManager
	grpcServer          *grpc.Server
}

// NewMpaServer creates a new MpaServer
func NewMpaServer(logger logr.Logger) *MpaServer {
	return &MpaServer{
		logger:              logger.WithName("mpa-server"),
		subscriptionManager: NewSubscriptionManager(logger),
	}
}

// Start starts the gRPC server on the given port
func (s *MpaServer) Start(port int) error {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("failed to listen on port %d: %w", port, err)
	}

	opts := []grpc.ServerOption{}
	s.grpcServer = grpc.NewServer(opts...)
	gen.RegisterMpaServiceServer(s.grpcServer, s)

	s.logger.Info("Starting MPA gRPC server", "port", port)
	go func() {
		if err := s.grpcServer.Serve(lis); err != nil {
			s.logger.Error(err, "Failed to serve gRPC")
		}
	}()
	return nil
}

// Stop gracefully stops the gRPC server
func (s *MpaServer) Stop() {
	if s.grpcServer != nil {
		s.logger.Info("Stopping MPA gRPC server")
		s.grpcServer.GracefulStop()
	}
}

func (s *MpaServer) StreamWorkloadMetrics(stream gen.MpaService_StreamWorkloadMetricsServer) error {
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
func (s *MpaServer) PublishMetrics(metrics *collector.ContainerMetricsSnapshot, timestamp time.Time) {
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

func (sm *SubscriptionManager) UpdateSubscription(clientID string, sub *gen.MpaWorkloadSubscription) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	client, ok := sm.clients[clientID]
	if !ok {
		return
	}

	// Replace current interests
	newInterests := make([]WorkloadInterest, 0, len(sub.Workloads))
	for _, w := range sub.Workloads {
		sm.logger.Info("Registering workload interest", "clientID", clientID, "namespace", w.Namespace, "name", w.Name, "kind", w.Kind)
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
func (sm *SubscriptionManager) Broadcast(data *collector.ContainerMetricsSnapshot, timestamp time.Time) {
	// Parse metric data
	ns := data.Namespace
	pod := data.PodName
	container := data.ContainerName

	cpuMillis := data.CpuUsageMillis
	memBytes := data.MemoryUsageBytes

	restartCount := data.RestartCount
	lastReason := data.LastTerminationReason

	wKind := data.WorkloadKind
	wName := data.WorkloadName

	sm.mu.RLock()
	defer sm.mu.RUnlock()

	for _, client := range sm.clients {
		matched := false
		var matchedWorkload WorkloadInterest

		for _, interest := range client.Interests {
			if interest.Namespace != ns {
				continue
			}

			// Match by Workload Reference (Preferred)
			if wKind != "" && wName != "" {
				if interest.Kind == wKind && interest.Name == wName {
					matched = true
					matchedWorkload = interest
					break
				}
			}

			// Fallback: Exact name match (Standalone Pods)
			if !matched && interest.Kind == "Pod" && interest.Name == pod {
				matched = true
				matchedWorkload = interest
				break
			}
		}

		if matched {
			item := &gen.ContainerMetricItem{
				Workload: &gen.MpaWorkloadIdentifier{
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
				OomKillCount:          0, // Not explicitly tracked in snapshot yet
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
