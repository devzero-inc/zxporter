package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cilium/ebpf/rlimit"
	"github.com/devzero-inc/zxporter/internal/networkmonitor"
	"github.com/devzero-inc/zxporter/internal/networkmonitor/dns"
	"github.com/devzero-inc/zxporter/internal/networkmonitor/ebpf"
	"github.com/devzero-inc/zxporter/internal/transport"
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
	collectorMode   = flag.String("collector-mode", "netfilter", "Collector mode: 'netfilter' or 'cilium'.")
	standalone      = flag.Bool("standalone", false, "Run in standalone mode without K8s connection")
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
	default:
		logger.Error(fmt.Errorf("unknown collector mode: %s", *collectorMode), "Initialization failed")
		os.Exit(1)
	}

	if err != nil {
		logger.Error(err, "Failed to initialize conntrack client", "mode", *collectorMode)
		os.Exit(1)
	}

	// 5. Setup DNS Tracer (eBPF) if available
	var dnsCollector dns.DNSCollector
	if ebpf.IsKernelBTFAvailable() {
		logger.Info("Kernel BTF available, initializing DNS tracer")
		tracerCfg := ebpf.Config{
			QueueSize: 1000,
		}
		tracer := ebpf.NewTracer(logger, tracerCfg)
		dnsCollector = dns.NewIP2DNS(tracer, logger)
	} else {
		logger.Info("Kernel BTF not available, DNS tracing disabled")
	}

	// 6. Setup Dakr Client (Control Plane)
	dakrURL := os.Getenv("DAKR_URL")
	clusterToken := os.Getenv("CLUSTER_TOKEN")
	var dakrClient transport.DakrClient
	if dakrURL != "" && clusterToken != "" {
		logger.Info("Initializing Dakr Client", "url", dakrURL)
		dakrClient = transport.NewDakrClient(dakrURL, clusterToken, logger)
	} else {
		logger.Info("Control Plane credentials not found. Metrics will solely be exposed locally.")
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

	monitorCfg := networkmonitor.Config{
		ReadInterval:    *readInterval,
		CleanupInterval: *cleanupInterval,
		FlushInterval:   flushInterval,
		NodeName:        nodeName,
	}
	monitor, err := networkmonitor.NewMonitor(monitorCfg, logger, podCache, client, dnsCollector, dakrClient)
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
	}

	// Start Monitor in background
	go monitor.Start(ctx)

	// HTTP Server
	http.HandleFunc("/metrics", monitor.GetMetricsHandler)
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	server := &http.Server{
		Addr: *metricsAddr,
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
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	server.Shutdown(shutdownCtx)
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
