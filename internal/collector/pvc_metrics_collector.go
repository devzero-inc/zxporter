package collector

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	telemetry_logger "github.com/devzero-inc/zxporter/internal/logger"
	"github.com/devzero-inc/zxporter/internal/version"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// PersistentVolumeClaimMetricsCollectorConfig holds configuration for the PVC metrics collector
type PersistentVolumeClaimMetricsCollectorConfig struct {
	// UpdateInterval specifies how often to collect PVC metrics
	UpdateInterval time.Duration
}

// PersistentVolumeClaimMetricsCollector collects PVC storage usage metrics
type PersistentVolumeClaimMetricsCollector struct {
	k8sClient       kubernetes.Interface
	nodemonClient   *NodemonClient
	informerFactory informers.SharedInformerFactory
	pvcInformer     cache.SharedIndexInformer
	pvInformer      cache.SharedIndexInformer
	batchChan       chan CollectedResource   // Channel for individual resources -> input to batcher
	resourceChan    chan []CollectedResource // Channel for batched resources -> output from batcher
	batcher         *ResourcesBatcher
	stopCh          chan struct{}
	ticker          *time.Ticker
	config          PersistentVolumeClaimMetricsCollectorConfig
	namespaces      []string
	excludedPVCs    map[types.NamespacedName]bool
	logger          logr.Logger
	metrics         *TelemetryMetrics
	telemetryLogger telemetry_logger.Logger
	mu              sync.RWMutex
}

// NewPersistentVolumeClaimMetricsCollector creates a new collector for PVC storage metrics
func NewPersistentVolumeClaimMetricsCollector(
	k8sClient kubernetes.Interface,
	config PersistentVolumeClaimMetricsCollectorConfig,
	namespaces []string,
	excludedPVCs []ExcludedPVC,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	metrics *TelemetryMetrics,
	telemetryLogger telemetry_logger.Logger,
) *PersistentVolumeClaimMetricsCollector {
	excludedPVCsMap := make(map[types.NamespacedName]bool)
	for _, pvc := range excludedPVCs {
		excludedPVCsMap[types.NamespacedName{
			Namespace: pvc.Namespace,
			Name:      pvc.Name,
		}] = true
	}

	if config.UpdateInterval <= 0 {
		config.UpdateInterval = 60 * time.Second
	}

	batchChan := make(chan CollectedResource, 500)
	resourceChan := make(chan []CollectedResource, 200)

	batcher := NewResourcesBatcher(
		maxBatchSize,
		maxBatchTime,
		batchChan,
		resourceChan,
		logger,
	)

	return &PersistentVolumeClaimMetricsCollector{
		k8sClient:       k8sClient,
		batchChan:       batchChan,
		resourceChan:    resourceChan,
		batcher:         batcher,
		stopCh:          make(chan struct{}),
		config:          config,
		namespaces:      namespaces,
		excludedPVCs:    excludedPVCsMap,
		logger:          logger.WithName("pvc-metrics-collector"),
		metrics:         metrics,
		telemetryLogger: telemetryLogger,
	}
}

// Start begins the PVC metrics collection process
func (c *PersistentVolumeClaimMetricsCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting PVC metrics collector",
		"namespaces", c.namespaces,
		"updateInterval", c.config.UpdateInterval)

	// Initialize nodemon client for auto-discovery
	ns := os.Getenv("POD_NAMESPACE")
	if ns == "" {
		ns = "devzero-system"
	}
	c.nodemonClient = NewNodemonClient(c.k8sClient, ns, c.logger)
	c.logger.Info("Initialized nodemon client for PVC metrics (auto-discovery)", "namespace", ns)

	if len(c.namespaces) == 1 && c.namespaces[0] != "" {
		c.informerFactory = informers.NewSharedInformerFactoryWithOptions(
			c.k8sClient,
			0, // No resync period, rely on events
			informers.WithNamespace(c.namespaces[0]),
		)
	} else {
		c.informerFactory = informers.NewSharedInformerFactory(c.k8sClient, 0)
	}

	c.pvcInformer = c.informerFactory.Core().V1().PersistentVolumeClaims().Informer()

	c.pvInformer = c.informerFactory.Core().V1().PersistentVolumes().Informer()

	c.informerFactory.Start(c.stopCh)

	c.logger.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(c.stopCh, c.pvcInformer.HasSynced, c.pvInformer.HasSynced) {
		return fmt.Errorf("timed out waiting for caches to sync")
	}
	c.logger.Info("Informer caches synced successfully")

	c.logger.Info("Starting resources batcher for PVC metrics")
	c.batcher.start()

	c.ticker = time.NewTicker(c.config.UpdateInterval)

	go c.collectMetricsLoop(ctx)

	stopCh := c.stopCh
	go func() {
		select {
		case <-ctx.Done():
			if err := c.Stop(); err != nil {
				c.logger.Error(
					err,
					"Failed to stop PVC metrics collector during context cancellation",
				)
			}
		case <-stopCh:
		}
	}()

	return nil
}

