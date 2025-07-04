package snap

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/devzero-inc/zxporter/internal/collector"
	"github.com/devzero-inc/zxporter/internal/transport"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ResourceIdentifier contains minimal resource identification data
type ResourceIdentifier struct {
	Name string `json:"name"`
}

// ClusterSnapshot represents a minimal snapshot for deletion tracking
type ClusterSnapshot struct {
	ClusterInfo   ClusterInfo            `json:"clusterInfo"`
	Nodes         map[string]*NodeData   `json:"nodes"`
	Namespaces    map[string]*Namespace  `json:"namespaces"`
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

// ClusterSnapshotter takes periodic snapshots and computes deltas
type ClusterSnapshotter struct {
	client        kubernetes.Interface
	logger        logr.Logger
	sender        transport.DirectSender
	stopCh        chan struct{}
	ticker        *time.Ticker
	interval      time.Duration
	mu            sync.RWMutex
	namespaces    []string
	excludedPods  map[string]bool
	excludedNodes map[string]bool
}

func NewClusterSnapshotter(
	client kubernetes.Interface,
	interval time.Duration,
	sender transport.DirectSender,
	namespaces []string,
	excludedPods []collector.ExcludedPod,
	excludedNodes []string,
	logger logr.Logger,
) *ClusterSnapshotter {
	if interval <= 0 {
		interval = 15 * time.Minute // Default to 15 minutes
	}

	excludedPodsMap := make(map[string]bool)
	for _, pod := range excludedPods {
		key := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
		excludedPodsMap[key] = true
	}

	excludedNodesMap := make(map[string]bool)
	for _, node := range excludedNodes {
		excludedNodesMap[node] = true
	}

	return &ClusterSnapshotter{
		client:        client,
		logger:        logger.WithName("cluster-snapshotter"),
		sender:        sender,
		stopCh:        make(chan struct{}),
		interval:      interval,
		namespaces:    namespaces,
		excludedPods:  excludedPodsMap,
		excludedNodes: excludedNodesMap,
	}
}

func (c *ClusterSnapshotter) Start(ctx context.Context) error {
	c.logger.Info("Starting cluster snapshotter", "interval", c.interval)

	c.ticker = time.NewTicker(c.interval)

	go func() {
		c.takeSnapshot(ctx)
	}()

	go c.snapshotLoop(ctx)

	go func() {
		select {
		case <-ctx.Done():
			c.Stop()
		case <-c.stopCh:
		}
	}()

	return nil
}

func (c *ClusterSnapshotter) snapshotLoop(ctx context.Context) {
	for {
		select {
		case <-c.stopCh:
			return
		case <-c.ticker.C:
			c.takeSnapshot(ctx)
		}
	}
}

func (c *ClusterSnapshotter) takeSnapshot(ctx context.Context) {
	c.logger.Info("Taking cluster snapshot")
	startTime := time.Now()

	snapshot, err := c.captureClusterState(ctx)
	if err != nil {
		c.logger.Error(err, "Failed to capture cluster state")
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.sendSnapshot(ctx, snapshot, true)

	c.logger.Info("Snapshot completed", "duration", time.Since(startTime))
}

func (c *ClusterSnapshotter) captureClusterState(ctx context.Context) (*ClusterSnapshot, error) {
	snapshot := &ClusterSnapshot{
		Nodes:         make(map[string]*NodeData),
		Namespaces:    make(map[string]*Namespace),
		ClusterScoped: &ClusterScopedSnapshot{},
		Timestamp:     time.Now(),
		SnapshotID:    c.generateSnapshotID(),
	}

	if err := c.captureClusterInfo(ctx, snapshot); err != nil {
		return nil, fmt.Errorf("failed to capture cluster info: %w", err)
	}

	if err := c.captureNodes(ctx, snapshot); err != nil {
		return nil, fmt.Errorf("failed to capture nodes: %w", err)
	}

	if err := c.captureNamespaces(ctx, snapshot); err != nil {
		return nil, fmt.Errorf("failed to capture namespaces: %w", err)
	}

	if err := c.captureClusterScopedResources(ctx, snapshot); err != nil {
		return nil, fmt.Errorf("failed to capture cluster-scoped resources: %w", err)
	}

	return snapshot, nil
}

func (c *ClusterSnapshotter) captureClusterInfo(ctx context.Context, snapshot *ClusterSnapshot) error {
	version, err := c.client.Discovery().ServerVersion()
	if err != nil {
		c.logger.Error(err, "Failed to get server version")
		snapshot.ClusterInfo.Version = "unknown"
	} else {
		snapshot.ClusterInfo.Version = version.String()
	}

	namespaces, err := c.client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list namespaces: %w", err)
	}

	var nsNames []string
	for _, ns := range namespaces.Items {
		nsNames = append(nsNames, ns.Name)
	}
	snapshot.ClusterInfo.Namespaces = nsNames

	return nil
}

func (c *ClusterSnapshotter) captureNodes(ctx context.Context, snapshot *ClusterSnapshot) error {
	nodes, err := c.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}

	allPods, err := c.getAllPods(ctx)
	if err != nil {
		return fmt.Errorf("failed to get all pods: %w", err)
	}

	for _, node := range nodes.Items {
		if c.excludedNodes[node.Name] {
			continue
		}

		nodeData := &NodeData{
			Node: ResourceIdentifier{
				Name: node.Name,
			},
			Pods: make(map[string]ResourceIdentifier),
		}

		// Find pods assigned to this node, using UID as key
		for _, pod := range allPods {
			if pod.Spec.NodeName == node.Name && !c.isPodExcluded(pod) {
				uid := string(pod.UID)
				nodeData.Pods[uid] = ResourceIdentifier{
					Name: pod.Name,
				}
			}
		}

		nodeData.Hash = c.calculateNodeHash(nodeData)
		snapshot.Nodes[node.Name] = nodeData
	}

	snapshot.ClusterInfo.NodeCount = len(snapshot.Nodes)
	return nil
}

