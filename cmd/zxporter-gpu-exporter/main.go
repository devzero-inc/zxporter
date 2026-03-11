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

	"github.com/devzero-inc/zxporter/internal/gpuexporter"
	"github.com/devzero-inc/zxporter/internal/version"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	flag.Parse()

	// Initialize Logger
	zapLog, _ := zap.NewProduction()
	logger := zapr.NewLogger(zapLog)

	versionInfo := version.Get()
	logger.Info("Starting zxporter-gpu-exporter",
		"version", versionInfo.String(),
		"commit", versionInfo.GitCommit)

	cfg := gpuexporter.ExporterConfig{
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

	// Create components
	httpClient := &http.Client{Timeout: 15 * time.Second}
	scraper := gpuexporter.NewScraper(httpClient, logger)
	workloadResolver := gpuexporter.NewWorkloadResolver(
		dynClient,
		gpuexporter.WorkloadResolverConfig{
			LabelKeys: nil, // can be configured via env if needed
			CacheSize: 256,
		},
		logger,
	)
	mapper := gpuexporter.NewMapper(cfg.NodeName, workloadResolver, logger)

	// Create exporter
	exporter := gpuexporter.NewExporter(cfg, dynClient, scraper, mapper, logger)

	// Create HTTP handler and server
	containerMetricsHandler := gpuexporter.NewContainerMetricsHandler(exporter, logger)
	mux := gpuexporter.NewServerMux(containerMetricsHandler)

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
