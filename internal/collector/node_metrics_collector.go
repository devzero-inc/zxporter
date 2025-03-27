// internal/collector/node_metrics_collector.go
package collector

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// NodeMetricsCollector collects node metrics from node-exporter
type NodeMetricsCollector struct {
	k8sClient      kubernetes.Interface
	resourceChan   chan CollectedResource
	stopCh         chan struct{}
	ticker         *time.Ticker
	updateInterval time.Duration
	excludedNodes  map[string]bool
	nodePort       int
	logger         logr.Logger
	mu             sync.RWMutex
}

// NodeMetrics struct to store node level metrics
type NodeMetrics struct {
	NodeName         string
	Timestamp        time.Time
	CPUUsagePercent  float64
	MemoryUsageBytes int64
	MemoryTotalBytes int64
	DiskUsedBytes    int64
	DiskTotalBytes   int64
	NetworkRxBytes   int64
	NetworkTxBytes   int64
	LoadAvg1         float64
	LoadAvg5         float64
	LoadAvg15        float64
	SystemUptimeSecs int64
	RunningProcesses int64
	ContextSwitches  int64
	Interrupts       int64
	FilesystemInfo   []FilesystemMetrics
}

// FilesystemMetrics contains filesystem specific metrics
type FilesystemMetrics struct {
	Mountpoint string
	SizeBytes  int64
	UsedBytes  int64
	FsType     string
}

// NewNodeMetricsCollector creates a new collector for node metrics
func NewNodeMetricsCollector(
	k8sClient kubernetes.Interface,
	excludedNodes []string,
	updateInterval time.Duration,
	logger logr.Logger,
) *NodeMetricsCollector {
	// Convert excluded nodes to a map for quicker lookups
	excludedNodesMap := make(map[string]bool)
	for _, node := range excludedNodes {
		excludedNodesMap[node] = true
	}

	// Default update interval if not specified
	if updateInterval <= 0 {
		updateInterval = 60 * time.Second // Default to 1 minute
	}

	return &NodeMetricsCollector{
		k8sClient:      k8sClient,
		resourceChan:   make(chan CollectedResource, 100),
		stopCh:         make(chan struct{}),
		updateInterval: updateInterval,
		excludedNodes:  excludedNodesMap,
		nodePort:       9100, // Default node-exporter port
		logger:         logger.WithName("node-metrics-collector"),
	}
}

// Start begins the node metrics collection process
func (c *NodeMetricsCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting node metrics collector",
		"updateInterval", c.updateInterval,
		"excludedNodes", len(c.excludedNodes))

	// Start a ticker to collect metrics at regular intervals
	c.ticker = time.NewTicker(c.updateInterval)

	// Start the collection loop
	go c.collectMetricsLoop(ctx)

	return nil
}

// collectMetricsLoop collects node metrics at regular intervals
func (c *NodeMetricsCollector) collectMetricsLoop(ctx context.Context) {
	// Collect immediately on start
	if err := c.collectAllNodeMetrics(ctx); err != nil {
		c.logger.Error(err, "Failed to collect node metrics on startup")
	}

	// Then collect based on ticker
	for {
		select {
		case <-c.stopCh:
			c.logger.Info("Stopping node metrics collection loop")
			return
		case <-ctx.Done():
			c.logger.Info("Context done, stopping node metrics collection loop")
			return
		case <-c.ticker.C:
			if err := c.collectAllNodeMetrics(ctx); err != nil {
				c.logger.Error(err, "Failed to collect node metrics")
			}
		}
	}
}

