package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/oauth2/google"
	compute "google.golang.org/api/compute/v1"
	container "google.golang.org/api/container/v1"
	"google.golang.org/api/option"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// GCP metadata server endpoint
	gcpMetadataEndpoint = "http://metadata.google.internal/computeMetadata/v1"

	// GKE node label for node pools
	gkeNodePoolLabel = "cloud.google.com/gke-nodepool"
)

// GCPProvider implements provider interface for GCP GKE
type GCPProvider struct {
	logger       logr.Logger
	k8sClient    kubernetes.Interface
	httpClient   *http.Client
	containerSvc *container.Service
	computeSvc   *compute.Service
	projectID    string
	zone         string
	region       string
	clusterName  string
	nodePools    map[string]map[string]interface{}
}

// NewGCPProvider creates a new GCP provider
func NewGCPProvider(logger logr.Logger, k8sClient kubernetes.Interface) (*GCPProvider, error) {
	ctx := context.Background()

	// Create HTTP client for metadata server
	httpClient := &http.Client{Timeout: 5 * time.Second}

	// Check if running on GCP
	available, err := isGCPMetadataAvailable(httpClient)
	if err != nil || !available {
		return nil, fmt.Errorf("GCP metadata service not available: %w", err)
	}

	// Get project ID, zone and region
	projectID, err := getGCPMetadata(httpClient, "project/project-id")
	if err != nil {
		return nil, fmt.Errorf("getting project ID: %w", err)
	}

	zone, err := getGCPMetadata(httpClient, "instance/zone")
	if err != nil {
		return nil, fmt.Errorf("getting zone: %w", err)
	}
	// Zone is returned as "projects/PROJECT_NUM/zones/ZONE", extract just the zone name
	zoneParts := strings.Split(zone, "/")
	zone = zoneParts[len(zoneParts)-1]

	// Determine region from zone (remove the last character, which is the zone letter)
	region := zone
	if len(zone) > 2 {
		region = zone[:len(zone)-2]
	}

	// Get cluster name from metadata
	clusterName, err := getGCPMetadata(httpClient, "instance/attributes/cluster-name")
	if err != nil {
		logger.Info("Couldn't get cluster name from metadata, will try to discover from node labels")
	}

	// Create Google API clients
	creds, err := google.FindDefaultCredentials(ctx, container.CloudPlatformScope)
	if err != nil {
		return nil, fmt.Errorf("getting GCP credentials: %w", err)
	}

	containerSvc, err := container.NewService(ctx, option.WithCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("creating GKE client: %w", err)
	}

	computeSvc, err := compute.NewService(ctx, option.WithCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("creating Compute Engine client: %w", err)
	}

	return &GCPProvider{
		logger:       logger,
		k8sClient:    k8sClient,
		httpClient:   httpClient,
		containerSvc: containerSvc,
		computeSvc:   computeSvc,
		projectID:    projectID,
		zone:         zone,
		region:       region,
		clusterName:  clusterName,
		nodePools:    make(map[string]map[string]interface{}),
	}, nil
}

// Name returns the provider name
func (p *GCPProvider) Name() string {
	return "gke"
}

