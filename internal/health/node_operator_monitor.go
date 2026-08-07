package health

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
)

const (
	// karpenterInstanceLabel is the Helm release-name label. It is what
	// separates the live install from the one it supersedes when both are
	// present, so the Service lookup scopes on it.
	karpenterInstanceLabel = "app.kubernetes.io/instance"

	// karpenterLabelName selects the Karpenter controller deployment/service.
	// The name label is unusable: DevZero's dzkarp charts set
	// app.kubernetes.io/name to the chart name ("dzkarp-aws-karpenter"), not
	// "karpenter". That leaves the Helm release name
	// (app.kubernetes.io/instance), which is not constant either — dz-installer
	// installs as "dzkarp" (dz-installer/pkg/component/dzkarp: HelmReleaseName),
	// deliberately distinct so it can coexist with a pre-existing OSS Karpenter
	// release during migration, while manual and upstream installs use
	// "karpenter". Accept both; isDevZeroImage confirms a match is actually
	// DevZero-managed.
	karpenterLabelName = karpenterInstanceLabel + " in (karpenter,dzkarp)"

	// defaultHealthPort is where Karpenter serves /healthz and /readyz when the
	// Deployment's own kubelet probes do not say. It is a container port, not a
	// Service port: the chart publishes only the metrics port. See
	// discoverProbeTargets.
	defaultHealthPort   = "8081"
	defaultHealthzPath  = "/healthz"
	defaultReadyzPath   = "/readyz"
	defaultProbeTimeout = 5 * time.Second

	// maxProbedPods bounds how many replicas are probed per cycle. Karpenter runs
	// two replicas for HA, so this only truncates on unusual scale-outs, where
	// the extra probes would cost report latency without adding signal.
	maxProbedPods = 3

	// probeBudget bounds the whole probe phase. Probes run sequentially and each
	// can block for the client timeout, so without a shared deadline a cluster
	// that blackholes Pod traffic would stall every report cycle by
	// 2 x maxProbedPods x defaultProbeTimeout.
	probeBudget = 10 * time.Second
)

// probeOutcome is deliberately three-valued. "The endpoint answered and said it
// is not OK" and "nothing answered" are different facts about the controller,
// and collapsing both into a bool is what made a healthy 2/2 Karpenter report
// "service healthz=false readyz=false": the probe never reached a health server
// at all, and the report presented that as the controller failing its checks.
type probeOutcome int

const (
	probeOutcomeUnknown probeOutcome = iota // never got an answer
	probeOutcomeOK                          // answered 200
	probeOutcomeNotOK                       // answered, and it was not healthy
)

// String renders the outcome for health metadata. OK and NotOK keep the "true"
// and "false" the field has always carried; only the previously-conflated
// unreachable case reads differently.
func (o probeOutcome) String() string {
	switch o {
	case probeOutcomeOK:
		return "true"
	case probeOutcomeNotOK:
		return "false"
	default:
		return "unknown"
	}
}

// merge folds one replica's outcome into the aggregate across replicas. A
// definite not-OK anywhere is the fact worth surfacing, and an unknown never
// overwrites an answer another replica actually gave.
func (o probeOutcome) merge(other probeOutcome) probeOutcome {
	switch {
	case o == probeOutcomeNotOK || other == probeOutcomeNotOK:
		return probeOutcomeNotOK
	case o == probeOutcomeOK || other == probeOutcomeOK:
		return probeOutcomeOK
	default:
		return probeOutcomeUnknown
	}
}

// dzKarpImageIdentifiers are substrings that identify a DevZero-managed
// Karpenter image, matched with Contains so any registry works (public ECR,
// private ECR, ACR, GCR). DevZero republishes every provider's controller under
// a devzeroinc repository — dzkarp-aws, dzkarp-azure and dzkarp-gcp all resolve
// to .../devzeroinc/dzkarp-<provider>/controller — so one substring covers all
// three.
//
// Must agree with dakr's models.KarpenterDevZeroManagedImageRepo: when the two
// disagreed, the same cluster could be reported as both dzkarp-managed and not,
// depending on which side answered.
var dzKarpImageIdentifiers = []string{
	"devzeroinc", // AWS, Azure and GCP providers
}