// collectAllNodeMetrics collects metrics for all nodes
func (c *NodeMetricsCollector) collectAllNodeMetrics(ctx context.Context) error {
	c.logger.V(4).Info("Collecting metrics for all nodes")

	// List all nodes in the cluster
	nodeList, err := c.k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}

	c.logger.Info("Found nodes in cluster", "count", len(nodeList.Items))

	// Process each node
	for _, node := range nodeList.Items {
		nodeName := node.Name

		// Skip excluded nodes
		if c.excludedNodes[nodeName] {
			c.logger.V(4).Info("Skipping excluded node", "node", nodeName)
			continue
		}

		// Collect metrics for this node
		c.logger.V(4).Info("Collecting metrics for node", "node", nodeName)
		metrics, err := c.collectNodeMetrics(ctx, &node)
		if err != nil {
			c.logger.Error(err, "Failed to collect metrics for node", "node", nodeName)
			continue
		}

		// Send node metrics through the collector channel
		c.sendNodeMetrics(metrics, &node)
	}

	return nil
}

// collectNodeMetrics collects metrics for a single node
func (c *NodeMetricsCollector) collectNodeMetrics(ctx context.Context, node *corev1.Node) (*NodeMetrics, error) {
	var nodeIP string
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			nodeIP = addr.Address
			break
		}
	}

	if nodeIP == "" {
		return nil, fmt.Errorf("could not find internal IP for node %s", node.Name)
	}

	c.logger.Info("Collecting metrics from node exporter",
		"node", node.Name,
		"ip", nodeIP,
		"port", c.nodePort)

	// Get node metrics directly from node-exporter with timeout
	httpClient := &http.Client{
		Timeout: 5 * time.Second,
	}

	url := fmt.Sprintf("http://%s:%d/metrics", nodeIP, c.nodePort)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		c.logger.Error(err, "Failed to create request to node exporter")
		return c.fallbackMetrics(node)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		c.logger.Error(err, "Failed to connect to node exporter endpoint",
			"node", node.Name,
			"url", url)
		return c.fallbackMetrics(node)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.logger.Error(fmt.Errorf("HTTP status %d", resp.StatusCode), "Bad status from node exporter",
			"node", node.Name,
			"url", url)
		return c.fallbackMetrics(node)
	}

	// Parse metrics
	metrics, err := c.collectNodeExporterMetrics(resp)
	if err != nil {
		c.logger.Error(err, "Failed to parse node exporter metrics", "node", node.Name)
		return c.fallbackMetrics(node)
	}

	metrics.NodeName = node.Name
	c.logNodeMetrics(metrics)

	return metrics, nil
}

