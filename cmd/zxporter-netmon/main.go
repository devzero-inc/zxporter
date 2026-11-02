package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"syscall"
	"time"

	// Sets GOMEMLIMIT to 0.9 x the container's cgroup memory limit at startup so
	// the GC collects aggressively before the kernel OOM-kills the pod. No-ops
	// safely when no cgroup memory limit is set. Mirrors the controller-manager
	// (operators/zxporter/cmd/main.go) and nodemon.
	//
	// Without it this binary ran at GOGC=100 with no knowledge of its cgroup
	// ceiling: it idles at ~98MiB and the GC will not collect until the heap has
	// doubled, so the first decodable load on the node OOM-killed it in 26s.
	_ "github.com/KimMachineGun/automemlimit"

	"github.com/cilium/ebpf/rlimit"
	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	"github.com/devzero-inc/zxporter/internal/health"
	"github.com/devzero-inc/zxporter/internal/networkmonitor"
	"github.com/devzero-inc/zxporter/internal/networkmonitor/dns"
	"github.com/devzero-inc/zxporter/internal/networkmonitor/ebpf"
	"github.com/devzero-inc/zxporter/internal/networkmonitor/redmetrics"
	"github.com/devzero-inc/zxporter/internal/networkmonitor/redmetrics/socketprobe"
	"github.com/devzero-inc/zxporter/internal/transport"
	"github.com/devzero-inc/zxporter/internal/version"
	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	metricsAddr = flag.String(
		"metrics-bind-address",
		":9101",
		"The address the metric endpoint binds to.",
	)
	readInterval = flag.Duration(
		"read-interval",
		5*time.Second,
		"Interval between conntrack reads.",
	)
	cleanupInterval = flag.Duration(
		"cleanup-interval",
		60*time.Second,
		"Interval for cleaning up stale entries.",
	)
	kubeconfig = flag.String(
		"metrics-kubeconfig",
		"",
		"Path to a kubeconfig. Only required if out-of-cluster.",
	)
	collectorMode = flag.String(
		"collector-mode",
		"netfilter",
		"Collector mode: 'netfilter' or 'bpf'.",
	)
	standalone = flag.Bool(
		"standalone",
		false,
		"Run in standalone mode without K8s connection",
	)
	containerRedMetrics = flag.Bool(
		"container-red-metrics",
		false,
		"Record per-container inbound RED metrics using socket probes. Off by default: "+
			"it attaches kprobes and holds per-connection state, so it must be switched on deliberately.",
	)
	pprofAddr = flag.String(
		"pprof-bind-address",
		"",
		"Address for the pprof server, e.g. 127.0.0.1:6060. Empty disables it entirely. "+
			"Bind to loopback: this pod runs with hostNetwork, so any other address publishes "+
			"profiles on the node's network, and a heap profile of this binary contains "+
			"buffered request bytes from the traffic it is decoding.",
	)
	pprofMutexFraction = flag.Int(
		"pprof-mutex-fraction",
		0,
		"runtime.SetMutexProfileFraction: report 1/N contended mutex events. 0 disables "+
			"mutex profiling. Requires -pprof-bind-address, and costs something on every "+
			"contended lock, so switch it on for an investigation and off again after.",
	)
	pprofBlockRate = flag.Int(
		"pprof-block-rate",
		0,
		"runtime.SetBlockProfileRate: sample one blocking event per N nanoseconds blocked. "+
			"0 disables block profiling. Same caveats as -pprof-mutex-fraction.",
	)
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
		logger.Error(
			fmt.Errorf("unknown collector mode: %s", *collectorMode),
			"Initialization failed",
		)
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

	// Read back the GOMEMLIMIT set by the automemlimit side-effect import above,
	// so a pod that is still being OOM-killed can be told apart from one whose
	// limit was never applied.
	logger.Info("effective GOMEMLIMIT", "bytes", debug.SetMemoryLimit(-1))

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

	stopRedMetrics := startContainerRedMetrics(
		ctx, *containerRedMetrics, logger, monitor, podCache, healthManager,
	)
	defer stopRedMetrics()

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
						logger.Info(
							"Health status report",
							"component",
							name,
							"status",
							status.Status,
							"message",
							status.Message,
						)
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

	pprofServer := startPprofServer(logger)

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
	shutdownPprofServer(shutdownCtx, pprofServer, logger)
}

// shutdownPprofServer is a nil-tolerant Shutdown. It lives outside main because main is
// already over this repo's cyclomatic complexity threshold, and a diagnostic that is off by
// default has no business pushing it further.
func shutdownPprofServer(ctx context.Context, srv *http.Server, logger logr.Logger) {
	if srv == nil {
		return
	}
	if err := srv.Shutdown(ctx); err != nil {
		logger.Error(err, "pprof server shutdown failed")
	}
}

