package collector

import (
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	maxWireLayers      = 50
	maxWireWastedFiles = 20
	maxWireFilePath    = 120
)

// ImageAnalysisWireFormat is the trimmed payload sent over the wire to dakr.
// It strips unnecessary detail (full workloadReferences, excess layers/files)
// to keep compressed size under ~50KB per CRD.
type ImageAnalysisWireFormat struct {
	ImageRef       string `json:"imageRef"`
	ImageDigest    string `json:"imageDigest"`
	ImageSizeBytes int64  `json:"imageSizeBytes,omitempty"`

	// Core analysis metrics
	Efficiency        string `json:"efficiency"`
	WastedBytes       int64  `json:"wastedBytes"`
	UserWastedPercent string `json:"userWastedPercent"`
	Passed            bool   `json:"passed"`
	LayerCount        int    `json:"layerCount"`

	// Trimmed detail arrays
	Layers      []LayerWire      `json:"layers,omitempty"`
	WastedFiles []WastedFileWire `json:"wastedFiles,omitempty"`

	// File analysis aggregate
	FileAnalysis *FileAnalysisWire `json:"fileAnalysis,omitempty"`

	// Workload summary (aggregate only, no individual refs)
	WorkloadSummary WorkloadSummaryWire `json:"workloadSummary"`

	// Metadata
	ImageSourceType      string `json:"imageSourceType,omitempty"`
	AnalyzedAt           string `json:"analyzedAt"`
	AnalysisDurationSecs int    `json:"analysisDurationSeconds,omitempty"`
}

// LayerWire is the wire representation of a single image layer.
type LayerWire struct {
	Index     int    `json:"index"`
	Digest    string `json:"digest,omitempty"`
	SizeBytes int64  `json:"sizeBytes"`
	Command   string `json:"command,omitempty"`
}

