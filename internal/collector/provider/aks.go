package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice"
	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// Azure metadata server endpoint
	azureMetadataEndpoint = "http://169.254.169.254/metadata/instance"

	// AKS node label for node pools
	aksNodePoolLabel = "agentpool"
)

// AzureProvider implements provider interface for Azure AKS
type AzureProvider struct {
	logger         logr.Logger
	k8sClient      kubernetes.Interface
	httpClient     *http.Client
	resourceGroup  string
	subscriptionID string
	location       string
	clusterName    string
	aksClient      *armcontainerservice.ManagedClustersClient
	nodePools      map[string]map[string]interface{}
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

// NewAzureProvider creates a new Azure provider
func NewAzureProvider(logger logr.Logger, k8sClient kubernetes.Interface) (*AzureProvider, error) {
	// Create HTTP client for metadata server
	httpClient := &http.Client{Timeout: 5 * time.Second}

	// Check if running on Azure
	available, err := isAzureMetadataAvailable(httpClient)
	if err != nil || !available {
		return nil, fmt.Errorf("azure metadata service not available: %w", err)
	}

	// Get Azure instance metadata
	metadata, err := getAzureMetadata(httpClient)
	if err != nil {
		return nil, fmt.Errorf("getting Azure metadata: %w", err)
	}

	// Create Azure clients
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("getting Azure credentials: %w", err)
	}

	aksClient, err := armcontainerservice.NewManagedClustersClient(metadata.Compute.SubscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("creating AKS client: %w", err)
	}

	// Initialize provider
	provider := &AzureProvider{
		logger:         logger,
		k8sClient:      k8sClient,
		httpClient:     httpClient,
		resourceGroup:  metadata.Compute.ResourceGroupName,
		subscriptionID: metadata.Compute.SubscriptionID,
		location:       metadata.Compute.Location,
		aksClient:      aksClient,
		nodePools:      make(map[string]map[string]interface{}),
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

	// If we don't have the cluster name yet, try again
	if p.clusterName == "" {
		clusterName, err := p.discoverClusterName(ctx)
		if err != nil {
			p.logger.Error(err, "Failed to discover AKS cluster name")
			// Continue with partial metadata
		} else {
			p.clusterName = clusterName
		}
	}

	// Build base metadata even if we couldn't find the cluster
	metadata := map[string]interface{}{
		"provider":        "aks",
		"subscription_id": p.subscriptionID,
		"resource_group":  p.resourceGroup,
		"location":        p.location,
	}

	// If we have the cluster name, get detailed cluster info
	if p.clusterName != "" {
		metadata["cluster_name"] = p.clusterName

		// Get cluster details from AKS API
		cluster, err := p.aksClient.Get(ctx, p.resourceGroup, p.clusterName, nil)
		if err != nil {
			p.logger.Error(err, "Failed to get cluster details from AKS API",
				"cluster", p.clusterName,
				"resourceGroup", p.resourceGroup)
		} else if cluster.Properties != nil {
			// Extract and add cluster properties
			if cluster.Properties.KubernetesVersion != nil {
				metadata["kubernetes_version"] = *cluster.Properties.KubernetesVersion
			}

			if cluster.Properties.DNSPrefix != nil {
				metadata["dns_prefix"] = *cluster.Properties.DNSPrefix
			}

			if cluster.Properties.Fqdn != nil {
				metadata["fqdn"] = *cluster.Properties.Fqdn
			}

			if cluster.Properties.ProvisioningState != nil {
				metadata["provisioning_state"] = *cluster.Properties.ProvisioningState
			}

			// Extract network profile
			if cluster.Properties.NetworkProfile != nil {
				networkProfile := map[string]interface{}{}

				if cluster.Properties.NetworkProfile.NetworkPlugin != nil {
					networkProfile["network_plugin"] = *cluster.Properties.NetworkProfile.NetworkPlugin
				}

				if cluster.Properties.NetworkProfile.NetworkPolicy != nil {
					networkProfile["network_policy"] = *cluster.Properties.NetworkProfile.NetworkPolicy
				}

				if cluster.Properties.NetworkProfile.PodCidr != nil {
					networkProfile["pod_cidr"] = *cluster.Properties.NetworkProfile.PodCidr
				}

				if cluster.Properties.NetworkProfile.ServiceCidr != nil {
					networkProfile["service_cidr"] = *cluster.Properties.NetworkProfile.ServiceCidr
				}

				metadata["network_profile"] = networkProfile
			}

			// Get agent pools (node pools)
			if cluster.Properties.AgentPoolProfiles != nil {
				nodePools, err := p.getNodePoolsFromAPI(ctx, cluster.Properties.AgentPoolProfiles)
				if err != nil {
					p.logger.Error(err, "Failed to process agent pool profiles")
				} else {
					metadata["node_pools"] = nodePools
				}
			}
		}
	} else {
		// If we couldn't get cluster name, try to get node pools from node labels
		nodePools, err := p.getNodePoolsFromNodes(ctx)
		if err != nil {
			p.logger.Error(err, "Failed to get node pools from nodes")
		} else {
			metadata["node_pools"] = nodePools
		}
	}

	p.logger.Info("Collected AKS cluster metadata",
		"subscription", p.subscriptionID,
		"resourceGroup", p.resourceGroup,
		"location", p.location,
		"cluster", p.clusterName)

	return metadata, nil
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
		if clusterName, ok := node.Labels["kubernetes.azure.com/cluster"]; ok {
			return clusterName, nil
		}
	}

	// Method 2: Try to find it in node provider IDs (more complex)
	if len(nodes.Items) > 0 {
		// AKS provider ID format: azure:///subscriptions/{subscriptionId}/resourceGroups/{resourceGroupName}/providers/Microsoft.Compute/virtualMachineScaleSets/{nodeResourceGroup}/{vmssName}/virtualMachines/1
		providerID := nodes.Items[0].Spec.ProviderID
		if strings.HasPrefix(providerID, "azure://") {
			// Split the provider ID
			parts := strings.Split(providerID, "/")
			if len(parts) >= 9 {
				// Try to list all AKS clusters in the resource group
				pager := p.aksClient.NewListByResourceGroupPager(p.resourceGroup, nil)
				for pager.More() {
					page, err := pager.NextPage(ctx)
					if err != nil {
						return "", fmt.Errorf("listing AKS clusters: %w", err)
					}

					// If there's only one cluster, use that
					if len(page.Value) == 1 && page.Value[0].Name != nil {
						return *page.Value[0].Name, nil
					}

					// Otherwise try to match by node resource group or other properties
					for _, cluster := range page.Value {
						if cluster.Name != nil && cluster.Properties != nil && cluster.Properties.NodeResourceGroup != nil {
							// If we can find the node resource group in the provider ID
							if strings.Contains(providerID, *cluster.Properties.NodeResourceGroup) {
								return *cluster.Name, nil
							}
						}
					}
				}
			}
		}
	}

	// Method 3: Try listing all clusters in the resource group
	pager := p.aksClient.NewListByResourceGroupPager(p.resourceGroup, nil)
	page, err := pager.NextPage(ctx)
	if err != nil {
		return "", fmt.Errorf("listing AKS clusters: %w", err)
	}

	// If there's only one cluster in the resource group, use that
	if len(page.Value) == 1 && page.Value[0].Name != nil {
		return *page.Value[0].Name, nil
	}

	return "", fmt.Errorf("could not determine AKS cluster name")
}

