// internal/collector/scheduler_metrics_collector_test.go
package collector

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

// schedulerMetricsDoFunc lets a test build an HTTPClient from a plain function,
// mirroring nodemon/scraper_test.go's mock transport (that file's roundTripFunc
// implements http.RoundTripper instead, so this collector -- whose HTTPClient
// interface is satisfied by Do, not RoundTrip -- needs its own).
type schedulerMetricsDoFunc func(*http.Request) (*http.Response, error)

func (f schedulerMetricsDoFunc) Do(req *http.Request) (*http.Response, error) {
	return f(req)
}

// testSchedulerMetricsBatchTime is short enough that ResourcesBatcher (started inside
// Start()) flushes well within a test's drain window, instead of waiting out the 5s
// production default. Tests must read from GetResourceChannel(), never batchChan
// directly -- batchChan is the batcher's own input and is being concurrently drained by
// its goroutine once Start() runs; reading it from a test races that goroutine for the
// same items non-deterministically.
const testSchedulerMetricsBatchTime = 20 * time.Millisecond

// newTestSchedulerMetricsCollector uses a long scrape interval so Start's one immediate
// scrape is deterministically the only scrape within a test's drain window;
// TestSchedulerMetricsCollector_ScrapeFailureDoesNotKillTicker overrides this via
// newTestSchedulerMetricsCollectorWithInterval to exercise repeated ticks.
func newTestSchedulerMetricsCollector(k8sClient *k8sfake.Clientset, httpClient HTTPClient) *SchedulerMetricsCollector {
	return newTestSchedulerMetricsCollectorWithInterval(k8sClient, httpClient, time.Hour)
}

func newTestSchedulerMetricsCollectorWithInterval(k8sClient *k8sfake.Clientset, httpClient HTTPClient, interval time.Duration) *SchedulerMetricsCollector {
	c, _ := newTestSchedulerMetricsCollectorWithTelemetry(k8sClient, httpClient, interval)
	return c
}

// newTestSchedulerMetricsCollectorWithTelemetry also returns the fakeTelemetryLogger
// (defined in node_collector_test.go, reused here since it's package-scoped) so tests can
// assert on what convertFamily reported, not just what reached the resource channel.
func newTestSchedulerMetricsCollectorWithTelemetry(k8sClient *k8sfake.Clientset, httpClient HTTPClient, interval time.Duration) (*SchedulerMetricsCollector, *fakeTelemetryLogger) {
	fakeLogger := &fakeTelemetryLogger{}
	c := NewSchedulerMetricsCollector(
		k8sClient,
		httpClient,
		interval,
		DefaultMaxBatchSize,
		testSchedulerMetricsBatchTime,
		logr.Discard(),
		fakeLogger,
	)
	return c, fakeLogger
}

// dzSchedulerSelector mirrors the real Service's selector
// (operators/tunr/helm/dakr/templates/scheduler.yaml: component=scheduler,
// tier=control-plane -- deliberately NOT `app: dz-scheduler`, which looks plausible but
// does not exist in the real chart). Returns a fresh map every call (rather than a shared
// package var) so no test can accidentally mutate another test's fixture through it;
// dzSchedulerService and dzSchedulerPod both call this, which is what keeps them in sync:
// schedulerVerifyPodMatchesServiceSelector checks exactly this relationship.
func dzSchedulerSelector() map[string]string {
	return map[string]string{"component": "scheduler", "tier": "control-plane"}
}

// dzSchedulerService mirrors the real Service's Port/TargetPort split
// (operators/tunr/helm/dakr/templates/scheduler.yaml: port 443, targetPort 10259) --
// distinct on purpose, since resolveEndpoint now reads whichever of the two fits the host
// it's about to scrape (see schedulerMetricsPortName's doc comment). A fixture that
// collapsed them to the same value could hide a Port/TargetPort mixup.
func dzSchedulerService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      schedulerMetricsServiceName,
			Namespace: schedulerMetricsNamespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: dzSchedulerSelector(),
			Ports: []corev1.ServicePort{
				{Name: "https-healthz", Port: 443, TargetPort: intstr.FromInt32(10259)},
			},
		},
	}
}

// dzSchedulerLease builds the dz-scheduler leader-election Lease with the given
// holderIdentity. A nil holderIdentity is represented by passing "" -- schedulerLeaseHolderPodName
// treats a nil *string and an explicit "" identically (see resolveEndpoint), so most tests
// only need the string form; dzSchedulerLeaseNilHolder below covers the *string itself
// being nil, which this helper can never produce.
func dzSchedulerLease(holderIdentity string) *coordinationv1.Lease {
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      schedulerMetricsLeaseName,
			Namespace: schedulerMetricsNamespace,
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity: strPtr(holderIdentity),
		},
	}
}

// dzSchedulerLeaseNilHolder builds the Lease with a nil HolderIdentity *string --
// unreachable via dzSchedulerLease(""), which always allocates a non-nil pointer to an
// empty string. Both must resolve identically (see resolveEndpoint's nil-guard), but only
// this fixture actually exercises the nil-pointer branch of that guard.
func dzSchedulerLeaseNilHolder() *coordinationv1.Lease {
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      schedulerMetricsLeaseName,
			Namespace: schedulerMetricsNamespace,
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity: nil,
		},
	}
}