// collectMetricsLoop collects PVC metrics at regular intervals
func (c *PersistentVolumeClaimMetricsCollector) collectMetricsLoop(ctx context.Context) {
	// Collect immediately on start
	c.collectAllPVCMetrics(ctx)

	// Then collect based on ticker
	for {
		select {
		case <-c.stopCh:
			return
		case <-c.ticker.C:
			c.collectAllPVCMetrics(ctx)
		}
	}
}

// collectAllPVCMetrics collects storage metrics for all PVCs
func (c *PersistentVolumeClaimMetricsCollector) collectAllPVCMetrics(ctx context.Context) {
	startTime := time.Now()

	// Get all PVCs from the informer cache
	pvcLister := c.informerFactory.Core().V1().PersistentVolumeClaims().Lister()

	var pvcs []*corev1.PersistentVolumeClaim
	var err error

	if len(c.namespaces) == 1 && c.namespaces[0] != "" {
		// Get PVCs from specific namespace
		pvcs, err = pvcLister.PersistentVolumeClaims(c.namespaces[0]).List(labels.Everything())
		if err != nil {
			if c.telemetryLogger != nil {
				c.telemetryLogger.Report(
					gen.LogLevel_LOG_LEVEL_ERROR,
					"PVCMetricsCollector",
					"Failed to list PVCs from specific namespace",
					err,
					map[string]string{
						"namespace":        c.namespaces[0],
						"error_type":       "pvc_list_failed",
						"zxporter_version": version.Get().String(),
					},
				)
			}
			c.logger.Error(err, "Failed to list PVCs from namespace", "namespace", c.namespaces[0])
			return
		}
	} else {
		// Get PVCs from all namespaces
		if len(c.namespaces) > 0 && c.namespaces[0] != "" {
			for _, ns := range c.namespaces {
				nsPVCs, err := pvcLister.PersistentVolumeClaims(ns).List(labels.Everything())
				if err != nil {
					if c.telemetryLogger != nil {
						c.telemetryLogger.Report(
							gen.LogLevel_LOG_LEVEL_ERROR,
							"PVCMetricsCollector",
							"Failed to list PVCs from namespace",
							err,
							map[string]string{
								"namespace":        ns,
								"error_type":       "pvc_list_failed",
								"zxporter_version": version.Get().String(),
							},
						)
					}
					c.logger.Error(err, "Failed to list PVCs from namespace", "namespace", ns)
					continue
				}
				pvcs = append(pvcs, nsPVCs...)
			}
		} else {
			// All namespaces
			pvcs, err = pvcLister.List(labels.Everything())
			if err != nil {
				if c.telemetryLogger != nil {
					c.telemetryLogger.Report(
						gen.LogLevel_LOG_LEVEL_ERROR,
						"PVCMetricsCollector",
						"Failed to list PVCs from all namespaces",
						err,
						map[string]string{
							"namespaces":       "all",
							"error_type":       "pvc_list_failed",
							"zxporter_version": version.Get().String(),
						},
					)
				}
				c.logger.Error(err, "Failed to list PVCs from all namespaces")
				return
			}
		}
	}

	if c.telemetryLogger != nil {
		c.telemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_INFO,
			"PVCMetricsCollector",
			"Successfully retrieved PVCs from cache",
			nil,
			map[string]string{
				"pvc_total":        fmt.Sprintf("%d", len(pvcs)),
				"namespaces":       fmt.Sprintf("%v", c.namespaces),
				"event_type":       "pvc_list_success",
				"zxporter_version": version.Get().String(),
			},
		)
	}

	c.logger.Info("Collecting metrics for PVCs",
		"pvc_total", len(pvcs),
		"namespaces", c.namespaces)

	// Track collection stats
	processedCount := 0
	skippedCount := 0
	errorCount := 0
	metricsEmittedCount := 0
	statsUnavailableCount := 0

	// Process each PVC , one failure doesn't stop the loop
	for _, pvc := range pvcs {
		if c.isExcluded(pvc) {
			skippedCount++
			continue
		}

		// Skip PVCs that aren't bound yet
		if pvc.Status.Phase != corev1.ClaimBound {
			c.logger.V(1).Info("Skipping PVC - not bound",
				"namespace", pvc.Namespace,
				"name", pvc.Name,
				"phase", pvc.Status.Phase)
			skippedCount++
			continue
		}

		// Process the PVC metrics
		snapshot, err := c.processPVCMetrics(ctx, pvc)
		if err != nil {
			errorCount++
			c.logger.Error(err, "Failed to process PVC metrics (continuing with next PVC)",
				"namespace", pvc.Namespace,
				"name", pvc.Name)
			continue
		}

		processedCount++
		metricsEmittedCount++

		if snapshot != nil && !snapshot.StatsAvailable {
			statsUnavailableCount++
		}
	}

	tickDuration := time.Since(startTime)

	c.logger.Info("PVC metrics collection completed",
		"pvc_total", len(pvcs),
		"processed", processedCount,
		"skipped", skippedCount,
		"errors", errorCount,
		"pvc_metrics_emitted", metricsEmittedCount,
		"pvc_stats_unavailable", statsUnavailableCount,
		"tick_duration_ms", tickDuration.Milliseconds())

	if c.telemetryLogger != nil {
		c.telemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_INFO,
			"PVCMetricsCollector",
			"PVC metrics collection cycle completed",
			nil,
			map[string]string{
				"pvc_total":             fmt.Sprintf("%d", len(pvcs)),
				"processed_count":       fmt.Sprintf("%d", processedCount),
				"skipped_count":         fmt.Sprintf("%d", skippedCount),
				"error_count":           fmt.Sprintf("%d", errorCount),
				"pvc_metrics_emitted":   fmt.Sprintf("%d", metricsEmittedCount),
				"pvc_stats_unavailable": fmt.Sprintf("%d", statsUnavailableCount),
				"tick_duration_ms":      fmt.Sprintf("%d", tickDuration.Milliseconds()),
				"event_type":            "collection_cycle_complete",
				"zxporter_version":      version.Get().String(),
			},
		)
	}
}

