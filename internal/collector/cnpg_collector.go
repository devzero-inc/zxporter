package collector

import (
	"context"
	"fmt"
	"sync"
	"time"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	telemetry_logger "github.com/devzero-inc/zxporter/internal/logger"
	"github.com/devzero-inc/zxporter/internal/version"
	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/api"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	"k8s.io/apimachinery/pkg/types"
)

// CNPGCollectorConfig holds configuration for the CloudNativePG metrics collector
type CNPGCollectorConfig struct {
	// UpdateInterval specifies how often to collect CNPG metrics
	UpdateInterval time.Duration

	// PrometheusURL specifies the URL of the Prometheus instance to query
	PrometheusURL string

	// QueryTimeout specifies the timeout for Prometheus queries
	QueryTimeout time.Duration
}

// CNPGCollector polls Prometheus for CloudNativePG cluster metrics and forwards them to Dakr
type CNPGCollector struct {
	prometheusAPI   v1.API
	batchChan       chan CollectedResource   // input to batcher
	resourceChan    chan []CollectedResource // output from batcher
	batcher         *ResourcesBatcher
	stopCh          chan struct{}
	ticker          *time.Ticker
	config          CNPGCollectorConfig
	namespaces      []string
	excludedClusters map[types.NamespacedName]bool
	logger          logr.Logger
	metrics         *TelemetryMetrics
	telemetryLogger telemetry_logger.Logger
	mu              sync.RWMutex
}

// NewCNPGCollector creates a new collector for CloudNativePG cluster metrics
func NewCNPGCollector(
	config CNPGCollectorConfig,
	namespaces []string,
	excludedClusters []ExcludedCNPGCluster,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	metrics *TelemetryMetrics,
	telemetryLogger telemetry_logger.Logger,
) *CNPGCollector {
	excludedMap := make(map[types.NamespacedName]bool)
	for _, c := range excludedClusters {
		excludedMap[types.NamespacedName{Namespace: c.Namespace, Name: c.Name}] = true
	}

	if config.UpdateInterval <= 0 {
		config.UpdateInterval = 60 * time.Second
	}
	if config.PrometheusURL == "" {
		config.PrometheusURL = "http://prometheus-service.monitoring.svc.cluster.local:9090"
	}
	if config.QueryTimeout <= 0 {
		config.QueryTimeout = 10 * time.Second
	}

	batchChan := make(chan CollectedResource, 500)
	resourceChan := make(chan []CollectedResource, 200)
	batcher := NewResourcesBatcher(maxBatchSize, maxBatchTime, batchChan, resourceChan, logger)

	return &CNPGCollector{
		batchChan:        batchChan,
		resourceChan:     resourceChan,
		batcher:          batcher,
		stopCh:           make(chan struct{}),
		config:           config,
		namespaces:       namespaces,
		excludedClusters: excludedMap,
		logger:           logger.WithName("cnpg-collector"),
		metrics:          metrics,
		telemetryLogger:  telemetryLogger,
	}
}

// Start begins the CNPG metrics collection process
func (c *CNPGCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting CNPG collector",
		"namespaces", c.namespaces,
		"updateInterval", c.config.UpdateInterval,
		"prometheusURL", c.config.PrometheusURL)

	httpClient := NewPrometheusClient(c.metrics)

	client, err := api.NewClient(api.Config{
		Address: c.config.PrometheusURL,
		Client:  httpClient,
	})
	if err != nil {
		c.telemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_ERROR,
			"CNPGCollector",
			"Failed to create Prometheus client",
			err,
			map[string]string{
				"prometheus_url":   c.config.PrometheusURL,
				"zxporter_version": version.Get().String(),
			},
		)
		return fmt.Errorf("failed to create Prometheus client: %w", err)
	}

	c.prometheusAPI = v1.NewAPI(client)

	c.batcher.start()
	c.ticker = time.NewTicker(c.config.UpdateInterval)

	go c.collectLoop(ctx)

	stopCh := c.stopCh
	go func() {
		select {
		case <-ctx.Done():
			if err := c.Stop(); err != nil {
				c.logger.Error(err, "Failed to stop CNPG collector during context cancellation")
			}
		case <-stopCh:
		}
	}()

	return nil
}

// collectLoop collects CNPG metrics at regular intervals
func (c *CNPGCollector) collectLoop(ctx context.Context) {
	c.collectAllClusters(ctx)

	for {
		select {
		case <-c.stopCh:
			return
		case <-c.ticker.C:
			c.collectAllClusters(ctx)
		}
	}
}