// dzSchedulerPod builds a dz-scheduler replica pod with the given name and IP, labelled to
// match dzSchedulerSelector -- i.e. a genuine Service backend, the case
// schedulerVerifyPodMatchesServiceSelector is meant to accept. Tests that specifically
// exercise the selector-mismatch rejection build a Pod without these labels directly
// rather than through this fixture.
func dzSchedulerPod(name, podIP string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: schedulerMetricsNamespace,
			Labels:    dzSchedulerSelector(),
		},
		Status: corev1.PodStatus{
			PodIP: podIP,
		},
	}
}

func TestSchedulerMetricsCollector_IsAvailable(t *testing.T) {
	t.Run("false when the dz-scheduler Service does not exist", func(t *testing.T) {
		c := newTestSchedulerMetricsCollector(k8sfake.NewSimpleClientset(), nil)
		assert.False(t, c.IsAvailable(context.Background()))
	})

	t.Run("true when the dz-scheduler Service exists", func(t *testing.T) {
		c := newTestSchedulerMetricsCollector(k8sfake.NewSimpleClientset(dzSchedulerService()), nil)
		assert.True(t, c.IsAvailable(context.Background()))
	})
}

// counterAndHistogramPayload is a scrape response mixing two allow-listed families
// (a counter and a histogram, the latter with an explicit +Inf bucket) with one
// non-allow-listed family that must never reach the resource channel.
const counterAndHistogramPayload = `# HELP scheduler_schedule_attempts_total Number of attempts to schedule pods
# TYPE scheduler_schedule_attempts_total counter
scheduler_schedule_attempts_total{profile="default-scheduler",result="scheduled"} 42
# HELP scheduler_framework_extension_point_duration_seconds Latency for scheduler framework extension points.
# TYPE scheduler_framework_extension_point_duration_seconds histogram
scheduler_framework_extension_point_duration_seconds_bucket{extension_point="Filter",profile="default-scheduler",status="Success",le="0.001"} 5
scheduler_framework_extension_point_duration_seconds_bucket{extension_point="Filter",profile="default-scheduler",status="Success",le="0.01"} 9
scheduler_framework_extension_point_duration_seconds_bucket{extension_point="Filter",profile="default-scheduler",status="Success",le="+Inf"} 10
scheduler_framework_extension_point_duration_seconds_sum{extension_point="Filter",profile="default-scheduler",status="Success"} 0.042
scheduler_framework_extension_point_duration_seconds_count{extension_point="Filter",profile="default-scheduler",status="Success"} 10
# HELP scheduler_cache_size Number of nodes/pods/assumed pods in the scheduler cache.
# TYPE scheduler_cache_size gauge
scheduler_cache_size{type="nodes"} 3
`

// unmodeledLabelPayload carries an allow-listed family whose series has a label
// ("node", not one of the six modeled columns) this collector cannot represent.
const unmodeledLabelPayload = `# HELP dz_scheduler_plugin_score Score assigned to a node by a scoring plugin.
# TYPE dz_scheduler_plugin_score gauge
dz_scheduler_plugin_score{plugin="NodeResourcesFit",profile="default-scheduler",node="ip-10-0-0-1"} 87
`

// staticScrapeClient always returns body with a 200 status. Tests exercising a non-200
// response (TestSchedulerMetricsCollector_ScrapeFailureDoesNotKillTicker) build their own
// schedulerMetricsDoFunc since they need per-call behavior, not a fixed response.
func staticScrapeClient(body string) HTTPClient {
	return schedulerMetricsDoFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})
}

// drainSchedulerMetricsResources reads batches off the collector's post-batcher resource channel until
// timeout, flattening them into one slice. This is the channel the reconciler actually
// consumes in production (see CollectionManager), so it is also the correct one for
// tests to observe rather than the pre-batcher batchChan.
func drainSchedulerMetricsResources(t *testing.T, ch <-chan []CollectedResource, timeout time.Duration) []CollectedResource {
	t.Helper()
	var resources []CollectedResource
	deadline := time.After(timeout)
	for {
		select {
		case batch, ok := <-ch:
			if !ok {
				return resources
			}
			resources = append(resources, batch...)
		case <-deadline:
			return resources
		}
	}
}

func TestSchedulerMetricsCollector_ScrapeAllowListsFamilies(t *testing.T) {
	httpClient := staticScrapeClient(counterAndHistogramPayload)
	c := newTestSchedulerMetricsCollector(k8sfake.NewSimpleClientset(dzSchedulerService()), httpClient)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, c.Start(ctx))
	defer func() { _ = c.Stop() }()

	resources := drainSchedulerMetricsResources(t, c.GetResourceChannel(), 500*time.Millisecond)
	require.NotEmpty(t, resources)

	names := map[string]int{}
	for _, r := range resources {
		require.Equal(t, SchedulerPlacementMetric, r.ResourceType)
		item, ok := r.Object.(map[string]interface{})
		require.True(t, ok)
		name, _ := item["metric_name"].(string)
		names[name]++
		// scheduler_cache_size is not in the six-family allow-list and must never
		// appear on the channel.
		assert.NotEqual(t, "scheduler_cache_size", name)
	}

	assert.Equal(t, 1, names["scheduler_schedule_attempts_total"])
	// 3 buckets + sum + count = 5 rows for the one histogram series.
	assert.Equal(t, 5, names["scheduler_framework_extension_point_duration_seconds"])
}