func (c *ClusterSnapshotter) captureNamespaces(ctx context.Context, snapshot *ClusterSnapshot) error {
	targetNamespaces := c.getTargetNamespaces(ctx)

	for _, nsName := range targetNamespaces {
		nsData := &Namespace{
			Deployments:          make(map[string]ResourceIdentifier),
			StatefulSets:         make(map[string]ResourceIdentifier),
			DaemonSets:           make(map[string]ResourceIdentifier),
			ReplicaSets:          make(map[string]ResourceIdentifier),
			Services:             make(map[string]ResourceIdentifier),
			ConfigMaps:           make(map[string]ResourceIdentifier),
			Secrets:              make(map[string]ResourceIdentifier),
			PVCs:                 make(map[string]ResourceIdentifier),
			Jobs:                 make(map[string]ResourceIdentifier),
			CronJobs:             make(map[string]ResourceIdentifier),
			Ingresses:            make(map[string]ResourceIdentifier),
			NetworkPolicies:      make(map[string]ResourceIdentifier),
			ServiceAccounts:      make(map[string]ResourceIdentifier),
			Roles:                make(map[string]ResourceIdentifier),
			RoleBindings:         make(map[string]ResourceIdentifier),
			PodDisruptionBudgets: make(map[string]ResourceIdentifier),
			Endpoints:            make(map[string]ResourceIdentifier),
			LimitRanges:          make(map[string]ResourceIdentifier),
			ResourceQuotas:       make(map[string]ResourceIdentifier),
			UnscheduledPods:      make(map[string]ResourceIdentifier),
		}

		ns, err := c.client.CoreV1().Namespaces().Get(ctx, nsName, metav1.GetOptions{})
		if err != nil {
			c.logger.Error(err, "Failed to get namespace", "namespace", nsName)
			continue
		}
		nsData.Namespace = ResourceIdentifier{
			Name: ns.Name,
		}

		if err := c.captureNamespaceResources(ctx, nsName, nsData); err != nil {
			c.logger.Error(err, "Failed to capture resources for namespace", "namespace", nsName)
			continue
		}

		nsData.Hash = c.calculateNamespaceHash(nsData)
		snapshot.Namespaces[nsName] = nsData
	}

	return nil
}

