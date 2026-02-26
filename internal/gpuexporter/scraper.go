package gpuexporter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-logr/logr"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"golang.org/x/sync/errgroup"
)

const maxConcurrentScrapes = 15

// Scraper fetches and parses Prometheus-format metrics from DCGM exporter endpoints.
type Scraper interface {
	Scrape(ctx context.Context, urls []string) ([]MetricFamilyMap, error)
}

// HTTPClient abstracts *http.Client for testing.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

type scrapeResult struct {
	metricFamilyMap MetricFamilyMap
	err             error
}

type scraper struct {
	httpClient HTTPClient
	log        logr.Logger
}

// NewScraper creates a new DCGM metrics scraper.
func NewScraper(httpClient HTTPClient, log logr.Logger) Scraper {
	return &scraper{
		httpClient: httpClient,
		log:        log.WithName("gpu-scraper"),
	}
}

func (s *scraper) Scrape(ctx context.Context, urls []string) ([]MetricFamilyMap, error) {
	var g errgroup.Group
	g.SetLimit(maxConcurrentScrapes)

	resultsChan := make(chan scrapeResult, maxConcurrentScrapes)

	for i := range urls {
		url := urls[i]
		g.Go(func() error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				metrics, err := s.scrapeURL(ctx, url)
				if err != nil {
					err = fmt.Errorf("error fetching metrics from '%s': %w", url, err)
				}
				resultsChan <- scrapeResult{metricFamilyMap: metrics, err: err}
			}
			return nil
		})
	}

	go func() {
		if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
			s.log.Error(err, "Error during scrape")
		}
		close(resultsChan)
	}()

	metrics := make([]MetricFamilyMap, 0, len(urls))
	for r := range resultsChan {
		if r.err != nil {
			s.log.Error(r.err, "Failed to scrape metrics")
			continue
		}
		metrics = append(metrics, r.metricFamilyMap)
	}

	return metrics, nil
}

func (s *scraper) scrapeURL(ctx context.Context, url string) (map[string]*dto.MetricFamily, error) {
	ctxWithTimeout, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctxWithTimeout, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("cannot create request: %w", err)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("request failed with status %d", resp.StatusCode)
	}

	var parser expfmt.TextParser
	families, err := parser.TextToMetricFamilies(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("cannot parse metrics: %w", err)
	}

	return families, nil
}