func TestSchedulerMetricsCollector_HistogramBucketsFlattenIncludingPlusInf(t *testing.T) {
	httpClient := staticScrapeClient(counterAndHistogramPayload)
	c := newTestSchedulerMetricsCollector(k8sfake.NewSimpleClientset(dzSchedulerService()), httpClient)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, c.Start(ctx))
	defer func() { _ = c.Stop() }()

	resources := drainSchedulerMetricsResources(t, c.GetResourceChannel(), 500*time.Millisecond)
	require.NotEmpty(t, resources)

	var bucketLEs []interface{}
	var sawSum, sawCount bool
	for _, r := range resources {
		item := r.Object.(map[string]interface{})
		if item["metric_name"] != "scheduler_framework_extension_point_duration_seconds" {
			continue
		}
		switch item["metric_type"] {
		case "histogram_bucket":
			bucketLEs = append(bucketLEs, item["bucket_le"])
		case "histogram_sum":
			sawSum = true
			assert.InDelta(t, 0.042, item["value"], 0.0001)
		case "histogram_count":
			sawCount = true
			assert.Equal(t, float64(10), item["value"])
		}
	}

	require.True(t, sawSum, "expected a histogram_sum row")
	require.True(t, sawCount, "expected a histogram_count row")
	require.Len(t, bucketLEs, 3)

	// The overflow bucket MUST serialize as the JSON string "+Inf" -- Go cannot
	// marshal math.Inf as a JSON number, and dakr's model only accepts the string
	// form for that bucket.
	assert.Contains(t, bucketLEs, "+Inf")
	assert.Contains(t, bucketLEs, 0.001)
	assert.Contains(t, bucketLEs, 0.01)
}

func TestSchedulerMetricsCollector_UnmodeledLabelIsRejectedNotDropped(t *testing.T) {
	httpClient := staticScrapeClient(unmodeledLabelPayload)
	c := newTestSchedulerMetricsCollector(k8sfake.NewSimpleClientset(dzSchedulerService()), httpClient)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, c.Start(ctx))
	defer func() { _ = c.Stop() }()

	// The series carries a label ("node") outside the six modeled columns, so the
	// collector must reject it outright rather than silently dropping the label --
	// dropping it would collapse two distinct observations into one indistinguishable
	// row with no error anywhere in the pipeline. Nothing should reach the channel.
	resources := drainSchedulerMetricsResources(t, c.GetResourceChannel(), 200*time.Millisecond)
	assert.Empty(t, resources)
}

func TestSchedulerMetricsCollector_ScrapeFailureDoesNotKillTicker(t *testing.T) {
	var calls int
	var mu sync.Mutex
	httpClient := schedulerMetricsDoFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return &http.Response{
				StatusCode: http.StatusInternalServerError,
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     make(http.Header),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(counterAndHistogramPayload)),
			Header:     make(http.Header),
		}, nil
	})

	c := newTestSchedulerMetricsCollectorWithInterval(k8sfake.NewSimpleClientset(dzSchedulerService()), httpClient, 50*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, c.Start(ctx))
	defer func() { _ = c.Stop() }()

	resources := drainSchedulerMetricsResources(t, c.GetResourceChannel(), time.Second)
	require.NotEmpty(t, resources, "a later successful scrape should still emit after an earlier failure")

	mu.Lock()
	defer mu.Unlock()
	assert.GreaterOrEqual(t, calls, 2, "the ticker should have retried after the first failed scrape")
}

func TestSchedulerMetricsCollector_StopIsClean(t *testing.T) {
	httpClient := staticScrapeClient(counterAndHistogramPayload)
	c := newTestSchedulerMetricsCollector(k8sfake.NewSimpleClientset(dzSchedulerService()), httpClient)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, c.Start(ctx))

	assert.NoError(t, c.Stop())
	// A second Stop must not panic (double-close protection).
	assert.NoError(t, c.Stop())
}

func TestSchedulerMetricsCollector_GetType(t *testing.T) {
	c := newTestSchedulerMetricsCollector(k8sfake.NewSimpleClientset(), nil)
	assert.Equal(t, "scheduler_placement_metric", c.GetType())
}

// TestSchedulerMetricsCollector_CounterRowAlwaysCarriesAllSixLabelKeys guards the wire
// invariant dakr's model requires: every one of the six label columns is present as a key
// on every row, "" when the series didn't carry that Prometheus label -- never omitted.
// An "only set labels that are present" cleanup would still pass every other test in this
// file while silently producing rows dakr's decoder can't distinguish from a genuinely
// different series (an omitted JSON key and an explicit "" both decode to Go's zero value,
// so nothing downstream would even error).
func TestSchedulerMetricsCollector_CounterRowAlwaysCarriesAllSixLabelKeys(t *testing.T) {
	httpClient := staticScrapeClient(counterAndHistogramPayload)
	c := newTestSchedulerMetricsCollector(k8sfake.NewSimpleClientset(dzSchedulerService()), httpClient)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, c.Start(ctx))
	defer func() { _ = c.Stop() }()

	resources := drainSchedulerMetricsResources(t, c.GetResourceChannel(), 500*time.Millisecond)
	require.NotEmpty(t, resources)

	var found bool
	for _, r := range resources {
		item := r.Object.(map[string]interface{})
		if item["metric_name"] != "scheduler_schedule_attempts_total" {
			continue
		}
		found = true
		for _, key := range []string{"plugin", "extension_point", "result", "status", "profile", "queue"} {
			require.Containsf(t, item, key, "row is missing label key %q entirely, not just empty", key)
		}
		// scheduler_schedule_attempts_total{profile="default-scheduler",result="scheduled"}
		// -- profile and result are the labels this series actually carries.
		assert.Equal(t, "default-scheduler", item["profile"])
		assert.Equal(t, "scheduled", item["result"])
		// The four the series does NOT carry must be present and empty, not absent.
		assert.Equal(t, "", item["plugin"])
		assert.Equal(t, "", item["extension_point"])
		assert.Equal(t, "", item["status"])
		assert.Equal(t, "", item["queue"])
	}
	require.True(t, found, "expected a scheduler_schedule_attempts_total row")
}

