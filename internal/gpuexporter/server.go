package gpuexporter

import (
	"context"
	"fmt"
	"net/http"
	"net/http/pprof"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// ExporterConfig holds environment-driven configuration.
type ExporterConfig struct {
	HTTPListenPort      int
	DCGMHost            string
	DCGMPort            int
	DCGMMetricsEndpoint string
	DCGMLabels          string // label selector, e.g. "app.kubernetes.io/name=dcgm-exporter"
	NodeName            string
}

// Exporter ties together the scraper, mapper, and DCGM pod discovery.
type Exporter struct {
	cfg     ExporterConfig
	dynamic dynamic.Interface
	scraper Scraper
	mapper  MetricMapper
	log     logr.Logger
}

// NewExporter creates a new GPU metrics exporter.
func NewExporter(cfg ExporterConfig, dynClient dynamic.Interface, scraper Scraper, mapper MetricMapper, log logr.Logger) *Exporter {
	return &Exporter{
		cfg:     cfg,
		dynamic: dynClient,
		scraper: scraper,
		mapper:  mapper,
		log:     log.WithName("gpu-exporter"),
	}
}

var dcgmPodGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}

// QueryMetrics scrapes DCGM exporters on demand and returns mapped GPU metrics.
func (e *Exporter) QueryMetrics(ctx context.Context) ([]GPUMetric, error) {
	urls, err := e.getDCGMUrls(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting DCGM URLs: %w", err)
	}

	if len(urls) == 0 {
		return nil, nil
	}

	metricFamilies, err := e.scraper.Scrape(ctx, urls)
	if err != nil {
		return nil, fmt.Errorf("scraping DCGM exporters: %w", err)
	}

	if len(metricFamilies) == 0 {
		return nil, nil
	}

	return e.mapper.MapToGPUMetrics(ctx, metricFamilies), nil
}

func (e *Exporter) getDCGMUrls(ctx context.Context) ([]string, error) {
	if e.cfg.DCGMHost != "" {
		return []string{
			fmt.Sprintf("http://%s:%d%s", e.cfg.DCGMHost, e.cfg.DCGMPort, e.cfg.DCGMMetricsEndpoint),
		}, nil
	}

	fieldSelector := "status.phase=Running"
	if e.cfg.NodeName != "" {
		fieldSelector = fmt.Sprintf("%s,spec.nodeName=%s", fieldSelector, e.cfg.NodeName)
	}

	dcgmExporterList, err := e.dynamic.Resource(dcgmPodGVR).Namespace("").List(ctx, metav1.ListOptions{
		LabelSelector: e.cfg.DCGMLabels,
		FieldSelector: fieldSelector,
	})
	if err != nil {
		return nil, fmt.Errorf("listing DCGM exporter pods: %w", err)
	}

	urls := make([]string, len(dcgmExporterList.Items))
	for i := range dcgmExporterList.Items {
		var pod corev1.Pod
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(dcgmExporterList.Items[i].Object, &pod); err != nil {
			return nil, fmt.Errorf("converting unstructured to pod: %w", err)
		}
		urls[i] = fmt.Sprintf("http://%s:%d%s", pod.Status.PodIP, e.cfg.DCGMPort, e.cfg.DCGMMetricsEndpoint)
	}

	return urls, nil
}

// NewServerMux creates the HTTP mux for the GPU exporter.
func NewServerMux(containerMetricsHandler http.Handler) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})

	if containerMetricsHandler != nil {
		mux.Handle("/container/metrics", containerMetricsHandler)
	}

	return mux
}
