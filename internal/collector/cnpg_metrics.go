package collector

// CNPGDatabaseMetrics holds per-database metrics for a CNPG cluster
type CNPGDatabaseMetrics struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"sizeBytes"`
}

// CNPGClusterMetricsSnapshot represents a strongly-typed snapshot of CloudNativePG cluster metrics
type CNPGClusterMetricsSnapshot struct {
	// Cluster identification
	ClusterName string `json:"clusterName"`
	Namespace   string `json:"namespace"`

	// Overall health
	CollectorUp bool `json:"collectorUp"`

	// Database-level metrics
	Databases              []CNPGDatabaseMetrics `json:"databases,omitempty"`
	TotalDatabaseSizeBytes int64                 `json:"totalDatabaseSizeBytes"`

	// Connection metrics
	TotalBackends int64 `json:"totalBackends"`

	// Replication
	ReplicationLagSeconds float64 `json:"replicationLagSeconds"`

	// Quality and source information
	StatsAvailable    bool   `json:"statsAvailable"`
	UnavailableReason string `json:"unavailableReason,omitempty"`
}
