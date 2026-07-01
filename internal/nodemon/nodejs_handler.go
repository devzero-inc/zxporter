package nodemon

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-logr/logr"
)

// NodeJSMetricsQuerier provides on-demand Node.js metrics.
type NodeJSMetricsQuerier interface {
	QueryNodeJSMetrics(ctx context.Context) ([]NodeJSMetric, error)
}

type nodeJSMetricsFilter struct {
	Container string
	Pod       string
	Namespace string
	Node      string
}

func (f nodeJSMetricsFilter) matches(m *NodeJSMetric) bool {
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

type nodeJSMetricsHandler struct {
	querier NodeJSMetricsQuerier
	log     logr.Logger
}

// NewNodeJSMetricsHandler creates an HTTP handler for GET /container/nodejs-metrics.
// Supports ?container=, ?pod=, ?namespace=, ?node= query filters.
func NewNodeJSMetricsHandler(querier NodeJSMetricsQuerier, log logr.Logger) http.Handler {
	return &nodeJSMetricsHandler{
		querier: querier,
		log:     log.WithName("nodejs-metrics-handler"),
	}
}

func (h *nodeJSMetricsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	h.log.Info("NodeJSMetrics request start", "path", r.URL.Path, "rawQuery", r.URL.RawQuery)
	defer func() {
		h.log.Info("NodeJSMetrics request end", "took", time.Since(start).String())
	}()

	filter := nodeJSMetricsFilter{
		Container: r.URL.Query().Get("container"),
		Pod:       r.URL.Query().Get("pod"),
		Namespace: r.URL.Query().Get("namespace"),
		Node:      r.URL.Query().Get("node"),
	}

	// Hard cap to avoid stalling the HTTP server / probes, matching the JVM handler.
	ctx, cancel := context.WithTimeout(r.Context(), 2500*time.Millisecond)
	defer cancel()

	metrics, err := h.querier.QueryNodeJSMetrics(ctx)
	if err != nil {
		if ctx.Err() != nil {
			h.log.Error(ctx.Err(), "Timed out querying Node.js metrics")
			http.Error(w, "nodejs metrics query timed out", http.StatusGatewayTimeout)
			return
		}

		h.log.Error(err, "Failed to query Node.js metrics")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	result := make([]NodeJSMetric, 0, len(metrics))
	for i := range metrics {
		if filter.matches(&metrics[i]) {
			result = append(result, metrics[i])
		}
	}

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		h.log.Error(err, "Failed to encode Node.js metrics response")
	}
}