type podProbeResult struct {
	healthz probeOutcome
	readyz  probeOutcome
	// attempted is how many Pods were probed. It makes a not-OK interpretable —
	// without it, "false" cannot be told apart from "false, from one replica of
	// four" — and a zero tells the reader no probe ran at all.
	attempted int
}

// probeEndpointSpec locates one HTTP health endpoint on a controller Pod.
type probeEndpointSpec struct {
	scheme string
	port   string
	path   string
}

func (s probeEndpointSpec) url(host string) string {
	path := s.path
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	// JoinHostPort brackets IPv6 Pod IPs, which a bare host:port concat corrupts.
	return s.scheme + "://" + net.JoinHostPort(host, s.port) + path
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

	pods, err := m.discoverProbeTargets(ctx, dep)
	if err != nil {
		m.logger.Error(err, "Failed to discover dzKarp pods to probe",
			"namespace", dep.Namespace, "deployment", dep.Name)
		report := make(map[string]ComponentStatus, 1)
		report[ComponentKarpenterDeployment] = m.buildDeploymentStatus(dep)
		return report, version, commit, uptimeSince
	}

	healthzSpec, readyzSpec := m.healthEndpoints(dep)
	probe := m.probePodHealth(ctx, pods, healthzSpec, readyzSpec)

	report := make(map[string]ComponentStatus, 1)
	status := m.buildDeploymentStatus(dep)

	if status.Metadata == nil {
		status.Metadata = make(map[string]string)
	}
	status.Metadata["controller_healthz"] = probe.healthz.String()
	status.Metadata["controller_readyz"] = probe.readyz.String()
	status.Metadata["probed_pods"] = strconv.Itoa(probe.attempted)

	// Annotate the message only when an endpoint actually answered not-OK, and
	// even then do not downgrade a healthy deployment: K8s replica health is the
	// authoritative signal, and the kubelet already acts on these same probes by
	// restarting or de-readying the Pod.
	//
	// An unreachable endpoint is deliberately not annotated. It says nothing
	// about the controller — a NetworkPolicy denying Pod-to-Pod traffic produces
	// it on a perfectly healthy Karpenter — so surfacing it in the message sends
	// diagnosis after a controller fault that does not exist. It stays in
	// metadata as "unknown" and is logged for whoever is actually debugging.
	switch {
	case probe.healthz == probeOutcomeNotOK || probe.readyz == probeOutcomeNotOK:
		status.Message = fmt.Sprintf("%s (controller healthz=%s readyz=%s)",
			status.Message, probe.healthz, probe.readyz)
	case probe.healthz == probeOutcomeUnknown || probe.readyz == probeOutcomeUnknown:
		m.logger.V(1).Info("Karpenter health endpoints did not answer; reporting replica health only",
			"namespace", dep.Namespace,
			"deployment", dep.Name,
			"probedPods", probe.attempted,
			"healthzURL", healthzSpec.url("<pod-ip>"),
			"readyzURL", readyzSpec.url("<pod-ip>"))
	}

	report[ComponentKarpenterDeployment] = status

	return report, version, commit, uptimeSince
}