// sendNodeMetrics formats and sends node metrics through the resource channel
func (c *NodeMetricsCollector) sendNodeMetrics(metrics *NodeMetrics, node *corev1.Node) {
	// Extract CPU capacity if available
	var cpuCapacity int64 = 0
	if cpuQuantity := node.Status.Capacity.Cpu(); cpuQuantity != nil {
		cpuCapacity = cpuQuantity.Value()
	}

	// Create resource data with node metrics
	resourceData := map[string]interface{}{
		// Node identification
		"dz_node_id":                  node.Name,
		"node_status_capacity_cpu":    cpuCapacity,
		"node_status_capacity_memory": metrics.MemoryTotalBytes,
		"node_status_capacity_pods":   node.Status.Capacity.Pods().Value(),

		// Allocatable resources
		"node_status_allocatable_cpu":    node.Status.Allocatable.Cpu().Value(),
		"node_status_allocatable_memory": node.Status.Allocatable.Memory().Value(),
		"node_status_allocatable_pods":   node.Status.Allocatable.Pods().Value(),

		// Current usage metrics
		"node_cpu_seconds_total":            metrics.CPUUsagePercent * float64(cpuCapacity) / 100.0,
		"node_memory_memavailable_bytes":    metrics.MemoryTotalBytes - metrics.MemoryUsageBytes,
		"node_filesystem_avail_bytes":       metrics.DiskTotalBytes - metrics.DiskUsedBytes,
		"node_network_receive_bytes_total":  metrics.NetworkRxBytes,
		"node_network_transmit_bytes_total": metrics.NetworkTxBytes,

		// Load metrics
		"node_load_avg1":  metrics.LoadAvg1,
		"node_load_avg5":  metrics.LoadAvg5,
		"node_load_avg15": metrics.LoadAvg15,

		// System metrics
		"node_procs_running":          metrics.RunningProcesses,
		"node_context_switches_total": metrics.ContextSwitches,
		"node_interrupts_total":       metrics.Interrupts,

		// Timestamp
		"ts": metrics.Timestamp,

		// Include the full node object for reference
		"node": node,
	}

	// Add filesystem info
	if len(metrics.FilesystemInfo) > 0 {
		// We could take the main filesystem or just the first one
		mainFS := metrics.FilesystemInfo[0]
		resourceData["node_filesystem_size_bytes"] = mainFS.SizeBytes
		resourceData["node_filesystem_used_bytes"] = mainFS.UsedBytes
	}

	// Include node runtime info if available
	if node.Status.NodeInfo.ContainerRuntimeVersion != "" {
		resourceData["node_status_nodeinfo_container_runtime_version"] = node.Status.NodeInfo.ContainerRuntimeVersion
	}
	if node.Status.NodeInfo.KubeletVersion != "" {
		resourceData["node_status_nodeinfo_kubelet_version"] = node.Status.NodeInfo.KubeletVersion
	}
	if node.Status.NodeInfo.OSImage != "" {
		resourceData["node_status_nodeinfo_os_image"] = node.Status.NodeInfo.OSImage
	}
	if node.Status.NodeInfo.KernelVersion != "" {
		resourceData["node_status_nodeinfo_kernel_version"] = node.Status.NodeInfo.KernelVersion
	}

	// Send the resource data to the channel
	c.resourceChan <- CollectedResource{
		ResourceType: NodeResource,
		Object:       resourceData,
		Timestamp:    metrics.Timestamp,
		EventType:    "metrics",
		Key:          node.Name,
	}
}

// Stop gracefully stops the collector
func (c *NodeMetricsCollector) Stop() error {
	c.logger.Info("Stopping node metrics collector")

	if c.ticker != nil {
		c.ticker.Stop()
	}

	close(c.stopCh)
	return nil
}

// GetResourceChannel returns the channel for receiving collected resources
func (c *NodeMetricsCollector) GetResourceChannel() <-chan CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *NodeMetricsCollector) GetType() string {
	return "node_resources"
}

