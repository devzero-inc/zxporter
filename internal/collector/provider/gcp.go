package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/oauth2/google"
	compute "google.golang.org/api/compute/v1"
	container "google.golang.org/api/container/v1"
	"google.golang.org/api/option"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// GCP metadata server endpoint
	gcpMetadataEndpoint = "http://metadata.google.internal/computeMetadata/v1"

	// GKE node label for node pools
	gkeNodePoolLabel = "cloud.google.com/gke-nodepool"

	// GKE node label for SOPT instance
	gkeSpotLabel = "cloud.google.com/gke-spot"

	// GKE node label for provisioning instance
	gkeProvisioningLabel = "cloud.google.com/gke-provisioning"
)

// GCPProvider implements provider interface for GCP GKE
type GCPProvider struct {
	sync.Mutex

	logger       logr.Logger
	k8sClient    kubernetes.Interface
	httpClient   *http.Client
	containerSvc *container.Service
	computeSvc   *compute.Service
	projectID    string
	zone         string
	region       string
	clusterName  string
	nodePools    map[NodePoolName]map[GKENodePoolType][]string
}

// NewGCPProvider creates a new GCP provider
func NewGCPProvider(logger logr.Logger, k8sClient kubernetes.Interface) (*GCPProvider, error) {
	ctx := context.Background()

	// Create HTTP client for metadata server
	httpClient := &http.Client{Timeout: 5 * time.Second}

	// Check if running on GCP
	available, err := isGCPMetadataAvailable(httpClient)
	if err != nil || !available {
		return nil, fmt.Errorf("[NewGCPProvider] GCP metadata service not available: %w", err)
	}

	// Get project ID, zone and region
	projectID, err := getGCPMetadata(httpClient, "project/project-id")
	if err != nil {
		return nil, fmt.Errorf("[NewGCPProvider] getting project ID: %w", err)
	}

	zone, err := getGCPMetadata(httpClient, "instance/zone")
	// example API response: projects/354385559261/zones/us-central1-a
	if err != nil {
		return nil, fmt.Errorf("[NewGCPProvider] getting zone: %w", err)
	}
	// Zone is returned as "projects/PROJECT_NUM/zones/ZONE", extract just the zone name
	zoneParts := strings.Split(zone, "/")
	zone = zoneParts[len(zoneParts)-1]
	// based on prev example, zone will be set to: us-central1-a

	// Determine region from zone (remove the last character, which is the zone letter)
	region := zone
	if len(zone) > 2 {
		region = zone[:len(zone)-2]
	}
	// based on prev example, region will be set to: us-central1

	// Get cluster name from metadata
	clusterName, err := getGCPMetadata(httpClient, "instance/attributes/cluster-name")
	if err != nil {
		logger.Info("[NewGCPProvider] Couldn't get cluster name from metadata, will try to discover from node labels")
	}

	// Create Google API clients
	creds, err := google.FindDefaultCredentials(ctx, container.CloudPlatformScope)
	if err != nil {
		return nil, fmt.Errorf("[NewGCPProvider] getting GCP credentials: %w", err)
	}

	containerSvc, err := container.NewService(ctx, option.WithCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("[NewGCPProvider] creating GKE client: %w", err)
	}

	computeSvc, err := compute.NewService(ctx, option.WithCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("[NewGCPProvider] creating Compute Engine client: %w", err)
	}

	provider := &GCPProvider{
		logger:       logger,
		k8sClient:    k8sClient,
		httpClient:   httpClient,
		containerSvc: containerSvc,
		computeSvc:   computeSvc,
		projectID:    projectID,
		zone:         zone,
		region:       region,
		clusterName:  clusterName,
		nodePools:    make(map[NodePoolName]map[GKENodePoolType][]string),
	}
	return provider, nil
}

// Name returns the provider name
func (p *GCPProvider) Name() string {
	return "gke"
}