// discoverDeployment finds the DevZero-managed Karpenter controller Deployment
// whose health this monitor reports, or (nil, nil) on a cluster with no dzKarp
// install.
//
// Two candidates is normal, not exceptional: this List is cluster-wide and
// karpenterLabelName accepts both release names, so a cluster mid-cutover
// matches the dzkarp release plus the DevZero-managed one it replaces, usually
// left scaled to zero rather than uninstalled. (A coexisting OSS release is
// excluded by isDevZeroImage, so it is never a candidate.)
//
// Which of the two gets reported must not depend on List order. A superseded
// release reports 0 ready replicas at the old chart version, and having no Pods
// it has nothing to probe either — a healthy controller then reads as down at
// the wrong version, with nothing to say a different object was measured.
// preferDeployment chooses deterministically instead.
func (m *NodeOperatorMonitor) discoverDeployment(ctx context.Context) (*appsv1.Deployment, error) {
	deployments, err := m.clientset.AppsV1().Deployments("").List(ctx, metav1.ListOptions{
		LabelSelector: karpenterLabelName,
	})
	if err != nil {
		return nil, fmt.Errorf("listing deployments with selector %q: %w", karpenterLabelName, err)
	}

	var chosen *appsv1.Deployment
	candidates := 0
	for i := range deployments.Items {
		dep := &deployments.Items[i]
		if !isDevZeroImage(dep) {
			continue
		}
		candidates++
		if chosen == nil || preferDeployment(dep, chosen) {
			chosen = dep
		}
	}

	if candidates > 1 {
		m.logger.Info("Multiple DevZero-managed Karpenter deployments found, reporting health for the preferred one",
			"candidates", candidates,
			"chosen", chosen.Namespace+"/"+chosen.Name,
			"chosenReadyReplicas", chosen.Status.ReadyReplicas)
	}

	return chosen, nil
}

// preferDeployment reports whether candidate better represents the live
// DevZero-managed Karpenter controller than current.
//
// Ordering, highest priority first:
//
//  1. A Deployment with ready replicas beats one with none. A release scaled to
//     zero is not the controller doing the work, whatever its labels say.
//  2. The more recently created one. When a cutover leaves two running
//     releases, the newer is the one being migrated to. This also applies when
//     neither is ready, so a controller genuinely failing to come up is still
//     reported rather than hidden behind an older, equally dead release.
//  3. Namespace, then name, lexicographically. Rarely reached — it takes two
//     releases created in the same second, metav1.Time being second-granular —
//     but it is what makes the result independent of List order, so reported
//     health cannot flip between collection cycles.
func preferDeployment(candidate, current *appsv1.Deployment) bool {
	candidateLive := candidate.Status.ReadyReplicas > 0
	currentLive := current.Status.ReadyReplicas > 0
	if candidateLive != currentLive {
		return candidateLive
	}
	if !candidate.CreationTimestamp.Time.Equal(current.CreationTimestamp.Time) {
		return candidate.CreationTimestamp.Time.After(current.CreationTimestamp.Time)
	}
	if candidate.Namespace != current.Namespace {
		return candidate.Namespace < current.Namespace
	}
	return candidate.Name < current.Name
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

// discoverProbeTargets returns the Pods to probe for dep's health.
//
// It probes Pods, not dep's Service, because Karpenter's Service cannot serve
// these probes: the chart publishes exactly one Service port — the metrics port,
// named "http-metrics" (8080) — while /healthz and /readyz are served on a
// separate container port (8081, named "http") that the Service never fronts.
// Probing the Service therefore failed two ways at once, and both were measured
// on a live 2/2 cluster:
//
//   - The old port lookup matched Service ports named "http" or "health". The
//     only port is "http-metrics", so it always fell through to port 8081 —
//     which is not a port on that Service, so every request was blackholed and
//     timed out. That is the reported "healthz=false readyz=false" on a healthy
//     controller, twice per cycle, forever.
//   - Had the name matched, port 8080 answers /healthz with 404: it is the
//     metrics server, not the health server.
//
// Scoping is dep's own Pod selector, which keeps the guarantee the Service
// lookup was reaching for: on a mid-cutover cluster both Karpenter releases can
// share a namespace, and probing the superseded release — which has no Pods —
// would attribute its silence to the live one.
func (m *NodeOperatorMonitor) discoverProbeTargets(ctx context.Context, dep *appsv1.Deployment) ([]corev1.Pod, error) {
	if dep.Spec.Selector == nil {
		return nil, fmt.Errorf("deployment %s/%s has no pod selector", dep.Namespace, dep.Name)
	}
	selector, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("converting pod selector of %s/%s: %w", dep.Namespace, dep.Name, err)
	}
	// An empty selector matches every Pod in the namespace. Probing unrelated
	// workloads and reporting the answer as Karpenter's is worse than not
	// probing.
	if selector.Empty() {
		return nil, fmt.Errorf("deployment %s/%s has an empty pod selector", dep.Namespace, dep.Name)
	}

	pods, err := m.clientset.CoreV1().Pods(dep.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector.String(),
	})
	if err != nil {
		return nil, fmt.Errorf("listing pods with selector %q in namespace %q: %w", selector.String(), dep.Namespace, err)
	}

	targets := make([]corev1.Pod, 0, len(pods.Items))
	for i := range pods.Items {
		if probeablePod(&pods.Items[i]) {
			targets = append(targets, pods.Items[i])
		}
	}

	// Sort before truncating so which replicas get probed does not depend on
	// List order, and a report cannot flip between cycles for that reason alone.
	sort.Slice(targets, func(i, j int) bool { return targets[i].Name < targets[j].Name })
	if len(targets) > maxProbedPods {
		targets = targets[:maxProbedPods]
	}
	return targets, nil
}