// collectNodeExporterMetrics parses metrics from a node-exporter HTTP response
func (c *NodeMetricsCollector) collectNodeExporterMetrics(resp *http.Response) (*NodeMetrics, error) {
	// Initialize metrics object
	metrics := &NodeMetrics{
		Timestamp:      time.Now(),
		FilesystemInfo: []FilesystemMetrics{},
	}

	// Storage for aggregated values
	cpuIdleValues := make(map[string]float64)
	cpuTotalValues := make(map[string]float64)
	networkRxBytes := make(map[string]float64)
	networkTxBytes := make(map[string]float64)

	// Maps to track filesystem metrics
	deviceSizeByMount := make(map[string]map[string]float64)  // device -> mountpoint -> size
	deviceAvailByMount := make(map[string]map[string]float64) // device -> mountpoint -> avail
	deviceFsType := make(map[string]string)                   // device -> fstype

	var memoryTotal, memoryAvailable int64

	// Process the metrics
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()

		// Skip comments and empty lines
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}

		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}

		metricName := parts[0]
		valueStr := parts[len(parts)-1] // Take the last field as the value
		value, err := strconv.ParseFloat(valueStr, 64)
		if err != nil {
			// Skip lines with invalid values
			continue
		}

		// CPU seconds - use string.Contains for more robust matching
		if strings.Contains(metricName, "node_cpu_seconds_total") {
			// Extract CPU ID and mode from the metric name
			cpuMatch := regexp.MustCompile(`cpu="([^"]+)"`).FindStringSubmatch(metricName)
			modeMatch := regexp.MustCompile(`mode="([^"]+)"`).FindStringSubmatch(metricName)

			if cpuMatch != nil && modeMatch != nil {
				cpuID := cpuMatch[1]
				mode := modeMatch[1]

				// Track total CPU time
				cpuTotalValues[cpuID] += value

				// Track idle CPU time
				if mode == "idle" {
					cpuIdleValues[cpuID] = value
				}
			}
			continue
		}

		if metricName == "node_memory_MemTotal_bytes" {
			memoryTotal = int64(value)
			continue
		}

		if metricName == "node_memory_MemAvailable_bytes" {
			memoryAvailable = int64(value)
			continue
		}

		// Load averages
		if metricName == "node_load1" {
			metrics.LoadAvg1 = value
			continue
		}
		if metricName == "node_load5" {
			metrics.LoadAvg5 = value
			continue
		}
		if metricName == "node_load15" {
			metrics.LoadAvg15 = value
			continue
		}

		// Running processes
		if metricName == "node_procs_running" {
			metrics.RunningProcesses = int64(value)
			continue
		}

		// Context switches
		if metricName == "node_context_switches_total" {
			metrics.ContextSwitches = int64(value)
			continue
		}

		// Interrupts
		if metricName == "node_intr_total" {
			metrics.Interrupts = int64(value)
			continue
		}

		// Boot time (to calculate uptime)
		if metricName == "node_boot_time_seconds" {
			bootTime := int64(value)
			metrics.SystemUptimeSecs = time.Now().Unix() - bootTime
			continue
		}

		// Filesystem size
		if strings.Contains(metricName, "node_filesystem_size_bytes") {
			deviceMatch := regexp.MustCompile(`device="([^"]+)"`).FindStringSubmatch(metricName)
			fstypeMatch := regexp.MustCompile(`fstype="([^"]+)"`).FindStringSubmatch(metricName)
			mountpointMatch := regexp.MustCompile(`mountpoint="([^"]+)"`).FindStringSubmatch(metricName)

			if deviceMatch != nil && fstypeMatch != nil && mountpointMatch != nil {
				device := deviceMatch[1]
				fstype := fstypeMatch[1]
				mountpoint := mountpointMatch[1]

				// Skip special filesystems and mountpoints
				if c.isSpecialFs(fstype) || c.isSpecialMount(mountpoint) {
					continue
				}

				// Store filesystem type
				deviceFsType[device] = fstype

				// Store size for this device and mountpoint
				if _, exists := deviceSizeByMount[device]; !exists {
					deviceSizeByMount[device] = make(map[string]float64)
				}
				deviceSizeByMount[device][mountpoint] = value
			}
			continue
		}

		// Filesystem available space
		if strings.Contains(metricName, "node_filesystem_avail_bytes") {
			deviceMatch := regexp.MustCompile(`device="([^"]+)"`).FindStringSubmatch(metricName)
			mountpointMatch := regexp.MustCompile(`mountpoint="([^"]+)"`).FindStringSubmatch(metricName)

			if deviceMatch != nil && mountpointMatch != nil {
				device := deviceMatch[1]
				mountpoint := mountpointMatch[1]

				// Skip if we're not tracking this device
				if _, exists := deviceSizeByMount[device]; !exists {
					continue
				}

				// Skip if we're not tracking this mountpoint for this device
				if _, exists := deviceSizeByMount[device][mountpoint]; !exists {
					continue
				}

				// Store available space for this device and mountpoint
				if _, exists := deviceAvailByMount[device]; !exists {
					deviceAvailByMount[device] = make(map[string]float64)
				}
				deviceAvailByMount[device][mountpoint] = value
			}
			continue
		}

		// Network receive bytes
		if strings.Contains(metricName, "node_network_receive_bytes_total") {
			deviceMatch := regexp.MustCompile(`device="([^"]+)"`).FindStringSubmatch(metricName)

			if deviceMatch != nil {
				device := deviceMatch[1]
				if !c.isLoopbackOrVirtual(device) {
					networkRxBytes[device] = value
				}
			}
			continue
		}

		// Network transmit bytes
		if strings.Contains(metricName, "node_network_transmit_bytes_total") {
			deviceMatch := regexp.MustCompile(`device="([^"]+)"`).FindStringSubmatch(metricName)

			if deviceMatch != nil {
				device := deviceMatch[1]
				if !c.isLoopbackOrVirtual(device) {
					networkTxBytes[device] = value
				}
			}
			continue
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error scanning metrics: %v", err)
	}

	// Set memory metrics
	if memoryTotal > 0 && memoryAvailable > 0 {
		metrics.MemoryTotalBytes = memoryTotal
		metrics.MemoryUsageBytes = memoryTotal - memoryAvailable
	}

	// Safety check for invalid memory values
	if metrics.MemoryTotalBytes <= 0 {
		// Set to a reasonable default to avoid division by zero
		metrics.MemoryTotalBytes = 1
	}
	if metrics.MemoryUsageBytes < 0 {
		metrics.MemoryUsageBytes = 0
	}

	// Calculate CPU usage percentage
	totalCPUCount := len(cpuIdleValues)
	if totalCPUCount > 0 {
		cpuUsagePercentSum := 0.0
		for cpuID, idleValue := range cpuIdleValues {
			totalValue := cpuTotalValues[cpuID]
			if totalValue > 0 {
				cpuUsagePercent := 100.0 * (1.0 - idleValue/totalValue)
				cpuUsagePercentSum += cpuUsagePercent
			}
		}
		metrics.CPUUsagePercent = cpuUsagePercentSum / float64(totalCPUCount)
	}

	// Process filesystem information - only include real filesystems
	for device, mountpoints := range deviceSizeByMount {
		// Skip special devices like tmpfs, shm, etc.
		if strings.HasPrefix(device, "tmpfs") || strings.HasPrefix(device, "shm") {
			continue
		}

		// Choose the best mountpoint for this device
		var bestMountpoint string
		var bestScore int

		for mountpoint := range mountpoints {
			score := 0

			if mountpoint == "/" {
				score = 100 // Root is best
			} else if mountpoint == "/var" {
				score = 90 // /var is second best
			} else if mountpoint == "/etc" {
				score = 80 // /etc is okay
			} else if strings.HasPrefix(mountpoint, "/etc/") {
				score = 70 // Specific files are less preferred
			} else {
				score = 50 // Other mountpoints
			}

			if score > bestScore {
				bestScore = score
				bestMountpoint = mountpoint
			}
		}

		// If we found a good mountpoint
		if bestMountpoint != "" {
			size := mountpoints[bestMountpoint]

			// Get available space (if known)
			avail, availExists := deviceAvailByMount[device][bestMountpoint]
			if !availExists {
				// Skip if we don't have availability information
				continue
			}

			// Calculate used space
			used := size - avail
			if used < 0 {
				used = 0
			}

			// Get fstype
			fstype := deviceFsType[device]

			// Add to metrics
			metrics.FilesystemInfo = append(metrics.FilesystemInfo, FilesystemMetrics{
				Mountpoint: bestMountpoint,
				SizeBytes:  int64(size),
				UsedBytes:  int64(used),
				FsType:     fstype,
			})

			// Add to totals
			metrics.DiskTotalBytes += int64(size)
			metrics.DiskUsedBytes += int64(used)
		}
	}

	// Sum up network metrics
	for _, rxBytes := range networkRxBytes {
		metrics.NetworkRxBytes += int64(rxBytes)
	}

	for _, txBytes := range networkTxBytes {
		metrics.NetworkTxBytes += int64(txBytes)
	}

	// Safety check for uptime
	if metrics.SystemUptimeSecs < 0 || metrics.SystemUptimeSecs > 31536000 { // Max 1 year
		// Reasonable default if uptime calculation is wrong
		metrics.SystemUptimeSecs = 0
	}

	return metrics, nil
}