// getNodePoolsFromAPI processes agent pool profiles from the AKS API
func (p *AzureProvider) getNodePoolsFromAPI(ctx context.Context, profiles []*armcontainerservice.ManagedClusterAgentPoolProfile) ([]map[string]interface{}, error) {
	var nodePools []map[string]interface{}

	for _, profile := range profiles {
		if profile.Name == nil {
			continue
		}

		// Create node pool data
		nodePool := map[string]interface{}{
			"name": *profile.Name,
		}

		if profile.VMSize != nil {
			nodePool["vm_size"] = *profile.VMSize
		}

		if profile.Count != nil {
			nodePool["node_count"] = *profile.Count
		}

		if profile.OSDiskSizeGB != nil {
			nodePool["os_disk_size_gb"] = *profile.OSDiskSizeGB
		}

		if profile.OSType != nil {
			nodePool["os_type"] = string(*profile.OSType)
		}

		if profile.Mode != nil {
			nodePool["mode"] = string(*profile.Mode)
		}

		// Auto scaling settings
		if profile.EnableAutoScaling != nil && *profile.EnableAutoScaling {
			autoscaling := map[string]interface{}{
				"enabled": true,
			}

			if profile.MinCount != nil {
				autoscaling["min_count"] = *profile.MinCount
			}

			if profile.MaxCount != nil {
				autoscaling["max_count"] = *profile.MaxCount
			}

			nodePool["autoscaling"] = autoscaling
		}

		// Check if spot/low priority
		if profile.ScaleSetPriority != nil && *profile.ScaleSetPriority == armcontainerservice.ScaleSetPrioritySpot {
			nodePool["spot"] = true
		} else {
			nodePool["spot"] = false
		}

		// Add to result
		nodePools = append(nodePools, nodePool)

		// Store for later node mapping
		p.nodePools[*profile.Name] = nodePool
	}

	return nodePools, nil
}