// probeablePod reports whether a Pod is one whose answer means something.
//
// A pending Pod has no IP and a terminating one is closing its listeners; the
// connection error each produces says nothing about controller health, so
// counting it as a failed probe would manufacture the same false alarm this
// monitor exists to report.
//
// Readiness is required for a subtler reason. A Pod that is Running but not yet
// Ready answers /readyz with a non-200 entirely correctly — that is what "not
// ready yet" means — and every rolling update has one for a few seconds. Probing
// it would annotate a healthy report with "readyz=false" on nothing but a
// deploy in progress. Restricting to Ready Pods also keeps the signal that is
// genuinely additive: whether replicas Kubernetes currently counts as ready can
// in fact still serve their health endpoints. A Ready Pod failing /healthz is
// real news, and arrives before the kubelet's failureThreshold restarts it.
func probeablePod(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodRunning &&
		pod.Status.PodIP != "" &&
		pod.DeletionTimestamp == nil &&
		podReady(pod)
}

func podReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// healthEndpoints derives where to probe from the controller container's own
// kubelet probes, so this monitor asks exactly what the kubelet asks rather than
// guessing. Karpenter's Deployment declares livenessProbe /healthz and
// readinessProbe /readyz against the container port named "http".
//
// Fallbacks, in order: a container port named "http" or "health" (where that
// name actually appears — it is a container port name, which is why matching it
// against Service port names never worked), then defaultHealthPort.
func (m *NodeOperatorMonitor) healthEndpoints(dep *appsv1.Deployment) (healthz, readyz probeEndpointSpec) {
	fallbackPort := m.healthPort
	if fallbackPort == "" {
		fallbackPort = defaultHealthPort
	}
	healthz = probeEndpointSpec{scheme: "http", port: fallbackPort, path: defaultHealthzPath}
	readyz = probeEndpointSpec{scheme: "http", port: fallbackPort, path: defaultReadyzPath}

	healthzFound, readyzFound := false, false
	containers := dep.Spec.Template.Spec.Containers
	for i := range containers {
		c := &containers[i]
		if !healthzFound {
			if spec, ok := endpointFromProbe(c, c.LivenessProbe, defaultHealthzPath); ok {
				healthz, healthzFound = spec, true
			}
		}
		if !readyzFound {
			if spec, ok := endpointFromProbe(c, c.ReadinessProbe, defaultReadyzPath); ok {
				readyz, readyzFound = spec, true
			}
		}
	}

	if !healthzFound || !readyzFound {
		if port, ok := namedHealthContainerPort(dep); ok {
			if !healthzFound {
				healthz.port = port
			}
			if !readyzFound {
				readyz.port = port
			}
		}
	}
	return healthz, readyz
}

