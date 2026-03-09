package health

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	karpenterLabelName  = "app.kubernetes.io/name=karpenter"
	devzeroManagedLabel = "dakr.devzero.io/managed=true"
	defaultHealthPort   = "8081"
	defaultProbeTimeout = 5 * time.Second
)

type podProbeResult struct {
	healthzOK bool
	readyzOK  bool
}

type NodeOperatorMonitor struct {
	logger     logr.Logger
	clientset  kubernetes.Interface
	httpClient *http.Client
	healthPort string
}

func NewNodeOperatorMonitor(logger logr.Logger, clientset kubernetes.Interface, httpClient *http.Client) *NodeOperatorMonitor {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultProbeTimeout}
	}
	return &NodeOperatorMonitor{
		logger:     logger,
		clientset:  clientset,
		httpClient: httpClient,
		healthPort: defaultHealthPort,
	}
}

func (m *NodeOperatorMonitor) BuildNodeOperatorReport(ctx context.Context) (map[string]ComponentStatus, string, string, time.Time) {
	dep, err := m.discoverDeployment(ctx)
	if err != nil {
		m.logger.Error(err, "Failed to discover dzKarp deployment")
		return nil, "", "", time.Time{}
	}
	if dep == nil {
		m.logger.V(1).Info("No DevZero-managed Karpenter deployment found, skipping node operator health report")
		return nil, "", "", time.Time{}
	}

	version, commit := extractVersionInfo(dep)
	uptimeSince := dep.CreationTimestamp.Time

	selectorLabels := dep.Spec.Selector.MatchLabels
	pods, err := m.discoverPods(ctx, dep.Namespace, selectorLabels)
	if err != nil {
		m.logger.Error(err, "Failed to discover dzKarp pods", "namespace", dep.Namespace)
		report := make(map[string]ComponentStatus, 2)
		report[ComponentKarpenterHealth] = ComponentStatus{
			Status:  HealthStatusUnhealthy,
			Message: fmt.Sprintf("failed to list pods: %v", err),
		}
		report[ComponentKarpenterDeployment] = m.buildDeploymentStatus(dep)
		return report, version, commit, uptimeSince
	}

	var probes []podProbeResult
	for _, pod := range pods {
		if pod.Status.PodIP == "" || pod.Status.Phase != corev1.PodRunning {
			probes = append(probes, podProbeResult{healthzOK: false, readyzOK: false})
			continue
		}
		result := m.probePodHealth(ctx, fmt.Sprintf("%s:%s", pod.Status.PodIP, m.healthPort))
		probes = append(probes, result)
	}

	report := make(map[string]ComponentStatus, 2)

	healthStatus, healthMsg, healthMeta := aggregateProbeStatus(probes)
	report[ComponentKarpenterHealth] = ComponentStatus{
		Status:   healthStatus,
		Message:  healthMsg,
		Metadata: healthMeta,
	}

	report[ComponentKarpenterDeployment] = m.buildDeploymentStatus(dep)

	return report, version, commit, uptimeSince
}

func (m *NodeOperatorMonitor) discoverDeployment(ctx context.Context) (*appsv1.Deployment, error) {
	labelSelector := karpenterLabelName + "," + devzeroManagedLabel
	deployments, err := m.clientset.AppsV1().Deployments("").List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return nil, fmt.Errorf("listing deployments with selector %q: %w", labelSelector, err)
	}
	if len(deployments.Items) == 0 {
		return nil, nil
	}
	return &deployments.Items[0], nil
}

func (m *NodeOperatorMonitor) discoverPods(ctx context.Context, namespace string, labels map[string]string) ([]corev1.Pod, error) {
	var parts []string
	for k, v := range labels {
		parts = append(parts, fmt.Sprintf("%s=%s", k, v))
	}
	labelSelector := strings.Join(parts, ",")

	podList, err := m.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return nil, fmt.Errorf("listing pods with selector %q in namespace %q: %w", labelSelector, namespace, err)
	}
	return podList.Items, nil
}

func (m *NodeOperatorMonitor) probePodHealth(ctx context.Context, hostPort string) podProbeResult {
	result := podProbeResult{}
	result.healthzOK = m.probeEndpoint(ctx, fmt.Sprintf("http://%s/healthz", hostPort))
	result.readyzOK = m.probeEndpoint(ctx, fmt.Sprintf("http://%s/readyz", hostPort))
	return result
}

func (m *NodeOperatorMonitor) probeEndpoint(ctx context.Context, url string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (m *NodeOperatorMonitor) buildDeploymentStatus(dep *appsv1.Deployment) ComponentStatus {
	var desired int32
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	status, msg, meta := aggregateDeploymentStatus(desired, dep.Status.ReadyReplicas, dep.Status.AvailableReplicas)
	meta["version"] = dep.Labels["app.kubernetes.io/version"]
	_, commit := extractVersionInfo(dep)
	if commit != "" {
		meta["commit"] = commit
	}
	return ComponentStatus{
		Status:   status,
		Message:  msg,
		Metadata: meta,
	}
}

func aggregateProbeStatus(probes []podProbeResult) (HealthStatus, string, map[string]string) {
	if len(probes) == 0 {
		return HealthStatusUnhealthy, "no pods found", map[string]string{
			"pod_count":    "0",
			"pods_healthy": "0",
		}
	}

	healthyCount := 0
	for _, p := range probes {
		if p.healthzOK && p.readyzOK {
			healthyCount++
		}
	}

	meta := map[string]string{
		"pod_count":    fmt.Sprintf("%d", len(probes)),
		"pods_healthy": fmt.Sprintf("%d", healthyCount),
	}

	switch {
	case healthyCount == len(probes):
		return HealthStatusHealthy, fmt.Sprintf("all %d pods healthy", len(probes)), meta
	case healthyCount > 0:
		return HealthStatusDegraded, fmt.Sprintf("%d/%d pods healthy", healthyCount, len(probes)), meta
	default:
		return HealthStatusUnhealthy, fmt.Sprintf("0/%d pods healthy", len(probes)), meta
	}
}

func aggregateDeploymentStatus(desired, ready, available int32) (HealthStatus, string, map[string]string) {
	meta := map[string]string{
		"replicas":           fmt.Sprintf("%d", desired),
		"ready_replicas":     fmt.Sprintf("%d", ready),
		"available_replicas": fmt.Sprintf("%d", available),
	}

	switch {
	case desired > 0 && ready == desired && available == desired:
		return HealthStatusHealthy, fmt.Sprintf("%d/%d replicas ready", ready, desired), meta
	case ready > 0:
		return HealthStatusDegraded, fmt.Sprintf("%d/%d replicas ready", ready, desired), meta
	default:
		return HealthStatusUnhealthy, fmt.Sprintf("0/%d replicas ready", desired), meta
	}
}

func extractVersionInfo(dep *appsv1.Deployment) (string, string) {
	version := dep.Labels["app.kubernetes.io/version"]
	commit := ""

	if len(dep.Spec.Template.Spec.Containers) > 0 {
		image := dep.Spec.Template.Spec.Containers[0].Image
		if atIdx := strings.Index(image, "@"); atIdx > 0 {
			image = image[:atIdx]
		}
		if colonIdx := strings.LastIndex(image, ":"); colonIdx > 0 {
			commit = image[colonIdx+1:]
		}
	}

	return version, commit
}