func (c *ClusterSnapshotter) captureNamespaceResources(ctx context.Context, namespace string, nsData *Namespace) error {
	// Capture unscheduled pods only (scheduled pods are captured with nodes)
	pods, err := c.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list pods: %w", err)
	}

	for _, pod := range pods.Items {
		if !c.isPodExcluded(&pod) && pod.Spec.NodeName == "" {
			uid := string(pod.UID)
			nsData.UnscheduledPods[uid] = ResourceIdentifier{
				Name: pod.Name,
			}
		}
	}

	// Capture workload controllers (without pods since pods are captured at node level)
	if deployments, err := c.client.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, deployment := range deployments.Items {
			uid := string(deployment.UID)
			nsData.Deployments[uid] = ResourceIdentifier{
				Name: deployment.Name,
			}
		}
	}

	if statefulSets, err := c.client.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, sts := range statefulSets.Items {
			uid := string(sts.UID)
			nsData.StatefulSets[uid] = ResourceIdentifier{
				Name: sts.Name,
			}
		}
	}

	if daemonSets, err := c.client.AppsV1().DaemonSets(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, ds := range daemonSets.Items {
			uid := string(ds.UID)
			nsData.DaemonSets[uid] = ResourceIdentifier{
				Name: ds.Name,
			}
		}
	}

	if replicaSets, err := c.client.AppsV1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, rs := range replicaSets.Items {
			uid := string(rs.UID)
			nsData.ReplicaSets[uid] = ResourceIdentifier{
				Name: rs.Name,
			}
		}
	}

	// Capture other resources
	c.captureOtherResources(ctx, namespace, nsData)

	return nil
}

