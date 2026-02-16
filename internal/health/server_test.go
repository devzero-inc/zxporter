package health

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHealthzHandler_Healthy(t *testing.T) {
	hm := NewHealthManager()
	hm.Register(ComponentCollectorManager)
	hm.UpdateStatus(ComponentCollectorManager, HealthStatusHealthy, "running", nil)

	srv := NewHealthServer(hm, ":8081")
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()

	srv.healthzHandler(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp HealthResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "healthy", resp.Status)
}

func TestHealthzHandler_Unhealthy(t *testing.T) {
	hm := NewHealthManager()
	hm.Register(ComponentCollectorManager)
	hm.UpdateStatus(ComponentCollectorManager, HealthStatusUnhealthy, "no collectors running", nil)

	srv := NewHealthServer(hm, ":8081")
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()

	srv.healthzHandler(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)

	var resp HealthResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "unhealthy", resp.Status)
	assert.Contains(t, resp.Error, "collector_manager")
}

func TestReadyzHandler_Ready(t *testing.T) {
	hm := NewHealthManager()
	hm.Register(ComponentCollectorManager)
	hm.Register(ComponentDakrTransport)
	hm.UpdateStatus(ComponentCollectorManager, HealthStatusHealthy, "running", nil)
	hm.UpdateStatus(ComponentDakrTransport, HealthStatusHealthy, "connected", nil)

	srv := NewHealthServer(hm, ":8081")
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()

	srv.readyzHandler(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp HealthResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "ready", resp.Status)
}

func TestReadyzHandler_NotReady(t *testing.T) {
	hm := NewHealthManager()
	hm.Register(ComponentCollectorManager)
	hm.Register(ComponentDakrTransport)
	// collector_manager still Unspecified
	hm.UpdateStatus(ComponentDakrTransport, HealthStatusHealthy, "connected", nil)

	srv := NewHealthServer(hm, ":8081")
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()

	srv.readyzHandler(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)

	var resp HealthResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "not ready", resp.Status)
}

func TestHealthzHandler_IncludesComponents(t *testing.T) {
	hm := NewHealthManager()
	hm.Register(ComponentCollectorManager)
	hm.Register(ComponentDakrTransport)
	hm.UpdateStatus(ComponentCollectorManager, HealthStatusHealthy, "55/60 active", map[string]string{"active": "55"})
	hm.UpdateStatus(ComponentDakrTransport, HealthStatusDegraded, "retrying", nil)

	srv := NewHealthServer(hm, ":8081")
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()

	srv.healthzHandler(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp HealthResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Len(t, resp.Components, 2)
}