// TestSchedulerMetricsCollector_ScrapeURLReflectsServiceAndReResolvesEachTick closes two
// gaps at once: nothing previously asserted the scrape URL at all (a bug could point it at
// the wrong path or host and every other test would still pass), and nothing verified
// resolveEndpoint is actually called fresh every tick rather than once at Start and cached
// -- which would silently break the "rescheduled/reconfigured scheduler picked up without
// a zxporter restart" requirement this collector exists to satisfy.
func TestSchedulerMetricsCollector_ScrapeURLReflectsServiceAndReResolvesEachTick(t *testing.T) {
	urls := make(chan string, 16)
	httpClient := schedulerMetricsDoFunc(func(req *http.Request) (*http.Response, error) {
		urls <- req.URL.String()
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(counterAndHistogramPayload)),
			Header:     make(http.Header),
		}, nil
	})

	k8sClient := k8sfake.NewSimpleClientset(dzSchedulerService())
	c := newTestSchedulerMetricsCollectorWithInterval(k8sClient, httpClient, 30*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, c.Start(ctx))
	defer func() { _ = c.Stop() }()

	var firstURL string
	select {
	case firstURL = <-urls:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the first scrape request")
	}
	assert.Equal(t, "https://dz-scheduler.devzero-system.svc:443/metrics", firstURL)

	// Change the Service's port. If resolveEndpoint were hoisted out of the per-tick path
	// (cached once at Start), every subsequent request would keep using the stale URL and
	// this would time out.
	updated := dzSchedulerService()
	updated.Spec.Ports[0].Port = 8443
	_, err := k8sClient.CoreV1().Services(schedulerMetricsNamespace).Update(context.Background(), updated, metav1.UpdateOptions{})
	require.NoError(t, err)

	deadline := time.After(2 * time.Second)
	for {
		select {
		case u := <-urls:
			if u == "https://dz-scheduler.devzero-system.svc:8443/metrics" {
				return
			}
		case <-deadline:
			t.Fatal("scrape URL never reflected the updated Service port -- endpoint is being cached instead of re-resolved every tick")
		}
	}
}

// TestSchedulerMetricsCollector_LeasePresentScrapesLeaderPodDirectly is the core
// regression test for issue #10055: when the dz-scheduler Lease exists, the scrape must
// target the leader pod's own IP and the Service's TargetPort (10259) -- never the Service
// DNS name or its Port (443), which is what let kube-proxy/keep-alive stickiness pin a
// scrape to a standby replica forever.
func TestSchedulerMetricsCollector_LeasePresentScrapesLeaderPodDirectly(t *testing.T) {
	urls := make(chan string, 16)
	httpClient := schedulerMetricsDoFunc(func(req *http.Request) (*http.Response, error) {
		urls <- req.URL.String()
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(counterAndHistogramPayload)),
			Header:     make(http.Header),
		}, nil
	})

	leaderPod := dzSchedulerPod("dz-scheduler-68b79bbff5-w4jhd", "10.244.0.6")
	lease := dzSchedulerLease("dz-scheduler-68b79bbff5-w4jhd_8f7453e4-c02d-4fe5-96b2-eb88ad29983a")
	k8sClient := k8sfake.NewSimpleClientset(dzSchedulerService(), leaderPod, lease)
	c := newTestSchedulerMetricsCollector(k8sClient, httpClient)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, c.Start(ctx))
	defer func() { _ = c.Stop() }()

	select {
	case url := <-urls:
		assert.Equal(t, "https://10.244.0.6:10259/metrics", url,
			"must scrape the leader pod's IP on the Service's TargetPort, not the Service DNS name/Port")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the scrape request")
	}
}

// TestSchedulerMetricsCollector_LeaderFailoverRetargetsNextScrape is the failover property
// resolveEndpoint's doc comment promises: since the endpoint is re-resolved every tick, a
// leader change is picked up on the very next scrape with no zxporter restart.
func TestSchedulerMetricsCollector_LeaderFailoverRetargetsNextScrape(t *testing.T) {
	urls := make(chan string, 16)
	httpClient := schedulerMetricsDoFunc(func(req *http.Request) (*http.Response, error) {
		urls <- req.URL.String()
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(counterAndHistogramPayload)),
			Header:     make(http.Header),
		}, nil
	})

	pod1 := dzSchedulerPod("dz-scheduler-68b79bbff5-w4jhd", "10.244.0.6")
	pod2 := dzSchedulerPod("dz-scheduler-68b79bbff5-bf78z", "10.244.2.5")
	lease := dzSchedulerLease("dz-scheduler-68b79bbff5-w4jhd_8f7453e4-c02d-4fe5-96b2-eb88ad29983a")
	k8sClient := k8sfake.NewSimpleClientset(dzSchedulerService(), pod1, pod2, lease)
	c := newTestSchedulerMetricsCollectorWithInterval(k8sClient, httpClient, 30*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, c.Start(ctx))
	defer func() { _ = c.Stop() }()

	select {
	case url := <-urls:
		assert.Equal(t, "https://10.244.0.6:10259/metrics", url, "first scrape should target the original leader")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the first scrape request")
	}

	// Leader election re-elects: the Lease's holderIdentity flips to the other replica.
	updated := lease.DeepCopy()
	updated.Spec.HolderIdentity = strPtr("dz-scheduler-68b79bbff5-bf78z_9a1b2c3d-0000-1111-2222-333344445555")
	_, err := k8sClient.CoordinationV1().Leases(schedulerMetricsNamespace).Update(context.Background(), updated, metav1.UpdateOptions{})
	require.NoError(t, err)

	deadline := time.After(2 * time.Second)
	for {
		select {
		case url := <-urls:
			if url == "https://10.244.2.5:10259/metrics" {
				return
			}
			// Stale scrapes still in flight against the old leader are expected; only
			// fail if the new leader's URL never shows up before the deadline.
		case <-deadline:
			t.Fatal("scrape never retargeted to the new leader after failover")
		}
	}
}

