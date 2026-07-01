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

	// Only start process-introspection collectors (JVM, Node.js) when explicitly enabled
	// via Helm values (runtimeMetrics.enabled). They require hostPID: true and SYS_PTRACE
	// capability, which are only granted in the pod spec when runtimeMetrics.enabled is true.
	// All of them share a single PodContainerIndex (one Pod informer/watch) rather than
	// each running their own.
	//
	// /container/runtime-metrics (RuntimeCollector) is the combined endpoint the zxporter
	// collector actually polls each cycle — one /proc walk covering every runtime. The
	// legacy per-runtime /container/jvm-metrics and /container/nodejs-metrics endpoints
	// (their own JVMCollector/NodeJSCollector, each with its own /proc walk) are kept
	// alongside it for backward compatibility (existing CI/docs/direct debugging use).
	var jvmMetricsHandler http.Handler
	var nodeJSMetricsHandler http.Handler
	var runtimeMetricsHandler http.Handler
	var podContainerIndex *nodemon.PodContainerIndex
	runtimeMetricsEnabled := os.Getenv("RUNTIME_METRICS_ENABLED") == "true"
	if runtimeMetricsEnabled {
		podContainerIndex = nodemon.NewPodContainerIndex(cfg.NodeName, k8sClient, logger)
		if err := podContainerIndex.Start(); err != nil {
			logger.Error(err,
				"Failed to start pod container index — JVM/Node.js metrics unavailable, nodemon will continue")
			podContainerIndex = nil
		} else {
			jvmCollector := nodemon.NewJVMCollector(cfg.NodeName, podContainerIndex, logger)
			jvmMetricsHandler = nodemon.NewJVMMetricsHandler(jvmCollector, logger)
			logger.Info("JVM metrics collection enabled")

			nodeJSCollector := nodemon.NewNodeJSCollector(cfg.NodeName, podContainerIndex, logger)
			nodeJSMetricsHandler = nodemon.NewNodeJSMetricsHandler(nodeJSCollector, logger)
			logger.Info("Node.js metrics collection enabled")

			runtimeCollector := nodemon.NewRuntimeCollector(cfg.NodeName, podContainerIndex, logger)
			runtimeMetricsHandler = nodemon.NewRuntimeMetricsHandler(runtimeCollector, logger)
			logger.Info("Combined runtime metrics collection enabled")
		}
	} else {
		logger.Info("Runtime metrics collection disabled (set runtimeMetrics.enabled=true in Helm values to enable)")
	}

	mux := nodemon.NewServerMux(containerMetricsHandler, jvmMetricsHandler, nodeJSMetricsHandler, runtimeMetricsHandler)

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
	_ = ctx // ctx available for future use

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	logger.Info("Shutting down...")
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
