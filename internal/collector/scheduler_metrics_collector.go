// internal/collector/scheduler_metrics_collector.go
package collector

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	telemetry_logger "github.com/devzero-inc/zxporter/internal/logger"
	"github.com/go-logr/logr"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/transport"
)

const (
	// schedulerMetricsNamespace and schedulerMetricsServiceName locate dz-scheduler's
	// Service (operators/tunr/helm/dakr/templates/scheduler.yaml), which forwards to
	// kube-scheduler's secure port (10259) -- the same port that serves /healthz and
	// /metrics together.
	schedulerMetricsNamespace   = "devzero-system"
	schedulerMetricsServiceName = "dz-scheduler"

	// schedulerMetricsLeaseName is the coordination.k8s.io/v1 Lease dz-scheduler's
	// leader election maintains in schedulerMetricsNamespace. It is a distinct object
	// from the Service above that happens to share its name -- not by leader-election
	// convention (kube-scheduler's own default lock is named "kube-scheduler" in
	// "kube-system"), but because dz-scheduler's own component config explicitly pins
	// it there: operators/tunr/helm/dakr/templates/scheduler.yaml sets
	// leaderElection.resourceName: dz-scheduler and .resourceNamespace: devzero-system.
	// That is the coupling that must not drift -- if either side is ever renamed
	// without the other, this constant and the chart fall out of sync silently (the
	// Lease Get in resolveEndpoint would just start returning NotFound, which reads as
	// the legitimate "no Lease" fallback case, not as a misconfiguration). See
	// resolveEndpoint: the Lease's holderIdentity is how this collector finds the
	// replica that is actually scheduling pods (issue #10055).
	schedulerMetricsLeaseName = "dz-scheduler"

	schedulerMetricsPath = "/metrics"

	// schedulerMetricsPortName is the named port on the dz-scheduler Service
	// (operators/tunr/helm/dakr/templates/scheduler.yaml) that forwards to the secure
	// port. resolveEndpoint matches on this name first, falling back to the first port
	// only if the name isn't found -- selecting Ports[0] unconditionally would silently
	// redirect every scrape if a port were ever added ahead of it. The matched
	// ServicePort is then read two different ways depending on which host
	// resolveEndpoint is about to scrape: its Port (443) when addressing the Service
	// VIP by DNS name (kube-proxy forwards that to the pod's TargetPort itself), or its
	// TargetPort (10259) when dialing a specific pod's IP directly, bypassing the
	// Service and kube-proxy entirely -- a pod has no listener on 443.
	schedulerMetricsPortName = "https-healthz"

	// DefaultSchedulerMetricsScrapeInterval is design decision 5 in the pipeline plan:
	// placement metrics feed a trend dashboard, not an alerting path, so 60s is fresh
	// enough.
	DefaultSchedulerMetricsScrapeInterval = 60 * time.Second

	schedulerMetricsScrapeTimeout = 10 * time.Second

	// schedulerMetricsCAFileEnv optionally names a CA bundle to verify dz-scheduler's
	// serving certificate. Unset by default because dz-scheduler's kube-scheduler
	// process currently runs with no --tls-cert-file/--tls-private-key-file (see
	// scheduler.yaml), so it serves an ephemeral self-signed certificate that nothing
	// in the pod's mounted trust bundle -- including the API server's own CA -- can
	// validate. Wiring the API server's CA in as a default here would not make the
	// connection more trustworthy; it would just make every scrape fail a handshake
	// against a certificate that CA never signed. This env var exists so that if
	// dz-scheduler is ever given a real certificate from a resolvable CA, trust can be
	// turned on without a code change.
	//
	// Turning it on would need more than a cert, though, now that resolveEndpoint
	// dials the leader pod's IP directly rather than the Service's stable DNS name
	// (issue #10055): a certificate issued for the Service's DNS name would fail
	// hostname verification against a pod IP, and pod IPs are not stable identities to
	// issue a certificate for in the first place (they change every reschedule). A real
	// fix would need per-pod SAN certificates or a SAN covering the Service DNS name
	// verified separately from the dial target -- neither exists today.
	schedulerMetricsCAFileEnv = "SCHEDULER_METRICS_CA_FILE"
)

// The five wire values of dakr's SchedulerMetricType (dakr/models/scheduler_placement_metric.go),
// plus schedulerMetricBucketLEPositiveInf, which is NOT one of them -- it is BucketLE's
// own overflow-bucket encoding (see BucketLE's doc comment on the dakr side), not a
// metric type. Duplicated here as plain strings rather than imported: zxporter is a
// separate Go module and does not depend on dakr's model package.
//
// Both sides are pinned by test, not by import: TestSchedulerMetricsCollector_AllowedFamiliesMatchesDakrIngestAllowList
// (this package) reads these five constants and asserts them against dakr's vocabulary
// set-for-set, and dakr/server/scheduler_placement_metrics_wiring_test.go's
// TestSchedulerPlacementMetricCrossModuleRoundTrip (~:354-374) pins the same vocabulary
// from dakr's side; dakr also validates a decoded metric_type against that vocabulary at
// ingest (schedulerPlacementMetricDropReason). What remains unpinned: each test's copy of
// the five-value set is a frozen literal, not a read of the other side's source, so a
// rename applied consistently to both a production constant here and its mirrored
// literal in one of those tests would still leave both suites green -- the tests catch a
// one-sided drift, not a coordinated one.
const (
	schedulerMetricTypeCounter         = "counter"
	schedulerMetricTypeGauge           = "gauge"
	schedulerMetricTypeHistogramBucket = "histogram_bucket"
	schedulerMetricTypeHistogramSum    = "histogram_sum"
	schedulerMetricTypeHistogramCount  = "histogram_count"
	schedulerMetricBucketLEPositiveInf = "+Inf"
)

