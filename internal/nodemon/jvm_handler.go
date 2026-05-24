package nodemon

import (
	"context"
	"encoding/json"
	"net/http"

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

type jvmMetricsHandler struct {
	querier JVMMetricsQuerier
	log     logr.Logger
}

// NewJVMMetricsHandler creates an HTTP handler for GET /container/jvm-metrics.
// Supports ?container=, ?pod=, ?namespace=, ?node= query filters.
func NewJVMMetricsHandler(querier JVMMetricsQuerier, log logr.Logger) http.Handler {
	return &jvmMetricsHandler{
		querier: querier,
		log:     log.WithName("jvm-metrics-handler"),
	}
}

func (h *jvmMetricsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	filter := jvmMetricsFilter{
		Container: r.URL.Query().Get("container"),
		Pod:       r.URL.Query().Get("pod"),
		Namespace: r.URL.Query().Get("namespace"),
		Node:      r.URL.Query().Get("node"),
	}

	metrics, err := h.querier.QueryJVMMetrics(r.Context())
	if err != nil {
		h.log.Error(err, "Failed to query JVM metrics")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	result := make([]JVMMetric, 0, len(metrics))
	for i := range metrics {
		if filter.matches(&metrics[i]) {
			result = append(result, metrics[i])
		}
	}

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		h.log.Error(err, "Failed to encode JVM metrics response")
	}
}