// GetClusterMetadata retrieves GKE cluster metadata
func (p *GCPProvider) GetClusterMetadata(ctx context.Context) (map[string]interface{}, error) {
	p.logger.Info("Collecting GCP GKE cluster metadata")

	// If we don't have the cluster name yet, try to get it from a node
	if p.clusterName == "" {
		nodes, err := p.k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{Limit: 1})
		if err != nil {
			return nil, fmt.Errorf("listing nodes: %w", err)
		}

		if len(nodes.Items) > 0 {
			// Try to extract cluster name from node provider ID
			// GKE provider ID format: gce://PROJECT/ZONE/NODE_NAME
			providerID := nodes.Items[0].Spec.ProviderID
			if strings.HasPrefix(providerID, "gce://") {
				// Check for cluster-name label
				for _, node := range nodes.Items {
					if name, ok := node.Labels["cloud.google.com/gke-cluster"]; ok {
						p.clusterName = name
						break
					}
					if name, ok := node.Labels["cluster_name"]; ok {
						p.clusterName = name
						break
					}
				}
			}
		}

		if p.clusterName == "" {
			return nil, fmt.Errorf("couldn't determine GKE cluster name")
		}
	}

	// Get the cluster details from GKE API
	var cluster *container.Cluster
	var err error

	// Try getting cluster from zone
	cluster, err = p.containerSvc.Projects.Zones.Clusters.Get(p.projectID, p.zone, p.clusterName).Do()
	if err != nil {
		// If zone lookup fails, try region
		cluster, err = p.containerSvc.Projects.Locations.Clusters.Get(
			fmt.Sprintf("projects/%s/locations/%s/clusters/%s", p.projectID, p.region, p.clusterName)).Do()
		if err != nil {
			return nil, fmt.Errorf("getting cluster details: %w", err)
		}
	}

	// Get node pools
	nodePools, err := p.getNodePools(ctx)
	if err != nil {
		p.logger.Error(err, "Failed to get node pools, continuing with partial metadata")
	}

	// Extract network info
	networkConfig := map[string]interface{}{
		"network":    cluster.Network,
		"subnetwork": cluster.Subnetwork,
	}

	if cluster.NetworkConfig != nil {
		// Add available fields from NetworkConfig
		if cluster.NetworkConfig.EnableIntraNodeVisibility {
			networkConfig["enable_intra_node_visibility"] = cluster.NetworkConfig.EnableIntraNodeVisibility
		}

		if cluster.NetworkConfig.DatapathProvider != "" {
			networkConfig["datapath_provider"] = cluster.NetworkConfig.DatapathProvider
		}

		if cluster.NetworkConfig.DefaultEnablePrivateNodes {
			networkConfig["default_enable_private_nodes"] = cluster.NetworkConfig.DefaultEnablePrivateNodes
		}

		if cluster.NetworkConfig.DefaultSnatStatus != nil {
			networkConfig["default_snat_disabled"] = cluster.NetworkConfig.DefaultSnatStatus.Disabled
		}

		if cluster.NetworkConfig.DnsConfig != nil {
			networkConfig["dns_config"] = map[string]interface{}{
				"cluster_dns":        cluster.NetworkConfig.DnsConfig.ClusterDns,
				"cluster_dns_scope":  cluster.NetworkConfig.DnsConfig.ClusterDnsScope,
				"cluster_dns_domain": cluster.NetworkConfig.DnsConfig.ClusterDnsDomain,
			}
		}

		// Add other relevant fields from NetworkConfig
		networkConfig["enable_l4ilb_subsetting"] = cluster.NetworkConfig.EnableL4ilbSubsetting
		networkConfig["enable_multi_networking"] = cluster.NetworkConfig.EnableMultiNetworking
		networkConfig["private_ipv6_google_access"] = cluster.NetworkConfig.PrivateIpv6GoogleAccess
	}

	// Build metadata
	metadata := map[string]interface{}{
		"provider":           "gke",
		"project_id":         p.projectID,
		"zone":               p.zone,
		"region":             p.region,
		"cluster_name":       cluster.Name,
		"kubernetes_version": cluster.CurrentMasterVersion,
		"create_time":        cluster.CreateTime,
		"node_pools":         nodePools,
		"network_config":     networkConfig,
		"endpoint":           cluster.Endpoint,
		"location_type":      getLocationType(cluster), // zonal or regional
		"status":             cluster.Status,
		"cluster_ipv4_cidr":  cluster.ClusterIpv4Cidr,
	}

	// Add addon configurations if available
	if cluster.AddonsConfig != nil {
		metadata["addons_config"] = map[string]interface{}{
			"http_load_balancing_enabled":        cluster.AddonsConfig.HttpLoadBalancing == nil || !cluster.AddonsConfig.HttpLoadBalancing.Disabled,
			"horizontal_pod_autoscaling_enabled": cluster.AddonsConfig.HorizontalPodAutoscaling == nil || !cluster.AddonsConfig.HorizontalPodAutoscaling.Disabled,
			"kubernetes_dashboard_enabled":       cluster.AddonsConfig.KubernetesDashboard != nil && !cluster.AddonsConfig.KubernetesDashboard.Disabled,
			"network_policy_enabled":             cluster.AddonsConfig.NetworkPolicyConfig != nil && !cluster.AddonsConfig.NetworkPolicyConfig.Disabled,
		}
	}

	p.logger.Info("Collected GKE cluster metadata",
		"cluster", cluster.Name,
		"project", p.projectID,
		"region", p.region)

	return metadata, nil
}

