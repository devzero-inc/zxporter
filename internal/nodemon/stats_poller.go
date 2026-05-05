package nodemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-logr/logr"
)

const statsSummaryPath = "/stats/summary"

// StatsPoller fetches the kubelet /stats/summary endpoint and parses the response.
type StatsPoller struct {
	baseURL    string
	httpClient HTTPClient
	log        logr.Logger
}

// NewStatsPoller creates a new StatsPoller targeting the given kubelet base URL.
func NewStatsPoller(baseURL string, httpClient HTTPClient, log logr.Logger) *StatsPoller {
	return &StatsPoller{
		baseURL:    baseURL,
		httpClient: httpClient,
		log:        log.WithName("stats-poller"),
	}
}

// Poll fetches and parses /stats/summary from the kubelet. Uses a 10-second timeout.
func (p *StatsPoller) Poll(ctx context.Context) (*StatsSummary, error) {
	ctxWithTimeout, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	url := p.baseURL + statsSummaryPath

	req, err := http.NewRequestWithContext(ctxWithTimeout, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("stats poller: cannot create request: %w", err)
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("stats poller: HTTP request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("stats poller: request failed with status %d", resp.StatusCode)
	}

	var summary StatsSummary
	if err := json.NewDecoder(resp.Body).Decode(&summary); err != nil {
		return nil, fmt.Errorf("stats poller: failed to decode response: %w", err)
	}

	p.log.V(1).Info("Polled kubelet stats/summary",
		"node", summary.Node.NodeName,
		"podCount", len(summary.Pods),
	)

	return &summary, nil
}