// processPVCMetrics processes metrics for a single PVC
// Returns the snapshot and an error if metrics collection fails
func (c *PersistentVolumeClaimMetricsCollector) processPVCMetrics(
	ctx context.Context,
	pvc *corev1.PersistentVolumeClaim,
) (*PersistentVolumeClaimMetricsSnapshot, error) {
	c.logger.V(1).Info("Processing PVC metrics",
		"namespace", pvc.Namespace,
		"name", pvc.Name,
		"pvcUID", pvc.UID,
		"pvName", pvc.Spec.VolumeName)

	pvName := pvc.Spec.VolumeName
	var pv *corev1.PersistentVolume
	if pvName != "" {
		pvLister := c.informerFactory.Core().V1().PersistentVolumes().Lister()
		var err error
		pv, err = pvLister.Get(pvName)
		if err != nil {
			c.logger.V(1).Info("Could not get PV from cache (continuing without PV metadata)",
				"pvName", pvName,
				"error", err)
		}
	}

	metricsSnapshot := &PersistentVolumeClaimMetricsSnapshot{
		PvcName:   pvc.Name,
		Namespace: pvc.Namespace,
		PvcUID:    string(pvc.UID),

		PvName:           pvName,
		StorageClassName: getStorageClassName(pvc),
		VolumeMode:       getVolumeMode(pvc),

		AccessModes:    getAccessModes(pvc),
		RequestedBytes: getRequestedBytes(pvc),

		CapacityBytes:  getCapacityBytes(pvc),
		UsedBytes:      0,
		AvailableBytes: 0,
		UtilizationPct: 0.0,

		StatsAvailable:    false,
		StatsSource:       "unknown",
		UnavailableReason: "",
	}

	if pvc.Spec.VolumeMode != nil && *pvc.Spec.VolumeMode == corev1.PersistentVolumeBlock {
		metricsSnapshot.UnavailableReason = "block_volume"
		c.logger.V(1).Info("Skipping block volume PVC (filesystem metrics not applicable)",
			"namespace", pvc.Namespace,
			"name", pvc.Name)
		c.emitSnapshot(pvc, metricsSnapshot)
		return metricsSnapshot, nil
	}

	usage, err := c.getFilesystemUsage(ctx, pvc)
	if err != nil {
		c.logger.V(1).Info("Failed to get filesystem usage from nodemon",
			"namespace", pvc.Namespace,
			"name", pvc.Name,
			"error", err)
		metricsSnapshot.UnavailableReason = fmt.Sprintf("nodemon_query_failed: %v", err)

		if c.telemetryLogger != nil {
			c.telemetryLogger.Report(
				gen.LogLevel_LOG_LEVEL_WARN,
				"PVCMetricsCollector",
				"Failed to collect PVC metrics from nodemon",
				err,
				map[string]string{
					"namespace":        pvc.Namespace,
					"pvc":              pvc.Name,
					"error_type":       "nodemon_pvc_query_failed",
					"zxporter_version": version.Get().String(),
				},
			)
		}
	} else if usage != nil {
		metricsSnapshot.StatsAvailable = true
		metricsSnapshot.StatsSource = "nodemon"
		metricsSnapshot.UsedBytes = usage.UsedBytes
		metricsSnapshot.CapacityBytes = usage.CapacityBytes
		metricsSnapshot.AvailableBytes = usage.AvailableBytes

		if usage.CapacityBytes > 0 {
			metricsSnapshot.UtilizationPct = (float64(usage.UsedBytes) / float64(usage.CapacityBytes)) * 100.0
		}

		c.logger.V(1).Info("Successfully collected PVC filesystem usage",
			"namespace", pvc.Namespace,
			"name", pvc.Name,
			"usedBytes", usage.UsedBytes,
			"capacityBytes", usage.CapacityBytes,
			"utilizationPct", metricsSnapshot.UtilizationPct)
	} else {
		metricsSnapshot.UnavailableReason = "no_metrics_available"
	}

	c.emitSnapshot(pvc, metricsSnapshot)

	_ = pv // Suppress unused warning (PV metadata could be used for future enrichment)

	return metricsSnapshot, nil
}

