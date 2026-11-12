package collector

import (
	"strings"
	"testing"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	dto "github.com/prometheus/client_model/go"
)

// sampleExpfmt is a synthetic Prometheus exposition text that exercises:
//   - keda_scaler_metrics_value with "triggerIndex" label (modern)
//   - keda_scaler_metrics_value with "scalerIndex" label (legacy)
//   - keda_scaler_active for a subset of the metric series
const sampleExpfmt = `# HELP keda_scaler_metrics_value The current value for each scaler's metric that would be used by the HPA in computing the target replica count.
# TYPE keda_scaler_metrics_value gauge
keda_scaler_metrics_value{metric="queue-depth",namespace="prod",scaledObject="my-worker",scaler="rabbitmq",triggerIndex="0"} 42
keda_scaler_metrics_value{metric="queue-depth",namespace="prod",scaledObject="my-worker",scaler="rabbitmq",triggerIndex="1"} 0
keda_scaler_metrics_value{metric="queue-depth",namespace="staging",scaledObject="my-worker",scaler="rabbitmq",scalerIndex="0"} 7
# HELP keda_scaler_active Indicates whether a scaler is active.
# TYPE keda_scaler_active gauge
keda_scaler_active{metric="queue-depth",namespace="prod",scaledObject="my-worker",scaler="rabbitmq",triggerIndex="0"} 1
keda_scaler_active{metric="queue-depth",namespace="prod",scaledObject="my-worker",scaler="rabbitmq",triggerIndex="1"} 0
keda_scaler_active{metric="queue-depth",namespace="staging",scaledObject="my-worker",scaler="rabbitmq",scalerIndex="0"} 1
`

// TestKedaScalerMetrics_classifyScrapeOutcome pins the diagnostic classification
// that decides whether an empty scrape is benign (idle cluster) or the actionable
// misconfiguration that leaves the scaler-metrics graph silently empty (operator
// reachable + ScaledObjects present, yet no keda_scaler_metrics_value samples).
func TestKedaScalerMetrics_classifyScrapeOutcome(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name             string
		numSamples       int
		numScaledObjects int
		want             scrapeOutcome
	}{
		{"samples produced -> sent", 3, 5, outcomeSent},
		{"samples with zero SOs still sent", 2, 0, outcomeSent},
		{"empty and no ScaledObjects -> idle", 0, 0, outcomeIdle},
		{"empty but ScaledObjects present -> misconfigured", 0, 4, outcomeMisconfigured},
		{"empty with a single ScaledObject -> misconfigured", 0, 1, outcomeMisconfigured},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyScrapeOutcome(tc.numSamples, tc.numScaledObjects); got != tc.want {
				t.Fatalf("classifyScrapeOutcome(%d, %d) = %d, want %d",
					tc.numSamples, tc.numScaledObjects, got, tc.want)
			}
		})
	}
}

// TestKedaScalerMetrics_parseTriggerIndex validates the happy-path parse using
// the modern "triggerIndex" label.
func TestKedaScalerMetrics_parseTriggerIndex(t *testing.T) {
	t.Parallel()

	mfs, err := parsePrometheusExposition(strings.NewReader(sampleExpfmt), "text/plain")
	if err != nil {
		t.Fatalf("parsePrometheusExposition: %v", err)
	}

	// Provide a resolve map for the prod ScaledObject only.
	resolveMap := map[scaledObjectKey]workloadRef{
		{Namespace: "prod", Name: "my-worker"}: {Kind: "Deployment", Name: "my-worker-deploy"},
	}

	samples := parseScalerMetrics(mfs, resolveMap)

	// Only the prod namespace entries are in the resolveMap; staging should be
	// skipped.  prod has two triggers → two samples.
	if len(samples) != 2 {
		t.Fatalf("expected 2 samples, got %d: %v", len(samples), samples)
	}

	for _, s := range samples {
		if s.Namespace != "prod" {
			t.Errorf("unexpected namespace %q", s.Namespace)
		}
		if s.Scaledobject != "my-worker" {
			t.Errorf("unexpected scaledobject %q", s.Scaledobject)
		}
		if s.WorkloadKind != "Deployment" {
			t.Errorf("unexpected workload kind %q", s.WorkloadKind)
		}
		if s.WorkloadName != "my-worker-deploy" {
			t.Errorf("unexpected workload name %q", s.WorkloadName)
		}
		if s.ScalerType != "rabbitmq" {
			t.Errorf("unexpected scaler type %q", s.ScalerType)
		}
		if s.MetricName != "queue-depth" {
			t.Errorf("unexpected metric name %q", s.MetricName)
		}
	}

	// Check individual trigger values.
	byTrigger := make(map[int32]*gen.ScalerMetricSample)
	for _, s := range samples {
		byTrigger[s.TriggerIndex] = s
	}

	s0 := byTrigger[0]
	if s0 == nil {
		t.Fatal("no sample for triggerIndex=0")
	}
	if s0.Value != 42 {
		t.Errorf("triggerIndex=0 value: want 42, got %v", s0.Value)
	}
	if !s0.Active {
		t.Error("triggerIndex=0 should be active")
	}

	s1 := byTrigger[1]
	if s1 == nil {
		t.Fatal("no sample for triggerIndex=1")
	}
	if s1.Value != 0 {
		t.Errorf("triggerIndex=1 value: want 0, got %v", s1.Value)
	}
	if s1.Active {
		t.Error("triggerIndex=1 should be inactive")
	}
}

