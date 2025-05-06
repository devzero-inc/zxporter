package provider

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// Common role labels
	nodeRoleWorkerLabel    = "node-role.kubernetes.io/worker"
	nodeRoleMasterLabel    = "node-role.kubernetes.io/master"
	kubernetesRoleLabel    = "kubernetes.io/role"
	nodeInstanceGroupLabel = "node.kubernetes.io/instance-group"
	nodeGroupLabel         = "node-group"

	// Instance type labels
	nodeInstanceTypeLabel     = "node.kubernetes.io/instance-type"
	nodeBetaInstanceTypeLabel = "beta.kubernetes.io/instance-type"

	// Zone labels
	topologyZoneLabel      = "topology.kubernetes.io/zone"
	failureDomainZoneLabel = "failure-domain.beta.kubernetes.io/zone"

	// Region labels
	topologyRegionLabel      = "topology.kubernetes.io/region"
	failureDomainRegionLabel = "failure-domain.beta.kubernetes.io/region"

	// Spot/preemptible instance labels
	nodeInstanceLifecycleLabel = "node.kubernetes.io/instance-lifecycle"
)

// Node type constants
const (
	NodeTypeRegular = "Regular"
	NodeTypeSpot    = "Spot"
)

// TODO: for generic provider we can process node level data to dakr

// GenericProvider implements a fallback provider for when cloud detection fails
type GenericProvider struct {
	sync.Mutex

	logger     logr.Logger
	k8sClient  kubernetes.Interface
	nodeGroups map[NodePoolName]map[string][]string
}

// NewGenericProvider creates a new generic provider
func NewGenericProvider(logger logr.Logger, k8sClient kubernetes.Interface) *GenericProvider {
	return &GenericProvider{
		logger:     logger.WithName("generic-provider"),
		k8sClient:  k8sClient,
		nodeGroups: make(map[NodePoolName]map[string][]string),
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
			if region, ok := node.Labels[topologyRegionLabel]; ok {
				regions[region] = true
			} else if region, ok := node.Labels[failureDomainRegionLabel]; ok {
				regions[region] = true
			}

			if zone, ok := node.Labels[topologyZoneLabel]; ok {
				zones[zone] = true
			} else if zone, ok := node.Labels[failureDomainZoneLabel]; ok {
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

func (p *GenericProvider) GetNodeGroupMetadata(ctx context.Context) map[string]map[string][]string {
	nodeList, err := p.k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil { // failed to call K8s API
		// return cached info, if available
		return genericTypesToGeneric(p.nodeGroups)
	}

	refreshedNodeGroupMetadata := getGenericNodeGroupMetadata(nodeList)
	p.Lock()
	p.nodeGroups = refreshedNodeGroupMetadata
	p.Unlock()

	return genericTypesToGeneric(p.nodeGroups)
}

func getGenericNodeGroupMetadata(nodeList *corev1.NodeList) map[NodePoolName]map[string][]string {
	nodeGroupInfo := make(map[NodePoolName]map[string][]string)
	if nodeList == nil {
		return nodeGroupInfo
	}

	for _, node := range nodeList.Items {
		nodePoolLabel := getGenericNodePoolLabel(&node)
		nodeType := getGenericNodeType(node)

		if _, ok := nodeGroupInfo[nodePoolLabel]; !ok {
			nodeGroupInfo[nodePoolLabel] = make(map[string][]string)
		}

		if _, ok := nodeGroupInfo[nodePoolLabel][nodeType]; !ok {
			nodeGroupInfo[nodePoolLabel][nodeType] = []string{}
		}
		nodeGroupInfo[nodePoolLabel][nodeType] = append(nodeGroupInfo[nodePoolLabel][nodeType], node.Name)
	}
	return nodeGroupInfo
}

func getGenericNodePoolLabel(node *corev1.Node) NodePoolName {
	// In generic there is not consistent node group label unlike providers
	// Try to determine a meaningful node group name from various common labels
	// Order by preference: explicit role labels, instance type, zone

	// Check for common role labels
	roleLabelCandidates := []string{
		nodeRoleWorkerLabel,
		nodeRoleMasterLabel,
		kubernetesRoleLabel,
		nodeInstanceGroupLabel,
		nodeGroupLabel,
	}

	for _, labelKey := range roleLabelCandidates {
		if val, ok := node.Labels[labelKey]; ok && val != "" {
			return NodePoolName(val)
		}
	}

	if instanceType, ok := node.Labels[nodeInstanceTypeLabel]; ok {
		return NodePoolName(instanceType)
	} else if instanceType, ok := node.Labels[nodeBetaInstanceTypeLabel]; ok {
		return NodePoolName(instanceType)
	}

	if zone, ok := node.Labels[topologyZoneLabel]; ok {
		return NodePoolName("zone-" + zone)
	} else if zone, ok := node.Labels[failureDomainZoneLabel]; ok {
		return NodePoolName("zone-" + zone)
	}

	return NodePoolName("default-pool")
}

func getGenericNodeType(node corev1.Node) string {
	// Check for common spot/preemptible labels across different providers
	spotLabelCandidates := []string{
		nodeInstanceLifecycleLabel,
		azureScalePriorityLabel,
		awsCapacityTypeLabel,
		gkeSpotLabel,
	}

	for _, labelKey := range spotLabelCandidates {
		if val, ok := node.Labels[labelKey]; ok {
			if val == "spot" || val == "Spot" || val == "SPOT" || val == "true" {
				return NodeTypeSpot
			}
		}
	}

	return NodeTypeRegular
}

func genericTypesToGeneric(metadata map[NodePoolName]map[string][]string) map[string]map[string][]string {
	result := make(map[string]map[string][]string)
	if metadata == nil {
		return result
	}

	for nodePool, nodePoolTypeToNodeList := range metadata {
		result[string(nodePool)] = make(map[string][]string)
		for nodePoolType, nodeList := range nodePoolTypeToNodeList {
			result[string(nodePool)][nodePoolType] = nodeList
		}
	}
	return result
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
		if _, ok := node.Labels[awsNodeGroupLabel]; ok {
			return "aws"
		}

		// GCP GKE specific labels
		if _, ok := node.Labels[gkeNodePoolLabel]; ok {
			return "gcp"
		}

		// Azure AKS specific labels
		if _, ok := node.Labels[azureClusterLbel]; ok {
			return "azure"
		}
	}

	return ""
}
