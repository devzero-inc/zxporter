// internal/collector/keda_scaler_metrics_collector.go
//
// KedaScalerMetricsCollector scrapes the KEDA operator's Prometheus /metrics
// endpoint on a configurable ticker interval, parses keda_scaler_metrics_value
// and keda_scaler_active, joins them by matching label sets, resolves the
// workload kind/name from the ScaledObject's scaleTargetRef, and forwards the
// resulting ScalerMetricSample batch to dakr via SendScalerMetrics.
//
// IsAvailable returns false when the configured endpoint is unreachable so the
// collector is silently skipped on non-KEDA clusters.
package collector

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	telemetry_logger "github.com/devzero-inc/zxporter/internal/logger"
	"github.com/go-logr/logr"
	kedaclient "github.com/kedacore/keda/v2/pkg/generated/clientset/versioned"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"google.golang.org/protobuf/types/known/timestamppb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// DefaultKedaMetricsEndpoint is the in-cluster address for the KEDA operator
	// metrics endpoint. DevZero installs KEDA into the devzero-system namespace and
	// renames the operator Service to dzkeda-operator (dzkeda.operator.name in the
	// dakr chart's values.yaml — a DevZero name-prefix footprint), so that is the
	// primary default. Override via KedaScalerMetricsConfig.Endpoint (wired from
	// the KEDA_METRICS_ENDPOINT env var / CollectionPolicy) when KEDA runs in a
	// different namespace or on a different port. Keep the Service name here in
	// sync with dzkeda.operator.name in operators/tunr/helm/dakr/values.yaml.
	DefaultKedaMetricsEndpoint = "http://dzkeda-operator.devzero-system.svc.cluster.local:8080/metrics"

	// DefaultKedaScraperInterval is the default scrape cadence.
	DefaultKedaScraperInterval = 30 * time.Second

	// kedaScalerMetricValueName is the Prometheus metric name for current scaler values.
	kedaScalerMetricValueName = "keda_scaler_metrics_value"

	// kedaScalerActiveName is the Prometheus metric name for scaler active state.
	kedaScalerActiveName = "keda_scaler_active"

	// defaultScaleTargetKind is the workload kind KEDA assumes when the
	// ScaledObject's scaleTargetRef.kind field is omitted.
	defaultScaleTargetKind = "Deployment"

	// maxMetricsResponseBytes caps how much of the KEDA /metrics response is read
	// into the parser, bounding worst-case memory if the endpoint returns a
	// pathologically large body (thousands of scalers, or a misbehaving
	// operator). A realistic KEDA exposition is well under a few MB; this is a
	// safety ceiling, not a normal-operation limit. Exceeding it truncates the
	// stream — the already-parsed families still return.
	maxMetricsResponseBytes = 16 << 20 // 16 MiB

	// emptyWithSOsLogEvery rate-limits the "reachable but exposing no scaler
	// samples despite ScaledObjects existing" diagnostic (see classifyScrapeOutcome).
	// Log the first tick of a streak, then once per this many scrapes (~10 min at
	// the 30s default) while it persists — visible enough to notice, quiet enough
	// not to spam.
	emptyWithSOsLogEvery = 20
)

// scrapeOutcome classifies a completed scrape so the collector can log the
// actionable case loudly and the benign case quietly. It is derived purely from
// two counts the scrape already has, so it is unit-testable without HTTP or a
// Kubernetes client.
type scrapeOutcome int

const (
	// outcomeSent: at least one scaler sample was produced and will be forwarded.
	outcomeSent scrapeOutcome = iota
	// outcomeIdle: no samples AND no ScaledObjects — an empty result is expected
	// (idle or non-KEDA-workload cluster). Benign; logged at debug.
	outcomeIdle
	// outcomeMisconfigured: no samples BUT ScaledObjects exist — the scraped
	// operator is reachable yet not exposing keda_scaler_metrics_value, so the
	// scaler-metrics graph stays silently empty. This is the exact signature of a
	// broken/duplicate operator or disabled Prometheus scaler metrics, so it is
	// surfaced at a visible level instead of hidden at debug.
	outcomeMisconfigured
)

// classifyScrapeOutcome decides how a scrape result should be reported from the
// number of emitted samples and the number of ScaledObjects present.
func classifyScrapeOutcome(numSamples, numScaledObjects int) scrapeOutcome {
	if numSamples > 0 {
		return outcomeSent
	}
	if numScaledObjects > 0 {
		return outcomeMisconfigured
	}
	return outcomeIdle
}

