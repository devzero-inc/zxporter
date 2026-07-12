package nodemon

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

// stubHandler returns a fixed status so we only assert on route registration,
// not handler internals (those are covered by the handler's own tests).
func stubHandler(status int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	})
}

// TestNewServerMux_HighPrivilegeMode mirrors cmd/zxporter-nodemon/main.go's wiring
// when RUNTIME_METRICS_ENABLED=true (the default, privileged install): the JVM and
// combined runtime-metrics handlers are non-nil and must be reachable.
func TestNewServerMux_HighPrivilegeMode(t *testing.T) {
	mux := NewServerMux(stubHandler(http.StatusOK), stubHandler(http.StatusOK), stubHandler(http.StatusOK))

	for _, path := range []string{"/container/metrics", "/container/jvm-metrics", "/container/runtime-metrics"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equalf(t, http.StatusOK, rec.Code, "expected %s to be registered in high-privilege mode", path)
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
}

// TestNewServerMux_LowPrivilegeMode mirrors main.go's wiring when
// RUNTIME_METRICS_ENABLED=false (or unset) — the low-privilege install path with no
// hostPID/root/SYS_PTRACE. JVM and runtime-metrics collection never start, so main.go
// passes nil handlers for them; those routes must not be registered, while the
// always-on endpoints (container metrics, healthz) keep working.
func TestNewServerMux_LowPrivilegeMode(t *testing.T) {
	mux := NewServerMux(stubHandler(http.StatusOK), nil, nil)

	for _, path := range []string{"/container/jvm-metrics", "/container/runtime-metrics"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equalf(t, http.StatusNotFound, rec.Code, "expected %s to be absent in low-privilege mode", path)
	}

	for _, path := range []string{"/container/metrics", "/healthz"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equalf(t, http.StatusOK, rec.Code, "expected %s to still work in low-privilege mode", path)
	}
}
