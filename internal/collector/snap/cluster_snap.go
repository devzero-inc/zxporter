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
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ClusterSnapshot represents a complete snapshot of cluster resources
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
	Version    string            `json:"version"`
	NodeCount  int               `json:"nodeCount"`
	Namespaces []string          `json:"namespaces"`
	Labels     map[string]string `json:"labels,omitempty"`
}

// NodeData represents a node and all resources assigned to it
type NodeData struct {
	Node    *corev1.Node           `json:"node"`
	Pods    map[string]*corev1.Pod `json:"pods"`
	Hash    string                 `json:"hash"`
	Changed bool                   `json:"-"`
}

// Namespace represents a namespace and all resources within it
type Namespace struct {
	Namespace            *corev1.Namespace                        `json:"namespace"`
	Deployments          map[string]*DeploymentWithPods           `json:"deployments"`
	StatefulSets         map[string]*StatefulSetWithPods          `json:"statefulSets"`
	DaemonSets           map[string]*DaemonSetWithPods            `json:"daemonSets"`
	ReplicaSets          map[string]*ReplicaSetWithPods           `json:"replicaSets"`
	Services             map[string]*corev1.Service               `json:"services"`
	ConfigMaps           map[string]*corev1.ConfigMap             `json:"configMaps"`
	Secrets              map[string]*corev1.Secret                `json:"secrets"`
	PVCs                 map[string]*corev1.PersistentVolumeClaim `json:"pvcs"`
	Jobs                 map[string]*batchv1.Job                  `json:"jobs"`
	CronJobs             map[string]*batchv1.CronJob              `json:"cronJobs"`
	Ingresses            map[string]*networkingv1.Ingress         `json:"ingresses"`
	NetworkPolicies      map[string]*networkingv1.NetworkPolicy   `json:"networkPolicies"`
	ServiceAccounts      map[string]*corev1.ServiceAccount        `json:"serviceAccounts"`
	Roles                map[string]*rbacv1.Role                  `json:"roles"`
	RoleBindings         map[string]*rbacv1.RoleBinding           `json:"roleBindings"`
	PodDisruptionBudgets map[string]*policyv1.PodDisruptionBudget `json:"podDisruptionBudgets"`
	Endpoints            map[string]*corev1.Endpoints             `json:"endpoints"`
	LimitRanges          map[string]*corev1.LimitRange            `json:"limitRanges"`
	ResourceQuotas       map[string]*corev1.ResourceQuota         `json:"resourceQuotas"`
	UnscheduledPods      map[string]*corev1.Pod                   `json:"unscheduledPods"`
	Hash                 string                                   `json:"hash"`
	Changed              bool                                     `json:"-"`
}

// DeploymentWithPods represents a deployment and its associated pods
type DeploymentWithPods struct {
	Deployment *appsv1.Deployment     `json:"deployment"`
	Pods       map[string]*corev1.Pod `json:"pods"`
	Hash       string                 `json:"hash"`
	Changed    bool                   `json:"-"`
}

// StatefulSetWithPods represents a statefulset and its associated pods
type StatefulSetWithPods struct {
	StatefulSet *appsv1.StatefulSet    `json:"statefulSet"`
	Pods        map[string]*corev1.Pod `json:"pods"`
	Hash        string                 `json:"hash"`
	Changed     bool                   `json:"-"`
}

// DaemonSetWithPods represents a daemonset and its associated pods
type DaemonSetWithPods struct {
	DaemonSet *appsv1.DaemonSet      `json:"daemonSet"`
	Pods      map[string]*corev1.Pod `json:"pods"`
	Hash      string                 `json:"hash"`
	Changed   bool                   `json:"-"`
}

// ReplicaSetWithPods represents a replicaset and its associated pods
type ReplicaSetWithPods struct {
	ReplicaSet *appsv1.ReplicaSet     `json:"replicaSet"`
	Pods       map[string]*corev1.Pod `json:"pods"`
	Hash       string                 `json:"hash"`
	Changed    bool                   `json:"-"`
}

// ClusterScopedSnapshot contains cluster-scoped resources
type ClusterScopedSnapshot struct {
	PersistentVolumes   map[string]*corev1.PersistentVolume   `json:"persistentVolumes"`
	StorageClasses      map[string]*storagev1.StorageClass    `json:"storageClasses"`
	ClusterRoles        map[string]*rbacv1.ClusterRole        `json:"clusterRoles"`
	ClusterRoleBindings map[string]*rbacv1.ClusterRoleBinding `json:"clusterRoleBindings"`
	Hash                string                                `json:"hash"`
	Changed             bool                                  `json:"-"`
}

