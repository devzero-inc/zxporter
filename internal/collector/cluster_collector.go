package collector

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	metricsv1 "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/devzero-inc/zxporter/internal/collector/provider"
)

const (
	KUBE_SYSTEM_NS = "kube-system"
)

// ClusterCollector collects comprehensive cluster information
type ClusterCollector struct {
	k8sClient      kubernetes.Interface
	metricsClient  metricsv1.Interface
	provider       provider.Provider
	resourceChan   chan []CollectedResource
	stopCh         chan struct{}
	ticker         *time.Ticker
	updateInterval time.Duration
	logger         logr.Logger
	mu             sync.RWMutex
}

// NewClusterCollector creates a new collector for cluster data
func NewClusterCollector(
	k8sClient kubernetes.Interface,
	metricsClient metricsv1.Interface,
	provider provider.Provider,
	updateInterval time.Duration,
	logger logr.Logger,
) *ClusterCollector {
	// Default to 30 minute update interval if not specified
	if updateInterval <= 0 {
		updateInterval = 30 * time.Minute
	}

	return &ClusterCollector{
		k8sClient:      k8sClient,
		metricsClient:  metricsClient,
		provider:       provider,
		resourceChan:   make(chan []CollectedResource, 10),
		stopCh:         make(chan struct{}),
		updateInterval: updateInterval,
		logger:         logger.WithName("cluster-collector"),
	}
}

// Start begins the cluster data collection process
func (c *ClusterCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting cluster collector",
		"provider", c.provider.Name(),
		"interval", c.updateInterval)

	// Start a ticker for periodic collection
	c.ticker = time.NewTicker(c.updateInterval)

	// Start the collection loop
	go c.collectLoop(ctx)

	return nil
}

// collectLoop collects cluster data at regular intervals
func (c *ClusterCollector) collectLoop(ctx context.Context) {
	// Collect immediately on start
	if err := c.collectClusterData(ctx); err != nil {
		c.logger.Error(err, "Failed to collect cluster data on startup")
	}

	// Then collect based on ticker
	for {
		select {
		case <-c.stopCh:
			c.logger.Info("Stopping cluster collection loop")
			return
		case <-ctx.Done():
			c.logger.Info("Context done, stopping cluster collection loop")
			return
		case <-c.ticker.C:
			if err := c.collectClusterData(ctx); err != nil {
				c.logger.Error(err, "Failed to collect cluster data")
			}
		}
	}
}

// collectClusterData gathers comprehensive information about the cluster
func (c *ClusterCollector) collectClusterData(ctx context.Context) error {
	c.logger.Info("Collecting cluster data")

	// 1. Get provider-specific info
	providerData, err := c.provider.GetClusterMetadata(ctx)
	if err != nil {
		c.logger.Error(err, "Failed to get provider-specific cluster metadata")
		providerData = map[string]interface{}{
			"provider": "unknown",
			"error":    err.Error(),
		}
	}

	// 2. Get Kubernetes version
	k8sVersion := c.getKubernetesVersion(ctx)

	// 3. Get all nodes
	nodes, err := c.k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing nodes: %w", err)
	}

	// 4. Get node groups and map nodes to node groups
	nodeGroupMetadata := c.provider.GetNodeGroupMetadata(ctx)

	// 5. Get namespaces count
	namespaces, err := c.k8sClient.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing namespaces: %w", err)
	}

	// 6. Get resource capacity and usage
	totalCPUCapacity, totalMemoryCapacity, cpuUsage, memoryUsage := int64(0), int64(0), int64(0), int64(0)
	if c.metricsClient == nil {
		totalCPUCapacity, totalMemoryCapacity, cpuUsage, memoryUsage = c.getResourceMetrics(ctx, nodes.Items)
	}

	// 7. Get workload counts across all namespaces
	workloadCount := c.getWorkloadCount(ctx)

	// 8. Get CNI plugins (from annotations, configmaps, etc.)
	cniPlugins := c.detectCNIPlugins(ctx)

	// 9. Get cluster API endpoint (usually from kubeadm ConfigMap)
	clusterAPI := c.getClusterAPIEndpoint(ctx)

	// 10. Create the cluster data object
	clusterData := map[string]interface{}{
		"id":                    fmt.Sprintf("%s-%s", providerData["cluster_name"], providerData["region"]),
		"name":                  providerData["cluster_name"],
		"cluster_api":           clusterAPI,
		"version":               k8sVersion,
		"cni_plugins":           cniPlugins,
		"node_group_metadata":   nodeGroupMetadata,
		"provider_specific":     providerData,
		"node_count":            len(nodes.Items),
		"namespace_count":       len(namespaces.Items),
		"workload_count":        workloadCount,
		"provider":              providerData["provider"],
		"region":                providerData["region"],
		"total_cpu_capacity":    totalCPUCapacity,
		"total_memory_capacity": totalMemoryCapacity,
		"cpu_usage":             cpuUsage,
		"memory_usage":          memoryUsage,
		"cpu_utilization":       calculatePercentage(cpuUsage, totalCPUCapacity),
		"memory_utilization":    calculatePercentage(memoryUsage, totalMemoryCapacity),
		"created_at":            time.Now().Unix(),
		"updated_at":            time.Now().Unix(),
	}

	// 11. Send the data through the channel as a slice
	c.resourceChan <- []CollectedResource{{
		ResourceType: Cluster,
		Object:       clusterData,
		Timestamp:    time.Now(),
		EventType:    EventTypeMetadata,
		Key:          fmt.Sprintf("%s", providerData["cluster_name"]),
	}}

	c.logger.Info("Cluster data collected successfully",
		"cluster", providerData["cluster_name"],
		"provider", providerData["provider"],
		"region", providerData["region"],
		"nodes", len(nodes.Items),
		"namespaces", len(namespaces.Items),
		"all_data", clusterData)

	return nil
}