// TestSchedulerMetricsCollector_ResolveEndpoint_LeaseAbsentFallsBackToServiceVIP is the
// dedicated, direct-call counterpart to
// TestSchedulerMetricsCollector_ScrapeURLReflectsServiceAndReResolvesEachTick (which also
// exercises this path incidentally, since it creates no Lease): a genuinely absent Lease
// falls back to the Service VIP, on the Service's own Port (443), not the TargetPort used
// for direct pod scrapes. (This is NOT the single-replica case under the real chart --
// leaderElect is hardcoded true regardless of replica count -- see resolveEndpoint's doc
// comment for which cases actually reach this branch in practice.)
func TestSchedulerMetricsCollector_ResolveEndpoint_LeaseAbsentFallsBackToServiceVIP(t *testing.T) {
	k8sClient := k8sfake.NewSimpleClientset(dzSchedulerService())
	c := newTestSchedulerMetricsCollector(k8sClient, nil)

	endpoint, err := c.resolveEndpoint(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "https://dz-scheduler.devzero-system.svc:443/metrics", endpoint)
}

// TestSchedulerMetricsCollector_ResolveEndpoint_LeaseForbiddenErrorsNotFallsBack is the
// branch the whole leader-scrape design rests on and was previously untested: a Lease Get
// that fails for a reason OTHER than NotFound (RBAC denial being the realistic one -- see
// NewSchedulerMetricsHTTPClient's doc comment on which principals can and can't read this
// Lease) must be treated as "endpoint unresolvable, skip this tick," never as "Lease
// absent, fall back to the Service VIP." Collapsing Forbidden into the NotFound branch
// would silently reintroduce the exact bug this collector exists to fix, on every cluster
// where the RBAC grant is missing or misconfigured, which is precisely the failure mode
// the code-review round that added this test caught.
func TestSchedulerMetricsCollector_ResolveEndpoint_LeaseForbiddenErrorsNotFallsBack(t *testing.T) {
	k8sClient := k8sfake.NewSimpleClientset(dzSchedulerService())
	k8sClient.PrependReactor("get", "leases", func(action ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"},
			schedulerMetricsLeaseName,
			fmt.Errorf("simulated RBAC denial"),
		)
	})
	c := newTestSchedulerMetricsCollector(k8sClient, nil)

	endpoint, err := c.resolveEndpoint(context.Background())
	require.Error(t, err)
	assert.Empty(t, endpoint)
	assert.NotEqual(t, "https://dz-scheduler.devzero-system.svc:443/metrics", endpoint,
		"a Forbidden Lease Get must never resolve to the Service VIP URL")
}