// schedulerMetricsAllowedFamilies is the CLOSED set of Prometheus metric families this
// collector forwards (pipeline plan design decision 2). dz-scheduler exposes hundreds of
// series; storing whatever arrives would make cardinality a property of the agent rather
// than the schema, and would let a buggy or compromised agent write unbounded series.
var schedulerMetricsAllowedFamilies = map[string]bool{
	"dz_scheduler_plugin_score":                            true,
	"scheduler_schedule_attempts_total":                    true,
	"scheduler_framework_extension_point_duration_seconds": true,
	"scheduler_plugin_execution_duration_seconds":          true,
	"scheduler_plugin_evaluation_total":                    true,
	"scheduler_pending_pods":                               true,
}

// schedulerMetricsLabelColumns is the closed set of Prometheus label names this collector
// understands, matching dakr's SchedulerPlacementMetric six label columns (Plugin,
// ExtensionPoint, Result, Status, Profile, Queue) field-for-field. A label outside this
// set is not silently dropped -- see convertFamily -- because dropping it would collapse
// two logically distinct observations into one indistinguishable ClickHouse row with no
// error anywhere in the pipeline.
//
// This set is the union of every label the six allow-listed families (decision 2) are
// actually defined with, verified against the two source-of-truth metric definitions
// rather than assumed:
//   - devzero-inc/dz-scheduler pkg/metrics/metrics.go: PluginScore is
//     []string{"plugin"} only -- deliberately, per that file's own comment: Score runs
//     once per candidate node per pod, so a node or pod label would make cardinality
//     plugins x nodes x scheduling-rate.
//   - kubernetes/kubernetes pkg/scheduler/metrics/metrics.go, at the version
//     dz-scheduler VENDORS (its go.mod pins k8s.io/kubernetes v1.34.2), not the version
//     of the cluster's own control plane -- dz-scheduler ships its own kube-scheduler
//     binary, so these five families come from that vendored library and a customer
//     running a newer Kubernetes does not change them:
//     schedule_attempts_total {result, profile}; pending_pods {queue};
//     framework_extension_point_duration_seconds {extension_point, status, profile};
//     plugin_execution_duration_seconds {plugin, extension_point, status};
//     plugin_evaluation_total {plugin, extension_point, profile}.
//     Re-verified unchanged at v1.36.3 and v1.37.0-rc.0, so the exposure is a bump of
//     dz-scheduler's own dependency -- ours to make, in a PR -- not cluster drift.
//
// Union: {plugin, extension_point, result, status, profile, queue} -- exactly this set,
// no more and no fewer. A future family added to schedulerMetricsAllowedFamilies must
// have its label set re-derived from its own source before assuming it fits here, and
// so must a bump of dz-scheduler's vendored k8s.io/kubernetes.
//
// RE-DERIVE ON EITHER OF THOSE TWO EVENTS, because the failure is silent-by-design: a
// label ADDED to one of the five upstream families makes convertFamily reject every
// series in that family, so the metric vanishes from the dashboard with only a throttled
// telemetry report as the signal. Adding a NEW family upstream is harmless by contrast --
// scrapeOnce filters against schedulerMetricsAllowedFamilies before it ever calls
// convertFamily, so an un-allow-listed family is skipped without its labels being parsed
// and never reaches this set (v1.37's queued_entities {queue, type} is exactly that case:
// it carries an unmodeled `type` label and is inert). convertFamily itself holds no
// allow-list check -- do not read its absence there as the gate having been removed.
//
// Adding or removing dz-scheduler PLUGINS does not touch this set. A plugin name is a
// label VALUE, not a label name: every plugin observes through the one shared definition
// (dzmetrics.PluginScore.WithLabelValues(Name)), and the upstream plugin_* families are
// likewise labelled by plugin name. Plugin churn therefore changes cardinality -- more or
// fewer series under the same six names -- and `plugin` is part of the ClickHouse ORDER BY
// identity, so distinct plugins get distinct rows rather than colliding. The plugin-shaped
// risk is a new plugin registering its own new metric FAMILY, which is then dropped as
// un-allow-listed until added to both this collector's allow-list and dakr's.
var schedulerMetricsLabelColumns = map[string]bool{
	"plugin":          true,
	"extension_point": true,
	"result":          true,
	"status":          true,
	"profile":         true,
	"queue":           true,
}

