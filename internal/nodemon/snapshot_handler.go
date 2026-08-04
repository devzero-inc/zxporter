package nodemon

import (
	"encoding/json"
	"net/http"

	"github.com/go-logr/logr"
)

// NodeSnapshotQuerier reads the last published node snapshot.
type NodeSnapshotQuerier interface {
	QueryNodeSnapshot() (*NodeMetricsResponse, SnapshotSectionStatus)
}

// GPUSnapshotQuerier reads the last published node GPU summary.
type GPUSnapshotQuerier interface {
	QueryGPUSnapshot() (*NodeGPUSummary, SnapshotSectionStatus)
}

// ContainerSnapshotQuerier reads the last published container snapshot.
type ContainerSnapshotQuerier interface {
	QueryContainerSnapshot() ([]ContainerMetricsResponse, SnapshotSectionStatus)
}

// RuntimeSnapshotQuerier reads the last published process-runtime snapshot.
type RuntimeSnapshotQuerier interface {
	QueryRuntimeSnapshot() (RuntimeMetrics, SnapshotSectionStatus)
}

type nodeSnapshotHandler struct {
	node NodeSnapshotQuerier
	gpu  GPUSnapshotQuerier
	log  logr.Logger
}

// NewNodeSnapshotHandler serves cache-only node and GPU snapshot responses.
func NewNodeSnapshotHandler(
	node NodeSnapshotQuerier,
	gpu GPUSnapshotQuerier,
	log logr.Logger,
) http.Handler {
	return &nodeSnapshotHandler{
		node: node,
		gpu:  gpu,
		log:  log.WithName("node-snapshot-handler"),
	}
}

func (h *nodeSnapshotHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet {
		writeSnapshotError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	nodeMetrics, nodeStatus := h.node.QueryNodeSnapshot()
	gpuSummary, gpuStatus := h.gpu.QueryGPUSnapshot()
	response := NodeSnapshotResponse{
		SchemaVersion: SnapshotSchemaVersion,
		Sections: NodeSnapshotSections{
			Node: nodeStatus,
			GPU:  gpuStatus,
		},
	}
	if snapshotSectionHasPayload(nodeStatus.State) {
		response.NodeMetrics = nodeMetrics
	}
	if snapshotSectionHasPayload(gpuStatus.State) {
		response.GPUSummary = gpuSummary
	}

	status := http.StatusOK
	if !snapshotSectionUsable(nodeStatus.State) && !snapshotSectionUsable(gpuStatus.State) {
		status = http.StatusServiceUnavailable
	}
	writeSnapshotJSON(w, status, response, h.log)
}

type containerSnapshotHandler struct {
	containers ContainerSnapshotQuerier
	runtime    RuntimeSnapshotQuerier
	log        logr.Logger
}

// NewContainerSnapshotHandler serves cache-only container and runtime snapshot responses.
func NewContainerSnapshotHandler(
	containers ContainerSnapshotQuerier,
	runtime RuntimeSnapshotQuerier,
	log logr.Logger,
) http.Handler {
	return &containerSnapshotHandler{
		containers: containers,
		runtime:    runtime,
		log:        log.WithName("container-snapshot-handler"),
	}
}

type containerSnapshotWireResponse struct {
	SchemaVersion    int                         `json:"schema_version"`
	ContainerMetrics *[]ContainerMetricsResponse `json:"container_metrics,omitempty"`
	RuntimeMetrics   *RuntimeMetrics             `json:"runtime_metrics,omitempty"`
	Sections         ContainerSnapshotSections   `json:"sections"`
}

func (h *containerSnapshotHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet {
		writeSnapshotError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	containerMetrics, containerStatus := h.containers.QueryContainerSnapshot()
	runtimeStatus := SnapshotSectionStatus{State: SnapshotStateDisabled}
	var runtimeMetrics RuntimeMetrics
	if h.runtime != nil {
		runtimeMetrics, runtimeStatus = h.runtime.QueryRuntimeSnapshot()
	}

	response := containerSnapshotWireResponse{
		SchemaVersion: SnapshotSchemaVersion,
		Sections: ContainerSnapshotSections{
			Containers: containerStatus,
			Runtime:    runtimeStatus,
		},
	}
	if snapshotSectionHasPayload(containerStatus.State) {
		if containerMetrics == nil {
			containerMetrics = []ContainerMetricsResponse{}
		}
		response.ContainerMetrics = &containerMetrics
	}
	if snapshotSectionHasPayload(runtimeStatus.State) {
		if runtimeMetrics.JVM == nil {
			runtimeMetrics.JVM = []JVMMetric{}
		}
		if runtimeMetrics.Runtimes == nil {
			runtimeMetrics.Runtimes = []RuntimeProcessMetric{}
		}
		response.RuntimeMetrics = &runtimeMetrics
	}

	status := http.StatusOK
	if !snapshotSectionUsable(containerStatus.State) && !snapshotSectionUsable(runtimeStatus.State) {
		status = http.StatusServiceUnavailable
	}
	writeSnapshotJSON(w, status, response, h.log)
}

func snapshotSectionHasPayload(state SnapshotSectionState) bool {
	return state == SnapshotStateReady || state == SnapshotStateStale
}

func snapshotSectionUsable(state SnapshotSectionState) bool {
	return snapshotSectionHasPayload(state) || state == SnapshotStateDisabled
}

func writeSnapshotJSON(w http.ResponseWriter, status int, response any, log logr.Logger) {
	data, err := json.Marshal(response)
	if err != nil {
		log.Error(err, "Failed to encode snapshot response")
		writeSnapshotError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	w.WriteHeader(status)
	if _, err := w.Write(data); err != nil {
		log.Error(err, "Failed to write snapshot response")
	}
}

func writeSnapshotError(w http.ResponseWriter, status int, message string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
