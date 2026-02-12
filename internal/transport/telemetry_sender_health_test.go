package transport

import (
	"testing"

	"github.com/devzero-inc/zxporter/internal/health"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
)

func TestTelemetrySender_HealthyOnSuccess(t *testing.T) {
	hm := health.NewHealthManager()
	hm.Register(health.ComponentDakrTransport)

	sender := NewTelemetrySender(logr.Discard(), nil, nil, 0, hm)

	sender.recordSuccess()

	status, exists := hm.GetStatus(health.ComponentDakrTransport)
	assert.True(t, exists)
	assert.Equal(t, health.HealthStatusHealthy, status.Status)
}

func TestTelemetrySender_DegradedOnFailure(t *testing.T) {
	hm := health.NewHealthManager()
	hm.Register(health.ComponentDakrTransport)

	sender := NewTelemetrySender(logr.Discard(), nil, nil, 0, hm)

	sender.recordFailure()

	status, _ := hm.GetStatus(health.ComponentDakrTransport)
	assert.Equal(t, health.HealthStatusDegraded, status.Status)
	assert.Equal(t, "1", status.Metadata["consecutive_failures"])
}

func TestTelemetrySender_UnhealthyOnCircuitOpen(t *testing.T) {
	hm := health.NewHealthManager()
	hm.Register(health.ComponentDakrTransport)

	sender := NewTelemetrySender(logr.Discard(), nil, nil, 0, hm)

	for i := 0; i < maxConsecutiveFailures; i++ {
		sender.recordFailure()
	}

	status, _ := hm.GetStatus(health.ComponentDakrTransport)
	assert.Equal(t, health.HealthStatusUnhealthy, status.Status)
	assert.Equal(t, "open", status.Metadata["circuit_breaker"])
}

func TestTelemetrySender_NilHealthManager(t *testing.T) {
	sender := NewTelemetrySender(logr.Discard(), nil, nil, 0, nil)
	sender.recordSuccess()
	sender.recordFailure()
}