// HTTPClient abstracts *http.Client for testing, mirroring nodemon.HTTPClient.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// SchedulerMetricsCollector scrapes dz-scheduler's auth-gated /metrics endpoint on a
// ticker, allow-lists the six contract Prometheus families, flattens them into rows
// matching dakr's SchedulerPlacementMetric, and emits them as
// RESOURCE_TYPE_SCHEDULER_PLACEMENT_METRIC.
//
// Availability (whether dz-scheduler's Service exists) is expressed only through
// IsAvailable, checked by the reconciler before Start is ever called -- a collector whose
// Start returns nil early is never retried (CollectionManager treats nil as success), so
// unlike karpenter_settings_collector.go's single Start-time probe, this collector also
// re-resolves the scrape target on every tick (Gatekeeper-style) so a leader failover, a
// scheduler that is later replaced, or a Service whose port changes is picked up without a
// zxporter restart. See resolveEndpoint: the target is normally the current leader pod
// (per the dz-scheduler Lease), not the Service VIP -- only the leader replica runs
// scheduleOne, so scraping the Service risks kube-proxy/keep-alive stickiness pinning the
// scrape to a standby that reports structurally-valid all-zero placement metrics forever
// (issue #10055).
type SchedulerMetricsCollector struct {
	k8sClient      kubernetes.Interface
	httpClient     HTTPClient
	scrapeInterval time.Duration

	batchChan       chan CollectedResource
	resourceChan    chan []CollectedResource
	batcher         *ResourcesBatcher
	stopCh          chan struct{}
	logger          logr.Logger
	telemetryLogger telemetry_logger.Logger

	mu sync.Mutex
	// chanMu guards batchChan against the send-after-close shutdown race: the scrape
	// loop RLocks around sends, Stop write-locks before closing.
	chanMu  sync.RWMutex
	stopped bool
}

// NewSchedulerMetricsCollector constructs the collector. httpClient must already be
// configured with dz-scheduler's authentication and TLS trust -- see
// NewSchedulerMetricsHTTPClient. scrapeInterval <= 0 falls back to
// DefaultSchedulerMetricsScrapeInterval.
func NewSchedulerMetricsCollector(
	k8sClient kubernetes.Interface,
	httpClient HTTPClient,
	scrapeInterval time.Duration,
	maxBatchSize int,
	maxBatchTime time.Duration,
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
) *SchedulerMetricsCollector {
	if scrapeInterval <= 0 {
		scrapeInterval = DefaultSchedulerMetricsScrapeInterval
	}

	batchChan := make(chan CollectedResource, 100)
	resourceChan := make(chan []CollectedResource, 100)
	batcher := NewResourcesBatcher(maxBatchSize, maxBatchTime, batchChan, resourceChan, logger)

	return &SchedulerMetricsCollector{
		k8sClient:       k8sClient,
		httpClient:      httpClient,
		scrapeInterval:  scrapeInterval,
		batchChan:       batchChan,
		resourceChan:    resourceChan,
		batcher:         batcher,
		stopCh:          make(chan struct{}),
		logger:          logger.WithName("scheduler-metrics-collector"),
		telemetryLogger: telemetryLogger,
	}
}

// NewSchedulerMetricsHTTPClient builds the authenticated HTTPS client used to scrape
// dz-scheduler's /metrics endpoint.
//
// Auth: zxporter's own ServiceAccount bearer token, via client-go's transport package so
// a projected token that rotates during the process lifetime (the common case on modern
// clusters) is picked up from disk without restarting zxporter, rather than being read
// once and cached forever.
//
// TLS trust: see schedulerMetricsCAFileEnv's doc comment for why this defaults to
// InsecureSkipVerify against dz-scheduler specifically, rather than reusing the API
// server's CA.
//
// What that trade-off actually exposes: this sends inClusterConfig.BearerToken --
// zxporter's full ServiceAccount token, default audience, not a token narrowed to this
// scrape. The devzero-zxporter-metrics-reader ClusterRole ADDS a permission to that
// identity; it does not mint a differently-scoped credential. The same token already
// authenticates zxporter's full manager-role grant (config/rbac/role.yaml): cluster-wide
// get/list/watch across pods, nodes, namespaces, endpoints, PV(C)s, and more, plus
// `secrets: create` cluster-wide and get/patch/update on the `token` secret. So a MITM
// on this connection captures a token usable for all of that against the real API
// server -- not merely "the same scrape data any legitimate client reads."
//
// Why InsecureSkipVerify is still the right call, and what actually reaches this token
// now (issue #10055 review): before that fix, resolveEndpoint always dialed the Service's
// stable DNS name, so this token could only ever land on one of dz-scheduler's own two
// replicas -- kube-proxy picks the backend, but only from the Service's own Endpoints.
// After that fix, resolveEndpoint dials the current Lease holder's pod IP directly, so
// the real exposure this client now carries is narrower than "intercept the pod network":
// it is write access to the dz-scheduler Lease's spec.holderIdentity field in
// devzero-system. Anything holding that -- confirmed to include the dakr/tunr operator's
// ClusterRole, which grants leases update/patch cluster-wide with no resourceNames
// restriction (operators/tunr/helm/dakr/templates/operator-rbac.yaml) -- can, in
// principle, point holderIdentity at a pod's name and have this client hand it zxporter's
// full ServiceAccount bearer token. schedulerVerifyPodMatchesServiceSelector (see
// resolveEndpoint) narrows this back down: it refuses to scrape a resolved pod unless the
// pod's own labels satisfy the dz-scheduler Service's selector, so exploiting this also
// requires running a correctly-labelled pod in devzero-system, not just Lease write
// access alone. That check is corroboration, not a cryptographic guarantee: a principal
// with BOTH Lease-write and pod-create/label access in devzero-system is not stopped by
// it. The actual fix, if dz-scheduler ever needs to defend against that combination
// specifically, is a projected ServiceAccount token scoped to a non-apiserver audience
// (so a token captured this way fails TokenReview if replayed against the real API
// server) -- not a TLS decision on an already self-signed endpoint. No such audience
// restriction exists today; this comment records the gap rather than papering over it.
func NewSchedulerMetricsHTTPClient() (HTTPClient, error) {
	inClusterConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("load in-cluster config: %w", err)
	}

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12} //nolint:gosec // InsecureSkipVerify set conditionally below, not unconditionally.
	if caFile := os.Getenv(schedulerMetricsCAFileEnv); caFile != "" {
		caData, err := os.ReadFile(caFile) //nolint:gosec // operator-provided path via env var, not user input.
		pool := x509.NewCertPool()
		if err == nil && pool.AppendCertsFromPEM(caData) {
			tlsConfig.RootCAs = pool
		} else {
			tlsConfig.InsecureSkipVerify = true //nolint:gosec // see schedulerMetricsCAFileEnv doc comment.
		}
	} else {
		tlsConfig.InsecureSkipVerify = true //nolint:gosec // see schedulerMetricsCAFileEnv doc comment.
	}

	authTransport, err := transport.NewBearerAuthWithRefreshRoundTripper(
		inClusterConfig.BearerToken,
		inClusterConfig.BearerTokenFile,
		&http.Transport{TLSClientConfig: tlsConfig},
	)
	if err != nil {
		return nil, fmt.Errorf("build bearer auth round tripper: %w", err)
	}

	return &http.Client{Transport: authTransport, Timeout: schedulerMetricsScrapeTimeout}, nil
}

