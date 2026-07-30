package nodemon

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-logr/logr"
)

// JVMMetricsQuerier provides on-demand JVM metrics.
type JVMMetricsQuerier interface {
	QueryJVMMetrics(ctx context.Context) ([]JVMMetric, error)
}

type jvmMetricsFilter struct {
	Container string
	Pod       string
	Namespace string
	Node      string
}

func (f jvmMetricsFilter) matches(m *JVMMetric) bool {
	if f.Container != "" && m.Container != f.Container {
		return false
	}
	if f.Pod != "" && m.Pod != f.Pod {
		return false
	}
	if f.Namespace != "" && m.Namespace != f.Namespace {
		return false
	}
	if f.Node != "" && m.NodeName != f.Node {
		return false
	}
	return true
}

// DefaultScanHandlerTimeout is the fallback per-request time budget for the
// scan-heavy handlers (JVM and combined runtime metrics), used when a
// non-positive timeout is passed to their constructors. It exists to avoid
// a slow /proc walk or binary version-sniff stalling the HTTP server /
// readiness probes.
const DefaultScanHandlerTimeout = 2500 * time.Millisecond

type jvmMetricsHandler struct {
	querier JVMMetricsQuerier
	log     logr.Logger
	timeout time.Duration
}

// NewJVMMetricsHandler creates an HTTP handler for GET /container/jvm-metrics.
// Supports ?container=, ?pod=, ?namespace=, ?node= query filters. timeout
// bounds how long a single request may take before returning whatever
// partial data is available; a non-positive value falls back to
// DefaultScanHandlerTimeout.
func NewJVMMetricsHandler(querier JVMMetricsQuerier, log logr.Logger, timeout time.Duration) http.Handler {
	if timeout <= 0 {
		timeout = DefaultScanHandlerTimeout
	}
	return &jvmMetricsHandler{
		querier: querier,
		log:     log.WithName("jvm-metrics-handler"),
		timeout: timeout,
	}
}

func (h *jvmMetricsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Non-verbose log so we can see where requests stall.
	h.log.Info("JVMMetrics request start", "path", r.URL.Path, "rawQuery", r.URL.RawQuery)
	defer func() {
		h.log.Info("JVMMetrics request end", "took", time.Since(start).String())
	}()

	filter := jvmMetricsFilter{
		Container: r.URL.Query().Get("container"),
		Pod:       r.URL.Query().Get("pod"),
		Namespace: r.URL.Query().Get("namespace"),
		Node:      r.URL.Query().Get("node"),
	}

	// QueryJVMMetrics now just reads JVMCollector's background-refreshed cache
	// (see JVMCollector.StartCollectionLoop), so h.timeout is a defensive cap
	// rather than a bound on live /proc work.
	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()

	metrics, err := h.querier.QueryJVMMetrics(ctx)
	if err != nil {
		// Collect deliberately keeps the last good snapshot when a cycle fails
		// (see JVMCollector.Collect), so a fresh error doesn't imply there's
		// nothing usable. Only treat this as a hard failure when the cache is
		// genuinely empty; otherwise log it and serve the retained snapshot.
		if len(metrics) == 0 {
			if ctx.Err() != nil {
				h.log.Error(ctx.Err(), "Timed out querying JVM metrics")
				http.Error(w, "jvm metrics query timed out", http.StatusGatewayTimeout)
				return
			}

			h.log.Error(err, "Failed to query JVM metrics")
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		h.log.Error(err, "JVM metrics query partially failed; serving retained snapshot")
	}

	result := make([]JVMMetric, 0, len(metrics))
	for i := range metrics {
		if filter.matches(&metrics[i]) {
			result = append(result, metrics[i])
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		h.log.Error(err, "Failed to encode JVM metrics response")
	}
}
