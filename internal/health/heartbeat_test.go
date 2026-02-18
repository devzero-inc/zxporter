package health

import (
	"context"
	"testing"
	"time"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockDakrClient captures the ReportHealth call for testing.
type mockDakrClient struct {
	lastReq *gen.ReportHealthRequest
	err     error
}

func (m *mockDakrClient) ReportHealth(ctx context.Context, req *gen.ReportHealthRequest) error {
	m.lastReq = req
	return m.err
}

func TestBuildHeartbeatRequest(t *testing.T) {
	hm := NewHealthManager()
	hm.Register(ComponentCollectorManager)
	hm.Register(ComponentDakrTransport)
	hm.UpdateStatus(ComponentCollectorManager, HealthStatusHealthy, "all good", map[string]string{"active": "5"})
	hm.UpdateStatus(ComponentDakrTransport, HealthStatusDegraded, "retrying", nil)

	startTime := time.Now().Add(-10 * time.Minute)

	req := BuildHeartbeatRequest(hm, "cluster-123", "1.2.3", startTime)

	assert.Equal(t, "cluster-123", req.ClusterId)
	assert.Equal(t, "1.2.3", req.Version)
	assert.Equal(t, gen.OperatorType_OPERATOR_TYPE_READ, req.OperatorType)
	require.NotNil(t, req.UptimeSince)
	assert.Equal(t, startTime.Unix(), req.UptimeSince.AsTime().Unix())

	// Overall status should be the worst across components
	assert.Equal(t, gen.HealthStatus_HEALTH_STATUS_DEGRADED, req.OverallStatus)

	// Components should be present
	assert.Len(t, req.Components, 2)
}

func TestBuildHeartbeatRequest_OverallStatus(t *testing.T) {
	tests := []struct {
		name     string
		statuses []HealthStatus
		expected gen.HealthStatus
	}{
		{
			name:     "all healthy",
			statuses: []HealthStatus{HealthStatusHealthy, HealthStatusHealthy},
			expected: gen.HealthStatus_HEALTH_STATUS_HEALTHY,
		},
		{
			name:     "one degraded",
			statuses: []HealthStatus{HealthStatusHealthy, HealthStatusDegraded},
			expected: gen.HealthStatus_HEALTH_STATUS_DEGRADED,
		},
		{
			name:     "one unhealthy",
			statuses: []HealthStatus{HealthStatusHealthy, HealthStatusUnhealthy},
			expected: gen.HealthStatus_HEALTH_STATUS_UNHEALTHY,
		},
		{
			name:     "all unspecified",
			statuses: []HealthStatus{HealthStatusUnspecified},
			expected: gen.HealthStatus_HEALTH_STATUS_UNSPECIFIED,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hm := NewHealthManager()
			for i, s := range tt.statuses {
				name := ComponentCollectorManager
				if i > 0 {
					name = ComponentDakrTransport
				}
				hm.Register(name)
				hm.UpdateStatus(name, s, "", nil)
			}

			req := BuildHeartbeatRequest(hm, "c1", "1.0.0", time.Now())
			assert.Equal(t, tt.expected, req.OverallStatus)
		})
	}
}