// TODO: I take a lot help from AI to get these data correctly, we have to know how vm works on this level

// Check if network interface is loopback or virtual
func (c *NodeMetricsCollector) isLoopbackOrVirtual(device string) bool {
	return device == "lo" ||
		strings.HasPrefix(device, "veth") ||
		strings.HasPrefix(device, "docker") ||
		strings.HasPrefix(device, "br-") ||
		strings.HasPrefix(device, "virbr") ||
		strings.HasPrefix(device, "tunl") ||
		strings.HasPrefix(device, "erspan") ||
		strings.HasPrefix(device, "gre") ||
		strings.HasPrefix(device, "ip") ||
		strings.HasPrefix(device, "sit")
}

// Check if filesystem type is special/virtual
func (c *NodeMetricsCollector) isSpecialFs(fstype string) bool {
	specialTypes := []string{
		"tmpfs", "devtmpfs", "devfs", "iso9660", "overlay", "aufs", "squashfs",
		"configfs", "debugfs", "tracefs", "securityfs", "sysfs", "proc", "cgroup",
		"cgroup2", "pstore", "bpf", "hugetlbfs", "mqueue", "fusectl", "binfmt_misc",
		"efivarfs", "fuse", "nsfs", "shm", "ramfs",
	}
	for _, specialType := range specialTypes {
		if fstype == specialType || fstype == "" {
			return true
		}
	}
	return fstype == "erofs" || fstype == "tmpfs"
}