// TestSchedulerMetricsCollector_ResolveEndpoint_LeaseHolderUnresolvable covers the "Lease
// exists but the holder can't be resolved to a usable pod IP" branch, which must error
// rather than silently fall back to the Service VIP -- falling back there would reproduce
// exactly the bug this collector exists to fix (see resolveEndpoint's doc comment).
func TestSchedulerMetricsCollector_ResolveEndpoint_LeaseHolderUnresolvable(t *testing.T) {
	t.Run("holder pod does not exist", func(t *testing.T) {
		lease := dzSchedulerLease("dz-scheduler-68b79bbff5-w4jhd_8f7453e4-c02d-4fe5-96b2-eb88ad29983a")
		k8sClient := k8sfake.NewSimpleClientset(dzSchedulerService(), lease)
		c := newTestSchedulerMetricsCollector(k8sClient, nil)

		_, err := c.resolveEndpoint(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "dz-scheduler-68b79bbff5-w4jhd", "error must name the unresolvable holder")
	})

	t.Run("holder pod has no IP yet", func(t *testing.T) {
		lease := dzSchedulerLease("dz-scheduler-68b79bbff5-w4jhd_8f7453e4-c02d-4fe5-96b2-eb88ad29983a")
		pod := dzSchedulerPod("dz-scheduler-68b79bbff5-w4jhd", "")
		k8sClient := k8sfake.NewSimpleClientset(dzSchedulerService(), lease, pod)
		c := newTestSchedulerMetricsCollector(k8sClient, nil)

		_, err := c.resolveEndpoint(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "dz-scheduler-68b79bbff5-w4jhd", "error must name the unresolvable holder")
		// dzSchedulerPod carries labels matching the Service selector, so the selector
		// check (which now runs before the PodIP check) passes and this pins the
		// empty-IP branch specifically. Without this assertion the substring above
		// would pass identically if the selector check rejected the pod instead --
		// every sibling subtest in this table asserts on message text; this was the
		// one relying on fixture shape alone to land in the right branch.
		assert.Contains(t, err.Error(), "has no pod IP yet")
	})

	t.Run("holder identity is empty", func(t *testing.T) {
		lease := dzSchedulerLease("")
		k8sClient := k8sfake.NewSimpleClientset(dzSchedulerService(), lease)
		c := newTestSchedulerMetricsCollector(k8sClient, nil)

		_, err := c.resolveEndpoint(context.Background())
		require.Error(t, err)
	})

	t.Run("holder identity is nil, not just empty", func(t *testing.T) {
		// dzSchedulerLease("") always allocates a non-nil *string; only this fixture
		// exercises the actual nil-pointer branch of resolveEndpoint's guard (a real
		// Lease that has genuinely never had a leader elected has a nil
		// HolderIdentity, not a pointer to "").
		lease := dzSchedulerLeaseNilHolder()
		k8sClient := k8sfake.NewSimpleClientset(dzSchedulerService(), lease)
		c := newTestSchedulerMetricsCollector(k8sClient, nil)

		_, err := c.resolveEndpoint(context.Background())
		require.Error(t, err, "a nil HolderIdentity must be treated as no leader, not dereferenced")
		assert.Contains(t, err.Error(), "no leader elected yet")
	})

	t.Run("holder pod exists but doesn't match the Service's own selector", func(t *testing.T) {
		// An "imposter" pod: the Lease's holderIdentity resolves it by NAME, and it has
		// an IP, but its labels don't satisfy the dz-scheduler Service's selector -- the
		// case schedulerVerifyPodMatchesServiceSelector exists to catch (issue #10055
		// review: without this check, anything able to write the Lease's holderIdentity
		// could point a scrape, and zxporter's bearer token, at an arbitrary pod).
		imposter := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "dz-scheduler-68b79bbff5-w4jhd",
				Namespace: schedulerMetricsNamespace,
				Labels:    map[string]string{"component": "not-the-scheduler"},
			},
			Status: corev1.PodStatus{PodIP: "10.244.9.9"},
		}
		lease := dzSchedulerLease("dz-scheduler-68b79bbff5-w4jhd_8f7453e4-c02d-4fe5-96b2-eb88ad29983a")
		k8sClient := k8sfake.NewSimpleClientset(dzSchedulerService(), imposter, lease)
		c := newTestSchedulerMetricsCollector(k8sClient, nil)

		_, err := c.resolveEndpoint(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "dz-scheduler-68b79bbff5-w4jhd", "error must name the unresolvable holder")
		assert.Contains(t, err.Error(), "not a Service backend")
	})
}

// TestSchedulerVerifyPodMatchesServiceSelector is a direct unit test on the
// selector-membership check itself: prior coverage only exercised it indirectly through
// resolveEndpoint's happy path (every fixture pod already carried matching labels), which
// can't distinguish "the check runs and passes" from "the check was never wired in at
// all." This asserts the function's cases directly.
func TestSchedulerVerifyPodMatchesServiceSelector(t *testing.T) {
	svc := dzSchedulerService()

	t.Run("pod carrying every selector label passes", func(t *testing.T) {
		pod := dzSchedulerPod("dz-scheduler-1", "10.0.0.1")
		assert.NoError(t, schedulerVerifyPodMatchesServiceSelector(svc, pod))
	})

	t.Run("pod missing one selector key fails, naming it", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "dz-scheduler-1",
				Namespace: schedulerMetricsNamespace,
				Labels:    map[string]string{"component": "scheduler"}, // missing "tier"
			},
		}
		err := schedulerVerifyPodMatchesServiceSelector(svc, pod)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "tier")
	})

	t.Run("empty service selector matches no pod", func(t *testing.T) {
		noSelectorSvc := dzSchedulerService()
		noSelectorSvc.Spec.Selector = nil
		pod := dzSchedulerPod("dz-scheduler-1", "10.0.0.1")
		err := schedulerVerifyPodMatchesServiceSelector(noSelectorSvc, pod)
		require.Error(t, err, "an empty selector must not be treated as \"matches everything\"")
	})

	t.Run("selector value is empty string, pod lacks the label entirely -- must still fail", func(t *testing.T) {
		// This is the exact case a `pod.Labels[key] != wantValue` comparison (no
		// presence check) gets wrong: a missing key reads back as Go's zero value ""
		// for a map, which is indistinguishable from a label that IS present with an
		// explicit empty value -- something Kubernetes permits. An unlabelled pod must
		// not pass just because the selector's value happens to be "".
		emptyValueSvc := dzSchedulerService()
		emptyValueSvc.Spec.Selector = map[string]string{"component": ""}
		unlabelledPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "dz-scheduler-1",
				Namespace: schedulerMetricsNamespace,
				// No Labels at all -- pod.Labels["component"] reads back "" either way.
			},
		}
		err := schedulerVerifyPodMatchesServiceSelector(emptyValueSvc, unlabelledPod)
		require.Error(t, err, "an unlabelled pod must not match a selector whose value is the empty string")
	})
}

