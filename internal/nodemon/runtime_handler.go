package nodemon

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-logr/logr"
)

// RuntimeMetricsQuerier provides on-demand combined JVM + Node.js metrics.
type RuntimeMetricsQuerier interface {
	QueryRuntimeMetrics(ctx context.Context) (RuntimeMetrics, error)
}

// matchesRuntimeProcess applies the same container/pod/namespace/node query
// filter to a generic-runtime metric.
func (f jvmMetricsFilter) matchesRuntimeProcess(m *RuntimeProcessMetric) bool {
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

type runtimeMetricsHandler struct {
	querier RuntimeMetricsQuerier
	log     logr.Logger
}

// NewRuntimeMetricsHandler creates an HTTP handler for GET /container/runtime-metrics,
// the combined JVM + Node.js endpoint backed by a single /proc walk. Supports the
// same ?container=, ?pod=, ?namespace=, ?node= query filters as the legacy
// /container/jvm-metrics endpoint, applied to both slices.
func NewRuntimeMetricsHandler(querier RuntimeMetricsQuerier, log logr.Logger) http.Handler {
	return &runtimeMetricsHandler{
		querier: querier,
		log:     log.WithName("runtime-metrics-handler"),
	}
}

func (h *runtimeMetricsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	h.log.Info("RuntimeMetrics request start", "path", r.URL.Path, "rawQuery", r.URL.RawQuery)
	defer func() {
		h.log.Info("RuntimeMetrics request end", "took", time.Since(start).String())
	}()

	jvmFilter := jvmMetricsFilter{
		Container: r.URL.Query().Get("container"),
		Pod:       r.URL.Query().Get("pod"),
		Namespace: r.URL.Query().Get("namespace"),
		Node:      r.URL.Query().Get("node"),
	}

	// Hard cap to avoid stalling the HTTP server / probes, matching the legacy handlers.
	ctx, cancel := context.WithTimeout(r.Context(), 2500*time.Millisecond)
	defer cancel()

	metrics, err := h.querier.QueryRuntimeMetrics(ctx)
	if err != nil {
		// QueryRuntimeMetrics attempts JVM and Node.js independently and joins
		// their errors, so a failure in one doesn't discard data the other side
		// successfully built. Only treat this as a hard failure when there's
		// nothing usable at all; otherwise log it and serve the partial result.
		if len(metrics.JVM) == 0 && len(metrics.Runtimes) == 0 {
			if ctx.Err() != nil {
				h.log.Error(ctx.Err(), "Timed out querying runtime metrics")
				http.Error(w, "runtime metrics query timed out", http.StatusGatewayTimeout)
				return
			}

			h.log.Error(err, "Failed to query runtime metrics")
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		h.log.Error(err, "Runtime metrics query partially failed; serving partial results")
	}

	result := RuntimeMetrics{
		JVM:      make([]JVMMetric, 0, len(metrics.JVM)),
		Runtimes: make([]RuntimeProcessMetric, 0, len(metrics.Runtimes)),
	}
	for i := range metrics.JVM {
		if jvmFilter.matches(&metrics.JVM[i]) {
			result.JVM = append(result.JVM, metrics.JVM[i])
		}
	}
	for i := range metrics.Runtimes {
		if jvmFilter.matchesRuntimeProcess(&metrics.Runtimes[i]) {
			result.Runtimes = append(result.Runtimes, metrics.Runtimes[i])
		}
	}

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		h.log.Error(err, "Failed to encode runtime metrics response")
	}
}