func (c *ClusterSnapshotter) captureOtherResources(ctx context.Context, namespace string, nsData *Namespace) {
	if services, err := c.client.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, svc := range services.Items {
			uid := string(svc.UID)
			nsData.Services[uid] = ResourceIdentifier{Name: svc.Name}
		}
	}

	if configMaps, err := c.client.CoreV1().ConfigMaps(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, cm := range configMaps.Items {
			uid := string(cm.UID)
			nsData.ConfigMaps[uid] = ResourceIdentifier{Name: cm.Name}
		}
	}

	if secrets, err := c.client.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, secret := range secrets.Items {
			uid := string(secret.UID)
			nsData.Secrets[uid] = ResourceIdentifier{Name: secret.Name}
		}
	}

	if pvcs, err := c.client.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, pvc := range pvcs.Items {
			uid := string(pvc.UID)
			nsData.PVCs[uid] = ResourceIdentifier{Name: pvc.Name}
		}
	}

	if jobs, err := c.client.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, job := range jobs.Items {
			uid := string(job.UID)
			nsData.Jobs[uid] = ResourceIdentifier{Name: job.Name}
		}
	}

	if cronJobs, err := c.client.BatchV1().CronJobs(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, cronJob := range cronJobs.Items {
			uid := string(cronJob.UID)
			nsData.CronJobs[uid] = ResourceIdentifier{Name: cronJob.Name}
		}
	}

	if ingresses, err := c.client.NetworkingV1().Ingresses(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, ingress := range ingresses.Items {
			uid := string(ingress.UID)
			nsData.Ingresses[uid] = ResourceIdentifier{Name: ingress.Name}
		}
	}

	if networkPolicies, err := c.client.NetworkingV1().NetworkPolicies(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, np := range networkPolicies.Items {
			uid := string(np.UID)
			nsData.NetworkPolicies[uid] = ResourceIdentifier{Name: np.Name}
		}
	}

	if serviceAccounts, err := c.client.CoreV1().ServiceAccounts(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, sa := range serviceAccounts.Items {
			uid := string(sa.UID)
			nsData.ServiceAccounts[uid] = ResourceIdentifier{Name: sa.Name}
		}
	}

	if roles, err := c.client.RbacV1().Roles(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, role := range roles.Items {
			uid := string(role.UID)
			nsData.Roles[uid] = ResourceIdentifier{Name: role.Name}
		}
	}

	if roleBindings, err := c.client.RbacV1().RoleBindings(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, rb := range roleBindings.Items {
			uid := string(rb.UID)
			nsData.RoleBindings[uid] = ResourceIdentifier{Name: rb.Name}
		}
	}

	if pdbs, err := c.client.PolicyV1().PodDisruptionBudgets(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, pdb := range pdbs.Items {
			uid := string(pdb.UID)
			nsData.PodDisruptionBudgets[uid] = ResourceIdentifier{Name: pdb.Name}
		}
	}

	if endpoints, err := c.client.CoreV1().Endpoints(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, ep := range endpoints.Items {
			uid := string(ep.UID)
			nsData.Endpoints[uid] = ResourceIdentifier{Name: ep.Name}
		}
	}

	if limitRanges, err := c.client.CoreV1().LimitRanges(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, lr := range limitRanges.Items {
			uid := string(lr.UID)
			nsData.LimitRanges[uid] = ResourceIdentifier{Name: lr.Name}
		}
	}

	if resourceQuotas, err := c.client.CoreV1().ResourceQuotas(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, rq := range resourceQuotas.Items {
			uid := string(rq.UID)
			nsData.ResourceQuotas[uid] = ResourceIdentifier{Name: rq.Name}
		}
	}
}

func (c *ClusterSnapshotter) captureClusterScopedResources(ctx context.Context, snapshot *ClusterSnapshot) error {
	clusterScoped := snapshot.ClusterScoped
	clusterScoped.PersistentVolumes = make(map[string]ResourceIdentifier)
	clusterScoped.StorageClasses = make(map[string]ResourceIdentifier)
	clusterScoped.ClusterRoles = make(map[string]ResourceIdentifier)
	clusterScoped.ClusterRoleBindings = make(map[string]ResourceIdentifier)

	if pvs, err := c.client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{}); err == nil {
		for _, pv := range pvs.Items {
			uid := string(pv.UID)
			clusterScoped.PersistentVolumes[uid] = ResourceIdentifier{Name: pv.Name}
		}
	}

	if scs, err := c.client.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{}); err == nil {
		for _, sc := range scs.Items {
			uid := string(sc.UID)
			clusterScoped.StorageClasses[uid] = ResourceIdentifier{Name: sc.Name}
		}
	}

	if clusterRoles, err := c.client.RbacV1().ClusterRoles().List(ctx, metav1.ListOptions{}); err == nil {
		for _, cr := range clusterRoles.Items {
			uid := string(cr.UID)
			clusterScoped.ClusterRoles[uid] = ResourceIdentifier{Name: cr.Name}
		}
	}

	if clusterRoleBindings, err := c.client.RbacV1().ClusterRoleBindings().List(ctx, metav1.ListOptions{}); err == nil {
		for _, crb := range clusterRoleBindings.Items {
			uid := string(crb.UID)
			clusterScoped.ClusterRoleBindings[uid] = ResourceIdentifier{Name: crb.Name}
		}
	}

	clusterScoped.Hash = c.calculateClusterScopedHash(clusterScoped)
	return nil
}