// TestKedaScalerMetrics_parseScalerIndex validates that the legacy "scalerIndex"
// label is accepted and its value used as the trigger index.
func TestKedaScalerMetrics_parseScalerIndex(t *testing.T) {
	t.Parallel()

	mfs, err := parsePrometheusExposition(strings.NewReader(sampleExpfmt), "text/plain")
	if err != nil {
		t.Fatalf("parsePrometheusExposition: %v", err)
	}

	resolveMap := map[scaledObjectKey]workloadRef{
		{Namespace: "staging", Name: "my-worker"}: {Kind: "StatefulSet", Name: "my-worker-sts"},
	}

	samples := parseScalerMetrics(mfs, resolveMap)
	if len(samples) != 1 {
		t.Fatalf("expected 1 sample, got %d", len(samples))
	}
	s := samples[0]
	if s.Namespace != "staging" {
		t.Errorf("namespace: want staging, got %q", s.Namespace)
	}
	if s.TriggerIndex != 0 {
		t.Errorf("trigger index: want 0, got %d", s.TriggerIndex)
	}
	if s.Value != 7 {
		t.Errorf("value: want 7, got %v", s.Value)
	}
	if !s.Active {
		t.Error("sample should be active (scalerIndex=0 has active=1)")
	}
	if s.WorkloadKind != "StatefulSet" {
		t.Errorf("workload kind: want StatefulSet, got %q", s.WorkloadKind)
	}
}

// TestKedaScalerMetrics_parseNilResolveMap confirms that a nil resolveMap emits
// ZERO samples: without a resolvable workload identity we must never send a
// sample with blank workload_kind/workload_name (it would poison dakr's
// GetByWorkload lookup).
func TestKedaScalerMetrics_parseNilResolveMap(t *testing.T) {
	t.Parallel()

	exposition := `# HELP keda_scaler_metrics_value metric
# TYPE keda_scaler_metrics_value gauge
keda_scaler_metrics_value{metric="q",namespace="ns",scaledObject="so",scaler="redis",triggerIndex="0"} 5
`
	mfs, err := parsePrometheusExposition(strings.NewReader(exposition), "text/plain")
	if err != nil {
		t.Fatalf("parsePrometheusExposition: %v", err)
	}

	if samples := parseScalerMetrics(mfs, nil); len(samples) != 0 {
		t.Fatalf("expected 0 samples for a nil resolveMap, got %d: %v", len(samples), samples)
	}

	// An empty (non-nil) map is equally unresolvable and must also emit nothing.
	if samples := parseScalerMetrics(mfs, map[scaledObjectKey]workloadRef{}); len(samples) != 0 {
		t.Fatalf("expected 0 samples for an empty resolveMap, got %d: %v", len(samples), samples)
	}

	// A resolveMap that resolves a DIFFERENT ScaledObject must skip the
	// unresolved one, still emitting nothing.
	other := map[scaledObjectKey]workloadRef{
		{Namespace: "ns", Name: "different-so"}: {Kind: "Deployment", Name: "x"},
	}
	if samples := parseScalerMetrics(mfs, other); len(samples) != 0 {
		t.Fatalf("expected 0 samples when the ScaledObject is unresolved, got %d: %v", len(samples), samples)
	}
}

// TestKedaScalerMetrics_triggerIndexFallback tests the triggerIndex helper for
// both label names.
func TestKedaScalerMetrics_triggerIndexFallback(t *testing.T) {
	t.Parallel()

	makeLabel := func(name, value string) *dto.LabelPair {
		n, v := name, value
		return &dto.LabelPair{Name: &n, Value: &v}
	}

	cases := []struct {
		labels []*dto.LabelPair
		want   int32
	}{
		{[]*dto.LabelPair{makeLabel("triggerIndex", "3")}, 3},
		{[]*dto.LabelPair{makeLabel("scalerIndex", "7")}, 7},
		{[]*dto.LabelPair{makeLabel("triggerIndex", "1"), makeLabel("scalerIndex", "2")}, 1}, // triggerIndex preferred
		{[]*dto.LabelPair{}, -1},
		{[]*dto.LabelPair{makeLabel("triggerIndex", "abc")}, -1},
	}

	for _, tc := range cases {
		m := &dto.Metric{Label: tc.labels}
		got := triggerIndex(m)
		if got != tc.want {
			t.Errorf("triggerIndex(%v) = %d, want %d", tc.labels, got, tc.want)
		}
	}
}
