package health

// Component name constants used for HealthManager registration.
const (
	ComponentCollectorManager    = "collector_manager"
	ComponentBufferQueue         = "buffer_queue"
	ComponentDakrTransport       = "dakr_transport"
	ComponentMpaServer           = "mpa_server"
	ComponentPrometheus          = "prometheus"
	ComponentMonitor             = "monitor"
	ComponentEBPFTracer          = "ebpf_tracer"
	ComponentPodCache            = "pod_cache"
	ComponentKarpenterDeployment = "karpenter_deployment"
	ComponentGPURuntimeResolver  = "gpu_runtime_resolver"
	ComponentMemoryPressure      = "memory_pressure"

	// ComponentContainerRedMetrics covers the socket probe behind container RED
	// metrics. It is registered only when the feature is switched on, and is
	// DEREGISTERED rather than reported unhealthy when the kernel cannot support
	// the probe -- an unsupported kernel is a node that will never report, not a
	// node that is failing, and reporting it unhealthy would put a privileged
	// DaemonSet into a permanent alarm state it can do nothing about.
	ComponentContainerRedMetrics = "container_red_metrics"
)