// Check if mountpoint is special or temporary
func (c *NodeMetricsCollector) isSpecialMount(mountpoint string) bool {
	return strings.HasPrefix(mountpoint, "/run") ||
		strings.HasPrefix(mountpoint, "/sys") ||
		strings.HasPrefix(mountpoint, "/proc") ||
		strings.HasPrefix(mountpoint, "/dev") ||
		strings.Contains(mountpoint, "containerd") ||
		strings.Contains(mountpoint, "kubelet") ||
		strings.Contains(mountpoint, "kubernetes.io") ||
		strings.Contains(mountpoint, "docker")
}

// logNodeMetrics logs collected metrics for debugging
func (c *NodeMetricsCollector) logNodeMetrics(metrics *NodeMetrics) {
	memoryTotalMB := float64(metrics.MemoryTotalBytes) / 1024 / 1024
	memoryUsedMB := float64(metrics.MemoryUsageBytes) / 1024 / 1024
	diskTotalMB := float64(metrics.DiskTotalBytes) / 1024 / 1024
	diskUsedMB := float64(metrics.DiskUsedBytes) / 1024 / 1024

	if memoryTotalMB < 0 {
		memoryTotalMB = 0
	}
	if memoryUsedMB < 0 {
		memoryUsedMB = 0
	}
	if diskTotalMB < 0 {
		diskTotalMB = 0
	}
	if diskUsedMB < 0 {
		diskUsedMB = 0
	}

	var uptimeStr string
	if metrics.SystemUptimeSecs <= 0 {
		uptimeStr = "unknown"
	} else {
		uptimeStr = fmt.Sprintf("%.2f days", float64(metrics.SystemUptimeSecs)/86400)
	}

	c.logger.Info("Complete node metrics",
		"node", metrics.NodeName,
		"timestamp", metrics.Timestamp,
		"cpu_usage_percent", fmt.Sprintf("%.2f%%", metrics.CPUUsagePercent),
		"memory_usage", fmt.Sprintf("%.2f MB / %.2f MB", memoryUsedMB, memoryTotalMB),
		"memory_usage_bytes", metrics.MemoryUsageBytes,
		"memory_total_bytes", metrics.MemoryTotalBytes,
		"disk_usage", fmt.Sprintf("%.2f MB / %.2f MB", diskUsedMB, diskTotalMB),
		"disk_used_bytes", metrics.DiskUsedBytes,
		"disk_total_bytes", metrics.DiskTotalBytes,
		"network_rx_bytes", fmt.Sprintf("%.2f MB", float64(metrics.NetworkRxBytes)/1024/1024),
		"network_tx_bytes", fmt.Sprintf("%.2f MB", float64(metrics.NetworkTxBytes)/1024/1024),
		"load_avg", fmt.Sprintf("%.2f, %.2f, %.2f", metrics.LoadAvg1, metrics.LoadAvg5, metrics.LoadAvg15),
		"uptime", uptimeStr,
		"running_processes", metrics.RunningProcesses,
		"context_switches", metrics.ContextSwitches,
		"interrupts", metrics.Interrupts,
		"filesystem_count", len(metrics.FilesystemInfo),
	)

	for i, fs := range metrics.FilesystemInfo {
		fsUsedMB := float64(fs.UsedBytes) / 1024 / 1024
		fsSizeMB := float64(fs.SizeBytes) / 1024 / 1024

		var usagePercent float64
		if fs.SizeBytes > 0 {
			usagePercent = 100.0 * float64(fs.UsedBytes) / float64(fs.SizeBytes)
		} else {
			usagePercent = 0
		}

		if fsUsedMB < 0 {
			fsUsedMB = 0
		}
		if fsSizeMB < 0 {
			fsSizeMB = 0
		}
		if usagePercent < 0 {
			usagePercent = 0
		} else if usagePercent > 100 {
			usagePercent = 100
		}

		c.logger.Info("Filesystem details",
			"node", metrics.NodeName,
			"index", i,
			"mountpoint", fs.Mountpoint,
			"fstype", fs.FsType,
			"usage", fmt.Sprintf("%.2f MB / %.2f MB", fsUsedMB, fsSizeMB),
			"usage_percent", fmt.Sprintf("%.2f%%", usagePercent),
		)
	}
}

