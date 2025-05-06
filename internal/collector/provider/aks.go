package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// Azure metadata server endpoint
	azureMetadataEndpoint = "http://169.254.169.254/metadata/instance"

	// Azure agent pool label
	azureAgentPoolLabel = "kubernetes.azure.com/agentpool"

	// Azure scale priority label
	azureScalePriorityLabel = "kubernetes.azure.com/scalesetpriority"

	// Azure cluster label
	azureClusterLbel = "kubernetes.azure.com/cluster"

	// todo move some of the strings into consts
)

// AzureProvider implements provider interface for Azure AKS
type AzureProvider struct {
	sync.Mutex

	logger            logr.Logger
	k8sClient         kubernetes.Interface
	httpClient        *http.Client
	resourceGroupName string
	subscriptionID    string
	location          string
	clusterName       string
	resourceGroup     string
	nodePools         map[NodePoolName]map[armcontainerservice.ScaleSetPriority][]string
}

// AzureMetadata holds information returned from the Azure metadata service
type AzureMetadata struct {
	Compute struct {
		Location          string `json:"location"`
		ResourceGroupName string `json:"resourceGroupName"`
		SubscriptionID    string `json:"subscriptionId"`
		Name              string `json:"name"`
		VMSize            string `json:"vmSize"`
		Zone              string `json:"zone"`
	} `json:"compute"`
}

type AKSMetadata struct {
	ResourceGroup string
	ClusterName   string
	Region        string // also available in ".compute.location"
}

// NewAzureProvider creates a new Azure provider
func NewAzureProvider(logger logr.Logger, k8sClient kubernetes.Interface) (*AzureProvider, error) {
	// Create HTTP client for metadata server
	httpClient := &http.Client{Timeout: 5 * time.Second}

	// Get Azure instance metadata (test to see if azure or not)
	metadata, err := getAzureMetadata(httpClient)
	if err != nil || metadata == nil {
		return nil, fmt.Errorf("[NewAzureProvider] failed while getting Azure metadata: %w", err)
	}

	aksMetadata, err := parseAKSResourceGroupName(metadata.Compute.ResourceGroupName)
	if err != nil {
		logger.Error(err, "[NewAzureProvider] could not parse resource groupname",
			"resourceGroupName", metadata.Compute.ResourceGroupName)
		return nil, fmt.Errorf("[NewAzureProvider] resource grouped name could not be parsed: %w", err)
	}

	// Initialize provider
	provider := &AzureProvider{
		logger:            logger,
		k8sClient:         k8sClient,
		httpClient:        httpClient,
		resourceGroupName: metadata.Compute.ResourceGroupName,
		subscriptionID:    metadata.Compute.SubscriptionID,
		location:          metadata.Compute.Location, // can also be read from aksMetadata.Region
		nodePools:         make(map[NodePoolName]map[armcontainerservice.ScaleSetPriority][]string),
		clusterName:       aksMetadata.ClusterName,
		resourceGroup:     aksMetadata.ResourceGroup,
	}

	// Try to discover cluster name (this is challenging in Azure)
	clusterName, err := provider.discoverClusterName(context.Background())
	if err != nil {
		logger.Info("Couldn't automatically discover AKS cluster name", "error", err)
		// We continue anyway, as we might be able to provide some useful info
	} else {
		provider.clusterName = clusterName
	}

	return provider, nil
}

// Name returns the provider name
func (p *AzureProvider) Name() string {
	return "aks"
}

// GetClusterMetadata retrieves AKS cluster metadata
func (p *AzureProvider) GetClusterMetadata(ctx context.Context) (map[string]interface{}, error) {
	p.logger.Info("Collecting Azure AKS cluster metadata")

	// Build base metadata even if we couldn't find the cluster
	metadata := map[string]interface{}{
		"provider":        "aks",
		"subscription_id": p.subscriptionID,
		"resource_group":  p.resourceGroup,
		"location":        p.location,
		"cluster_name":    p.clusterName,
	}

	// kubernetes version
	version, err := p.k8sClient.Discovery().ServerVersion()
	if err != nil {
		p.logger.Error(err, "failed to get kubernetes version")
	} else {
		metadata["kubernetes_version"] = version
	}

	nodeList, err := p.k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		p.logger.Error(err, "failed to get node list")
	}

	refreshedNodPoolMetadata := getAKSNodePoolMetadata(nodeList)
	metadata["node_pools"] = refreshedNodPoolMetadata
	p.Lock()
	p.nodePools = refreshedNodPoolMetadata
	p.Unlock()

	p.logger.Info("Collected AKS cluster metadata",
		"subscription", p.subscriptionID,
		"resourceGroup", p.resourceGroup,
		"location", p.location,
		"cluster", p.clusterName,
		"metadata", metadata,
	)

	return metadata, nil
}

func (p *AzureProvider) GetNodeGroupMetadata(ctx context.Context) map[string]map[string][]string {
	nodeList, err := p.k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil { // failed to call K8s API
		// return cached info, if available
		return azureTypesToGeneric(p.nodePools)
	}

	refreshedNodPoolMetadata := getAKSNodePoolMetadata(nodeList)
	p.Lock()
	p.nodePools = refreshedNodPoolMetadata
	p.Unlock()

	return azureTypesToGeneric(p.nodePools)
}