// filesystemUsage holds the usage stats for a PVC
type filesystemUsage struct {
	UsedBytes      int64
	CapacityBytes  int64
	AvailableBytes int64
}

// getFilesystemUsage retrieves PVC filesystem usage from the nodemon DaemonSet.
func (c *PersistentVolumeClaimMetricsCollector) getFilesystemUsage(
	ctx context.Context,
	pvc *corev1.PersistentVolumeClaim,
) (*filesystemUsage, error) {
	allMetrics, err := c.nodemonClient.FetchAllPVCMetrics(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetching PVC metrics from nodemon: %w", err)
	}

	for _, m := range allMetrics {
		if m.Namespace == pvc.Namespace && m.PVCName == pvc.Name {
			return &filesystemUsage{
				UsedBytes:      int64(m.UsedBytes),
				CapacityBytes:  int64(m.CapacityBytes),
				AvailableBytes: int64(m.AvailableBytes),
			}, nil
		}
	}

	return nil, fmt.Errorf("no nodemon metrics found for PVC %s/%s", pvc.Namespace, pvc.Name)
}

// emitSnapshot sends the metrics snapshot to the batch channel
func (c *PersistentVolumeClaimMetricsCollector) emitSnapshot(
	pvc *corev1.PersistentVolumeClaim,
	snapshot *PersistentVolumeClaimMetricsSnapshot,
) {
	pvcKey := fmt.Sprintf("pvc/%s/%s", pvc.Namespace, pvc.Name)

	c.logger.V(1).Info("Emitting PVC metrics snapshot",
		"key", pvcKey,
		"pvcUID", pvc.UID,
		"statsAvailable", snapshot.StatsAvailable,
		"statsSource", snapshot.StatsSource,
		"capacityBytes", snapshot.CapacityBytes,
		"usedBytes", snapshot.UsedBytes,
		"utilizationPct", snapshot.UtilizationPct)

	c.batchChan <- CollectedResource{
		ResourceType: PersistentVolumeClaimMetrics,
		Object:       snapshot,
		Timestamp:    time.Now(),
		EventType:    EventTypeMetrics,
		Key:          pvcKey,
	}
}