// KedaScalerMetricsConfig holds the configurable parameters for the scraper.
type KedaScalerMetricsConfig struct {
	// Endpoint is the full URL to the KEDA operator /metrics endpoint.
	// Defaults to DefaultKedaMetricsEndpoint.
	Endpoint string

	// Interval is the scrape cadence.  Defaults to DefaultKedaScraperInterval.
	Interval time.Duration
}

// scalerSender is the subset of DakrClient that KedaScalerMetricsCollector needs.
// Using a narrow interface makes the collector easier to test.
type scalerSender interface {
	SendScalerMetrics(ctx context.Context, clusterID, teamID string, samples []*gen.ScalerMetricSample) error
}

// scaledObjectKey identifies a ScaledObject by namespace+name.
type scaledObjectKey struct {
	Namespace string
	Name      string
}

// workloadRef holds the resolved workload identity.
type workloadRef struct {
	Kind string
	Name string
}

// KedaScalerMetricsCollector implements ResourceCollector by scraping the KEDA
// operator metrics endpoint and forwarding scaler sample batches to dakr.
type KedaScalerMetricsCollector struct {
	config          KedaScalerMetricsConfig
	kedaClient      kedaclient.Interface
	sender          scalerSender
	resourceChan    chan []CollectedResource
	stopCh          chan struct{}
	mu              sync.Mutex
	logger          logr.Logger
	telemetryLogger telemetry_logger.Logger
	httpClient      *http.Client

	// emptyWithSOsStreak counts consecutive scrapes that returned zero scaler
	// samples while ScaledObjects existed — the actionable misconfiguration
	// signature (see classifyScrapeOutcome). Written only from the single scrape
	// goroutine started by Start, so it needs no lock. Used to rate-limit the
	// diagnostic log via emptyWithSOsLogEvery.
	emptyWithSOsStreak int
}

