package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Metrics holds Prometheus metrics for the resource adjustment controller.
type Metrics struct {
	CpuRecommendation       *prometheus.GaugeVec
	MemoryRecommendation    *prometheus.GaugeVec
	ControlSignal           *prometheus.GaugeVec
	IntegralTerm            *prometheus.GaugeVec
	DerivativeTerm          *prometheus.GaugeVec
	ReconciliationFrequency *prometheus.GaugeVec
}

// NewMetrics initializes and registers Prometheus metrics.
func NewMetrics(namespace string) *Metrics {
	m := &Metrics{
		CpuRecommendation: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name:      "cpu_recommendation",
				Namespace: namespace,
				Help:      "Current CPU recommendation for a container.",
			},
			[]string{"namespace", "pod", "container"},
		),
		MemoryRecommendation: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name:      "memory_recommendation",
				Namespace: namespace,
				Help:      "Current memory recommendation for a container.",
			},
			[]string{"namespace", "pod", "container"},
		),
		ControlSignal: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name:      "pid_control_signal",
				Namespace: namespace,
				Help:      "Current control signal from the PID controller.",
			},
			[]string{"namespace", "pod", "container"},
		),
		IntegralTerm: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name:      "pid_integral_term",
				Namespace: namespace,
				Help:      "Current integral term from the PID controller.",
			},
			[]string{"namespace", "pod", "container"},
		),
		DerivativeTerm: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name:      "pid_derivative_term",
				Namespace: namespace,
				Help:      "Current derivative term from the PID controller.",
			},
			[]string{"namespace", "pod", "container"},
		),
		ReconciliationFrequency: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name:      "reconciliation_frequency_seconds",
				Namespace: namespace,
				Help:      "Current frequency of reconciliation in seconds.",
			},
			[]string{"namespace", "pod", "container"},
		),
	}

	// Register all metrics with the global Prometheus registry
	metrics.Registry.MustRegister(
		m.CpuRecommendation,
		m.MemoryRecommendation,
		m.ControlSignal,
		m.IntegralTerm,
		m.DerivativeTerm,
		m.ReconciliationFrequency,
	)

	return m
}