// endpointFromProbe converts a container's kubelet HTTP probe into a probe
// target. Non-HTTP probes (exec, TCP) and probes naming a port the container
// does not declare are reported as unusable so the caller falls back.
func endpointFromProbe(c *corev1.Container, probe *corev1.Probe, defaultPath string) (probeEndpointSpec, bool) {
	if probe == nil || probe.HTTPGet == nil {
		return probeEndpointSpec{}, false
	}
	port, ok := resolveContainerPort(c, probe.HTTPGet.Port)
	if !ok {
		return probeEndpointSpec{}, false
	}

	spec := probeEndpointSpec{scheme: "http", port: port, path: defaultPath}
	if probe.HTTPGet.Path != "" {
		spec.path = probe.HTTPGet.Path
	}
	if probe.HTTPGet.Scheme == corev1.URISchemeHTTPS {
		spec.scheme = "https"
	}
	return spec, true
}

// resolveContainerPort turns a probe's port — a number, or the name of one of
// the container's own ports, exactly as the kubelet resolves it — into something
// dialable.
func resolveContainerPort(c *corev1.Container, target intstr.IntOrString) (string, bool) {
	if target.Type == intstr.Int {
		if target.IntVal <= 0 {
			return "", false
		}
		return strconv.Itoa(int(target.IntVal)), true
	}

	for _, p := range c.Ports {
		if p.Name == target.StrVal {
			return strconv.Itoa(int(p.ContainerPort)), true
		}
	}
	// A quoted number in the manifest decodes as a string and matches no port
	// name. The kubelet rejects it; dialing it is still better than discarding a
	// port the author plainly meant.
	if n, err := strconv.Atoi(target.StrVal); err == nil && n > 0 {
		return strconv.Itoa(n), true
	}
	return "", false
}

// namedHealthContainerPort finds a container port conventionally used for the
// health server.
func namedHealthContainerPort(dep *appsv1.Deployment) (string, bool) {
	for _, c := range dep.Spec.Template.Spec.Containers {
		for _, p := range c.Ports {
			if p.Name == "http" || p.Name == "health" {
				return strconv.Itoa(int(p.ContainerPort)), true
			}
		}
	}
	return "", false
}

// probePodHealth probes every target Pod and folds the answers together.
//
// All probes share one deadline. Each request can block for the client timeout,
// so on a cluster that drops this traffic the per-cycle cost would otherwise
// scale with replica count.
func (m *NodeOperatorMonitor) probePodHealth(
	ctx context.Context,
	pods []corev1.Pod,
	healthz, readyz probeEndpointSpec,
) podProbeResult {
	result := podProbeResult{attempted: len(pods)}
	if len(pods) == 0 {
		return result
	}

	ctx, cancel := context.WithTimeout(ctx, probeBudget)
	defer cancel()

	for i := range pods {
		ip := pods[i].Status.PodIP
		result.healthz = result.healthz.merge(m.probeEndpoint(ctx, healthz.url(ip)))
		result.readyz = result.readyz.merge(m.probeEndpoint(ctx, readyz.url(ip)))
	}
	return result
}

func (m *NodeOperatorMonitor) probeEndpoint(ctx context.Context, url string) probeOutcome {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return probeOutcomeUnknown
	}
	resp, err := m.httpClient.Do(req)
	if err != nil {
		// Nothing answered: DNS, refused, timed out, TLS. None of these are the
		// controller reporting itself unhealthy.
		return probeOutcomeUnknown
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		return probeOutcomeOK
	case http.StatusNotFound, http.StatusNotImplemented:
		// Something is listening but does not implement this path — we are
		// probing the wrong port (Karpenter's metrics port answers /healthz with
		// 404). That is a fact about our target, not about the controller.
		return probeOutcomeUnknown
	default:
		return probeOutcomeNotOK
	}
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
