package provider

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/go-logr/logr"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	providerNameUnknown      = "unknown"
	providerNameAws          = "aws"
	providerNameGcp          = "gcp"
	providerNameAzure        = "azure"
	providerNameOpenstack    = "openstack"
	providerNameDigitalocean = "digitalocean"

	nodeProviderPrefixAws          = "aws://"
	nodeProviderPrefixGcp          = "gce://"
	nodeProviderPrefixAzure        = "azure://"
	nodeProviderPrefixOpenstack    = "openstack://"
	nodeProviderPrefixDigitalocean = "digitalocean://"

	awsKubeConfigClusterContextPrefix    = "arn:"
	awsKubeConfigClusterContextDelimiter = "/"
	gcpKubeConfigClusterContextPrefix    = "gke_"
	gcpKubeConfigClusterContextDelimiter = "_"

	// Region labels
	topologyRegionLabel = "topology.kubernetes.io/region"
)

// GenericProvider implements basic cloud provider, cluster name, cluster version and cluster region detection
type GenericProvider struct {
	sync.Mutex

	logger          logr.Logger
	k8sClient       kubernetes.Interface
	kubeContextName string
}

// NewGenericProvider creates a new generic provider
func NewGenericProvider(logger logr.Logger, k8sClient kubernetes.Interface, kubeContextName string) *GenericProvider {
	return &GenericProvider{
		logger:          logger.WithName("generic-provider"),
		k8sClient:       k8sClient,
		kubeContextName: kubeContextName,
	}
}

// Name returns the provider name
func (p *GenericProvider) Name() string {
	return "generic"
}

// GetClusterMetadata gathers basic metadata without cloud-specific information
func (p *GenericProvider) GetClusterMetadata(ctx context.Context) (map[string]interface{}, error) {
	p.logger.Info("Collecting generic cluster metadata")

	// Try to determine a cluster name
	clusterName, err := p.discoverClusterName(ctx)
	if err != nil {
		p.logger.Info("Could not determine cluster name", "error", err)
		clusterName = "unknown-cluster"
	}

	// Create basic metadata
	metadata := map[string]interface{}{
		"cluster_name": clusterName,
	}

	infraRegion := ""
	infraProvider := ""

	// Get nodes to determine various cloud metadata
	nodes, err := p.k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("Failed to get nodes to infer cluster metadata %w", err)
	}
	if len(nodes.Items) > 0 {
		// Detect infra provider from node provider IDs
		detectedProvider := p.getProviderFromNodes(nodes.Items)
		if detectedProvider != "" {
			infraProvider = detectedProvider
		}

		for _, node := range nodes.Items {
			// Get region from region labels
			if region, ok := node.Labels[topologyRegionLabel]; ok {
				infraRegion = region
				break
			}
		}
	}

	p.logger.Info("Collected generic cluster metadata",
		zap.String("cluster_name", clusterName),
		zap.String("provider", infraProvider),
		zap.String("region", infraRegion),
	)

	metadata["provider"] = infraProvider
	metadata["region"] = infraRegion

	return metadata, nil
}

// discoverClusterName attempts to determine a cluster name
func (p *GenericProvider) discoverClusterName(ctx context.Context) (string, error) {
	if p.kubeContextName != "" {
		switch {
		case strings.HasPrefix(p.kubeContextName, awsKubeConfigClusterContextPrefix) && len(strings.Split(p.kubeContextName, awsKubeConfigClusterContextDelimiter)) >= 2:
			splits := strings.Split(p.kubeContextName, awsKubeConfigClusterContextDelimiter)
			return splits[len(splits)-1], nil
		case strings.HasPrefix(p.kubeContextName, gcpKubeConfigClusterContextPrefix) && len(strings.Split(p.kubeContextName, gcpKubeConfigClusterContextDelimiter)) >= 2:
			splits := strings.Split(p.kubeContextName, gcpKubeConfigClusterContextDelimiter)
			return splits[len(splits)-1], nil
		default:
			return p.kubeContextName, nil
		}
	}

	// Try to get from kube-system namespace UID
	ns, err := p.k8sClient.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err == nil {
		return fmt.Sprintf("cluster-%s", string(ns.UID)[:8]), nil
	}

	return "", fmt.Errorf("could not determine cluster name")
}

// getProviderFromNodes identifies infrastructure provider from node metadata
func (p *GenericProvider) getProviderFromNodes(nodes []corev1.Node) string {
	if len(nodes) == 0 {
		return providerNameUnknown
	}

	// Check provider IDs for common patterns
	for _, node := range nodes {
		providerID := node.Spec.ProviderID

		switch {
		case strings.HasPrefix(providerID, nodeProviderPrefixAws):
			return providerNameAws
		case strings.HasPrefix(providerID, nodeProviderPrefixGcp):
			return providerNameGcp
		case strings.HasPrefix(providerID, nodeProviderPrefixAzure):
			return providerNameAzure
		case strings.HasPrefix(providerID, nodeProviderPrefixOpenstack):
			return providerNameOpenstack
		case strings.HasPrefix(providerID, nodeProviderPrefixDigitalocean):
			return providerNameDigitalocean
		}
	}

	return ""
}