// getKubernetesVersion gets the Kubernetes version from the API server
func (c *ClusterCollector) getKubernetesVersion(ctx context.Context) string {
	serverVersion, err := c.k8sClient.Discovery().ServerVersion()
	if err != nil {
		c.logger.Error(err, "Failed to get Kubernetes version")
		return "unknown"
	}
	return serverVersion.String()
}

// getResourceMetrics calculates cluster-wide resource usage metrics
func (c *ClusterCollector) getResourceMetrics(ctx context.Context, nodes []corev1.Node) (int64, int64, int64, int64) {
	var totalCPUCapacity, totalMemoryCapacity int64

	// Calculate total capacity
	for _, node := range nodes {
		if !node.Spec.Unschedulable {
			totalCPUCapacity += node.Status.Allocatable.Cpu().MilliValue()
			totalMemoryCapacity += node.Status.Allocatable.Memory().Value()
		}
	}

	var cpuUsage, memoryUsage int64

	// Get metrics for nodes
	nodeMetrics, err := c.metricsClient.MetricsV1beta1().NodeMetricses().List(ctx, metav1.ListOptions{})
	if err != nil {
		c.logger.Error(err, "Failed to get node metrics")
	} else {
		for _, metric := range nodeMetrics.Items {
			cpuUsage += metric.Usage.Cpu().MilliValue()
			memoryUsage += metric.Usage.Memory().Value()
		}
	}

	return totalCPUCapacity, totalMemoryCapacity, cpuUsage, memoryUsage
}

// getWorkloadCount counts the total number of workloads across all namespaces
func (c *ClusterCollector) getWorkloadCount(ctx context.Context) int {
	deployments, err := c.k8sClient.AppsV1().Deployments("").List(ctx, metav1.ListOptions{})
	if err != nil {
		c.logger.Error(err, "Failed to count deployments")
	}

	statefulSets, err := c.k8sClient.AppsV1().StatefulSets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		c.logger.Error(err, "Failed to count statefulsets")
	}

	daemonSets, err := c.k8sClient.AppsV1().DaemonSets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		c.logger.Error(err, "Failed to count daemonsets")
	}

	cronJobs, err := c.k8sClient.BatchV1().CronJobs("").List(ctx, metav1.ListOptions{})
	if err != nil {
		c.logger.Error(err, "Failed to count cronjobs")
	}

	return len(deployments.Items) + len(statefulSets.Items) + len(daemonSets.Items) + len(cronJobs.Items)
}

