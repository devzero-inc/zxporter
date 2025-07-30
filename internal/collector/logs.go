// Package collector provides functionality for collecting metrics from Kubernetes resources
package collector

import (
	"fmt"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
)

// TelemetryLogs sends logs to the control plane
type TelemetryLogsClient struct {
	logChan chan *gen.LogEntry
}

// NewTelemetryMetrics creates and registers Prometheus metrics
func NewTelemetryLogsClient(
	logChan chan *gen.LogEntry,
) *TelemetryLogsClient {
	return &TelemetryLogsClient{
		logChan: logChan,
	}
}

func (tl *TelemetryLogsClient) Send(entry *gen.LogEntry) {
	select {
	case tl.logChan <- entry:
	default:
		// optionally drop or log if channel is full
		fmt.Println("telemetry log channel full, dropping log")
	}
}