// NewSchedulerMetricsHTTPClientOrFailing returns a working client when the in-cluster
// bearer token is resolvable, otherwise a stub whose every request fails with the
// original error. This exists so registration call sites (registerResourceCollectors and
// the pending-collector 5-min retry path) never have to handle a failed HTTP client build
// by skipping registration: the pending-retry path in particular calls IsAvailable
// directly on whatever a factory returns, with no nil check, so a factory that could
// return nil would panic there. Returning a collector that is registered but whose scrapes
// fail (logged, ticker keeps running -- see Start's doc comment) is consistent with how
// every other scrape failure in this collector is already handled, and self-heals if
// zxporter is later restarted with working in-cluster credentials.
func NewSchedulerMetricsHTTPClientOrFailing(logger logr.Logger) HTTPClient {
	httpClient, err := NewSchedulerMetricsHTTPClient()
	if err != nil {
		logger.Error(err, "Failed to build authenticated scheduler metrics HTTP client; scrapes will fail until this is resolved")
		return failingHTTPClient{err: err}
	}
	return httpClient
}

// failingHTTPClient is a HTTPClient whose every request fails with a fixed error.
type failingHTTPClient struct{ err error }

func (f failingHTTPClient) Do(*http.Request) (*http.Response, error) {
	return nil, f.err
}

// IsAvailable reports whether dz-scheduler's Service exists in devzero-system. This is
// what makes install order work: the reconciler retries an unavailable collector with a
// factory every 5 minutes (see registerResourceCollectors), so zxporter can be installed
// before dz-scheduler and pick the collector up once the scheduler is deployed.
func (c *SchedulerMetricsCollector) IsAvailable(ctx context.Context) bool {
	_, err := c.k8sClient.CoreV1().Services(schedulerMetricsNamespace).Get(ctx, schedulerMetricsServiceName, metav1.GetOptions{})
	return err == nil
}

