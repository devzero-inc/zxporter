package nodemon_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devzero-inc/zxporter/internal/nodemon"
)

// fakeRuntimeQuerier is a test double for RuntimeMetricsQuerier.
type fakeRuntimeQuerier struct {
	metrics nodemon.RuntimeMetrics
	err     error
}

func (f *fakeRuntimeQuerier) QueryRuntimeMetrics(_ context.Context) (nodemon.RuntimeMetrics, error) {
	return f.metrics, f.err
}

// TestRuntimeMetricsHandler_CompactJSON asserts the response body is compact
// (no indentation) — the zxporter collector parses this programmatically, so
// pretty-printing only costs CPU and bytes on every request.
func TestRuntimeMetricsHandler_CompactJSON(t *testing.T) {
	r := require.New(t)
	zapLog, _ := zap.NewDevelopment()
	log := zapr.NewLogger(zapLog)

	metrics := nodemon.RuntimeMetrics{
		JVM: []nodemon.JVMMetric{
			{NodeName: "node-1", Pod: "app", Namespace: "default", Container: "main"},
		},
		Runtimes: []nodemon.RuntimeProcessMetric{
			{Runtime: "python", NodeName: "node-1", Pod: "worker", Namespace: "default", Container: "main"},
		},
	}
	handler := nodemon.NewRuntimeMetricsHandler(&fakeRuntimeQuerier{metrics: metrics}, log, 0)

	req := httptest.NewRequest(http.MethodGet, "/container/runtime-metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	r.Equal(http.StatusOK, rec.Code)

	wantBody, err := json.Marshal(metrics)
	r.NoError(err)
	wantBody = append(wantBody, '\n')

	r.Equal(string(wantBody), rec.Body.String())
}