func getStorageClassName(pvc *corev1.PersistentVolumeClaim) string {
	if pvc.Spec.StorageClassName != nil {
		return *pvc.Spec.StorageClassName
	}
	return ""
}

func getVolumeMode(pvc *corev1.PersistentVolumeClaim) string {
	if pvc.Spec.VolumeMode != nil {
		return string(*pvc.Spec.VolumeMode)
	}
	return string(corev1.PersistentVolumeFilesystem)
}

func getCapacityBytes(pvc *corev1.PersistentVolumeClaim) int64 {
	if capacity, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
		return capacity.Value()
	}
	return 0
}

func getAccessModes(pvc *corev1.PersistentVolumeClaim) []string {
	modes := make([]string, len(pvc.Spec.AccessModes))
	for i, mode := range pvc.Spec.AccessModes {
		modes[i] = string(mode)
	}
	return modes
}

func getRequestedBytes(pvc *corev1.PersistentVolumeClaim) int64 {
	if req, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
		return req.Value()
	}
	return 0
}

// isExcluded checks if a PVC should be excluded from collection
func (c *PersistentVolumeClaimMetricsCollector) isExcluded(pvc *corev1.PersistentVolumeClaim) bool {
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		found := false
		for _, ns := range c.namespaces {
			if ns == pvc.Namespace {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	key := types.NamespacedName{
		Namespace: pvc.Namespace,
		Name:      pvc.Name,
	}
	return c.excludedPVCs[key]
}

// Stop gracefully shuts down the PVC metrics collector
func (c *PersistentVolumeClaimMetricsCollector) Stop() error {
	c.logger.Info("Stopping PVC metrics collector")

	if c.ticker != nil {
		c.ticker.Stop()
		c.logger.Info("Stopped PVC metrics collector ticker")
	}

	select {
	case <-c.stopCh:
		c.logger.Info("PVC metrics collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed PVC metrics collector stop channel")
	}

	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
		c.logger.Info("Closed PVC metrics collector batch input channel")
	}

	if c.batcher != nil {
		c.batcher.stop()
		c.logger.Info("PVC metrics collector batcher stopped")
	}

	return nil
}

func (c *PersistentVolumeClaimMetricsCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

func (c *PersistentVolumeClaimMetricsCollector) GetType() string {
	return "pvc_metrics"
}

func (c *PersistentVolumeClaimMetricsCollector) IsAvailable(ctx context.Context) bool {
	return c.nodemonClient != nil && c.nodemonClient.HasExporters(ctx)
}

// AddResource is a no-op for PVC metrics collector
func (c *PersistentVolumeClaimMetricsCollector) AddResource(resource interface{}) error {
	// PVC metrics are collected automatically via polling, not via individual resource refresh
	return nil
}