// ClusterSnapshotter takes periodic snapshots and computes deltas
type ClusterSnapshotter struct {
	client   kubernetes.Interface
	logger   logr.Logger
	sender   transport.DirectSender
	stopCh   chan struct{}
	ticker   *time.Ticker
	interval time.Duration
	// lastSnapshot  *ClusterSnapshot
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
			// Channel was closed by Stop() method
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

// takeSnapshot captures the current cluster state and computes delta
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

	// if c.lastSnapshot == nil {
	// 	// First snapshot - send everything
	// 	c.logger.Info("First snapshot - sending complete cluster state")
	// } else {
	// 	delta := c.computeDelta(c.lastSnapshot, snapshot)
	// 	if delta != nil {
	// 		c.sendSnapshot(ctx, delta, false)
	// 	} else {
	// 		c.logger.Info("No changes detected - skipping snapshot")
	// 	}
	// }

	// c.lastSnapshot = snapshot
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

// captureClusterInfo captures basic cluster information
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

// captureNodes captures all nodes and their assigned pods
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
			Node: node.DeepCopy(),
			Pods: make(map[string]*corev1.Pod),
		}

		// Find pods assigned to this node
		for _, pod := range allPods {
			if pod.Spec.NodeName == node.Name && !c.isPodExcluded(pod) {
				key := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
				nodeData.Pods[key] = pod.DeepCopy()
			}
		}

		nodeData.Hash = c.calculateNodeHash(nodeData)
		snapshot.Nodes[node.Name] = nodeData
	}

	snapshot.ClusterInfo.NodeCount = len(snapshot.Nodes)
	return nil
}

// captureNamespaces captures all namespaces and their resources
func (c *ClusterSnapshotter) captureNamespaces(ctx context.Context, snapshot *ClusterSnapshot) error {
	// Get target namespaces
	targetNamespaces := c.getTargetNamespaces(ctx)

	for _, nsName := range targetNamespaces {
		nsData := &Namespace{
			Deployments:          make(map[string]*DeploymentWithPods),
			StatefulSets:         make(map[string]*StatefulSetWithPods),
			DaemonSets:           make(map[string]*DaemonSetWithPods),
			ReplicaSets:          make(map[string]*ReplicaSetWithPods),
			Services:             make(map[string]*corev1.Service),
			ConfigMaps:           make(map[string]*corev1.ConfigMap),
			Secrets:              make(map[string]*corev1.Secret),
			PVCs:                 make(map[string]*corev1.PersistentVolumeClaim),
			Jobs:                 make(map[string]*batchv1.Job),
			CronJobs:             make(map[string]*batchv1.CronJob),
			Ingresses:            make(map[string]*networkingv1.Ingress),
			NetworkPolicies:      make(map[string]*networkingv1.NetworkPolicy),
			ServiceAccounts:      make(map[string]*corev1.ServiceAccount),
			Roles:                make(map[string]*rbacv1.Role),
			RoleBindings:         make(map[string]*rbacv1.RoleBinding),
			PodDisruptionBudgets: make(map[string]*policyv1.PodDisruptionBudget),
			Endpoints:            make(map[string]*corev1.Endpoints),
			LimitRanges:          make(map[string]*corev1.LimitRange),
			ResourceQuotas:       make(map[string]*corev1.ResourceQuota),
			UnscheduledPods:      make(map[string]*corev1.Pod),
		}

		ns, err := c.client.CoreV1().Namespaces().Get(ctx, nsName, metav1.GetOptions{})
		if err != nil {
			c.logger.Error(err, "Failed to get namespace", "namespace", nsName)
			continue
		}
		nsData.Namespace = ns.DeepCopy()

		if err := c.captureNamespaceResources(ctx, nsName, nsData); err != nil {
			c.logger.Error(err, "Failed to capture resources for namespace", "namespace", nsName)
			continue
		}

		nsData.Hash = c.calculateNamespaceHash(nsData)
		snapshot.Namespaces[nsName] = nsData
	}

	return nil
}