// resolveEndpoint re-fetches dz-scheduler's Service and Lease and builds the /metrics URL
// to scrape. Called on every tick rather than cached once, so a leader failover, a Service
// that is deleted/recreated, or a port change is reflected without a zxporter restart.
//
// Target selection (issue #10055 -- see the package doc comment above for why the Service
// VIP is unsafe to scrape unconditionally):
//   - Lease absent: falls back to the Service VIP URL (schemeless DNS name, Service Port).
//     Under dz-scheduler's own chart this is NOT the single-replica case -- leaderElect is
//     hardcoded true regardless of replica count (scheduler.yaml), so a Lease exists even
//     at replicas: 1. The branch is reachable in practice for a hand-rolled, non-chart
//     dz-scheduler deployment with leader election disabled outright, and briefly on a
//     freshly created chart install before the first election completes. Logged at V(1) by
//     the caller either way. This is the ONLY case that uses the Service VIP.
//   - Lease present, holder resolves to a pod with an IP that also matches the Service's
//     own selector (schedulerVerifyPodMatchesServiceSelector): scrapes that pod's IP
//     directly, on the Service's TargetPort (the pod has no listener on the Service's own
//     Port; see schedulerMetricsPortName's doc comment).
//   - Lease present but the holder cannot be resolved to a usable, Service-backing pod
//     (holder empty, pod missing, pod has no IP yet, pod doesn't match the Service
//     selector, or the target port can't be resolved): returns an error so the scrape is
//     skipped for this tick. This deliberately does NOT fall back to the Service VIP --
//     doing so would silently reproduce the exact bug this function exists to fix, by
//     scraping whichever replica kube-proxy happens to pick.
func (c *SchedulerMetricsCollector) resolveEndpoint(ctx context.Context) (string, error) {
	svc, err := c.k8sClient.CoreV1().Services(schedulerMetricsNamespace).Get(ctx, schedulerMetricsServiceName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("resolve dz-scheduler service %s/%s: %w", schedulerMetricsNamespace, schedulerMetricsServiceName, err)
	}
	if len(svc.Spec.Ports) == 0 {
		return "", fmt.Errorf("dz-scheduler service %s/%s has no ports", schedulerMetricsNamespace, schedulerMetricsServiceName)
	}

	svcPort := svc.Spec.Ports[0]
	for _, p := range svc.Spec.Ports {
		if p.Name == schedulerMetricsPortName {
			svcPort = p
			break
		}
	}

	lease, err := c.k8sClient.CoordinationV1().Leases(schedulerMetricsNamespace).Get(ctx, schedulerMetricsLeaseName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return fmt.Sprintf("https://%s.%s.svc:%d%s", svc.Name, svc.Namespace, svcPort.Port, schedulerMetricsPath), nil
	}
	if err != nil {
		// Deliberately not a fallback to the Service VIP -- see doc comment above.
		return "", fmt.Errorf("resolve dz-scheduler lease %s/%s: %w", schedulerMetricsNamespace, schedulerMetricsLeaseName, err)
	}

	// HolderIdentity is a *string (unset means no leader has been elected yet);
	// schedulerLeaseHolderPodName treats "" as its own error case, so a nil pointer and
	// an explicit empty string are handled identically here.
	holderIdentity := ""
	if lease.Spec.HolderIdentity != nil {
		holderIdentity = *lease.Spec.HolderIdentity
	}

	podName, err := schedulerLeaseHolderPodName(holderIdentity)
	if err != nil {
		return "", fmt.Errorf("resolve leader pod from dz-scheduler lease %s/%s holder %q: %w",
			schedulerMetricsNamespace, schedulerMetricsLeaseName, holderIdentity, err)
	}

	pod, err := c.k8sClient.CoreV1().Pods(schedulerMetricsNamespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("resolve dz-scheduler leader pod %s/%s (lease holder %q): %w",
			schedulerMetricsNamespace, podName, holderIdentity, err)
	}

	// Verify the resolved pod is actually a backend of the dz-scheduler Service before
	// trusting its IP enough to send it zxporter's full ServiceAccount bearer token (see
	// NewSchedulerMetricsHTTPClient's doc comment for what that token exposes). Without
	// this, the Lease's holderIdentity alone decides where the token goes -- and that
	// field isn't only ever written by dz-scheduler's own two replicas (issue #10055
	// review). Reads the Service's own selector rather than hardcoding a label like
	// `component: scheduler`, so this holds regardless of which labels a particular
	// chart version uses, and needs no new RBAC.
	if err := schedulerVerifyPodMatchesServiceSelector(svc, pod); err != nil {
		return "", fmt.Errorf("dz-scheduler leader pod %s/%s (lease holder %q) is not a Service backend: %w",
			schedulerMetricsNamespace, podName, holderIdentity, err)
	}

	if pod.Status.PodIP == "" {
		return "", fmt.Errorf("dz-scheduler leader pod %s/%s (lease holder %q) has no pod IP yet",
			schedulerMetricsNamespace, podName, holderIdentity)
	}

	targetPort, err := schedulerLeaderTargetPort(pod, svcPort.TargetPort)
	if err != nil {
		return "", fmt.Errorf("resolve scrape port on dz-scheduler leader pod %s/%s (lease holder %q): %w",
			schedulerMetricsNamespace, podName, holderIdentity, err)
	}

	return fmt.Sprintf("https://%s:%d%s", pod.Status.PodIP, targetPort, schedulerMetricsPath), nil
}

// schedulerLeaseHolderPodName extracts the pod name from a Lease's holderIdentity.
// client-go's leaderelection package writes holderIdentity as
// "<hostname>_<random-uuid>" (LeaseSpec.HolderIdentity's own doc comment: this string
// only needs to be unique per election, but the resourcelock implementations all build it
// this way, and dz-scheduler's pod IS its own hostname). Everything before the LAST
// underscore is the pod name. First-vs-last is actually equivalent for any real pod: a
// Kubernetes pod name is an RFC 1123 DNS subdomain, which cannot contain "_" at all, so
// the hostname component here never contains one. Splitting on the LAST underscore is
// still the defensive choice, because it is the split that stays correct even for a
// holder that ISN'T a real pod name -- a hand-rolled resourcelock, or a value nobody
// should trust as well-formed in the first place, where the "pod names can't have
// underscores" assumption doesn't apply. A holder with no underscore at all is used
// as-is (a hand-rolled or future resourcelock implementation might not append a UUID). An
// empty holder, or a holder whose derived pod name comes out empty (e.g. a holder that IS
// just "_" or starts with "_"), is an error -- there is no leader pod name to resolve, and
// silently falling through to Pods().Get(ctx, "") would ask the API server a different,
// confusing question instead of failing here with a clear reason.
//
// If Kubernetes' CoordinatedLeaderElection feature is ever enabled for dz-scheduler,
// holderIdentity stops being client-go's "<hostname>_<uuid>" and instead becomes
// whatever the CoordinatedLeaderElection controller copies from the winning
// LeaseCandidate's name -- a different format this parsing rule has not been verified
// against. dz-scheduler's own ClusterRole already grants leasecandidates permissions
// (scheduler.yaml), so the binary supports the feature even though nothing enables it
// today; re-verify this function if that ever changes.
func schedulerLeaseHolderPodName(holderIdentity string) (string, error) {
	if holderIdentity == "" {
		return "", fmt.Errorf("lease has no holderIdentity (no leader elected yet)")
	}
	podName := holderIdentity
	if idx := strings.LastIndex(holderIdentity, "_"); idx >= 0 {
		podName = holderIdentity[:idx]
	}
	if podName == "" {
		return "", fmt.Errorf("lease holderIdentity %q has no pod name before the last underscore", holderIdentity)
	}
	return podName, nil
}