// startPprofServer brings up net/http/pprof on its own listener, returning nil when
// -pprof-bind-address is empty, which is the default.
//
// It is deliberately NOT mounted on the metrics mux. That listener is published on the
// node -- this DaemonSet runs with hostNetwork -- and the profiles here are not the same
// kind of data as the counters there: the aggregator holds the leading bytes of requests
// it has not finished decoding, so a heap profile of this process contains fragments of
// whatever traffic the node is serving. It gets its own address so that exposing metrics
// can never expose profiles as a side effect, and the flag's default of "" means an
// operator has to ask for it.
//
// Access is via kubectl port-forward, which dials into the pod's network namespace and so
// reaches a loopback bind.
func startPprofServer(logger logr.Logger) *http.Server {
	if *pprofAddr == "" {
		return nil
	}

	// Both are off unless asked for: each adds work to every contended lock and every
	// blocking operation, which would distort the very measurement they are here to
	// provide. Set on the runtime rather than per-request because the runtime has to be
	// collecting before the profile is fetched.
	if *pprofMutexFraction > 0 {
		runtime.SetMutexProfileFraction(*pprofMutexFraction)
	}
	if *pprofBlockRate > 0 {
		runtime.SetBlockProfileRate(*pprofBlockRate)
	}

	// Registered explicitly rather than by importing net/http/pprof for its side effect:
	// that init() writes to http.DefaultServeMux, which this binary does not serve, so a
	// blank import would look like it enabled profiling and quietly do nothing.
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	// No write timeout, and that is the point: a CPU profile blocks for its full
	// -seconds duration and an execution trace for longer, so any timeout short enough
	// to be worth having would truncate exactly the profiles this exists to serve.
	srv := &http.Server{
		Addr:              *pprofAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		logger.Info("Starting pprof server", "addr", *pprofAddr,
			"mutex_fraction", *pprofMutexFraction, "block_rate", *pprofBlockRate)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// Not fatal. Profiling is a diagnostic; losing it must not take the node's
			// network monitoring down with it.
			logger.Error(err, "pprof server failed")
		}
	}()

	return srv
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

// socketProbe is the part of the socket probe this file uses. Naming it here
// rather than depending on the concrete type keeps the wiring testable without a
// kernel.
type socketProbe interface {
	Attach() error
	Run(ctx context.Context, sink func(redmetrics.SocketEvent)) error
	Close() error
}

// redMetricsSink is the part of the monitor this file uses.
type redMetricsSink interface {
	EnableContainerRedMetrics(agg *redmetrics.Aggregator)
}

// startContainerRedMetrics switches on container RED metrics, returning a stop
// function. When the feature is off -- which is the default -- it does nothing
// and returns a no-op, so the disabled path costs one branch and touches no
// kernel state at all.
func startContainerRedMetrics(
	ctx context.Context,
	enabled bool,
	logger logr.Logger,
	monitor redMetricsSink,
	pods redmetrics.PodLookup,
	healthManager *health.HealthManager,
) func() {
	if !enabled {
		return func() {}
	}
	return enableContainerRedMetrics(ctx, logger, monitor, pods, healthManager, socketprobe.New(logger))
}

// enableContainerRedMetrics does the work, against an injected probe.
func enableContainerRedMetrics(
	ctx context.Context,
	logger logr.Logger,
	monitor redMetricsSink,
	pods redmetrics.PodLookup,
	healthManager *health.HealthManager,
	probe socketProbe,
) func() {
	healthManager.Register(health.ComponentContainerRedMetrics)

	if err := probe.Attach(); err != nil {
		if errors.Is(err, socketprobe.ErrUnsupported) {
			// The kernel cannot carry the probe. That is a node that will never
			// report, not a node that is failing, so the component is removed from
			// the report rather than left in it as unhealthy -- otherwise a
			// privileged DaemonSet sits in a permanent alarm state over something no
			// operator action can fix.
			logger.Info("Container RED metrics unsupported on this kernel, continuing without them")
			healthManager.Deregister(health.ComponentContainerRedMetrics)
			return func() {}
		}
		logger.Error(err, "Failed to attach container RED metrics socket probe")
		healthManager.UpdateStatus(
			health.ComponentContainerRedMetrics, health.HealthStatusUnhealthy, err.Error(), nil,
		)
		return func() {}
	}

	aggregator := redmetrics.NewAggregator(redmetrics.DefaultConfig(), redmetrics.SystemClock{}, pods, logger)
	monitor.EnableContainerRedMetrics(aggregator)
	healthManager.UpdateStatus(
		health.ComponentContainerRedMetrics, health.HealthStatusHealthy, "attached", nil,
	)

	go func() {
		if err := probe.Run(ctx, aggregator.Observe); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error(err, "Container RED metrics socket probe stopped")
			healthManager.UpdateStatus(
				health.ComponentContainerRedMetrics, health.HealthStatusUnhealthy, err.Error(), nil,
			)
		}
	}()

	return func() {
		if err := probe.Close(); err != nil {
			logger.Error(err, "Failed to close container RED metrics socket probe")
		}
	}
}
