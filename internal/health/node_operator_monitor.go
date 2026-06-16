package health

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// karpenterLabelName selects the Karpenter controller deployment/service.
	// DevZero's dzkarp charts (dzkarp-aws-karpenter, dzkarp-azure-karpenter,
	// dzkarp-gcp-karpenter, ...) set app.kubernetes.io/name to the chart name —
	// e.g. "dzkarp-aws-karpenter" — NOT "karpenter", so selecting on the name
	// label misses every DevZero-managed install. The Helm release is
	// consistently "karpenter" across providers (and for upstream Karpenter),
	// so app.kubernetes.io/instance is the stable selector; isDevZeroImage()
	// then confirms the deployment is DevZero-managed.
	karpenterLabelName  = "app.kubernetes.io/instance=karpenter"
	defaultHealthPort   = "8081"
	defaultProbeTimeout = 5 * time.Second
)

// dzKarpImageIdentifiers are substrings that identify a DevZero-managed
// Karpenter image regardless of the container registry used (public ECR,
// private ECR, ACR, GCR, etc.). Matches the default helm chart repositories
// across karpenter-provider-aws, karpenter-provider-azure, and
// karpenter-provider-gcp.
var dzKarpImageIdentifiers = []string{
	"devzeroinc",   // AWS and Azure providers
	"cloudpilotai", // GCP provider
}

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

	svcEndpoint, err := m.discoverServiceEndpoint(ctx, dep.Namespace)
	if err != nil {
		m.logger.Error(err, "Failed to discover dzKarp service", "namespace", dep.Namespace)
		report := make(map[string]ComponentStatus, 1)
		report[ComponentKarpenterDeployment] = m.buildDeploymentStatus(dep)
		return report, version, commit, uptimeSince
	}

	probe := m.probePodHealth(ctx, svcEndpoint)

	report := make(map[string]ComponentStatus, 1)
	status := m.buildDeploymentStatus(dep)

	if status.Metadata == nil {
		status.Metadata = make(map[string]string)
	}
	status.Metadata["service_healthz"] = fmt.Sprintf("%t", probe.healthzOK)
	status.Metadata["service_readyz"] = fmt.Sprintf("%t", probe.readyzOK)

	// Annotate the message with probe results when probes fail, but do not
	// downgrade a healthy deployment: K8s replica health is the authoritative
	// signal. Probe failures on a ready deployment are typically transient
	// (network policy, brief endpoint churn during rollouts, service port
	// mismatches) and should not cause false DEGRADED alerts.
	if !probe.healthzOK || !probe.readyzOK {
		status.Message = fmt.Sprintf("%s (service healthz=%t readyz=%t)", status.Message, probe.healthzOK, probe.readyzOK)
	}

	report[ComponentKarpenterDeployment] = status

	return report, version, commit, uptimeSince
}

func (m *NodeOperatorMonitor) discoverDeployment(ctx context.Context) (*appsv1.Deployment, error) {
	deployments, err := m.clientset.AppsV1().Deployments("").List(ctx, metav1.ListOptions{
		LabelSelector: karpenterLabelName,
	})
	if err != nil {
		return nil, fmt.Errorf("listing deployments with selector %q: %w", karpenterLabelName, err)
	}
	for i := range deployments.Items {
		if isDevZeroImage(&deployments.Items[i]) {
			return &deployments.Items[i], nil
		}
	}
	return nil, nil
}

// isDevZeroImage checks whether the deployment uses a DevZero-managed
// Karpenter image by looking for known image identifiers in the container
// image string. Uses Contains to match any registry (public ECR, private ECR,
// ACR, GCR, etc.).
func isDevZeroImage(dep *appsv1.Deployment) bool {
	for _, c := range dep.Spec.Template.Spec.Containers {
		for _, id := range dzKarpImageIdentifiers {
			if strings.Contains(c.Image, id) {
				return true
			}
		}
	}
	return false
}

func (m *NodeOperatorMonitor) discoverServiceEndpoint(ctx context.Context, namespace string) (string, error) {
	svcs, err := m.clientset.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: karpenterLabelName,
	})
	if err != nil {
		return "", fmt.Errorf("listing services with selector %q in namespace %q: %w", karpenterLabelName, namespace, err)
	}
	if len(svcs.Items) == 0 {
		return "", fmt.Errorf("no service with selector %q found in namespace %q", karpenterLabelName, namespace)
	}
	svc := svcs.Items[0]

	port := m.healthPort
	for _, p := range svc.Spec.Ports {
		if p.Name == "http" || p.Name == "health" {
			port = fmt.Sprintf("%d", p.Port)
			break
		}
	}

	return fmt.Sprintf("%s.%s.svc:%s", svc.Name, svc.Namespace, port), nil
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
	defer func() { _ = resp.Body.Close() }()
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