// collectAllClusters discovers all CNPG clusters from Prometheus and collects their metrics
func (c *CNPGCollector) collectAllClusters(ctx context.Context) {
	startTime := time.Now()

	queryCtx, cancel := context.WithTimeout(ctx, c.config.QueryTimeout)
	defer cancel()

	// Discover all (namespace, cluster) pairs from cnpg_collector_up
	clusterPairs, err := c.discoverClusters(queryCtx)
	if err != nil {
		c.logger.Error(err, "Failed to discover CNPG clusters from Prometheus")
		if c.telemetryLogger != nil {
			c.telemetryLogger.Report(
				gen.LogLevel_LOG_LEVEL_ERROR,
				"CNPGCollector",
				"Failed to discover CNPG clusters",
				err,
				map[string]string{
					"error_type":       "discover_clusters_failed",
					"zxporter_version": version.Get().String(),
				},
			)
		}
		return
	}

	processed, skipped, errors := 0, 0, 0

	for _, pair := range clusterPairs {
		if c.isExcluded(pair.namespace, pair.clusterName) {
			skipped++
			continue
		}

		if err := c.processCluster(ctx, pair.namespace, pair.clusterName); err != nil {
			errors++
			c.logger.Error(err, "Failed to collect metrics for CNPG cluster",
				"namespace", pair.namespace, "cluster", pair.clusterName)
			continue
		}
		processed++
	}

	c.logger.Info("CNPG metrics collection completed",
		"total", len(clusterPairs),
		"processed", processed,
		"skipped", skipped,
		"errors", errors,
		"tick_duration_ms", time.Since(startTime).Milliseconds())
}

type clusterPair struct {
	namespace   string
	clusterName string
}

// discoverClusters returns all (namespace, cluster) pairs visible in Prometheus
func (c *CNPGCollector) discoverClusters(ctx context.Context) ([]clusterPair, error) {
	result, _, err := c.prometheusAPI.Query(ctx, `cnpg_collector_up`, time.Now())
	if err != nil {
		return nil, fmt.Errorf("failed to query cnpg_collector_up: %w", err)
	}

	vector, ok := result.(model.Vector)
	if !ok {
		return nil, fmt.Errorf("unexpected result type for cnpg_collector_up: %T", result)
	}

	seen := make(map[clusterPair]bool)
	var pairs []clusterPair
	for _, sample := range vector {
		ns := string(sample.Metric["namespace"])
		cl := string(sample.Metric["cluster"])
		if ns == "" || cl == "" {
			continue
		}

		// Filter by namespaces if specified
		if !c.inNamespaces(ns) {
			continue
		}

		p := clusterPair{namespace: ns, clusterName: cl}
		if !seen[p] {
			seen[p] = true
			pairs = append(pairs, p)
		}
	}

	return pairs, nil
}

// processCluster collects all metrics for a single CNPG cluster and emits a snapshot
func (c *CNPGCollector) processCluster(ctx context.Context, namespace, clusterName string) error {
	queryCtx, cancel := context.WithTimeout(ctx, c.config.QueryTimeout)
	defer cancel()

	snapshot := &CNPGClusterMetricsSnapshot{
		ClusterName: clusterName,
		Namespace:   namespace,
	}

	now := time.Now()

	// --- collector_up ---
	upVal, err := c.querySingleValue(queryCtx,
		fmt.Sprintf(`cnpg_collector_up{namespace=%q, cluster=%q}`, namespace, clusterName), now)
	if err != nil {
		snapshot.UnavailableReason = fmt.Sprintf("collector_up_query_failed: %v", err)
	} else {
		snapshot.CollectorUp = upVal == 1
		snapshot.StatsAvailable = true
	}

	// --- database sizes ---
	dbSizes, err := c.queryDatabaseSizes(queryCtx, namespace, clusterName, now)
	if err != nil {
		c.logger.V(1).Info("Failed to query database sizes",
			"namespace", namespace, "cluster", clusterName, "error", err)
	} else {
		snapshot.Databases = dbSizes
		for _, db := range dbSizes {
			snapshot.TotalDatabaseSizeBytes += db.SizeBytes
		}
	}

	// --- total backends ---
	backends, err := c.querySumValue(queryCtx,
		fmt.Sprintf(`cnpg_pg_backends_total{namespace=%q, cluster=%q}`, namespace, clusterName), now)
	if err != nil {
		c.logger.V(1).Info("Failed to query backends", "namespace", namespace, "cluster", clusterName, "error", err)
	} else {
		snapshot.TotalBackends = int64(backends)
	}

	// --- replication lag ---
	lag, err := c.querySingleValue(queryCtx,
		fmt.Sprintf(`cnpg_pg_replication_lag{namespace=%q, cluster=%q}`, namespace, clusterName), now)
	if err != nil {
		c.logger.V(1).Info("Failed to query replication lag", "namespace", namespace, "cluster", clusterName, "error", err)
	} else {
		snapshot.ReplicationLagSeconds = lag
	}

	c.emitSnapshot(snapshot)
	return nil
}

