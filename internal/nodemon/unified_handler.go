package nodemon

import (
	"encoding/json"
	"net/http"

	"github.com/go-logr/logr"
)

// UnifiedQuerier provides merged metrics from all sources.
type UnifiedQuerier interface {
	QueryContainerMetrics() []ContainerMetricsResponse
	QueryNodeMetrics() *NodeMetricsResponse
	QueryPVCMetrics() []PVCMetricsResponse
}

// unifiedContainerHandler serves GET /v2/container/metrics.
type unifiedContainerHandler struct {
	querier UnifiedQuerier
	log     logr.Logger
}

// NewUnifiedContainerHandler creates an HTTP handler for GET /v2/container/metrics.
// Supports query parameters: namespace, pod, container.
func NewUnifiedContainerHandler(querier UnifiedQuerier, log logr.Logger) http.Handler {
	return &unifiedContainerHandler{
		querier: querier,
		log:     log.WithName("unified-container-handler"),
	}
}

func (h *unifiedContainerHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	namespace := r.URL.Query().Get("namespace")
	pod := r.URL.Query().Get("pod")
	container := r.URL.Query().Get("container")

	all := h.querier.QueryContainerMetrics()
	result := filterContainerMetrics(all, namespace, pod, container)

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		h.log.Error(err, "Failed to encode unified container metrics response")
	}
}

// filterContainerMetrics returns the subset of metrics matching all non-empty filter values.
func filterContainerMetrics(metrics []ContainerMetricsResponse, namespace, pod, container string) []ContainerMetricsResponse {
	if namespace == "" && pod == "" && container == "" {
		return metrics
	}
	result := make([]ContainerMetricsResponse, 0, len(metrics))
	for _, m := range metrics {
		if namespace != "" && m.Namespace != namespace {
			continue
		}
		if pod != "" && m.Pod != pod {
			continue
		}
		if container != "" && m.Container != container {
			continue
		}
		result = append(result, m)
	}
	return result
}

// nodeMetricsHandler serves GET /node/metrics.
type nodeMetricsHandler struct {
	querier UnifiedQuerier
	log     logr.Logger
}

// NewNodeMetricsHandler creates an HTTP handler for GET /node/metrics.
func NewNodeMetricsHandler(querier UnifiedQuerier, log logr.Logger) http.Handler {
	return &nodeMetricsHandler{
		querier: querier,
		log:     log.WithName("node-metrics-handler"),
	}
}

func (h *nodeMetricsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	node := h.querier.QueryNodeMetrics()
	if node == nil {
		http.Error(w, "node metrics not available", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(node); err != nil {
		h.log.Error(err, "Failed to encode node metrics response")
	}
}

// pvcMetricsHandler serves GET /pvc/metrics.
type pvcMetricsHandler struct {
	querier UnifiedQuerier
	log     logr.Logger
}

// NewPVCMetricsHandler creates an HTTP handler for GET /pvc/metrics.
// Supports query parameter: namespace.
func NewPVCMetricsHandler(querier UnifiedQuerier, log logr.Logger) http.Handler {
	return &pvcMetricsHandler{
		querier: querier,
		log:     log.WithName("pvc-metrics-handler"),
	}
}

func (h *pvcMetricsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	namespace := r.URL.Query().Get("namespace")
	all := h.querier.QueryPVCMetrics()
	result := filterPVCMetrics(all, namespace)

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		h.log.Error(err, "Failed to encode PVC metrics response")
	}
}

// filterPVCMetrics returns the subset of PVC metrics matching the given namespace (empty = all).
func filterPVCMetrics(metrics []PVCMetricsResponse, namespace string) []PVCMetricsResponse {
	if namespace == "" {
		return metrics
	}
	result := make([]PVCMetricsResponse, 0, len(metrics))
	for _, m := range metrics {
		if m.Namespace == namespace {
			result = append(result, m)
		}
	}
	return result
}