// NewKedaScalerMetricsCollector creates a new collector with the provided config.
func NewKedaScalerMetricsCollector(
	cfg KedaScalerMetricsConfig,
	kedaClient kedaclient.Interface,
	sender scalerSender,
	logger logr.Logger,
	telemetryLogger telemetry_logger.Logger,
) *KedaScalerMetricsCollector {
	if cfg.Endpoint == "" {
		cfg.Endpoint = DefaultKedaMetricsEndpoint
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultKedaScraperInterval
	}

	return &KedaScalerMetricsCollector{
		config:     cfg,
		kedaClient: kedaClient,
		sender:     sender,
		// This collector emits directly via sender.SendScalerMetrics and never
		// writes to resourceChan; the channel exists only to satisfy the
		// ResourceCollector interface, so it needs no real buffer.
		resourceChan:    make(chan []CollectedResource, 1),
		stopCh:          make(chan struct{}),
		logger:          logger.WithName("keda-scaler-metrics-collector"),
		telemetryLogger: telemetryLogger,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// Start launches the scrape loop in a background goroutine and returns
// immediately. The CollectionManager calls Start synchronously and only starts
// the collector's channel processor after Start returns (see manager.go
// startCollectorInternal), so Start MUST NOT block — a blocking Start stalls the
// caller (e.g. StartCollector via retryPendingCollectors) indefinitely. This
// matches the convention of the other streaming collectors (e.g.
// ScaledObjectCollector). The loop runs until the context is cancelled or Stop
// closes stopCh.
func (c *KedaScalerMetricsCollector) Start(ctx context.Context) error {
	c.logger.Info("Starting KEDA scaler metrics scraper",
		"endpoint", c.config.Endpoint,
		"interval", c.config.Interval.String())

	go func() {
		ticker := time.NewTicker(c.config.Interval)
		defer ticker.Stop()

		// Run the first scrape immediately so we do not wait a full interval.
		c.scrape(ctx)

		for {
			select {
			case <-ticker.C:
				c.scrape(ctx)
			case <-c.stopCh:
				c.logger.Info("KEDA scaler metrics collector stopped")
				return
			case <-ctx.Done():
				c.logger.Info("KEDA scaler metrics collector context cancelled")
				return
			}
		}
	}()

	return nil
}

// Stop signals the scrape loop to exit.
func (c *KedaScalerMetricsCollector) Stop() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	select {
	case <-c.stopCh:
		// already closed
	default:
		close(c.stopCh)
	}
	return nil
}

// GetResourceChannel returns the (unused) resource channel.
// This collector emits data directly via the sender rather than through the
// combined channel, but it must satisfy the ResourceCollector interface.
func (c *KedaScalerMetricsCollector) GetResourceChannel() <-chan []CollectedResource {
	return c.resourceChan
}

// GetType returns the string identifier for this collector type.
func (c *KedaScalerMetricsCollector) GetType() string {
	return "keda_scaler_metrics"
}

// IsAvailable reports whether the KEDA operator metrics endpoint is present.
// Availability is defined by reachability, NOT by HTTP status: any completed
// response — including a 405 to our HEAD probe — proves the operator's HTTP
// server is answering, so we return true. Only a transport-level error (DNS,
// connection refused, timeout) means the endpoint is absent, which is the
// non-KEDA-cluster case that silently skips registration. Do not "fix" this into
// checking resp.StatusCode: a 4xx/5xx still means the endpoint exists.
func (c *KedaScalerMetricsCollector) IsAvailable(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.config.Endpoint, nil)
	if err != nil {
		c.logger.V(1).Info("KEDA metrics endpoint HEAD request creation failed", "error", err.Error())
		return false
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Kept at debug on purpose: this collector is registered unconditionally
		// and skipped on non-KEDA clusters, so a higher level would log on every
		// pending-retry (~5m) across the fleet's non-KEDA clusters. The message
		// names the usual cause so it is actionable once debug is enabled.
		c.logger.V(1).Info("KEDA operator metrics endpoint unreachable — scaler metrics not collected "+
			"(expected when KEDA is not installed; if KEDA IS installed, enable its Prometheus metrics via "+
			"keda.prometheus.operator.enabled=true so the operator exposes :8080)",
			"endpoint", c.config.Endpoint, "error", err.Error())
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return true
}

// AddResource is a no-op: this collector does not watch Kubernetes resources.
func (c *KedaScalerMetricsCollector) AddResource(resource interface{}) error {
	return fmt.Errorf("KedaScalerMetricsCollector does not support AddResource")
}

// ─── internal helpers ────────────────────────────────────────────────────────

// scrape performs one full scrape-parse-resolve-send cycle.
func (c *KedaScalerMetricsCollector) scrape(ctx context.Context) {
	mfs, err := c.fetchMetrics(ctx)
	if err != nil {
		c.logger.Error(err, "Failed to fetch KEDA metrics")
		return
	}

	// Build the ScaledObject workload resolution map before parsing so we can
	// annotate each sample with its workload identity.  If resolution fails we
	// skip the entire scrape cycle: emitting samples with blank
	// workload_kind/workload_name would poison dakr's GetByWorkload lookup, so a
	// missed cycle is strictly better than sending unattributed samples.
	resolveMap, err := c.buildResolveMap(ctx)
	if err != nil {
		c.logger.Error(err, "Failed to list ScaledObjects for workload resolution; skipping scrape cycle")
		return
	}

	samples := parseScalerMetrics(mfs, resolveMap)

	// Classify the result so the actionable failure is not silent. resolveMap is
	// the ScaledObject inventory we already listed above, so this costs nothing
	// extra. "No samples but ScaledObjects exist" is the exact signature that made
	// the scaler-metrics graph sit empty for hours: the scraped operator was
	// reachable (IsAvailable passed) but never recorded keda_scaler_metrics_value
	// because its ServiceAccount had no RBAC, and a duplicate KEDA operator was
	// quietly winning the external.metrics APIService. Previously this branch was
	// an unconditional debug line that hid exactly that state.
	switch classifyScrapeOutcome(len(samples), len(resolveMap)) {
	case outcomeMisconfigured:
		c.emptyWithSOsStreak++
		if c.emptyWithSOsStreak == 1 || c.emptyWithSOsStreak%emptyWithSOsLogEvery == 0 {
			c.logger.Info("KEDA operator is reachable and has ScaledObjects, but exposed no "+
				kedaScalerMetricValueName+" samples — the scaler-metrics graph will stay empty. Verify: "+
				"(1) the scraped operator is the ACTIVE KEDA operator and its ServiceAccount has RBAC to "+
				"query scalers (list/watch scaledobjects, read trigger-auth secrets) — a broken/absent RBAC "+
				"makes the operator's metric-serving path error before it records keda_scaler_metrics_value; "+
				"(2) exactly ONE KEDA operator is installed — a leftover/duplicate operator can win the "+
				"external.metrics APIService while the scraped one stays idle; "+
				"(3) Prometheus scaler metrics are enabled (dzkeda.prometheus.operator.enabled=true).",
				"scaledObjects", len(resolveMap),
				"endpoint", c.config.Endpoint,
				"consecutiveEmptyScrapes", c.emptyWithSOsStreak)
		}
		return
	case outcomeIdle:
		c.emptyWithSOsStreak = 0
		c.logger.V(1).Info("No KEDA scaler metric samples after parsing, skipping send (no ScaledObjects present)")
		return
	}

	// outcomeSent: at least one attributable sample. Reset the streak so a later
	// misconfiguration logs from its first tick again.
	c.emptyWithSOsStreak = 0

	// cluster_id and team_id are injected by the transport layer via context
	// (the same mechanism used by SendClusterSnapshotStream).  Pass empty strings
	// here so the RealDakrClient can resolve them from the context values.
	if err := c.sender.SendScalerMetrics(ctx, "", "", samples); err != nil {
		c.logger.Error(err, "Failed to send KEDA scaler metrics", "samples", len(samples))
	} else {
		c.logger.V(1).Info("Sent KEDA scaler metrics", "samples", len(samples))
	}
}

// fetchMetrics GETs the /metrics endpoint and parses the Prometheus exposition.
func (c *KedaScalerMetricsCollector) fetchMetrics(ctx context.Context) (map[string]*dto.MetricFamily, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.config.Endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching KEDA metrics: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected HTTP status from KEDA metrics endpoint: %d", resp.StatusCode)
	}

	// Bound the bytes read into the parser so a pathologically large exposition
	// cannot balloon the collector's memory.
	return parsePrometheusExposition(io.LimitReader(resp.Body, maxMetricsResponseBytes), resp.Header.Get("Content-Type"))
}

