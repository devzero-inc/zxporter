package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cilium/ebpf/rlimit"
	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	"github.com/devzero-inc/zxporter/internal/health"
	"github.com/devzero-inc/zxporter/internal/networkmonitor"
	"github.com/devzero-inc/zxporter/internal/networkmonitor/dns"
	"github.com/devzero-inc/zxporter/internal/networkmonitor/ebpf"
	"github.com/devzero-inc/zxporter/internal/transport"
	"github.com/devzero-inc/zxporter/internal/version"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	metricsAddr     = flag.String("metrics-bind-address", ":8081", "The address the metric endpoint binds to.")
	readInterval    = flag.Duration("read-interval", 5*time.Second, "Interval between conntrack reads.")
	cleanupInterval = flag.Duration("cleanup-interval", 60*time.Second, "Interval for cleaning up stale entries.")
	kubeconfig      = flag.String("metrics-kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster.")
	collectorMode   = flag.String("collector-mode", "netfilter", "Collector mode: 'netfilter' or 'bpf'.")
	standalone      = flag.Bool("standalone", false, "Run in standalone mode without K8s connection")
)

const (
	CollectorModeEBPF = "ebpf"
)

func main() {
	flag.Parse()

	// Remove rlimit for eBPF
	if err := rlimit.RemoveMemlock(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to remove memlock rlimit: %v\n", err)
		os.Exit(1)
	}

	// Initialize Logger
	zapLog, _ := zap.NewProduction()
	logger := zapr.NewLogger(zapLog)

	// Create HealthManager and register components early so probes are
	// answered immediately, before component initialisation.
	healthManager := health.NewHealthManager()
	healthManager.Register(health.ComponentMonitor)
	healthManager.Register(health.ComponentDakrTransport)
	healthManager.Register(health.ComponentEBPFTracer)
	healthManager.Register(health.ComponentPodCache)

	// Allow 30 seconds for monitor, eBPF, and informer to initialize
	// before readiness checks start failing on Unspecified statuses.
	healthManager.SuppressReadiness(30 * time.Second)

	startTime := time.Now()

	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		logger.Info("NODE_NAME environment variable not set, defaulting to localhost (dev mode)")
	}

	var podCache *networkmonitor.PodCache
	var informerFactory informers.SharedInformerFactory

	if !*standalone {
		// 1. Setup K8s Client
		cfg, err := getKubeConfig(*kubeconfig)
		if err != nil {
			logger.Error(err, "Failed to get kubeconfig")
			os.Exit(1)
		}

		clientset, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			logger.Error(err, "Failed to create k8s client")
			os.Exit(1)
		}

		// 2. Setup Informer (Filtered by Node)
		informerFactory = informers.NewSharedInformerFactoryWithOptions(
			clientset,
			30*time.Second,
			informers.WithTweakListOptions(func(options *metav1.ListOptions) {
				if nodeName != "" {
					options.FieldSelector = "spec.nodeName=" + nodeName
				}
			}),
		)
		podInformer := informerFactory.Core().V1().Pods().Informer()

		// 3. Setup PodCache
		podCache = networkmonitor.NewPodCache(podInformer)
	} else {
		logger.Info("Running in STANDALONE mode. K8s connection disabled.")
		// Initialize PodCache with nil informer for standalone mode
		podCache = networkmonitor.NewPodCache(nil)
		healthManager.UpdateStatus(health.ComponentPodCache, health.HealthStatusHealthy, "standalone mode", nil)
	}

	// 4. Setup Client (Cilium or Netfilter)
	var client networkmonitor.Client
	var err error

	switch *collectorMode {
	case "cilium":
		if !networkmonitor.CiliumAvailable(*collectorMode) {
			logger.Error(nil, "Cilium requested but not available")
			os.Exit(1)
		}
		// Uses ktime as default clock source like egressd
		client, err = networkmonitor.NewCiliumClient(logger, networkmonitor.ClockSourceKtime)
	case "netfilter":
		client, err = networkmonitor.NewNetfilterClient(logger)
	case CollectorModeEBPF:
		logger.Info("Running in EBPF mode. Conntrack client disabled.")
		client = nil // Client is optional now
	default:
		logger.Error(fmt.Errorf("unknown collector mode: %s", *collectorMode), "Initialization failed")
		os.Exit(1)
	}

	if *collectorMode != CollectorModeEBPF && err != nil {
		logger.Error(err, "Failed to initialize conntrack client", "mode", *collectorMode)
		os.Exit(1)
	}

	// 5. Setup DNS Tracer (eBPF) AND/OR Flow Tracer
	var dnsCollector dns.DNSCollector
	var tracer *ebpf.Tracer

	// Check BTF availability
	if ebpf.IsKernelBTFAvailable() {
		logger.Info("Kernel BTF available, initializing eBPF tracer")
		tracerCfg := ebpf.Config{
			QueueSize: 1000,
		}
		tracer = ebpf.NewTracer(logger, tracerCfg)
		// DNS Collector uses the SAME tracer for DNS events
		dnsCollector = dns.NewIP2DNS(tracer, logger)
		healthManager.UpdateStatus(health.ComponentEBPFTracer, health.HealthStatusHealthy, "initialized", nil)
	} else {
		logger.Info("Kernel BTF not available, eBPF features disabled")
		if *collectorMode == CollectorModeEBPF {
			logger.Error(nil, "EBPF mode requested but BTF not available")
			os.Exit(1)
		}
		// eBPF not available — deregister so it doesn't affect health checks
		healthManager.Deregister(health.ComponentEBPFTracer)
	}

	// 6. Setup Dakr Client (Control Plane)
	dakrURL := os.Getenv("DAKR_URL")
	clusterToken := os.Getenv("CLUSTER_TOKEN")
	var dakrClient transport.DakrClient
	if dakrURL != "" && clusterToken != "" {
		logger.Info("Initializing Dakr Client", "url", dakrURL)
		dakrClient = transport.NewDakrClient(dakrURL, clusterToken, logger)
		healthManager.UpdateStatus(health.ComponentDakrTransport, health.HealthStatusHealthy, "initialized", nil)
	} else {
		logger.Info("Control Plane credentials not found. Metrics will solely be exposed locally.")
		// No dakr connection — deregister so it doesn't block readiness
		healthManager.Deregister(health.ComponentDakrTransport)
	}

	// 7. Setup Monitor
	flushInterval := 60 * time.Second
	if intervalStr := os.Getenv("FLUSH_INTERVAL"); intervalStr != "" {
		if d, err := time.ParseDuration(intervalStr); err == nil {
			flushInterval = d
		} else {
			logger.Error(err, "Invalid FLUSH_INTERVAL format, using default", "value", intervalStr)
		}
	}

	versionInfo := version.Get()
	monitorCfg := networkmonitor.Config{
		ReadInterval:    *readInterval,
		CleanupInterval: *cleanupInterval,
		FlushInterval:   flushInterval,
		NodeName:        nodeName,
		OperatorVersion: versionInfo.String(),
		OperatorCommit:  versionInfo.GitCommit,
	}
	monitor, err := networkmonitor.NewMonitor(
		monitorCfg, logger, podCache, client, tracer, dnsCollector, dakrClient, healthManager,
	)
	if err != nil {
		logger.Error(err, "Failed to initialize monitor")
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start Informer if not standalone
	if !*standalone && informerFactory != nil {
		informerFactory.Start(ctx.Done())
		informerFactory.WaitForCacheSync(ctx.Done())
		healthManager.UpdateStatus(health.ComponentPodCache, health.HealthStatusHealthy, "informer synced", nil)
	}

	// Start eBPF tracer if available (for DNS events and/or network flows)
	if tracer != nil {
		go func() {
			if err := tracer.Run(ctx); err != nil {
				logger.Error(err, "eBPF tracer failed")
				healthManager.UpdateStatus(health.ComponentEBPFTracer, health.HealthStatusUnhealthy, err.Error(), nil)
			}
		}()
	}

	// Start Monitor in background
	go monitor.Start(ctx)

	// Start heartbeat sender if dakr is configured
	if dakrClient != nil {
		go func() {
			clusterID := os.Getenv("CLUSTER_ID")
			if clusterID == "" {
				clusterID = "unknown"
			}
			// Send initial heartbeat immediately
			report := healthManager.BuildReport()
			req := health.BuildHeartbeatRequestFromReport(
				report, clusterID, gen.OperatorType_OPERATOR_TYPE_NETWORK,
				versionInfo.String(), versionInfo.GitCommit, startTime,
			)
			if err := dakrClient.ReportHealth(ctx, req); err != nil {
				logger.Error(err, "Failed to send initial health heartbeat to dakr")
			}

			ticker := time.NewTicker(60 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					report := healthManager.BuildReport()
					for name, status := range report {
						logger.Info("Health status report", "component", name, "status", status.Status, "message", status.Message)
					}
					req := health.BuildHeartbeatRequestFromReport(
						report, clusterID, gen.OperatorType_OPERATOR_TYPE_NETWORK,
						versionInfo.String(), versionInfo.GitCommit, startTime,
					)
					if err := dakrClient.ReportHealth(ctx, req); err != nil {
						logger.Error(err, "Failed to send health heartbeat to dakr")
					}
				}
			}
		}()
	}

	// HTTP Server
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", monitor.GetMetricsHandler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		report := healthManager.BuildReport()
		resp := map[string]any{"components": report}
		if cs, exists := report[health.ComponentMonitor]; exists && cs.Status == health.HealthStatusUnhealthy {
			resp["status"] = "unhealthy"
			resp["error"] = cs.Message
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		resp["status"] = "healthy"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		report := healthManager.BuildReport()
		resp := map[string]any{"components": report}
		// Check required components for readiness
		for _, name := range []string{health.ComponentMonitor, health.ComponentDakrTransport} {
			cs, exists := report[name]
			if !exists {
				// Component was deregistered (e.g., no dakr configured) — skip
				continue
			}
			if cs.Status != health.HealthStatusHealthy && cs.Status != health.HealthStatusDegraded {
				resp["status"] = "not ready"
				resp["error"] = fmt.Sprintf("%s is not ready (status: %s)", name, cs.Status)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(resp)
				return
			}
		}
		resp["status"] = "ready"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	})

	server := &http.Server{
		Addr:    *metricsAddr,
		Handler: mux,
	}

	go func() {
		logger.Info("Starting HTTP server", "addr", *metricsAddr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error(err, "HTTP server failed")
			os.Exit(1)
		}
	}()

	// Signal handling
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	logger.Info("Shutting down...")
	cancel() // Trigger monitor cleanup
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error(err, "HTTP server shutdown failed")
	}
}

func getKubeConfig(kubeconfigPath string) (*rest.Config, error) {
	if kubeconfigPath != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	}
	// Try in-cluster
	config, err := rest.InClusterConfig()
	if err == nil {
		return config, nil
	}
	// Fallback to default local rules (e.g. ~/.kube/config)
	return clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
}