// detectCNIPlugins attempts to identify CNI plugins used in the cluster
func (c *ClusterCollector) detectCNIPlugins(ctx context.Context) []string {
	// TODO: This is clearly a simplified version - a more robust implementation would check multiple sources

	// Common CNI plugins and their detection patterns
	cniPatterns := map[string][]string{
		"calico": {
			"calico",
			"projectcalico.org",
		},
		"cilium": {
			"cilium",
		},
		"flannel": {
			"flannel",
			"coreos.com/flannel",
		},
		"weave": {
			"weave",
			"weaveworks",
		},
		"aws-cni": {
			"aws-node",
			"amazon-k8s-cni",
		},
		"azure-cni": {
			"azure-cni",
		},
		"gke-cni": {
			"gke-node",
		},
	}

	// Check for pods in kube-system namespace that match CNI patterns
	pods, err := c.k8sClient.CoreV1().Pods(KUBE_SYSTEM_NS).List(ctx, metav1.ListOptions{})
	if err != nil {
		c.logger.Error(err, "Failed to list kube-system pods for CNI detection")
		return []string{"unknown"}
	}

	detectedCNIs := make(map[string]bool)

	// Check pod names and labels for CNI patterns
	for _, pod := range pods.Items {
		for cni, patterns := range cniPatterns {
			for _, pattern := range patterns {
				if containsString(pod.Name, pattern) {
					detectedCNIs[cni] = true
					break
				}

				// Check labels
				for _, labelValue := range pod.Labels {
					if containsString(labelValue, pattern) {
						detectedCNIs[cni] = true
						break
					}
				}
			}
		}
	}

	// Convert map to slice
	var cniSlice []string
	for cni := range detectedCNIs {
		cniSlice = append(cniSlice, cni)
	}

	// If no CNI detected, return unknown
	if len(cniSlice) == 0 {
		return []string{"unknown"}
	}

	return cniSlice
}

// getClusterAPIEndpoint tries to determine the Kubernetes API endpoint
func (c *ClusterCollector) getClusterAPIEndpoint(ctx context.Context) string {
	// Try to get from kubeadm ConfigMap
	configMap, err := c.k8sClient.CoreV1().ConfigMaps("kube-system").Get(ctx, "kubeadm-config", metav1.GetOptions{})
	if err == nil {
		if clusterConfig, ok := configMap.Data["ClusterConfiguration"]; ok {
			// Very simple extraction - in a real implementation you'd parse the YAML
			if !containsString(clusterConfig, "controlPlaneEndpoint:") {
				// Extract the endpoint
				lines := strings.Split(clusterConfig, "\n")
				for _, line := range lines {
					if strings.Contains(line, "controlPlaneEndpoint:") {
						parts := strings.SplitN(line, ":", 2)
						if len(parts) > 1 {
							return strings.TrimSpace(parts[1])
						}
					}
				}
			}
		}
	}

	// Fallback: try to get from environment or service
	svc, err := c.k8sClient.CoreV1().Services("default").Get(ctx, "kubernetes", metav1.GetOptions{})
	if err == nil {
		if len(svc.Spec.Ports) > 0 {
			port := svc.Spec.Ports[0].Port
			if len(svc.Spec.ClusterIP) > 0 {
				return fmt.Sprintf("https://%s:%d", svc.Spec.ClusterIP, port)
			}
		}
	}

	// Last resort: use master node if available
	nodes, err := c.k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: "node-role.kubernetes.io/master=,node-role.kubernetes.io/control-plane=",
	})
	if err == nil && len(nodes.Items) > 0 {
		for _, addr := range nodes.Items[0].Status.Addresses {
			if addr.Type == corev1.NodeInternalIP {
				return fmt.Sprintf("https://%s:6443", addr.Address)
			}
		}
	}

	return "unknown"
}

// Stop gracefully shuts down the collector
func (c *ClusterCollector) Stop() error {
	c.logger.Info("Stopping cluster collector")

	if c.ticker != nil {
		c.ticker.Stop()
	}

	if c.stopCh != nil {
		close(c.stopCh)
		c.stopCh = nil
	}
	return nil
}

// GetResourceChannel returns the channel for collected resources
func (c *ClusterCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *ClusterCollector) GetType() string {
	return "cluster"
}

// IsAvailable returns true if the collector is available
func (c *ClusterCollector) IsAvailable(ctx context.Context) bool {
	return true
}

// Helper functions
func calculatePercentage(used, total int64) float64 {
	if total == 0 {
		return 0
	}
	return float64(used) / float64(total) * 100
}

func containsString(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}