// captureNamespaceResources captures all resources within a namespace with improved pod association
func (c *ClusterSnapshotter) captureNamespaceResources(ctx context.Context, namespace string, nsData *Namespace) error {
	pods, err := c.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list pods: %w", err)
	}

	podMap := make(map[string]*corev1.Pod)
	for _, pod := range pods.Items {
		if !c.isPodExcluded(&pod) {
			key := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
			podMap[key] = pod.DeepCopy()

			// If pod is not scheduled to a node
			if pod.Spec.NodeName == "" {
				nsData.UnscheduledPods[key] = pod.DeepCopy()
			}
		}
	}

	// Capture Deployments and their pods using selector-based matching (owner referece was not wroking so choosing this path)
	deployments, err := c.client.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, deployment := range deployments.Items {
			deploymentPods := c.getPodsForDeploymentBySelector(ctx, &deployment)

			nsData.Deployments[deployment.Name] = &DeploymentWithPods{
				Deployment: deployment.DeepCopy(),
				Pods:       deploymentPods,
			}
			nsData.Deployments[deployment.Name].Hash = c.calculateDeploymentHash(nsData.Deployments[deployment.Name])
		}
	}

	statefulSets, err := c.client.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, sts := range statefulSets.Items {
			stsPods := c.getPodsForStatefulSetBySelector(ctx, &sts)
			nsData.StatefulSets[sts.Name] = &StatefulSetWithPods{
				StatefulSet: sts.DeepCopy(),
				Pods:        stsPods,
			}
			nsData.StatefulSets[sts.Name].Hash = c.calculateStatefulSetHash(nsData.StatefulSets[sts.Name])
		}
	}

	daemonSets, err := c.client.AppsV1().DaemonSets(namespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, ds := range daemonSets.Items {
			dsPods := c.getPodsForDaemonSetBySelector(ctx, &ds)
			nsData.DaemonSets[ds.Name] = &DaemonSetWithPods{
				DaemonSet: ds.DeepCopy(),
				Pods:      dsPods,
			}
			nsData.DaemonSets[ds.Name].Hash = c.calculateDaemonSetHash(nsData.DaemonSets[ds.Name])
		}
	}

	// Capture ReplicaSets and their pods
	replicaSets, err := c.client.AppsV1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, rs := range replicaSets.Items {
			rsPods := c.getPodsForReplicaSetBySelector(ctx, &rs)
			nsData.ReplicaSets[rs.Name] = &ReplicaSetWithPods{
				ReplicaSet: rs.DeepCopy(),
				Pods:       rsPods,
			}
			nsData.ReplicaSets[rs.Name].Hash = c.calculateReplicaSetHash(nsData.ReplicaSets[rs.Name])
		}
	}

	// Capture other resources
	c.captureOtherResources(ctx, namespace, nsData)

	return nil
}

// Helper methods for each resource type using label selectors
func (c *ClusterSnapshotter) getPodsForDeploymentBySelector(ctx context.Context, deployment *appsv1.Deployment) map[string]*corev1.Pod {
	result := make(map[string]*corev1.Pod)

	selector := deployment.Spec.Selector
	if selector == nil {
		return result
	}

	selectorString := metav1.FormatLabelSelector(selector)
	if selectorString == "" {
		return result
	}

	pods, err := c.client.CoreV1().Pods(deployment.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selectorString,
	})
	if err != nil {
		c.logger.Error(err, "Failed to list pods by selector for deployment", "deployment", deployment.Name)
		return result
	}

	for _, pod := range pods.Items {
		if !c.isPodExcluded(&pod) {
			key := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
			result[key] = pod.DeepCopy()
		}
	}

	return result
}

func (c *ClusterSnapshotter) getPodsForStatefulSetBySelector(ctx context.Context, sts *appsv1.StatefulSet) map[string]*corev1.Pod {
	result := make(map[string]*corev1.Pod)

	selector := sts.Spec.Selector
	if selector == nil {
		return result
	}

	selectorString := metav1.FormatLabelSelector(selector)
	if selectorString == "" {
		return result
	}

	pods, err := c.client.CoreV1().Pods(sts.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selectorString,
	})
	if err != nil {
		c.logger.Error(err, "Failed to list pods by selector for statefulset", "statefulset", sts.Name)
		return result
	}

	for _, pod := range pods.Items {
		if !c.isPodExcluded(&pod) {
			key := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
			result[key] = pod.DeepCopy()
		}
	}

	return result
}

func (c *ClusterSnapshotter) getPodsForDaemonSetBySelector(ctx context.Context, ds *appsv1.DaemonSet) map[string]*corev1.Pod {
	result := make(map[string]*corev1.Pod)

	selector := ds.Spec.Selector
	if selector == nil {
		return result
	}

	selectorString := metav1.FormatLabelSelector(selector)
	if selectorString == "" {
		return result
	}

	pods, err := c.client.CoreV1().Pods(ds.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selectorString,
	})
	if err != nil {
		c.logger.Error(err, "Failed to list pods by selector for daemonset", "daemonset", ds.Name)
		return result
	}

	for _, pod := range pods.Items {
		if !c.isPodExcluded(&pod) {
			key := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
			result[key] = pod.DeepCopy()
		}
	}

	return result
}