func getAKSNodePoolMetadata(nodeList *corev1.NodeList) map[NodePoolName]map[armcontainerservice.ScaleSetPriority][]string {
	nodePoolInfo := make(map[NodePoolName]map[armcontainerservice.ScaleSetPriority][]string)
	if nodeList == nil {
		return nodePoolInfo
	}

	for _, node := range nodeList.Items {
		nodePoolLabel := getAKSNodePoolLabel(&node)
		nodePoolScaleSet := getNodePoolScaleSetPriority(node)
		// update node count in node pool
		if _, ok := nodePoolInfo[nodePoolLabel]; !ok {
			nodePoolInfo[nodePoolLabel] = make(map[armcontainerservice.ScaleSetPriority][]string)
		}

		if _, ok := nodePoolInfo[nodePoolLabel][nodePoolScaleSet]; !ok {
			nodePoolInfo[nodePoolLabel][nodePoolScaleSet] = []string{}
		}
		nodePoolInfo[nodePoolLabel][nodePoolScaleSet] = append(nodePoolInfo[nodePoolLabel][nodePoolScaleSet], node.Name)
	}
	return nodePoolInfo
}

func azureTypesToGeneric(metadata map[NodePoolName]map[armcontainerservice.ScaleSetPriority][]string) map[string]map[string][]string {
	m := make(map[string]map[string][]string)
	if metadata == nil {
		return m
	}

	for nodePool, nodePoolTypeToNodeList := range metadata {
		m[string(nodePool)] = make(map[string][]string)
		for nodePoolType, nodeList := range nodePoolTypeToNodeList {
			m[string(nodePool)][string(nodePoolType)] = nodeList
		}
	}
	return m
}

// discoverClusterName attempts to determine the AKS cluster name
func (p *AzureProvider) discoverClusterName(ctx context.Context) (string, error) {
	// Method 1: Look for the label on nodes
	nodes, err := p.k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("listing nodes: %w", err)
	}

	for _, node := range nodes.Items {
		// Check for cluster name label
		if resourceGroupName, ok := node.Labels[azureClusterLbel]; ok {
			aksMetadata, err := parseAKSResourceGroupName(resourceGroupName)
			if err != nil {
				// TODO maybe log?
				continue
			}
			return aksMetadata.ClusterName, nil
		}
	}

	if len(nodes.Items) < 1 {
		return "", fmt.Errorf("cannot determine AKS cluster name since we have no nodes in this cluster")
	}

	// Method 2: Try to find it in node provider IDs (more complex)
	// AKS provider ID format: azure:///subscriptions/{subscriptionId}/resourceGroups/{resourceGroupName}/providers/Microsoft.Compute/virtualMachineScaleSets/{nodeResourceGroup}/{vmssName}/virtualMachines/1
	providerID := nodes.Items[0].Spec.ProviderID
	if !strings.HasPrefix(providerID, "azure://") {
		return "", fmt.Errorf("cannot determine AKS cluster name provider ID doesnt start with azure://")
	}

	if clusterName, err := getClusterNameFromProviderID(providerID); err == nil {
		return clusterName, nil
	}

	return "", fmt.Errorf("could not determine AKS cluster name")
}

type NodePoolName string

func getAKSNodePoolLabel(node *corev1.Node) NodePoolName {
	if val, ok := node.Labels["agentpool"]; ok {
		return NodePoolName(val)
	}
	if val, ok := node.Labels[azureAgentPoolLabel]; ok {
		return NodePoolName(val)
	}
	return NodePoolName("unknown")
}

func getNodePoolScaleSetPriority(node corev1.Node) armcontainerservice.ScaleSetPriority {
	if val, ok := node.Labels[azureScalePriorityLabel]; ok {
		return armcontainerservice.ScaleSetPriority(val) // usually this will be armcontainerservice.ScaleSetPrioritySpot => "Spot"
	}
	return armcontainerservice.ScaleSetPriorityRegular // "Regular"
}

func getClusterNameFromProviderID(providerID string) (string, error) {
	// providerID looks something like this:
	// azure:///subscriptions/a32b188c-43fd-4e28-8f67-c95649ab3119/resourceGroups/mc_dev-test_devzero_eastus/providers/Microsoft.Compute/virtualMachineScaleSets/aks-systemnp-35145560-vmss/virtualMachines/0
	const resourceGroupKey = "resourceGroups/"
	idx := strings.Index(providerID, resourceGroupKey)
	if idx == -1 {
		return "", fmt.Errorf("resourceGroups/ not found in providerID: %s", providerID)
	}

	start := idx + len(resourceGroupKey)
	remaining := providerID[start:]
	end := strings.Index(remaining, "/")
	if end == -1 {
		return "", fmt.Errorf("unable to parse resource group from providerID: %s", providerID)
	}

	resourceGroup := remaining[:end]
	aksMetadata, err := parseAKSResourceGroupName(resourceGroup)
	if err != nil {
		return "", err
	}
	return aksMetadata.ClusterName, nil
}

// Helper functions
func getAzureMetadata(client *http.Client) (*AzureMetadata, error) {
	url := fmt.Sprintf("%s?api-version=2021-02-01", azureMetadataEndpoint)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Metadata", "true")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metadata request returned status %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var metadata AzureMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, fmt.Errorf("unmarshaling metadata: %w", err)
	}

	return &metadata, nil
}

func parseAKSResourceGroupName(resourceGroupName string) (*AKSMetadata, error) {
	// https://learn.microsoft.com/en-us/azure/aks/faq#why-are-two-resource-groups-created-with-aks-
	// pattern is expected to be: MC_myResourceGroup_myAKSCluster_eastus
	parts := strings.Split(resourceGroupName, "_")
	if len(parts) != 4 || !strings.EqualFold(parts[0], "mc") {
		return nil, fmt.Errorf("unexpected format: %s", resourceGroupName)
	}
	return &AKSMetadata{
		ResourceGroup: parts[1],
		ClusterName:   parts[2],
		Region:        parts[3],
	}, nil
}