// getNodePools retrieves all node pools associated with the GKE cluster
func (p *GCPProvider) getNodePools(ctx context.Context) ([]map[string]interface{}, error) {
	// First try to get node pools from GKE API
	var nodePoolsResponse *container.ListNodePoolsResponse
	var err error

	// Try zonal endpoint
	nodePoolsResponse, err = p.containerSvc.Projects.Zones.Clusters.NodePools.List(
		p.projectID, p.zone, p.clusterName).Do()

	if err != nil {
		// If zonal fails, try regional endpoint
		nodePoolsResponse, err = p.containerSvc.Projects.Locations.Clusters.NodePools.List(
			fmt.Sprintf("projects/%s/locations/%s/clusters/%s", p.projectID, p.region, p.clusterName)).Do()
		if err != nil {
			// If API access fails, fall back to gathering from node labels
			return p.getNodePoolsFromNodes(ctx)
		}
	}

	var nodePools []map[string]interface{}

	for _, np := range nodePoolsResponse.NodePools {
		// Get machine type details
		var machineType string
		if len(np.Config.MachineType) > 0 {
			machineType = np.Config.MachineType
		}

		// Get autoscaling settings
		var autoscaling map[string]interface{}
		if np.Autoscaling != nil {
			autoscaling = map[string]interface{}{
				"enabled":     np.Autoscaling.Enabled,
				"min_nodes":   np.Autoscaling.MinNodeCount,
				"max_nodes":   np.Autoscaling.MaxNodeCount,
				"auto_repair": np.Management != nil && np.Management.AutoRepair,
			}
		}

		// Create node pool data
		nodePool := map[string]interface{}{
			"name":               np.Name,
			"version":            np.Version,
			"status":             np.Status,
			"machine_type":       machineType,
			"disk_size_gb":       np.Config.DiskSizeGb,
			"disk_type":          np.Config.DiskType,
			"image_type":         np.Config.ImageType,
			"initial_node_count": np.InitialNodeCount,
			"autoscaling":        autoscaling,
			"preemptible":        np.Config.Preemptible,
		}

		// Store for later node mapping
		p.nodePools[np.Name] = nodePool
		nodePools = append(nodePools, nodePool)
	}

	return nodePools, nil
}

// getNodePoolsFromNodes builds node pool info from node labels when API access fails
func (p *GCPProvider) getNodePoolsFromNodes(ctx context.Context) ([]map[string]interface{}, error) {
	nodes, err := p.k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}

	nodePoolMap := make(map[string]map[string]interface{})
	nodeCountByPool := make(map[string]int)

	for _, node := range nodes.Items {
		poolName, ok := node.Labels[gkeNodePoolLabel]
		if !ok {
			continue
		}

		// Count nodes in each pool
		nodeCountByPool[poolName]++

		// Create node pool entry if it doesn't exist
		if _, exists := nodePoolMap[poolName]; !exists {
			// Extract machine type from node
			machineType := ""
			if label, ok := node.Labels["beta.kubernetes.io/instance-type"]; ok {
				machineType = label
			} else if label, ok := node.Labels["node.kubernetes.io/instance-type"]; ok {
				machineType = label
			}

			// Check if preemptible
			preemptible := false
			if label, ok := node.Labels["cloud.google.com/gke-preemptible"]; ok && label == "true" {
				preemptible = true
			}

			nodePoolMap[poolName] = map[string]interface{}{
				"name":         poolName,
				"machine_type": machineType,
				"preemptible":  preemptible,
			}
		}
	}

	// Update node counts
	for poolName, count := range nodeCountByPool {
		if pool, ok := nodePoolMap[poolName]; ok {
			pool["current_node_count"] = count
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
func (p *GCPProvider) GetNodeGroupForNode(nodeName string) (map[string]interface{}, error) {
	// Try to get node details from K8s API
	node, err := p.k8sClient.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting node %s: %w", nodeName, err)
	}

	// Check for node pool label
	poolName, ok := node.Labels[gkeNodePoolLabel]
	if !ok {
		return nil, fmt.Errorf("node %s does not have GKE node pool label", nodeName)
	}

	// Load node pools if empty
	if len(p.nodePools) == 0 {
		_, err := p.getNodePools(context.Background())
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

	// Extract machine type from node
	if machineType, ok := node.Labels["beta.kubernetes.io/instance-type"]; ok {
		nodePool["machine_type"] = machineType
	} else if machineType, ok := node.Labels["node.kubernetes.io/instance-type"]; ok {
		nodePool["machine_type"] = machineType
	}

	// Check if preemptible
	preemptible := false
	if val, ok := node.Labels["cloud.google.com/gke-preemptible"]; ok && val == "true" {
		preemptible = true
	}
	nodePool["preemptible"] = preemptible

	// Store in cache for future use
	p.nodePools[poolName] = nodePool

	return nodePool, nil
}

// Helper functions
func isGCPMetadataAvailable(client *http.Client) (bool, error) {
	req, err := http.NewRequest("GET", gcpMetadataEndpoint, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Metadata-Flavor", "Google")

	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	return resp.StatusCode == http.StatusOK, nil
}

func getGCPMetadata(client *http.Client, path string) (string, error) {
	url := fmt.Sprintf("%s/%s", gcpMetadataEndpoint, path)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("metadata request returned status %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	return string(data), nil
}

func getLocationType(cluster *container.Cluster) string {
	if strings.Contains(cluster.SelfLink, "/zones/") {
		return "zonal"
	} else if strings.Contains(cluster.SelfLink, "/locations/") {
		// Further check if it's a regional cluster
		if !strings.HasSuffix(cluster.Location, "-a") &&
			!strings.HasSuffix(cluster.Location, "-b") &&
			!strings.HasSuffix(cluster.Location, "-c") &&
			!strings.HasSuffix(cluster.Location, "-d") {
			return "regional"
		}
	}
	return "zonal" // Default to zonal
}
