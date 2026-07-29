package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/devzero-inc/zxporter/internal/health"
	"github.com/devzero-inc/zxporter/internal/nodemon"
	"github.com/devzero-inc/zxporter/internal/version"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	flag.Parse()

	// Initialize Logger
	zapLog, _ := zap.NewProduction()
	logger := zapr.NewLogger(zapLog)

	versionInfo := version.Get()
	logger.Info("Starting zxporter-nodemon",
		"version", versionInfo.String(),
		"commit", versionInfo.GitCommit)

	cfg := nodemon.ExporterConfig{
		HTTPListenPort:      envInt("HTTP_LISTEN_PORT", 6061),
		DCGMHost:            os.Getenv("DCGM_HOST"),
		DCGMPort:            envInt("DCGM_PORT", 9400),
		DCGMMetricsEndpoint: envString("DCGM_METRICS_ENDPOINT", "/metrics"),
		DCGMLabels:          envString("DCGM_LABELS", "app.kubernetes.io/name=dcgm-exporter"),
		NodeName:            os.Getenv("NODE_NAME"),
	}

	logger.Info("Configuration",
		"httpListenPort", cfg.HTTPListenPort,
		"dcgmHost", cfg.DCGMHost,
		"dcgmPort", cfg.DCGMPort,
		"dcgmEndpoint", cfg.DCGMMetricsEndpoint,
		"dcgmLabels", cfg.DCGMLabels,
		"nodeName", cfg.NodeName)

	// Setup K8s dynamic client
	kubeConfig, err := getKubeConfig()
	if err != nil {
		logger.Error(err, "Failed to get kubeconfig")
		os.Exit(1)
	}

	dynClient, err := dynamic.NewForConfig(kubeConfig)
	if err != nil {
		logger.Error(err, "Failed to create dynamic client")
		os.Exit(1)
	}

	k8sClient, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		logger.Error(err, "Failed to create kubernetes client")
		os.Exit(1)
	}

	// Create components
	httpClient := &http.Client{Timeout: 15 * time.Second}
	scraper := nodemon.NewScraper(httpClient, logger)
	workloadResolver := nodemon.NewWorkloadResolver(
		dynClient,
		nodemon.WorkloadResolverConfig{
			LabelKeys: nil, // can be configured via env if needed
			CacheSize: 256,
		},
		logger,
	)
	mapper := nodemon.NewMapper(cfg.NodeName, workloadResolver, logger)

	// Create GPU exporter
	exporter := nodemon.NewExporter(cfg, dynClient, scraper, mapper, logger)

	// Create a K8s-authenticated HTTP client for kubelet API proxy access
	k8sTransport, err := rest.TransportFor(kubeConfig)
	if err != nil {
		logger.Error(err, "Failed to create K8s transport")
		os.Exit(1)
	}
	k8sHTTPClient := &http.Client{Transport: k8sTransport, Timeout: 15 * time.Second}

	// Use the K8s API server proxy for kubelet access (same as Cortex pattern)
	apiProxyBase := kubeConfig.Host + "/api/v1/nodes/" + cfg.NodeName + "/proxy"
	statsPoller := nodemon.NewStatsPoller(apiProxyBase, k8sHTTPClient, logger)
	cadvisorScraper := nodemon.NewCAdvisorScraper(apiProxyBase, k8sHTTPClient, logger)

	// Create unified exporter that combines all data sources
	unifiedExporter := nodemon.NewUnifiedExporter(statsPoller, cadvisorScraper, exporter, cfg.NodeName, logger)

	// Start unified collection loop (every 30 seconds)
	collectionCtx, collectionCancel := context.WithCancel(context.Background())
	defer collectionCancel()
	go unifiedExporter.StartCollectionLoop(collectionCtx, 30*time.Second)

	// Create HTTP handlers
	containerMetricsHandler := nodemon.NewContainerMetricsHandler(exporter, logger) // GPU-only (backward compat)

	// Only start process-introspection collectors when explicitly enabled via
	// Helm values (runtimeMetrics.enabled). They require hostPID: true and SYS_PTRACE
	// capability, which are only granted in the pod spec when runtimeMetrics.enabled is true.
	// All of them share a single PodContainerIndex (one Pod informer/watch) rather than
	// each running their own.
	//
	// /container/runtime-metrics (RuntimeCollector) is the combined endpoint the zxporter
	// collector actually polls each cycle — one /proc walk covering every runtime. The
	// legacy /container/jvm-metrics endpoint (its own JVMCollector and /proc walk) is
	// kept alongside it for backward compatibility with already-shipped consumers.
	var jvmMetricsHandler http.Handler
	var runtimeMetricsHandler http.Handler
	var podContainerIndex *nodemon.PodContainerIndex
	runtimeMetricsEnabled := os.Getenv("RUNTIME_METRICS_ENABLED") == "true"
	if runtimeMetricsEnabled {
		podContainerIndex = nodemon.NewPodContainerIndex(cfg.NodeName, k8sClient, logger)
		if err := podContainerIndex.Start(); err != nil {
			logger.Error(err,
				"Failed to start pod container index — runtime metrics unavailable, nodemon will continue")
			podContainerIndex = nil
		} else {
			jvmCollector := nodemon.NewJVMCollector(cfg.NodeName, podContainerIndex, logger)
			jvmMetricsHandler = nodemon.NewJVMMetricsHandler(jvmCollector, logger)
			logger.Info("JVM metrics collection enabled")

			runtimeCollector := nodemon.NewRuntimeCollector(cfg.NodeName, podContainerIndex, logger)
			runtimeMetricsHandler = nodemon.NewRuntimeMetricsHandler(runtimeCollector, logger)
			logger.Info("Combined runtime metrics collection enabled")
		}
	} else {
		logger.Info("Runtime metrics collection disabled (set runtimeMetrics.enabled=true in Helm values to enable)")
	}

	mux := nodemon.NewServerMux(containerMetricsHandler, jvmMetricsHandler, runtimeMetricsHandler)

	// Register unified endpoints
	mux.Handle("/v2/container/metrics", nodemon.NewUnifiedContainerHandler(unifiedExporter, logger))
	mux.Handle("/node/metrics", nodemon.NewNodeMetricsHandler(unifiedExporter, logger))
	mux.Handle("/pvc/metrics", nodemon.NewPVCMetricsHandler(unifiedExporter, logger))

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.HTTPListenPort),
		Handler: mux,
	}

	// Start server in background
	go func() {
		logger.Info("Starting HTTP server", "addr", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error(err, "HTTP server failed")
			os.Exit(1)
		}
	}()

	// Graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// nodemon has no telemetry logger / dakr client of its own (it's a passive
	// exporter scraped by the zxporter collector, not a push client), so pass a
	// nil telemetry sink — the monitor still logs threshold crossings locally,
	// which is a strict improvement over discovering OOM kills only after the
	// fact via k8s_events. Wiring an actual telemetry sink here is a follow-up
	// that needs its own DAKR_URL/cluster-token plumbing into this daemonset.
	memoryPressureMonitor := health.NewMemoryPressureMonitor(logger, nil, nil)
	go func() {
		if err := memoryPressureMonitor.Start(ctx); err != nil {
			logger.Error(err, "Memory pressure monitor exited with error")
		}
	}()

	// Retroactively check whether the *previous* instance of this pod was
	// OOM-killed, reading pod.status.containerStatuses[].lastState.terminated
	// off the Pod object — which Kubernetes persists regardless of whether the
	// dying process got to do anything, unlike the proactive monitor above
	// which can only warn ahead of a kill it catches in time. Runs once, not
	// on a ticker; nil telemetryLogger for the same reason as the memory
	// pressure monitor above (nodemon has no telemetry sink wired up yet), so
	// it degrades to local logr output only.
	restartOOMDetector := health.NewRestartOOMDetector(
		logger,
		nil,
		k8sClient,
		os.Getenv("POD_NAMESPACE"),
		os.Getenv("POD_NAME"),
		"zxporter-nodemon",
	)
	go restartOOMDetector.Check(ctx)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	logger.Info("Shutting down...")
	cancel() // stop the memory pressure monitor
	if podContainerIndex != nil {
		podContainerIndex.Stop()
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error(err, "HTTP server shutdown failed")
	}
}

func getKubeConfig() (*rest.Config, error) {
	kubeconfigPath := os.Getenv("KUBE_CONFIG_PATH")
	if kubeconfigPath != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	}
	config, err := rest.InClusterConfig()
	if err == nil {
		return config, nil
	}
	return clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
}

func envString(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}

func envInt(key string, defaultValue int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return defaultValue
}
