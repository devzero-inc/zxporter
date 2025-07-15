package snap

import "time"

// ResourceIdentifier contains minimal resource identification data
type ResourceIdentifier struct {
	Name string `json:"name"`
}

// ClusterSnapshot represents a minimal snapshot for deletion tracking
type ClusterSnapshot struct {
	ClusterInfo   ClusterInfo            `json:"clusterInfo"`
	Nodes         map[string]*NodeData   `json:"nodes"`      // node uid -> NodeData
	Namespaces    map[string]*Namespace  `json:"namespaces"` // namespace UID -> Namespace
	ClusterScoped *ClusterScopedSnapshot `json:"clusterScoped"`
	Timestamp     time.Time              `json:"timestamp"`
	SnapshotID    string                 `json:"snapshotId"`
}

// ClusterInfo contains basic cluster information
type ClusterInfo struct {
	Version    string   `json:"version"`
	NodeCount  int      `json:"nodeCount"`
	Namespaces []string `json:"namespaces"`
}

// NodeData represents a node with minimal data and assigned pod UIDs
type NodeData struct {
	Node ResourceIdentifier            `json:"node"`
	Pods map[string]ResourceIdentifier `json:"pods"` // UID -> ResourceIdentifier
	Hash string                        `json:"hash"`
}

// Namespace represents minimal namespace data with resource UIDs as keys
type Namespace struct {
	Namespace            ResourceIdentifier            `json:"namespace"`
	Deployments          map[string]ResourceIdentifier `json:"deployments"`          // UID -> ResourceIdentifier
	StatefulSets         map[string]ResourceIdentifier `json:"statefulSets"`         // UID -> ResourceIdentifier
	DaemonSets           map[string]ResourceIdentifier `json:"daemonSets"`           // UID -> ResourceIdentifier
	ReplicaSets          map[string]ResourceIdentifier `json:"replicaSets"`          // UID -> ResourceIdentifier
	Services             map[string]ResourceIdentifier `json:"services"`             // UID -> ResourceIdentifier
	ConfigMaps           map[string]ResourceIdentifier `json:"configMaps"`           // UID -> ResourceIdentifier
	Secrets              map[string]ResourceIdentifier `json:"secrets"`              // UID -> ResourceIdentifier
	PVCs                 map[string]ResourceIdentifier `json:"pvcs"`                 // UID -> ResourceIdentifier
	Jobs                 map[string]ResourceIdentifier `json:"jobs"`                 // UID -> ResourceIdentifier
	CronJobs             map[string]ResourceIdentifier `json:"cronJobs"`             // UID -> ResourceIdentifier
	Ingresses            map[string]ResourceIdentifier `json:"ingresses"`            // UID -> ResourceIdentifier
	NetworkPolicies      map[string]ResourceIdentifier `json:"networkPolicies"`      // UID -> ResourceIdentifier
	ServiceAccounts      map[string]ResourceIdentifier `json:"serviceAccounts"`      // UID -> ResourceIdentifier
	Roles                map[string]ResourceIdentifier `json:"roles"`                // UID -> ResourceIdentifier
	RoleBindings         map[string]ResourceIdentifier `json:"roleBindings"`         // UID -> ResourceIdentifier
	PodDisruptionBudgets map[string]ResourceIdentifier `json:"podDisruptionBudgets"` // UID -> ResourceIdentifier
	Endpoints            map[string]ResourceIdentifier `json:"endpoints"`            // UID -> ResourceIdentifier
	LimitRanges          map[string]ResourceIdentifier `json:"limitRanges"`          // UID -> ResourceIdentifier
	ResourceQuotas       map[string]ResourceIdentifier `json:"resourceQuotas"`       // UID -> ResourceIdentifier
	UnscheduledPods      map[string]ResourceIdentifier `json:"unscheduledPods"`      // UID -> ResourceIdentifier
	Hash                 string                        `json:"hash"`
}

// ClusterScopedSnapshot contains minimal cluster-scoped resource data
type ClusterScopedSnapshot struct {
	PersistentVolumes   map[string]ResourceIdentifier `json:"persistentVolumes"`   // UID -> ResourceIdentifier
	StorageClasses      map[string]ResourceIdentifier `json:"storageClasses"`      // UID -> ResourceIdentifier
	ClusterRoles        map[string]ResourceIdentifier `json:"clusterRoles"`        // UID -> ResourceIdentifier
	ClusterRoleBindings map[string]ResourceIdentifier `json:"clusterRoleBindings"` // UID -> ResourceIdentifier
	Hash                string                        `json:"hash"`
}
