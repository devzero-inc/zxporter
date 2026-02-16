package server

import (
	"testing"

	"github.com/devzero-inc/zxporter/internal/health"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
)

func TestMpaServer_HealthyAfterStart(t *testing.T) {
	hm := health.NewHealthManager()
	hm.Register(health.ComponentMpaServer)

	srv := NewMpaServer(logr.Discard(), nil, hm)
	err := srv.Start(0)
	assert.NoError(t, err)
	defer srv.Stop()

	status, exists := hm.GetStatus(health.ComponentMpaServer)
	assert.True(t, exists)
	assert.Equal(t, health.HealthStatusHealthy, status.Status)
}

func TestMpaServer_UnhealthyAfterStop(t *testing.T) {
	hm := health.NewHealthManager()
	hm.Register(health.ComponentMpaServer)

	srv := NewMpaServer(logr.Discard(), nil, hm)
	_ = srv.Start(0)
	srv.Stop()

	status, _ := hm.GetStatus(health.ComponentMpaServer)
	assert.Equal(t, health.HealthStatusUnhealthy, status.Status)
}

func TestMpaServer_NilHealthManager(t *testing.T) {
	srv := NewMpaServer(logr.Discard(), nil, nil)
	err := srv.Start(0)
	assert.NoError(t, err)
	srv.Stop()
}