func (c *ClusterSnapshotter) getPodsForReplicaSetBySelector(ctx context.Context, rs *appsv1.ReplicaSet) map[string]*corev1.Pod {
	result := make(map[string]*corev1.Pod)

	selector := rs.Spec.Selector
	if selector == nil {
		return result
	}

	selectorString := metav1.FormatLabelSelector(selector)
	if selectorString == "" {
		return result
	}

	pods, err := c.client.CoreV1().Pods(rs.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selectorString,
	})
	if err != nil {
		c.logger.Error(err, "Failed to list pods by selector for replicaset", "replicaset", rs.Name)
		return result
	}

	for _, pod := range pods.Items {
		if !c.isPodExcluded(&pod) {
			key := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
			result[key] = pod.DeepCopy()
		}
	}

	return result
}

// captureOtherResources captures other namespace-scoped resources
func (c *ClusterSnapshotter) captureOtherResources(ctx context.Context, namespace string, nsData *Namespace) {
	if services, err := c.client.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, svc := range services.Items {
			nsData.Services[svc.Name] = svc.DeepCopy()
		}
	}

	if configMaps, err := c.client.CoreV1().ConfigMaps(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, cm := range configMaps.Items {
			nsData.ConfigMaps[cm.Name] = cm.DeepCopy()
		}
	}

	if secrets, err := c.client.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, secret := range secrets.Items {
			nsData.Secrets[secret.Name] = secret.DeepCopy()
		}
	}

	if pvcs, err := c.client.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, pvc := range pvcs.Items {
			nsData.PVCs[pvc.Name] = pvc.DeepCopy()
		}
	}

	if jobs, err := c.client.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, job := range jobs.Items {
			nsData.Jobs[job.Name] = job.DeepCopy()
		}
	}

	if cronJobs, err := c.client.BatchV1().CronJobs(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, cronJob := range cronJobs.Items {
			nsData.CronJobs[cronJob.Name] = cronJob.DeepCopy()
		}
	}

	if ingresses, err := c.client.NetworkingV1().Ingresses(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, ingress := range ingresses.Items {
			nsData.Ingresses[ingress.Name] = ingress.DeepCopy()
		}
	}

	if networkPolicies, err := c.client.NetworkingV1().NetworkPolicies(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, np := range networkPolicies.Items {
			nsData.NetworkPolicies[np.Name] = np.DeepCopy()
		}
	}

	if serviceAccounts, err := c.client.CoreV1().ServiceAccounts(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, sa := range serviceAccounts.Items {
			nsData.ServiceAccounts[sa.Name] = sa.DeepCopy()
		}
	}

	if roles, err := c.client.RbacV1().Roles(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, role := range roles.Items {
			nsData.Roles[role.Name] = role.DeepCopy()
		}
	}

	if roleBindings, err := c.client.RbacV1().RoleBindings(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, rb := range roleBindings.Items {
			nsData.RoleBindings[rb.Name] = rb.DeepCopy()
		}
	}

	if pdbs, err := c.client.PolicyV1().PodDisruptionBudgets(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, pdb := range pdbs.Items {
			nsData.PodDisruptionBudgets[pdb.Name] = pdb.DeepCopy()
		}
	}

	if endpoints, err := c.client.CoreV1().Endpoints(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, ep := range endpoints.Items {
			nsData.Endpoints[ep.Name] = ep.DeepCopy()
		}
	}

	if limitRanges, err := c.client.CoreV1().LimitRanges(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, lr := range limitRanges.Items {
			nsData.LimitRanges[lr.Name] = lr.DeepCopy()
		}
	}

	if resourceQuotas, err := c.client.CoreV1().ResourceQuotas(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, rq := range resourceQuotas.Items {
			nsData.ResourceQuotas[rq.Name] = rq.DeepCopy()
		}
	}
}