// schedulerVerifyPodMatchesServiceSelector confirms pod is a backend of svc by checking
// that every key/value pair in svc.Spec.Selector is present in pod.Labels, presence and
// value both -- the same has-the-key-with-this-value membership test the Kubernetes
// endpoints controller's own label selector (k8s.io/apimachinery/pkg/labels) uses. A
// pod's label being ABSENT and a pod's label being present with an empty value are
// distinct outcomes here on purpose: collapsing them (comparing pod.Labels[key] against
// wantValue without checking ok) would let a pod that was never labelled at all pass
// against any selector whose value happens to be the empty string, which Kubernetes
// permits. This is the mitigation for the exposure documented on
// NewSchedulerMetricsHTTPClient: without it, resolveEndpoint
// would trust the Lease's holderIdentity to name ANY pod in schedulerMetricsNamespace and
// send it zxporter's full ServiceAccount bearer token, rather than only a pod dz-scheduler's
// own Service would actually route traffic to. Deliberately reads the Service's selector
// instead of a hardcoded label set (`app: dz-scheduler` would look plausible but does not
// exist -- the real selector is `component: scheduler, tier: control-plane`), so this
// stays correct across chart versions with no code change.
//
// An empty or nil Service selector matches nothing here on purpose: a Service with no
// selector has no defined backends of its own (it is typically paired with manually
// managed Endpoints/EndpointSlices instead), so there is no basis to call any pod "the"
// backend by this test, and treating an empty selector as "matches everything" would
// defeat the point of the check entirely.
func schedulerVerifyPodMatchesServiceSelector(svc *corev1.Service, pod *corev1.Pod) error {
	if len(svc.Spec.Selector) == 0 {
		return fmt.Errorf("service %s/%s has no selector, so no pod can be verified as its backend", svc.Namespace, svc.Name)
	}
	for key, wantValue := range svc.Spec.Selector {
		// Presence-checked, not just compared: a plain `pod.Labels[key] != wantValue`
		// reads a missing key as Go's zero value "", so a selector whose value is
		// itself the empty string (legal in Kubernetes) would let a pod that lacks the
		// label ENTIRELY pass as a match. That would collapse "this pod carries the
		// label with an empty value" and "this pod was never labelled at all" into the
		// same accepted case -- exactly the absent-vs-empty distinction the real
		// endpoints controller's Matches (k8s.io/apimachinery/pkg/labels) does not
		// collapse either.
		gotValue, ok := pod.Labels[key]
		if !ok || gotValue != wantValue {
			return fmt.Errorf("pod %s/%s label %q=%q (present=%t) does not match service %s/%s selector %q=%q",
				pod.Namespace, pod.Name, key, gotValue, ok, svc.Namespace, svc.Name, key, wantValue)
		}
	}
	return nil
}

// schedulerLeaderTargetPort resolves the Service's targetPort to a concrete container port
// on the leader pod. An int targetPort (dz-scheduler's Service uses one -- see
// schedulerMetricsPortName's doc comment) is used as-is. A named targetPort is resolved
// against the pod's own container ports, exactly as the kubelet/kube-proxy would; if the
// name isn't found there, that is an error rather than a guess -- unlike the Service-VIP
// fallback path, there is no safe default to fall back to when scraping a pod directly.
//
// dz-scheduler's actual container (scheduler.yaml) declares no `ports:` at all today --
// its livenessProbe references port 10259 by number, not by name -- so the named-port
// branch below is currently unreachable in production and only exercised by test. If a
// future chart revision ever switches the Service's targetPort to a name without also
// adding a matching named containerPort, every scrape would fail with this function's
// error on every tick, not silently degrade.
func schedulerLeaderTargetPort(pod *corev1.Pod, targetPort intstr.IntOrString) (int32, error) {
	if targetPort.Type == intstr.Int {
		return targetPort.IntVal, nil
	}

	name := targetPort.StrVal
	for _, container := range pod.Spec.Containers {
		for _, p := range container.Ports {
			if p.Name == name {
				return p.ContainerPort, nil
			}
		}
	}
	return 0, fmt.Errorf("named target port %q not found on any container of pod %s/%s", name, pod.Namespace, pod.Name)
}

// Start begins the scrape ticker. A scrape failure (endpoint resolution, HTTP, or parse)
// logs and the loop continues to the next tick -- never fatal, since a transient scheduler
// restart (routine under the #9848 settings design, which restarts dz-scheduler on every
// settings change) should not take this collector down.
func (c *SchedulerMetricsCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting SchedulerMetrics collector", "interval", c.scrapeInterval)

	c.batcher.start()

	stopCh := c.stopCh
	go func() {
		ticker := time.NewTicker(c.scrapeInterval)
		defer ticker.Stop()
		// Scrape once immediately rather than waiting a full interval for the first
		// data point.
		c.scrapeOnce(ctx)
		for {
			select {
			case <-ticker.C:
				c.scrapeOnce(ctx)
			case <-ctx.Done():
				c.Stop() //nolint:errcheck
				return
			case <-stopCh:
				return
			}
		}
	}()

	return nil
}

// scrapeOnce resolves the current endpoint, scrapes it, and emits every allow-listed row.
// Every failure path logs and returns rather than propagating -- see Start's doc comment.
func (c *SchedulerMetricsCollector) scrapeOnce(ctx context.Context) {
	scrapeCtx, cancel := context.WithTimeout(ctx, schedulerMetricsScrapeTimeout)
	defer cancel()

	endpoint, err := c.resolveEndpoint(scrapeCtx)
	if err != nil {
		// V(1), not V(4): V(4) is routinely suppressed at the configured log level, and
		// this is the failure mode for "dz-scheduler's Service was deleted after this
		// collector started" -- silent at V(4), that reads as a healthy collector that
		// has actually gone permanently dark.
		c.logger.V(1).Info("Skipping scheduler metrics scrape, endpoint not resolvable", "error", err.Error())
		return
	}

	families, err := c.scrape(scrapeCtx, endpoint)
	if err != nil {
		c.logger.Error(err, "Scheduler metrics scrape failed")
		return
	}

	scrapedAt := time.Now().UTC()
	for _, mf := range families {
		if !schedulerMetricsAllowedFamilies[mf.GetName()] {
			continue
		}
		c.convertFamily(mf, scrapedAt)
	}
}

