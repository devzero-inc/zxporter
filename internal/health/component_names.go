package health

// Component name constants used for HealthManager registration.
const (
	ComponentCollectorManager = "collector_manager"
	ComponentBufferQueue      = "buffer_queue"
	ComponentDakrTransport    = "dakr_transport"
	ComponentMpaServer        = "mpa_server"
	ComponentPrometheus       = "prometheus"
	ComponentMonitor          = "monitor"
	ComponentEBPFTracer       = "ebpf_tracer"
	ComponentPodCache         = "pod_cache"
)
