package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// TODO: for generic provider we can process node level data to dakr

// GenericProvider implements a fallback provider for when cloud detection fails
type GenericProvider struct {
	logger    logr.Logger
	k8sClient kubernetes.Interface
}

// NewGenericProvider creates a new generic provider
func NewGenericProvider(logger logr.Logger, k8sClient kubernetes.Interface) *GenericProvider {
	return &GenericProvider{
		logger:    logger.WithName("generic-provider"),
		k8sClient: k8sClient,
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
		"provider":     "generic",
		"cluster_name": clusterName,
	}

	// Try to get some information about the cluster infrastructure
	nodes, err := p.k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err == nil && len(nodes.Items) > 0 {
		// Check provider IDs for hints about the infrastructure
		provider := p.guessProviderFromNodes(nodes.Items)
		if provider != "" {
			metadata["detected_infrastructure"] = provider
		}

		// Extract region/zone hints if available
		regions := make(map[string]bool)
		zones := make(map[string]bool)

		for _, node := range nodes.Items {
			// Check common region/zone labels
			if region, ok := node.Labels["topology.kubernetes.io/region"]; ok {
				regions[region] = true
			} else if region, ok := node.Labels["failure-domain.beta.kubernetes.io/region"]; ok {
				regions[region] = true
			}

			if zone, ok := node.Labels["topology.kubernetes.io/zone"]; ok {
				zones[zone] = true
			} else if zone, ok := node.Labels["failure-domain.beta.kubernetes.io/zone"]; ok {
				zones[zone] = true
			}
		}

		// Add region/zone info if found
		if len(regions) > 0 {
			var regionList []string
			for region := range regions {
				regionList = append(regionList, region)
			}
			metadata["regions"] = regionList
		}

		if len(zones) > 0 {
			var zoneList []string
			for zone := range zones {
				zoneList = append(zoneList, zone)
			}
			metadata["zones"] = zoneList
		}
	}

	p.logger.Info("Collected generic cluster metadata", "cluster", clusterName)

	return metadata, nil
}

// GetNodeGroupForNode attempts to group nodes by common characteristics
func (p *GenericProvider) GetNodeGroupForNode(nodeName string) (map[string]interface{}, error) {
	// Try to get node details from K8s API
	node, err := p.k8sClient.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting node %s: %w", nodeName, err)
	}

	// Try to find a label that indicates a node group
	nodeGroupName := "unknown"

	// Common node group label candidates
	groupLabelCandidates := []string{
		"node-role.kubernetes.io/worker",
		"kubernetes.io/role",
		"node.kubernetes.io/instance-group",
		"beta.kubernetes.io/instance-type",
		"node.kubernetes.io/instance-type",
	}

	for _, labelKey := range groupLabelCandidates {
		if val, ok := node.Labels[labelKey]; ok {
			nodeGroupName = val
			break
		}
	}

	// Create a basic node group
	nodeGroup := map[string]interface{}{
		"name": nodeGroupName,
	}

	// Try to extract instance type
	if instanceType, ok := node.Labels["node.kubernetes.io/instance-type"]; ok {
		nodeGroup["instance_type"] = instanceType
	} else if instanceType, ok := node.Labels["beta.kubernetes.io/instance-type"]; ok {
		nodeGroup["instance_type"] = instanceType
	}

	return nodeGroup, nil
}

// discoverClusterName attempts to determine a cluster name
func (p *GenericProvider) discoverClusterName(ctx context.Context) (string, error) {
	// Try to get from kube-system namespace UID - fairly unique
	ns, err := p.k8sClient.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err == nil {
		return fmt.Sprintf("cluster-%s", string(ns.UID)[:8]), nil
	}

	// Try to get from cluster configuration
	kubeadmCM, err := p.k8sClient.CoreV1().ConfigMaps("kube-system").Get(ctx, "kubeadm-config", metav1.GetOptions{})
	if err == nil {
		if clusterConfig, ok := kubeadmCM.Data["ClusterConfiguration"]; ok {
			if strings.Contains(clusterConfig, "clusterName:") {
				lines := strings.Split(clusterConfig, "\n")
				for _, line := range lines {
					if strings.Contains(line, "clusterName:") {
						parts := strings.SplitN(line, ":", 2)
						if len(parts) > 1 {
							return strings.TrimSpace(parts[1]), nil
						}
					}
				}
			}
		}
	}

	return "", fmt.Errorf("could not determine cluster name")
}

// guessProviderFromNodes tries to identify the infrastructure provider from node metadata
func (p *GenericProvider) guessProviderFromNodes(nodes []corev1.Node) string {
	if len(nodes) == 0 {
		return ""
	}

	// Check provider IDs for common patterns
	for _, node := range nodes {
		providerID := node.Spec.ProviderID

		switch {
		case strings.HasPrefix(providerID, "aws://"):
			return "aws"
		case strings.HasPrefix(providerID, "gce://"):
			return "gcp"
		case strings.HasPrefix(providerID, "azure://"):
			return "azure"
		case strings.HasPrefix(providerID, "openstack://"):
			return "openstack"
		case strings.HasPrefix(providerID, "digitalocean://"):
			return "digitalocean"
		}
	}

	// Check for common provider-specific labels
	for _, node := range nodes {
		// AWS EKS specific labels
		if _, ok := node.Labels["eks.amazonaws.com/nodegroup"]; ok {
			return "aws"
		}

		// GCP GKE specific labels
		if _, ok := node.Labels["cloud.google.com/gke-nodepool"]; ok {
			return "gcp"
		}

		// Azure AKS specific labels
		if _, ok := node.Labels["kubernetes.azure.com/cluster"]; ok {
			return "azure"
		}
	}

	return ""
}

func (p *GenericProvider) GetNodeGroupMetadata(context.Context) map[string]map[string][]string {
	return nil
}