// captureClusterScopedResources captures cluster-scoped resources
func (c *ClusterSnapshotter) captureClusterScopedResources(ctx context.Context, snapshot *ClusterSnapshot) error {
	clusterScoped := snapshot.ClusterScoped
	clusterScoped.PersistentVolumes = make(map[string]*corev1.PersistentVolume)
	clusterScoped.StorageClasses = make(map[string]*storagev1.StorageClass)
	clusterScoped.ClusterRoles = make(map[string]*rbacv1.ClusterRole)
	clusterScoped.ClusterRoleBindings = make(map[string]*rbacv1.ClusterRoleBinding)

	if pvs, err := c.client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{}); err == nil {
		for _, pv := range pvs.Items {
			clusterScoped.PersistentVolumes[pv.Name] = pv.DeepCopy()
		}
	}

	if scs, err := c.client.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{}); err == nil {
		for _, sc := range scs.Items {
			clusterScoped.StorageClasses[sc.Name] = sc.DeepCopy()
		}
	}

	if clusterRoles, err := c.client.RbacV1().ClusterRoles().List(ctx, metav1.ListOptions{}); err == nil {
		for _, cr := range clusterRoles.Items {
			clusterScoped.ClusterRoles[cr.Name] = cr.DeepCopy()
		}
	}

	if clusterRoleBindings, err := c.client.RbacV1().ClusterRoleBindings().List(ctx, metav1.ListOptions{}); err == nil {
		for _, crb := range clusterRoleBindings.Items {
			clusterScoped.ClusterRoleBindings[crb.Name] = crb.DeepCopy()
		}
	}

	clusterScoped.Hash = c.calculateClusterScopedHash(clusterScoped)
	return nil
}

// // computeDelta computes the delta between old and new snapshots
// func (c *ClusterSnapshotter) computeDelta(oldSnapshot, newSnapshot *ClusterSnapshot) *ClusterSnapshot {
// 	hasChanges := false
// 	delta := &ClusterSnapshot{
// 		ClusterInfo:   newSnapshot.ClusterInfo,
// 		Nodes:         make(map[string]*NodeData),
// 		Namespaces:    make(map[string]*Namespace),
// 		ClusterScoped: &ClusterScopedSnapshot{},
// 		Timestamp:     newSnapshot.Timestamp,
// 		SnapshotID:    newSnapshot.SnapshotID,
// 	}

// 	// Check node changes
// 	for nodeName, newNode := range newSnapshot.Nodes {
// 		oldNode, exists := oldSnapshot.Nodes[nodeName]
// 		if !exists || oldNode.Hash != newNode.Hash {
// 			newNode.Changed = true
// 			delta.Nodes[nodeName] = newNode
// 			hasChanges = true
// 		}
// 	}

// 	// Check for deleted nodes
// 	for nodeName := range oldSnapshot.Nodes {
// 		if _, exists := newSnapshot.Nodes[nodeName]; !exists {
// 			// Node was deleted - so sending a tombstone
// 			deletedNode := &NodeData{
// 				Node:    nil, // Using nil to indicate deletion
// 				Pods:    make(map[string]*corev1.Pod),
// 				Hash:    "",
// 				Changed: true,
// 			}
// 			delta.Nodes[nodeName] = deletedNode
// 			hasChanges = true
// 		}
// 	}

// 	// Check namespace changes
// 	for nsName, newNs := range newSnapshot.Namespaces {
// 		oldNs, exists := oldSnapshot.Namespaces[nsName]
// 		if !exists || oldNs.Hash != newNs.Hash {
// 			newNs.Changed = true
// 			delta.Namespaces[nsName] = newNs
// 			hasChanges = true
// 		}
// 	}

// 	// Check for deleted namespaces
// 	for nsName := range oldSnapshot.Namespaces {
// 		if _, exists := newSnapshot.Namespaces[nsName]; !exists {
// 			// Namespace was deleted
// 			deletedNs := &Namespace{
// 				Namespace: nil, // Use nil to indicate deletion
// 				Changed:   true,
// 			}
// 			delta.Namespaces[nsName] = deletedNs
// 			hasChanges = true
// 		}
// 	}

// 	// Check cluster-scoped changes
// 	if oldSnapshot.ClusterScoped.Hash != newSnapshot.ClusterScoped.Hash {
// 		newSnapshot.ClusterScoped.Changed = true
// 		delta.ClusterScoped = newSnapshot.ClusterScoped
// 		hasChanges = true
// 	}

// 	if !hasChanges {
// 		return nil
// 	}

// 	return delta
// }

// Helper methods for resource collection and hashing
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