// WastedFileWire is the wire representation of a wasted file entry.
type WastedFileWire struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"sizeBytes"`
}

// FileAnalysisWire is the wire representation of aggregate file analysis.
type FileAnalysisWire struct {
	TotalFiles    int `json:"totalFiles,omitempty"`
	ModifiedFiles int `json:"modifiedFiles,omitempty"`
	AddedFiles    int `json:"addedFiles,omitempty"`
	RemovedFiles  int `json:"removedFiles,omitempty"`
}

// WorkloadSummaryWire is the wire representation of the workload summary.
type WorkloadSummaryWire struct {
	TotalContainers   int      `json:"totalContainers,omitempty"`
	TotalWorkloads    int      `json:"totalWorkloads,omitempty"`
	NodesRunningImage int      `json:"nodesRunningImage,omitempty"`
	Namespaces        []string `json:"namespaces,omitempty"`
}

// ToImageAnalysisWireFormat extracts and trims an ImageAnalysisResult CRD
// into a compact wire format suitable for gRPC transport.
func ToImageAnalysisWireFormat(obj *unstructured.Unstructured) (*ImageAnalysisWireFormat, error) {
	spec, found, err := unstructured.NestedMap(obj.Object, "spec")
	if err != nil || !found {
		return nil, fmt.Errorf("missing spec in ImageAnalysisResult: %w", err)
	}

	status, _, _ := unstructured.NestedMap(obj.Object, "status")

	wf := &ImageAnalysisWireFormat{}

	// Top-level fields
	wf.ImageRef, _, _ = unstructured.NestedString(spec, "imageRef")
	wf.ImageDigest, _, _ = unstructured.NestedString(spec, "imageDigest")
	wf.ImageSizeBytes, _, _ = unstructured.NestedInt64(spec, "imageSizeBytes")

	// Image source
	wf.ImageSourceType, _, _ = unstructured.NestedString(spec, "imageSource", "type")

	// Analysis fields
	analysis, _, _ := unstructured.NestedMap(spec, "analysis")
	if analysis != nil {
		wf.Efficiency, _, _ = unstructured.NestedString(analysis, "efficiency")
		wf.WastedBytes, _, _ = unstructured.NestedInt64(analysis, "wastedBytes")
		wf.UserWastedPercent, _, _ = unstructured.NestedString(analysis, "userWastedPercent")
		wf.Passed, _, _ = unstructured.NestedBool(analysis, "passed")
		wf.LayerCount = nestedInt(analysis, "layerCount")

		// Extract layers (capped)
		wf.Layers = extractLayers(analysis)

		// Extract wasted files (capped)
		wf.WastedFiles = extractWastedFiles(analysis)

		// Extract file analysis
		wf.FileAnalysis = extractFileAnalysis(analysis)
	}

	// Workload summary (aggregate only — drop individual workloadReferences)
	wf.WorkloadSummary = extractWorkloadSummary(spec)

	// Status fields
	if status != nil {
		wf.AnalyzedAt, _, _ = unstructured.NestedString(status, "analyzedAt")
		wf.AnalysisDurationSecs = nestedInt(status, "analysisDurationSeconds")
	}

	return wf, nil
}

func extractLayers(analysis map[string]interface{}) []LayerWire {
	raw, found, _ := unstructured.NestedSlice(analysis, "layers")
	if !found {
		return nil
	}

	limit := len(raw)
	if limit > maxWireLayers {
		limit = maxWireLayers
	}

	layers := make([]LayerWire, 0, limit)
	for i := 0; i < limit; i++ {
		m, ok := raw[i].(map[string]interface{})
		if !ok {
			continue
		}
		l := LayerWire{
			Index: nestedInt(m, "index"),
		}
		l.Digest, _, _ = unstructured.NestedString(m, "digest")
		l.SizeBytes, _, _ = unstructured.NestedInt64(m, "sizeBytes")
		l.Command, _, _ = unstructured.NestedString(m, "command")
		layers = append(layers, l)
	}
	return layers
}

func extractWastedFiles(analysis map[string]interface{}) []WastedFileWire {
	raw, found, _ := unstructured.NestedSlice(analysis, "wastedFiles")
	if !found {
		return nil
	}

	limit := len(raw)
	if limit > maxWireWastedFiles {
		limit = maxWireWastedFiles
	}

	files := make([]WastedFileWire, 0, limit)
	for i := 0; i < limit; i++ {
		m, ok := raw[i].(map[string]interface{})
		if !ok {
			continue
		}
		path, _, _ := unstructured.NestedString(m, "path")
		if len(path) > maxWireFilePath {
			path = path[:maxWireFilePath]
		}
		size, _, _ := unstructured.NestedInt64(m, "sizeBytes")
		files = append(files, WastedFileWire{Path: path, SizeBytes: size})
	}
	return files
}

func extractFileAnalysis(analysis map[string]interface{}) *FileAnalysisWire {
	fa, found, _ := unstructured.NestedMap(analysis, "fileAnalysis")
	if !found || fa == nil {
		return nil
	}
	return &FileAnalysisWire{
		TotalFiles:    nestedInt(fa, "totalFiles"),
		ModifiedFiles: nestedInt(fa, "modifiedFiles"),
		AddedFiles:    nestedInt(fa, "addedFiles"),
		RemovedFiles:  nestedInt(fa, "removedFiles"),
	}
}

func extractWorkloadSummary(spec map[string]interface{}) WorkloadSummaryWire {
	ws, found, _ := unstructured.NestedMap(spec, "workloadSummary")
	if !found || ws == nil {
		return WorkloadSummaryWire{}
	}

	summary := WorkloadSummaryWire{
		TotalContainers:   nestedInt(ws, "totalContainers"),
		TotalWorkloads:    nestedInt(ws, "totalWorkloads"),
		NodesRunningImage: nestedInt(ws, "nodesRunningImage"),
	}

	nsSlice, found, _ := unstructured.NestedStringSlice(ws, "namespaces")
	if found {
		summary.Namespaces = nsSlice
	}

	return summary
}

// nestedInt extracts an int from a map, handling JSON number types (float64).
func nestedInt(m map[string]interface{}, key string) int {
	val, ok := m[key]
	if !ok {
		return 0
	}
	switch v := val.(type) {
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	default:
		return 0
	}
}