// querySingleValue runs a PromQL query and returns the first scalar value
func (c *CNPGCollector) querySingleValue(ctx context.Context, query string, t time.Time) (float64, error) {
	result, _, err := c.prometheusAPI.Query(ctx, query, t)
	if err != nil {
		return 0, err
	}
	vector, ok := result.(model.Vector)
	if !ok || len(vector) == 0 {
		return 0, fmt.Errorf("no results for query: %s", query)
	}
	return float64(vector[0].Value), nil
}

// querySumValue runs a PromQL query and sums all returned sample values
func (c *CNPGCollector) querySumValue(ctx context.Context, query string, t time.Time) (float64, error) {
	result, _, err := c.prometheusAPI.Query(ctx, query, t)
	if err != nil {
		return 0, err
	}
	vector, ok := result.(model.Vector)
	if !ok {
		return 0, fmt.Errorf("unexpected result type: %T", result)
	}
	var sum float64
	for _, s := range vector {
		sum += float64(s.Value)
	}
	return sum, nil
}

// queryDatabaseSizes returns per-database size metrics for a cluster
func (c *CNPGCollector) queryDatabaseSizes(ctx context.Context, namespace, clusterName string, t time.Time) ([]CNPGDatabaseMetrics, error) {
	query := fmt.Sprintf(`cnpg_pg_database_size_bytes{namespace=%q, cluster=%q}`, namespace, clusterName)
	result, _, err := c.prometheusAPI.Query(ctx, query, t)
	if err != nil {
		return nil, fmt.Errorf("failed to query database sizes: %w", err)
	}

	vector, ok := result.(model.Vector)
	if !ok {
		return nil, fmt.Errorf("unexpected result type: %T", result)
	}

	var dbs []CNPGDatabaseMetrics
	for _, sample := range vector {
		dbs = append(dbs, CNPGDatabaseMetrics{
			Name:      string(sample.Metric["datname"]),
			SizeBytes: int64(sample.Value),
		})
	}
	return dbs, nil
}

// emitSnapshot sends the metrics snapshot to the batcher
func (c *CNPGCollector) emitSnapshot(snapshot *CNPGClusterMetricsSnapshot) {
	key := fmt.Sprintf("cnpg/%s/%s", snapshot.Namespace, snapshot.ClusterName)

	c.logger.V(1).Info("Emitting CNPG cluster metrics snapshot",
		"key", key,
		"collectorUp", snapshot.CollectorUp,
		"totalDatabaseSizeBytes", snapshot.TotalDatabaseSizeBytes,
		"totalBackends", snapshot.TotalBackends,
		"replicationLagSeconds", snapshot.ReplicationLagSeconds)

	c.batchChan <- CollectedResource{
		ResourceType: CNPGCluster,
		Object:       snapshot,
		Timestamp:    time.Now(),
		EventType:    EventTypeMetrics,
		Key:          key,
	}
}

// inNamespaces returns true if ns is in the collector's namespace list (or no filter set)
func (c *CNPGCollector) inNamespaces(ns string) bool {
	if len(c.namespaces) == 0 || (len(c.namespaces) == 1 && c.namespaces[0] == "") {
		return true
	}
	for _, n := range c.namespaces {
		if n == ns {
			return true
		}
	}
	return false
}

// isExcluded checks if a CNPG cluster should be skipped
func (c *CNPGCollector) isExcluded(namespace, name string) bool {
	if !c.inNamespaces(namespace) {
		return true
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.excludedClusters[types.NamespacedName{Namespace: namespace, Name: name}]
}

// Stop gracefully shuts down the CNPG collector
func (c *CNPGCollector) Stop() error {
	c.logger.Info("Stopping CNPG collector")

	if c.ticker != nil {
		c.ticker.Stop()
	}

	select {
	case <-c.stopCh:
		c.logger.Info("CNPG collector stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed CNPG collector stop channel")
	}

	if c.batchChan != nil {
		close(c.batchChan)
		c.batchChan = nil
	}

	if c.batcher != nil {
		c.batcher.stop()
	}

	return nil
}

// GetResourceChannel returns the channel for collected resource batches
func (c *CNPGCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the type of resource this collector handles
func (c *CNPGCollector) GetType() string {
	return "cnpg_cluster"
}

// IsAvailable checks if CNPG metrics are available by probing Prometheus
func (c *CNPGCollector) IsAvailable(ctx context.Context) bool {
	if c.prometheusAPI == nil {
		return true
	}
	queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	result, _, err := c.prometheusAPI.Query(queryCtx, `cnpg_collector_up`, time.Now())
	if err != nil {
		c.logger.Info("CNPG metrics not available in Prometheus", "error", err.Error())
		return false
	}

	vector, ok := result.(model.Vector)
	if !ok || len(vector) == 0 {
		c.logger.Info("No CNPG clusters found in Prometheus")
		return false
	}

	return true
}

// AddResource is a no-op — CNPG metrics are collected via polling
func (c *CNPGCollector) AddResource(resource interface{}) error {
	return nil
}