// Helper methods remain mostly the same
func (c *ClusterSnapshotter) getAllPods(ctx context.Context) ([]*corev1.Pod, error) {
	var allPods []*corev1.Pod

	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		for _, ns := range c.namespaces {
			pods, err := c.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
			if err != nil {
				return nil, fmt.Errorf("failed to list pods in namespace %s: %w", ns, err)
			}
			for _, pod := range pods.Items {
				allPods = append(allPods, pod.DeepCopy())
			}
		}
	} else {
		pods, err := c.client.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to list all pods: %w", err)
		}
		for _, pod := range pods.Items {
			allPods = append(allPods, pod.DeepCopy())
		}
	}

	return allPods, nil
}

func (c *ClusterSnapshotter) getTargetNamespaces(ctx context.Context) []string {
	if len(c.namespaces) > 0 && c.namespaces[0] != "" {
		return c.namespaces
	}

	namespaces, err := c.client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		c.logger.Error(err, "Failed to list namespaces")
		return []string{}
	}

	var nsNames []string
	for _, ns := range namespaces.Items {
		nsNames = append(nsNames, ns.Name)
	}
	return nsNames
}

func (c *ClusterSnapshotter) isPodExcluded(pod *corev1.Pod) bool {
	key := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	return c.excludedPods[key]
}

func (c *ClusterSnapshotter) generateSnapshotID() string {
	return fmt.Sprintf("snapshot-%d", time.Now().UnixNano())
}

// Simplified hash calculation methods
func (c *ClusterSnapshotter) calculateNodeHash(nodeData *NodeData) string {
	h := sha256.New()
	if nodeBytes, err := json.Marshal(nodeData); err == nil {
		h.Write(nodeBytes)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func (c *ClusterSnapshotter) calculateNamespaceHash(nsData *Namespace) string {
	h := sha256.New()
	if nsBytes, err := json.Marshal(nsData); err == nil {
		h.Write(nsBytes)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func (c *ClusterSnapshotter) calculateClusterScopedHash(clusterScoped *ClusterScopedSnapshot) string {
	h := sha256.New()
	if csBytes, err := json.Marshal(clusterScoped); err == nil {
		h.Write(csBytes)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func (c *ClusterSnapshotter) sendSnapshot(ctx context.Context, snapshot *ClusterSnapshot, isFullSnapshot bool) {
	snapshotType := "delta"
	if isFullSnapshot {
		snapshotType = "full"
	}

	c.logger.Info("Sending cluster snapshot",
		"type", snapshotType,
		"snapshotId", snapshot.SnapshotID,
		"nodes", len(snapshot.Nodes),
		"namespaces", len(snapshot.Namespaces))

	res := collector.CollectedResource{
		ResourceType: collector.ClusterSnapshot,
		Object:       snapshot,
		Timestamp:    snapshot.Timestamp,
		EventType:    collector.EventTypeSnapshot,
		Key:          snapshot.SnapshotID,
	}

	if _, err := c.sender.Send(ctx, res); err != nil {
		c.logger.Error(err, "Failed to send cluster snapshot")
	} else {
		c.logger.Info("Successfully sent cluster snapshot", "type", snapshotType)
	}
}

func (c *ClusterSnapshotter) Stop() error {
	c.logger.Info("Stopping cluster snapshotter")

	if c.ticker != nil {
		c.ticker.Stop()
		c.logger.Info("Stopped cluster snapshotter ticker")
	}

	select {
	case <-c.stopCh:
		c.logger.Info("Cluster snapshotter stop channel already closed")
	default:
		close(c.stopCh)
		c.logger.Info("Closed cluster snapshotter stop channel")
	}

	return nil
}

func (c *ClusterSnapshotter) GetResourceChannel() <-chan []collector.CollectedResource {
	return nil
}

func (c *ClusterSnapshotter) GetType() string {
	return "cluster_snapshot"
}

func (c *ClusterSnapshotter) IsAvailable(ctx context.Context) bool {
	_, err := c.client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{Limit: 1})
	return err == nil
}
