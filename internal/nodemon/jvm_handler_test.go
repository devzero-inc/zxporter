package nodemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devzero-inc/zxporter/internal/nodemon"
)

// fakeJVMQuerier is a test double for JVMMetricsQuerier.
type fakeJVMQuerier struct {
	metrics []nodemon.JVMMetric
	err     error
}

func (f *fakeJVMQuerier) QueryJVMMetrics(_ context.Context) ([]nodemon.JVMMetric, error) {
	return f.metrics, f.err
}

// TestJVMMetricsHandler_CompactJSON asserts the response body is compact
// (no indentation) — the zxporter collector parses this programmatically, so
// pretty-printing only costs CPU and bytes on every request.
func TestJVMMetricsHandler_CompactJSON(t *testing.T) {
	r := require.New(t)
	zapLog, _ := zap.NewDevelopment()
	log := zapr.NewLogger(zapLog)

	metrics := []nodemon.JVMMetric{
		{NodeName: "node-1", Pod: "app", Namespace: "default", Container: "main"},
	}
	handler := nodemon.NewJVMMetricsHandler(&fakeJVMQuerier{metrics: metrics}, log, 0)

	req := httptest.NewRequest(http.MethodGet, "/container/jvm-metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	r.Equal(http.StatusOK, rec.Code)

	wantBody, err := json.Marshal(metrics)
	r.NoError(err)
	wantBody = append(wantBody, '\n') // json.Encoder.Encode always appends a trailing newline

	r.Equal(string(wantBody), rec.Body.String())
}

// TestJVMMetricsHandler_ServesPartialResultsOnError proves the handler
// mirrors the runtime handler's behavior: when QueryJVMMetrics returns a
// non-empty snapshot alongside an error (JVMCollector.Collect deliberately
// keeps the last good snapshot when a cycle fails), the handler should serve
// that snapshot with 200, not discard it and hard-fail with 500.
func TestJVMMetricsHandler_ServesPartialResultsOnError(t *testing.T) {
	r := require.New(t)
	zapLog, _ := zap.NewDevelopment()
	log := zapr.NewLogger(zapLog)

	metrics := []nodemon.JVMMetric{
		{NodeName: "node-1", Pod: "app", Namespace: "default", Container: "main"},
	}
	handler := nodemon.NewJVMMetricsHandler(&fakeJVMQuerier{metrics: metrics, err: errors.New("last cycle failed")}, log, 0)

	req := httptest.NewRequest(http.MethodGet, "/container/jvm-metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	r.Equal(http.StatusOK, rec.Code, "a non-empty cached snapshot must still be served even if the last collection cycle errored")

	var result []nodemon.JVMMetric
	r.NoError(json.Unmarshal(rec.Body.Bytes(), &result))
	r.Equal(metrics, result)
}

// TestJVMMetricsHandler_HardFailsWhenNothingCached proves the handler still
// returns 500 when there's genuinely nothing usable — no metrics and an
// error (e.g. before the first background Collect completes).
func TestJVMMetricsHandler_HardFailsWhenNothingCached(t *testing.T) {
	r := require.New(t)
	zapLog, _ := zap.NewDevelopment()
	log := zapr.NewLogger(zapLog)

	handler := nodemon.NewJVMMetricsHandler(&fakeJVMQuerier{metrics: nil, err: errors.New("metrics not yet collected")}, log, 0)

	req := httptest.NewRequest(http.MethodGet, "/container/jvm-metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	r.Equal(http.StatusInternalServerError, rec.Code)
}