// parsePrometheusExposition reads a Prometheus text exposition from r.
//
// The contentType argument is currently ignored: KEDA serves the classic
// Prometheus text format (text/plain; version=0.0.4), which this text parser
// handles. TODO: if KEDA ever negotiates OpenMetrics
// (application/openmetrics-text), dispatch on contentType to an OpenMetrics
// parser here — the text parser does not accept that format.
func parsePrometheusExposition(r io.Reader, _ string) (map[string]*dto.MetricFamily, error) {
	parser := expfmt.NewTextParser(model.UTF8Validation)
	mfs, err := parser.TextToMetricFamilies(r)
	if err != nil && len(mfs) == 0 {
		return nil, fmt.Errorf("parsing Prometheus exposition: %w", err)
	}
	return mfs, nil
}

// buildResolveMap lists all ScaledObjects across all namespaces and constructs
// a (namespace, name) -> workloadRef lookup table.
//
// Scale note (what to do on larger clusters):
// This issues a full ScaledObjects("").List() on every scrape (~30s). It
// retains nothing — the listed objects are GC'd once the small map is built —
// so steady-state memory is flat regardless of cluster size; there is no leak
// or unbounded growth. The only costs are a transient per-scrape allocation and
// a periodic LIST against the kube-apiserver. Both are negligible at typical
// scale (~4 KB for a handful of ScaledObjects) and stay small into the
// hundreds. If a deployment ever runs many *thousands* of ScaledObjects and the
// every-30s LIST becomes an apiserver concern (API load, not zxporter memory),
// two options in preference order:
//
//  1. Cache this resolve map with a short TTL (refresh every ~5m instead of
//     every scrape). Retains only the tiny ns->workloadRef map, cuts the LIST
//     and the transient allocation ~10x, and stays self-contained. Cost: a new
//     ScaledObject's samples are unattributed (skipped) for up to the TTL.
//  2. Read from a shared ScaledObject informer cache. Zero per-scrape
//     allocation and zero LIST — but zxporter has no shared informer factory
//     (each collector owns its own), so this means either a *second* ScaledObject
//     informer (a duplicate permanent cache — worse for memory) or wiring
//     KedaScaledObjectCollector's informer through to here, which couples the two
//     collectors' lifecycles and namespace scoping. Only worth it as a genuine
//     shared-informer refactor, not a per-collector duplicate.
//
// Do NOT add a per-collector informer purely for consistency with the
// watch-based collectors: this is a pull-based scraper, so an always-on cache
// would raise steady-state memory to buy churn/API savings that only matter at
// extreme scale.
func (c *KedaScalerMetricsCollector) buildResolveMap(ctx context.Context) (map[scaledObjectKey]workloadRef, error) {
	list, err := c.kedaClient.KedaV1alpha1().ScaledObjects("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	m := make(map[scaledObjectKey]workloadRef, len(list.Items))
	for i := range list.Items {
		so := &list.Items[i]
		if so.Spec.ScaleTargetRef == nil {
			continue
		}
		kind := so.Spec.ScaleTargetRef.Kind
		if kind == "" {
			kind = defaultScaleTargetKind // KEDA defaults to Deployment when Kind is omitted
		}
		m[scaledObjectKey{Namespace: so.Namespace, Name: so.Name}] = workloadRef{
			Kind: kind,
			Name: so.Spec.ScaleTargetRef.Name,
		}
	}
	return m, nil
}

// labelValue extracts a label's value from a Prometheus Metric, returning ""
// if the label is absent.
func labelValue(m *dto.Metric, name string) string {
	for _, lp := range m.GetLabel() {
		if lp.GetName() == name {
			return lp.GetValue()
		}
	}
	return ""
}

// triggerIndex resolves the trigger index from either the modern "triggerIndex"
// label or the legacy "scalerIndex" label.  Returns -1 when neither is present
// or when the value cannot be parsed.
func triggerIndex(m *dto.Metric) int32 {
	// Prefer the modern label name first.
	for _, candidate := range []string{"triggerIndex", "scalerIndex"} {
		v := labelValue(m, candidate)
		if v == "" {
			continue
		}
		n, err := strconv.ParseInt(v, 10, 32)
		if err == nil {
			return int32(n)
		}
	}
	return -1
}

// scalerMetricKey is a composite key that uniquely identifies one
// (scaledObject, namespace, scaler, triggerIndex, metric) tuple.
type scalerMetricKey struct {
	scaledObject string
	namespace    string
	scaler       string
	tidx         int32
	metric       string
}

// parseScalerMetrics converts the raw MetricFamily map into a flat list of
// ScalerMetricSamples, joining keda_scaler_metrics_value with keda_scaler_active
// on a matching composite key.
//
// A sample is emitted ONLY when its ScaledObject resolves to a workload in
// resolveMap.  Any unresolved ScaledObject — and every sample when resolveMap is
// nil or empty — is skipped, so a sample with blank workload_kind/workload_name
// is never produced (that would poison dakr's GetByWorkload lookup).
func parseScalerMetrics(
	mfs map[string]*dto.MetricFamily,
	resolveMap map[scaledObjectKey]workloadRef,
) []*gen.ScalerMetricSample {
	ts := timestamppb.Now()

	// Collect gauge values indexed by composite key. The key already carries
	// every identifying field (scaledObject, namespace, scaler, trigger index,
	// metric), so the map stores only the value — no redundant per-series copy of
	// those strings.
	values := make(map[scalerMetricKey]float64)

	if vMF, ok := mfs[kedaScalerMetricValueName]; ok {
		for _, m := range vMF.GetMetric() {
			so := labelValue(m, "scaledObject")
			ns := labelValue(m, "namespace")
			scaler := labelValue(m, "scaler")
			metricLabel := labelValue(m, "metric")
			tidx := triggerIndex(m)

			if so == "" || ns == "" {
				continue
			}

			k := scalerMetricKey{scaledObject: so, namespace: ns, scaler: scaler, tidx: tidx, metric: metricLabel}
			values[k] = m.GetGauge().GetValue()
		}
	}

	// Build an active-state index.
	active := make(map[scalerMetricKey]bool)
	if aMF, ok := mfs[kedaScalerActiveName]; ok {
		for _, m := range aMF.GetMetric() {
			so := labelValue(m, "scaledObject")
			ns := labelValue(m, "namespace")
			scaler := labelValue(m, "scaler")
			metricLabel := labelValue(m, "metric")
			tidx := triggerIndex(m)

			if so == "" || ns == "" {
				continue
			}

			k := scalerMetricKey{scaledObject: so, namespace: ns, scaler: scaler, tidx: tidx, metric: metricLabel}
			active[k] = m.GetGauge().GetValue() == 1
		}
	}

	samples := make([]*gen.ScalerMetricSample, 0, len(values))
	for k, v := range values {
		soKey := scaledObjectKey{Namespace: k.namespace, Name: k.scaledObject}
		// Require a resolvable workload identity. A nil/empty resolveMap, or a
		// ScaledObject missing from it, means we cannot attribute the sample —
		// skip it rather than emit blank workload_kind/workload_name to dakr.
		ref, ok := resolveMap[soKey]
		if !ok {
			continue
		}

		s := &gen.ScalerMetricSample{
			Scaledobject: k.scaledObject,
			Namespace:    k.namespace,
			WorkloadKind: ref.Kind,
			WorkloadName: ref.Name,
			TriggerIndex: k.tidx,
			MetricName:   k.metric,
			ScalerType:   k.scaler,
			Value:        v,
			// A value row with no matching keda_scaler_active row defaults to
			// false: KEDA omits keda_scaler_active for a scaler it hasn't
			// evaluated yet, which means the scaler is not active.
			Active: active[k],
			Ts:     ts,
		}
		samples = append(samples, s)
	}
	return samples
}