// GetClusterMetadata retrieves GKE cluster metadata
func (p *GCPProvider) GetClusterMetadata(ctx context.Context) (map[string]interface{}, error) {
	p.logger.Info("Collecting GCP GKE cluster metadata")

	// Initialize default metadata with known values
	metadata := map[string]interface{}{
		"provider":   "gke",
		"project_id": p.projectID,
		"zone":       p.zone,
		"region":     p.region,
	}

	// Initialize node pools and network config as empty to avoid nil references
	networkConfig := map[string]interface{}{}

	// Try to determine cluster name if not already set
	if p.clusterName == "" {
		nodes, err := p.k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{Limit: 1})
		if err != nil {
			p.logger.Error(err, "Failed to list nodes to determine cluster name")
		} else if len(nodes.Items) > 0 {
			// Try to extract cluster name from node provider ID
			// GKE provider ID format: gce://PROJECT/ZONE/NODE_NAME
			for _, node := range nodes.Items {
				if name, ok := node.Labels["cluster_name"]; ok {
					p.clusterName = name
					break
				}
			}
		}
	}

	metadata["cluster_name"] = p.clusterName

	// Get the cluster details from GKE API, but don't fail if we can't
	var cluster *container.Cluster

	// Try getting cluster from zone first
	clusterZone, err := p.containerSvc.Projects.Zones.Clusters.Get(p.projectID, p.zone, p.clusterName).Do()
	if err != nil {
		p.logger.Error(err, "Failed to get clusters in project zone",
			"project_id", p.projectID,
			"zone", p.zone,
			"cluster_name", p.clusterName)

		// If zone lookup fails, try region
		clusterRegion, err := p.containerSvc.Projects.Locations.Clusters.Get(
			fmt.Sprintf("projects/%s/locations/%s/clusters/%s", p.projectID, p.region, p.clusterName)).Do()
		if err != nil {
			p.logger.Error(err, "Failed to get clusters in project region",
				"project_id", p.projectID,
				"region", p.region,
				"cluster_name", p.clusterName)
		} else {
			cluster = clusterRegion
		}
	} else {
		cluster = clusterZone
	}

	// Get node pools -> if it fails will just have an empty list
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

	refreshedNodPoolMetadata := getGKENodePoolMetadata(nodeList)
	metadata["node_pools"] = refreshedNodPoolMetadata
	p.Lock()
	p.nodePools = refreshedNodPoolMetadata
	p.Unlock()

	// Add all information we have to metadata
	metadata["network_config"] = networkConfig

	// Extract cluster information if available
	if cluster != nil {
		// Extract network info
		networkConfig["network"] = cluster.Network
		networkConfig["subnetwork"] = cluster.Subnetwork

		if cluster.NetworkConfig != nil {
			networkConfig["enable_intra_node_visibility"] = cluster.NetworkConfig.EnableIntraNodeVisibility
			networkConfig["datapath_provider"] = cluster.NetworkConfig.DatapathProvider
			networkConfig["default_enable_private_nodes"] = cluster.NetworkConfig.DefaultEnablePrivateNodes

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

		// Update metadata with cluster details
		metadata["cluster_name"] = cluster.Name
		metadata["kubernetes_version"] = cluster.CurrentMasterVersion
		metadata["create_time"] = cluster.CreateTime
		metadata["endpoint"] = cluster.Endpoint
		metadata["location_type"] = getLocationType(cluster)
		metadata["status"] = cluster.Status
		metadata["cluster_ipv4_cidr"] = cluster.ClusterIpv4Cidr

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
	} else {
		p.logger.Info("Collected partial GKE cluster metadata (cluster API not accessible)",
			"cluster_name", p.clusterName,
			"project", p.projectID,
			"region", p.region)
	}

	return metadata, nil
}

func (p *GCPProvider) GetNodeGroupMetadata(ctx context.Context) map[string]map[string][]string {
	nodeList, err := p.k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil { // failed to call K8s API
		// return cached info, if available
		return gkeTypesToGeneric(p.nodePools)
	}

	refreshedNodPoolMetadata := getGKENodePoolMetadata(nodeList)
	p.Lock()
	p.nodePools = refreshedNodPoolMetadata
	p.Unlock()

	return gkeTypesToGeneric(p.nodePools)
}

func gkeTypesToGeneric(metadata map[NodePoolName]map[GKENodePoolType][]string) map[string]map[string][]string {
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

func getGKENodePoolMetadata(nodeList *corev1.NodeList) map[NodePoolName]map[GKENodePoolType][]string {
	nodePoolInfo := make(map[NodePoolName]map[GKENodePoolType][]string)
	if nodeList == nil {
		return nodePoolInfo
	}

	for _, node := range nodeList.Items {
		nodePoolLabel := getGKENodePoolLabel(&node)
		nodePoolType := getGKENodePoolType(node)
		// update node count in node pool
		if _, ok := nodePoolInfo[nodePoolLabel]; !ok {
			nodePoolInfo[nodePoolLabel] = make(map[GKENodePoolType][]string)
		}

		if _, ok := nodePoolInfo[nodePoolLabel][nodePoolType]; !ok {
			nodePoolInfo[nodePoolLabel][nodePoolType] = []string{}
		}
		nodePoolInfo[nodePoolLabel][nodePoolType] = append(nodePoolInfo[nodePoolLabel][nodePoolType], node.Name)
	}
	return nodePoolInfo
}

func getGKENodePoolLabel(node *corev1.Node) NodePoolName {
	// https://cloud.google.com/kubernetes-engine/docs/how-to/creating-managing-labels#automatically-applied-labels
	if val, ok := node.Labels[gkeNodePoolLabel]; ok {
		return NodePoolName(val)
	}
	return NodePoolName("unknown")
}

type GKENodePoolType string

const (
	// GKE_Regular - Regular VMs.
	GKE_Regular GKENodePoolType = "Regular"
	// GKE_Regular - Spot/preemtible.
	GKE_Spot GKENodePoolType = "Spot"
)

func getGKENodePoolType(node corev1.Node) GKENodePoolType {
	// https://cloud.google.com/kubernetes-engine/docs/concepts/spot-vms
	if val, ok := node.Labels[gkeSpotLabel]; ok {
		if strings.EqualFold(val, "true") {
			return GKE_Spot
		}
	}
	if val, ok := node.Labels[gkeProvisioningLabel]; ok {
		if strings.EqualFold(val, "spot") {
			return GKE_Spot
		}
	}
	return GKE_Regular
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