// getNodePoolsFromNodes builds node pool info from node labels when API access fails
func (p *AzureProvider) getNodePoolsFromNodes(ctx context.Context) ([]map[string]interface{}, error) {
	nodes, err := p.k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}

	nodePoolMap := make(map[string]map[string]interface{})
	nodeCountByPool := make(map[string]int)

	for _, node := range nodes.Items {
		// Check for the agentpool label
		poolName, ok := node.Labels[aksNodePoolLabel]
		if !ok {
			continue
		}

		// Count nodes in each pool
		nodeCountByPool[poolName]++

		// Create node pool entry if it doesn't exist
		if _, exists := nodePoolMap[poolName]; !exists {
			// Extract VM size/type from node
			vmSize := ""
			if label, ok := node.Labels["beta.kubernetes.io/instance-type"]; ok {
				vmSize = label
			} else if label, ok := node.Labels["node.kubernetes.io/instance-type"]; ok {
				vmSize = label
			}

			// Check if spot instance
			isSpot := false
			if label, ok := node.Labels["kubernetes.azure.com/scalesetpriority"]; ok && label == "spot" {
				isSpot = true
			}

			nodePoolMap[poolName] = map[string]interface{}{
				"name":    poolName,
				"vm_size": vmSize,
				"spot":    isSpot,
			}
		}
	}

	// Update node counts
	for poolName, count := range nodeCountByPool {
		if pool, ok := nodePoolMap[poolName]; ok {
			pool["node_count"] = count
			nodePoolMap[poolName] = pool
		}
	}

	// Convert map to slice
	var nodePools []map[string]interface{}
	for _, pool := range nodePoolMap {
		nodePools = append(nodePools, pool)
		// Store for later node mapping
		p.nodePools[pool["name"].(string)] = pool
	}

	return nodePools, nil
}

// GetNodeGroupForNode identifies the node pool a node belongs to
func (p *AzureProvider) GetNodeGroupForNode(nodeName string) (map[string]interface{}, error) {
	// Try to get node details from K8s API
	node, err := p.k8sClient.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting node %s: %w", nodeName, err)
	}

	// Check for agentpool label
	poolName, ok := node.Labels[aksNodePoolLabel]
	if !ok {
		return nil, fmt.Errorf("node %s does not have AKS agent pool label", nodeName)
	}

	// Load node pools if empty
	if len(p.nodePools) == 0 {
		_, err := p.getNodePoolsFromNodes(context.Background())
		if err != nil {
			p.logger.Error(err, "Failed to load node pools")
		}
	}

	// Try to find node pool in our cache
	if nodePool, ok := p.nodePools[poolName]; ok {
		return nodePool, nil
	}

	// If not in cache, create basic info
	nodePool := map[string]interface{}{
		"name": poolName,
	}

	// Extract VM size from node
	if vmSize, ok := node.Labels["beta.kubernetes.io/instance-type"]; ok {
		nodePool["vm_size"] = vmSize
	} else if vmSize, ok := node.Labels["node.kubernetes.io/instance-type"]; ok {
		nodePool["vm_size"] = vmSize
	}

	// Check if spot instance
	isSpot := false
	if val, ok := node.Labels["kubernetes.azure.com/scalesetpriority"]; ok && val == "spot" {
		isSpot = true
	}
	nodePool["spot"] = isSpot

	// Store in cache for future use
	p.nodePools[poolName] = nodePool

	return nodePool, nil
}

// Helper functions
func isAzureMetadataAvailable(client *http.Client) (bool, error) {
	req, err := http.NewRequest("GET", azureMetadataEndpoint, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Metadata", "true")

	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	return resp.StatusCode == http.StatusOK, nil
}

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
