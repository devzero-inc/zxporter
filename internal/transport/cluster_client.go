package transport

import (
	"context"
	"crypto/tls"
	"fmt"
	"os"
	"time"

	apiv1 "github.com/devzero-inc/zxporter/gen/api/v1"
	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

// ClusterClient is a client for interacting with the DevZero Cluster service
type ClusterClient struct {
	url    string
	log    logr.Logger
	conn   *grpc.ClientConn
	client apiv1.ClusterServiceClient
}

// NewClusterClient creates a new cluster client
func NewClusterClient(url string, log logr.Logger) (*ClusterClient, error) {
	// Create TLS credentials
	config := &tls.Config{
		InsecureSkipVerify: false,
	}
	creds := credentials.NewTLS(config)

	// Create gRPC connection with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctx, url,
		grpc.WithTransportCredentials(creds),
		grpc.WithBlock(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to cluster service: %w", err)
	}

	return &ClusterClient{
		url:    url,
		log:    log,
		conn:   conn,
		client: apiv1.NewClusterServiceClient(conn),
	}, nil
}

// ExchangePATForClusterToken exchanges a PAT token for a cluster token
func (c *ClusterClient) ExchangePATForClusterToken(ctx context.Context, patToken, clusterName, k8sProvider string) (string, string, error) {
	// Add PAT token to the request context as authorization header
	md := metadata.Pairs("authorization", fmt.Sprintf("Bearer %s", patToken))
	ctx = metadata.NewOutgoingContext(ctx, md)

	// Prepare the request
	req := &apiv1.CreateClusterTokenRequest{
		ClusterName: clusterName,
		K8SProvider: k8sProvider,
	}

	// Make the request with retry logic
	var resp *apiv1.CreateClusterTokenResponse
	var err error

	for i := 0; i < 3; i++ {
		resp, err = c.client.CreateClusterToken(ctx, req)
		if err == nil {
			break
		}

		c.log.Error(err, "Failed to create cluster token, retrying", "attempt", i+1)
		if i < 2 {
			// Exponential backoff: 1s, 2s, 4s
			time.Sleep(time.Duration(1<<uint(i)) * time.Second)
		}
	}

	if err != nil {
		return "", "", fmt.Errorf("failed to exchange PAT for cluster token after 3 attempts: %w", err)
	}

	c.log.Info("Successfully exchanged PAT for cluster token", "clusterId", resp.ClusterId)
	return resp.Token, resp.ClusterId, nil
}

// Close closes the gRPC connection
func (c *ClusterClient) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// GetClusterNameAndProvider attempts to detect the cluster name and K8s provider
// This is a helper function that can be expanded with more detection logic
func GetClusterNameAndProvider() (string, string) {
	// Default values
	clusterName := "zxporter-cluster"
	k8sProvider := "other"

	// Check environment variables first
	if provider := os.Getenv("K8S_PROVIDER"); provider != "" {
		k8sProvider = provider
	}

	if name := os.Getenv("KUBE_CONTEXT_NAME"); name != "" {
		clusterName = name
	}

	// TODO: Add auto-detection logic for:
	// - AWS EKS: Check for EC2 metadata service, EKS-specific annotations
	// - Azure AKS: Check for Azure metadata service, AKS-specific labels
	// - GCP GKE: Check for GCE metadata service, GKE-specific annotations
	// - OCI OKE: Check for OCI metadata service, OKE-specific labels

	// For now, return the values (from env vars or defaults)
	return clusterName, k8sProvider
}