// scrape performs one authenticated HTTP GET against endpoint and parses the response as
// Prometheus text-format metrics.
func (c *SchedulerMetricsCollector) scrape(ctx context.Context, endpoint string) (map[string]*dto.MetricFamily, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build scrape request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("scrape %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("scrape %s: unexpected status %d", endpoint, resp.StatusCode)
	}

	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("parse metrics from %s: %w", endpoint, err)
	}
	return families, nil
}

// convertFamily flattens one allow-listed Prometheus metric family into per-series wire
// items and emits each on the batch channel. A series carrying a label outside the six
// modeled columns is rejected and logged with the offending label name (see
// schedulerMetricsLabelColumns' doc comment); rejecting one series does not affect its
// siblings in the same family or scrape.
func (c *SchedulerMetricsCollector) convertFamily(mf *dto.MetricFamily, scrapedAt time.Time) {
	name := mf.GetName()
	for _, m := range mf.GetMetric() {
		labels, err := schedulerMetricLabels(name, m.GetLabel())
		if err != nil {
			c.logger.Error(err, "Rejecting scheduler metric series with an unmodeled label", "metric", name)
			// A contract violation, not a transient scrape hiccup: a series carrying a
			// label this collector doesn't model means either dz-scheduler started
			// exposing a new dimension or the allow-list/label-column set has drifted
			// out of sync with it. Silent-in-pod-logs is not enough for that -- a
			// cluster where every series in a family is rejected looks identical to a
			// healthy one from outside the pod, so it goes through the same telemetry
			// path the reconciler already uses for collector-health signals.
			c.telemetryLogger.Report(gen.LogLevel_LOG_LEVEL_ERROR, "SchedulerMetricsCollector_convertFamily",
				"Rejecting scheduler metric series with an unmodeled label", err,
				map[string]string{"metric_family": name})
			continue
		}

		switch mf.GetType() {
		case dto.MetricType_COUNTER:
			c.emit(schedulerMetricItem(scrapedAt, name, schedulerMetricTypeCounter, labels, nil, m.GetCounter().GetValue()))
		case dto.MetricType_GAUGE:
			c.emit(schedulerMetricItem(scrapedAt, name, schedulerMetricTypeGauge, labels, nil, m.GetGauge().GetValue()))
		case dto.MetricType_HISTOGRAM:
			c.emitHistogram(m.GetHistogram(), scrapedAt, name, labels)
		default:
			c.logger.Error(fmt.Errorf("unsupported metric type %s", mf.GetType()), "Rejecting scheduler metric family with an unsupported type", "metric", name)
			return
		}
	}
}

// emitHistogram flattens one histogram series into its bucket rows (one per `le`,
// including the +Inf overflow bucket) plus its sum and count rows -- never
// pre-aggregated, per pipeline plan design decision 3: quantiles are not averageable, so
// collapsing buckets at the agent would make later cross-cluster percentiles impossible.
func (c *SchedulerMetricsCollector) emitHistogram(hist *dto.Histogram, scrapedAt time.Time, name string, labels map[string]string) {
	// Bucket.CumulativeCountFloat/Histogram.SampleCountFloat exist on dto for OpenMetrics
	// native (exponential) histograms, whose float counters override the integer ones
	// when set. expfmt's text-format parser (the only parser this collector uses -- see
	// scrape) never populates them; kube-scheduler and dz-scheduler both expose classic
	// bucketed histograms over the text format, not native ones. Reading them here would
	// be dead code.
	for _, bucket := range hist.GetBucket() {
		le := bucket.GetUpperBound()
		c.emit(schedulerMetricItem(scrapedAt, name, schedulerMetricTypeHistogramBucket, labels, &le, float64(bucket.GetCumulativeCount())))
	}

	c.emit(schedulerMetricItem(scrapedAt, name, schedulerMetricTypeHistogramSum, labels, nil, hist.GetSampleSum()))
	c.emit(schedulerMetricItem(scrapedAt, name, schedulerMetricTypeHistogramCount, labels, nil, float64(hist.GetSampleCount())))
}

