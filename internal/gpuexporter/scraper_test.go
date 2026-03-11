package gpuexporter_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/devzero-inc/zxporter/internal/gpuexporter"
)

var metricsString = `# HELP DCGM_FI_DEV_GPU_TEMP Current temperature readings for the device in degrees C.
# TYPE DCGM_FI_DEV_GPU_TEMP gauge
DCGM_FI_DEV_GPU_TEMP{gpu="0",UUID="GPU-93461651-6be6-8fb7-a69a-c9eedc6984db",device="nvidia0",modelName="Tesla T4",Hostname="gke-gpu-default-pool",container="",namespace="",pod=""} 40
`

// roundTripFunc implements http.RoundTripper for testing.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func newMockHTTPClient(fn roundTripFunc) gpuexporter.HTTPClient {
	return &http.Client{Transport: fn}
}

func TestScraper_Scrape(t *testing.T) {
	zapLog, _ := zap.NewDevelopment()
	log := zapr.NewLogger(zapLog)

	t.Run("scrapes metrics without error", func(t *testing.T) {
		httpClient := newMockHTTPClient(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(metricsString)),
			}, nil
		})
		scraper := gpuexporter.NewScraper(httpClient, log)

		metricsFamily, err := scraper.Scrape(
			context.Background(),
			[]string{
				"http://localhost:9400/metrics",
				"http://localhost:9410/metrics",
				"http://localhost:9420/metrics",
			})

		r := require.New(t)
		r.NoError(err)
		r.NotNil(metricsFamily)
		r.Len(metricsFamily, 3)
		for _, mf := range metricsFamily {
			r.NotEmpty(mf)
		}
	})

	t.Run("partially scrapes metrics when some exporter returns non-200", func(t *testing.T) {
		callCount := 0
		httpClient := newMockHTTPClient(func(req *http.Request) (*http.Response, error) {
			callCount++
			if req.URL.Host == "localhost:9410" {
				return &http.Response{
					StatusCode: http.StatusInternalServerError,
					Body:       io.NopCloser(strings.NewReader("")),
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(metricsString)),
			}, nil
		})
		scraper := gpuexporter.NewScraper(httpClient, log)

		metricsFamily, err := scraper.Scrape(
			context.Background(),
			[]string{
				"http://localhost:9400/metrics",
				"http://localhost:9410/metrics",
				"http://localhost:9420/metrics",
			})

		r := require.New(t)
		r.NoError(err)
		r.NotNil(metricsFamily)
		r.Len(metricsFamily, 2)
	})

	t.Run("partially scrapes metrics when some exporter cannot be reached", func(t *testing.T) {
		httpClient := newMockHTTPClient(func(req *http.Request) (*http.Response, error) {
			if req.URL.Host == "localhost:9410" {
				return nil, &net_error{msg: "connection refused"}
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(metricsString)),
			}, nil
		})
		scraper := gpuexporter.NewScraper(httpClient, log)

		metricsFamily, err := scraper.Scrape(
			context.Background(),
			[]string{
				"http://localhost:9400/metrics",
				"http://localhost:9410/metrics",
				"http://localhost:9420/metrics",
			})

		r := require.New(t)
		r.NoError(err)
		r.NotNil(metricsFamily)
		r.Len(metricsFamily, 2)
	})
}

type net_error struct {
	msg string
}

func (e *net_error) Error() string   { return e.msg }
func (e *net_error) Timeout() bool   { return false }
func (e *net_error) Temporary() bool { return false }