// Hash calculation methods
func (c *ClusterSnapshotter) calculateNodeHash(nodeData *NodeData) string {
	h := sha256.New()

	if nodeBytes, err := json.Marshal(nodeData.Node); err == nil {
		h.Write(nodeBytes)
	}

	for _, pod := range nodeData.Pods {
		if podBytes, err := json.Marshal(pod); err == nil {
			h.Write(podBytes)
		}
	}

	return fmt.Sprintf("%x", h.Sum(nil))
}

func (c *ClusterSnapshotter) calculateNamespaceHash(nsData *Namespace) string {
	h := sha256.New()

	// Hash namespace object
	if nsBytes, err := json.Marshal(nsData.Namespace); err == nil {
		h.Write(nsBytes)
	}

	// Hash all resource collections
	if deployBytes, err := json.Marshal(nsData.Deployments); err == nil {
		h.Write(deployBytes)
	}
	if stsBytes, err := json.Marshal(nsData.StatefulSets); err == nil {
		h.Write(stsBytes)
	}
	if dsBytes, err := json.Marshal(nsData.DaemonSets); err == nil {
		h.Write(dsBytes)
	}
	if rsBytes, err := json.Marshal(nsData.ReplicaSets); err == nil {
		h.Write(rsBytes)
	}
	if svcBytes, err := json.Marshal(nsData.Services); err == nil {
		h.Write(svcBytes)
	}
	if cmBytes, err := json.Marshal(nsData.ConfigMaps); err == nil {
		h.Write(cmBytes)
	}
	if secretBytes, err := json.Marshal(nsData.Secrets); err == nil {
		h.Write(secretBytes)
	}
	if pvcBytes, err := json.Marshal(nsData.PVCs); err == nil {
		h.Write(pvcBytes)
	}
	if jobBytes, err := json.Marshal(nsData.Jobs); err == nil {
		h.Write(jobBytes)
	}
	if cronBytes, err := json.Marshal(nsData.CronJobs); err == nil {
		h.Write(cronBytes)
	}
	if ingressBytes, err := json.Marshal(nsData.Ingresses); err == nil {
		h.Write(ingressBytes)
	}
	if netpolBytes, err := json.Marshal(nsData.NetworkPolicies); err == nil {
		h.Write(netpolBytes)
	}
	if saBytes, err := json.Marshal(nsData.ServiceAccounts); err == nil {
		h.Write(saBytes)
	}
	if roleBytes, err := json.Marshal(nsData.Roles); err == nil {
		h.Write(roleBytes)
	}
	if rbBytes, err := json.Marshal(nsData.RoleBindings); err == nil {
		h.Write(rbBytes)
	}
	if pdbBytes, err := json.Marshal(nsData.PodDisruptionBudgets); err == nil {
		h.Write(pdbBytes)
	}
	if epBytes, err := json.Marshal(nsData.Endpoints); err == nil {
		h.Write(epBytes)
	}
	if lrBytes, err := json.Marshal(nsData.LimitRanges); err == nil {
		h.Write(lrBytes)
	}
	if rqBytes, err := json.Marshal(nsData.ResourceQuotas); err == nil {
		h.Write(rqBytes)
	}
	if unscheduledBytes, err := json.Marshal(nsData.UnscheduledPods); err == nil {
		h.Write(unscheduledBytes)
	}

	return fmt.Sprintf("%x", h.Sum(nil))
}

func (c *ClusterSnapshotter) calculateDeploymentHash(deployment *DeploymentWithPods) string {
	h := sha256.New()
	if deployBytes, err := json.Marshal(deployment); err == nil {
		h.Write(deployBytes)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func (c *ClusterSnapshotter) calculateStatefulSetHash(sts *StatefulSetWithPods) string {
	h := sha256.New()
	if stsBytes, err := json.Marshal(sts); err == nil {
		h.Write(stsBytes)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func (c *ClusterSnapshotter) calculateDaemonSetHash(ds *DaemonSetWithPods) string {
	h := sha256.New()
	if dsBytes, err := json.Marshal(ds); err == nil {
		h.Write(dsBytes)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func (c *ClusterSnapshotter) calculateReplicaSetHash(rs *ReplicaSetWithPods) string {
	h := sha256.New()
	if rsBytes, err := json.Marshal(rs); err == nil {
		h.Write(rsBytes)
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

// sendSnapshot sends the snapshot to the collection channel
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

	// Send the snapshot directly
	if _, err := c.sender.Send(ctx, res); err != nil {
		c.logger.Error(err, "Failed to send cluster snapshot")
	} else {
		c.logger.Info("Successfully sent cluster snapshot", "type", snapshotType)
	}
}

// Stop gracefully shuts down the cluster snapshotter
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