// TODO: This is not important we ccan drop off it later

// fallbackMetrics provides basic metrics when node-exporter is unavailable
func (c *NodeMetricsCollector) fallbackMetrics(node *corev1.Node) (*NodeMetrics, error) {
	c.logger.Info("Using fallback metrics from Node object", "node", node.Name)

	metrics := &NodeMetrics{
		NodeName:         node.Name,
		Timestamp:        time.Now(),
		FilesystemInfo:   []FilesystemMetrics{},
		CPUUsagePercent:  0.0,
		LoadAvg1:         0.0,
		LoadAvg5:         0.0,
		LoadAvg15:        0.0,
		SystemUptimeSecs: 0,
		RunningProcesses: 0,
		ContextSwitches:  0,
		Interrupts:       0,
	}

	// Try to get essential metrics from the Node object directly
	if node.Status.Allocatable != nil {
		if mem := node.Status.Allocatable.Memory(); mem != nil && mem.Value() > 0 {
			metrics.MemoryTotalBytes = mem.Value()
		} else {
			metrics.MemoryTotalBytes = 8 * 1024 * 1024 * 1024 // 8 GB
		}

		if storage := node.Status.Allocatable.StorageEphemeral(); storage != nil && storage.Value() > 0 {
			metrics.DiskTotalBytes = storage.Value()
			// We don't know actual disk usage, so leave DiskUsedBytes as 0
		} else {
			metrics.DiskTotalBytes = 100 * 1024 * 1024 * 1024 // 100 GB
		}
	} else {
		metrics.MemoryTotalBytes = 8 * 1024 * 1024 * 1024 // 8 GB
		metrics.DiskTotalBytes = 100 * 1024 * 1024 * 1024 // 100 GB
	}

	// Add a placeholder filesystem for the root filesystem
	metrics.FilesystemInfo = append(metrics.FilesystemInfo, FilesystemMetrics{
		Mountpoint: "/",
		FsType:     "unknown",
		SizeBytes:  metrics.DiskTotalBytes,
		UsedBytes:  0, // Unknown usage
	})

	return metrics, nil
}