// emit sends one flattened row to the batch channel.
//
// The channel send is inside a select with c.stopCh rather than a bare blocking send:
// batchChan has a fixed 100-item buffer and a single scrape of all six families can
// exceed that, so if the batcher/downstream consumer ever stalls, a bare `c.batchChan <-
// resource` would block forever while still holding chanMu's read lock -- and Stop()
// would then deadlock waiting for chanMu's write lock to close batchChan, wedging this
// collector's goroutine permanently (the ticker never gets to observe ctx.Done(), and
// the pod eventually dies by SIGKILL rather than a clean shutdown). The select's
// <-c.stopCh case is always eventually ready, because Stop() closes stopCh before it
// ever tries to acquire chanMu -- so holding the read lock across this particular select
// cannot reproduce that deadlock: the moment Stop() runs, any in-flight emit
// unconditionally unblocks (abandoning at most the one item it was sending) and releases
// the lock, letting Stop() proceed to close batchChan.
func (c *SchedulerMetricsCollector) emit(item map[string]interface{}) {
	// A NaN or +/-Inf value fails json.Marshal ("json: unsupported value") deep inside
	// SendResourceBatch's structpb conversion (dakr_client.go's toStructpb), where the
	// failure is caught and the one item is silently `continue`d past -- no error
	// anywhere in the pipeline, the same failure mode bucket_le's string encoding exists
	// to avoid. Rejecting here, loudly, keeps that contract: a bad row never reaches the
	// wire path where it would vanish without a trace.
	if value, ok := item["value"].(float64); !ok || math.IsNaN(value) || math.IsInf(value, 0) {
		c.logger.Error(fmt.Errorf("non-finite value %v", item["value"]), "Rejecting scheduler metric row with a non-finite value",
			"metric", item["metric_name"], "metric_type", item["metric_type"])
		return
	}

	c.chanMu.RLock()
	defer c.chanMu.RUnlock()
	if c.stopped {
		return
	}
	scrapedAt, _ := item["scraped_at"].(time.Time)
	resource := CollectedResource{
		ResourceType: SchedulerPlacementMetric,
		Object:       item,
		Timestamp:    scrapedAt,
		EventType:    EventTypeMetrics,
		Key:          schedulerMetricKey(scrapedAt, item),
	}
	select {
	case c.batchChan <- resource:
	case <-c.stopCh:
	}
}

// schedulerMetricLabels extracts the six modeled label columns from a Prometheus series'
// label pairs. Returns an error naming the offending label if any label falls outside the
// closed set in schedulerMetricsLabelColumns -- silently dropping it would let two
// logically distinct series collapse into one indistinguishable ClickHouse row with no
// error anywhere in the pipeline.
func schedulerMetricLabels(metricFamily string, pairs []*dto.LabelPair) (map[string]string, error) {
	labels := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		name := pair.GetName()
		if !schedulerMetricsLabelColumns[name] {
			return nil, fmt.Errorf("scheduler metric %q carries unmodeled label %q", metricFamily, name)
		}
		labels[name] = pair.GetValue()
	}
	return labels, nil
}

// schedulerMetricItem builds one wire row matching dakr's SchedulerPlacementMetric
// field-for-field (dakr/models/scheduler_placement_metric.go). bucketLE is nil for every
// MetricType except histogram_bucket. The overflow bucket (bucketLE == +Inf) is encoded
// as the JSON string "+Inf" rather than a JSON number: Go's encoding/json cannot marshal
// math.Inf as a number (json: unsupported value: +Inf), and this is exactly the wire form
// dakr's model.UnmarshalJSON expects.
//
// Deliberately omits cluster_id and ingested_at: cluster_id is stamped by dakr from
// request metadata, never trusted from the payload, and ingested_at is stamped at insert
// -- see dakr/models/scheduler_placement_metric.go's doc comments.
func schedulerMetricItem(
	scrapedAt time.Time,
	metricName string,
	metricType string,
	labels map[string]string,
	bucketLE *float64,
	value float64,
) map[string]interface{} {
	item := map[string]interface{}{
		"scraped_at":      scrapedAt,
		"metric_name":     metricName,
		"metric_type":     metricType,
		"plugin":          labels["plugin"],
		"extension_point": labels["extension_point"],
		"result":          labels["result"],
		"status":          labels["status"],
		"profile":         labels["profile"],
		"queue":           labels["queue"],
		"value":           value,
	}

	switch {
	case bucketLE == nil:
		item["bucket_le"] = 0.0
	case math.IsInf(*bucketLE, 1):
		item["bucket_le"] = schedulerMetricBucketLEPositiveInf
	default:
		item["bucket_le"] = *bucketLE
	}

	return item
}

// schedulerMetricKey builds a per-row identity for the CollectedResource envelope.
// bucket_le is included so the multiple histogram_bucket rows a single series produces
// (one per `le`) get distinct keys despite sharing every other field.
func schedulerMetricKey(scrapedAt time.Time, item map[string]interface{}) string {
	return fmt.Sprintf("scheduler-metric/%v/%v/%v/%v/%v/%v/%v/%v/%v/%d",
		item["metric_name"], item["metric_type"],
		item["plugin"], item["extension_point"], item["result"], item["status"], item["profile"], item["queue"],
		item["bucket_le"],
		scrapedAt.UnixNano(),
	)
}

// Stop shuts down the collector.
func (c *SchedulerMetricsCollector) Stop() error {
	c.logger.Info("Stopping SchedulerMetrics collector")
	c.mu.Lock()
	defer c.mu.Unlock()

	select {
	case <-c.stopCh:
	default:
		close(c.stopCh)
	}

	c.chanMu.Lock()
	if !c.stopped {
		c.stopped = true
		close(c.batchChan)
	}
	c.chanMu.Unlock()

	if c.batcher != nil {
		c.batcher.stop()
	}
	return nil
}

// GetResourceChannel returns the output channel for batched resources.
func (c *SchedulerMetricsCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the string key used to identify this collector.
func (c *SchedulerMetricsCollector) GetType() string {
	return SchedulerPlacementMetric.String()
}

// AddResource implements ResourceCollector for manual injection (used in tests).
func (c *SchedulerMetricsCollector) AddResource(resource interface{}) error {
	item, ok := resource.(map[string]interface{})
	if !ok {
		return fmt.Errorf("expected map[string]interface{}, got %T", resource)
	}
	c.emit(item)
	return nil
}