// TestSchedulerLeaseHolderPodName_ParsesHolderIdentity is a direct unit test on the
// holderIdentity parsing rule: everything before the LAST underscore is the pod name
// (client-go's leaderelection writes "<hostname>_<uuid>", and a Kubernetes pod name can
// never itself contain "_" -- see schedulerLeaseHolderPodName's doc comment), a holder
// with no underscore is used as-is, and a holder whose derived pod name comes out empty
// (including a merely-empty holder) is an error.
func TestSchedulerLeaseHolderPodName_ParsesHolderIdentity(t *testing.T) {
	t.Run("pod name and UUID split on the last underscore", func(t *testing.T) {
		podName, err := schedulerLeaseHolderPodName("dz-scheduler-68b79bbff5-w4jhd_8f7453e4-c02d-4fe5-96b2-eb88ad29983a")
		require.NoError(t, err)
		assert.Equal(t, "dz-scheduler-68b79bbff5-w4jhd", podName)
	})

	t.Run("no underscore is used as-is", func(t *testing.T) {
		podName, err := schedulerLeaseHolderPodName("dz-scheduler-leader")
		require.NoError(t, err)
		assert.Equal(t, "dz-scheduler-leader", podName)
	})

	t.Run("empty holder is an error", func(t *testing.T) {
		_, err := schedulerLeaseHolderPodName("")
		require.Error(t, err)
	})

	t.Run("holder that is just an underscore derives an empty pod name, which is an error", func(t *testing.T) {
		_, err := schedulerLeaseHolderPodName("_")
		require.Error(t, err, "must not silently resolve to Pods().Get(ctx, \"\")")
	})

	t.Run("holder starting with an underscore derives an empty pod name, which is an error", func(t *testing.T) {
		_, err := schedulerLeaseHolderPodName("_8f7453e4-c02d-4fe5-96b2-eb88ad29983a")
		require.Error(t, err)
	})
}

func strPtr(s string) *string { return &s }

// TestSchedulerLeaderTargetPort is a direct unit test on the Service-targetPort ->
// container-port resolution: an int targetPort is used as-is, a named targetPort is
// resolved against the pod's own container ports, and a named targetPort that isn't
// declared on any container is an error rather than a guess. dz-scheduler's real
// container declares no `ports:` at all today (see this function's doc comment), so the
// named-port branches below are exercised only here, not by any resolveEndpoint-level
// test.
func TestSchedulerLeaderTargetPort(t *testing.T) {
	t.Run("int target port is used as-is", func(t *testing.T) {
		pod := dzSchedulerPod("dz-scheduler-1", "10.0.0.1")
		port, err := schedulerLeaderTargetPort(pod, intstr.FromInt32(10259))
		require.NoError(t, err)
		assert.Equal(t, int32(10259), port)
	})

	t.Run("named target port resolves against the pod's own container ports", func(t *testing.T) {
		pod := dzSchedulerPod("dz-scheduler-1", "10.0.0.1")
		pod.Spec.Containers = []corev1.Container{
			{
				Name:  "kube-second-scheduler",
				Ports: []corev1.ContainerPort{{Name: "https-healthz", ContainerPort: 10259}},
			},
		}
		port, err := schedulerLeaderTargetPort(pod, intstr.FromString("https-healthz"))
		require.NoError(t, err)
		assert.Equal(t, int32(10259), port)
	})

	t.Run("named target port absent from every container is an error, not a guess", func(t *testing.T) {
		// This is dz-scheduler's actual shape today: a container with no declared
		// ports at all.
		pod := dzSchedulerPod("dz-scheduler-1", "10.0.0.1")
		_, err := schedulerLeaderTargetPort(pod, intstr.FromString("https-healthz"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "https-healthz")
	})
}

// TestSchedulerMetricLabels_RejectsUnmodeledLabelNamingIt is a direct unit test on the
// rejection function itself: prior coverage only asserted "nothing came out the other
// end," which a logr.Discard()-based test can't tell apart from "the label name silently
// disappeared from the log line" -- deleting the log entirely (not just discarding it)
// would still pass that kind of assertion. This asserts the returned error names the
// specific offending label.
func TestSchedulerMetricLabels_RejectsUnmodeledLabelNamingIt(t *testing.T) {
	pairs := []*dto.LabelPair{
		{Name: strPtr("plugin"), Value: strPtr("NodeResourcesFit")},
		{Name: strPtr("node"), Value: strPtr("ip-10-0-0-1")},
	}

	_, err := schedulerMetricLabels("dz_scheduler_plugin_score", pairs)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "node", "error must name the offending label")
	assert.Contains(t, err.Error(), "dz_scheduler_plugin_score", "error must name the metric family")
}

// TestSchedulerMetricsCollector_UnmodeledLabelRejectsOnlyThatSeriesAndReportsTelemetry
// covers the sibling-isolation claim convertFamily's own doc comment makes (rejecting one
// series must not affect its siblings in the same family/scrape) and the telemetry-report
// half of I5: a rejection must be visible outside pod logs, not just logger.Error.
func TestSchedulerMetricsCollector_UnmodeledLabelRejectsOnlyThatSeriesAndReportsTelemetry(t *testing.T) {
	const mixedPayload = `# HELP dz_scheduler_plugin_score Score assigned to a node by a scoring plugin.
# TYPE dz_scheduler_plugin_score gauge
dz_scheduler_plugin_score{plugin="NodeResourcesFit",profile="default-scheduler"} 55
dz_scheduler_plugin_score{plugin="NodeResourcesFit",profile="default-scheduler",node="ip-10-0-0-1"} 87
`
	httpClient := staticScrapeClient(mixedPayload)
	k8sClient := k8sfake.NewSimpleClientset(dzSchedulerService())
	c, fakeLogger := newTestSchedulerMetricsCollectorWithTelemetry(k8sClient, httpClient, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, c.Start(ctx))
	defer func() { _ = c.Stop() }()

	resources := drainSchedulerMetricsResources(t, c.GetResourceChannel(), 500*time.Millisecond)
	require.Len(t, resources, 1, "the rejected series must not suppress its accepted sibling in the same family/scrape")

	item := resources[0].Object.(map[string]interface{})
	assert.Equal(t, "dz_scheduler_plugin_score", item["metric_name"])
	assert.Equal(t, "NodeResourcesFit", item["plugin"])
	assert.Equal(t, float64(55), item["value"])

	fakeLogger.mu.Lock()
	reports := append([]fakeReport(nil), fakeLogger.reports...)
	fakeLogger.mu.Unlock()
	require.Len(t, reports, 1, "expected exactly one telemetry report for the rejected series")
	require.Error(t, reports[0].err)
	assert.Contains(t, reports[0].err.Error(), "node")
	assert.Equal(t, "dz_scheduler_plugin_score", reports[0].fields["metric_family"])
}

// TestSchedulerMetricsCollector_AllowedFamiliesMatchesDakrIngestAllowList is the
// zxporter-side half of a pin that must live on both sides of the module boundary.
// dakr's ingest-side allow-list (schedulerPlacementMetricAllowedNames in
// dakr/server/metrics_collector.go) hardcodes this exact six-family set in its own
// test (dakr/server/scheduler_placement_metrics_wiring_test.go,
// TestSchedulerPlacementMetricCrossModuleRoundTrip's "metric family allow-list..."
// subtest) and asserts it against dakr's real map -- so that test catches a dakr-side
// rename or removal, but NOT a family added or removed here, because its copy of this
// list is a frozen literal, not a read of schedulerMetricsAllowedFamilies. This test is
// the missing other direction: it reads THIS collector's real map, so adding or
// removing a family here without updating the mirrored literal on dakr's side fails
// here instead of silently dropping (or never emitting) an entire metric family in
// production. A mechanical shared import isn't available -- operators/zxporter/go.mod
// does not require the root module -- so two mirrored literals, each pinned against
// its own side's real map, is the actual closure; neither test alone provides it.
//
// It carries the equivalent pin for metric_type. Before this, the histogram trio
// (schedulerMetricTypeHistogramBucket/Sum/Count) was pinned only incidentally, by the
// literal switch in TestSchedulerMetricsCollector_HistogramBucketsFlattenIncludingPlusInf,
// and nothing asserted schedulerMetricTypeCounter or schedulerMetricTypeGauge at all --
// a rename or typo of either constant would have passed both modules' test suites and
// silently dropped every counter and gauge row at dakr ingest (schedulerPlacementMetricDropReason
// rejects any metric_type outside models.AllSchedulerMetricTypes, logged only as a
// throttled Warn). This asserts all five of this collector's metric_type constants
// against dakr's vocabulary, set-for-set, the same bidirectional way as the family
// check above -- so a rename on either side that is not mirrored on both fails here.
func TestSchedulerMetricsCollector_AllowedFamiliesMatchesDakrIngestAllowList(t *testing.T) {
	dakrAllowList := map[string]bool{
		"dz_scheduler_plugin_score":                            true,
		"scheduler_schedule_attempts_total":                    true,
		"scheduler_framework_extension_point_duration_seconds": true,
		"scheduler_plugin_execution_duration_seconds":          true,
		"scheduler_plugin_evaluation_total":                    true,
		"scheduler_pending_pods":                               true,
	}

	require.Len(t, schedulerMetricsAllowedFamilies, len(dakrAllowList),
		"this collector's allow-list must have exactly the families dakr's ingest allow-list accepts")
	for name := range dakrAllowList {
		assert.True(t, schedulerMetricsAllowedFamilies[name],
			"dakr's ingest allow-list has family %q that this collector does not emit", name)
	}
	for name := range schedulerMetricsAllowedFamilies {
		assert.True(t, dakrAllowList[name],
			"this collector emits family %q that dakr's ingest allow-list does not accept", name)
	}

	// dakr's models.AllSchedulerMetricTypes, hardcoded rather than imported for the same
	// separate-module reason as dakrAllowList above.
	dakrMetricTypes := map[string]bool{
		"counter":          true,
		"gauge":            true,
		"histogram_bucket": true,
		"histogram_sum":    true,
		"histogram_count":  true,
	}
	// This collector's own constants, read directly so a rename or typo of any of them
	// fails this test.
	zxporterMetricTypes := map[string]bool{
		schedulerMetricTypeCounter:         true,
		schedulerMetricTypeGauge:           true,
		schedulerMetricTypeHistogramBucket: true,
		schedulerMetricTypeHistogramSum:    true,
		schedulerMetricTypeHistogramCount:  true,
	}

	require.Len(t, zxporterMetricTypes, len(dakrMetricTypes),
		"this collector's metric_type constants must have exactly the types dakr's vocabulary accepts")
	for name := range dakrMetricTypes {
		assert.True(t, zxporterMetricTypes[name],
			"dakr's metric_type vocabulary has %q that this collector's constants do not produce", name)
	}
	for name := range zxporterMetricTypes {
		assert.True(t, dakrMetricTypes[name],
			"this collector emits metric_type %q that dakr's vocabulary does not accept", name)
	}
}
