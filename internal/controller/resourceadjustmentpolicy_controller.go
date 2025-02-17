/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	v1 "github.com/devzero-inc/resource-adjustment-operator/api/v1"
	"github.com/devzero-inc/resource-adjustment-operator/internal/controller/metrics"
	"github.com/devzero-inc/resource-adjustment-operator/internal/controller/pid"
	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/api"
	pv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	k8log "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	minMemoryRecommendation = 12582912 // 12 MiB
	adjustedMemoryBuffer    = 3000000  // 3 MiB buffer
	minCPURecommendation    = 0.01     // 10 mCPU
	adjustedCPUBuffer       = 0.005    // 5 mCPU buffer
)

const (
	minFrequency = 30 * time.Second
	maxFrequency = 20 * time.Minute
)

type recommendationType int

const (
	cpuRecommendation recommendationType = iota
	memoryRecommendation
)

type purpose int

const (
	frequencyCalculation purpose = iota
	currentUsage
)

// Global configurations
var (
	// Recommender          recommender
	StatusUpdater        StatusUpdate
	defaultNamespaces    = []string{"default"}
	quit                 chan bool
	defaultCPUConfig     = ResourceConfig{RequestPercentile: 95.0, MarginFraction: 0.15, TargetUtilization: 1.0, BucketSize: 0.001}
	defaultMemoryConfig  = ResourceConfig{RequestPercentile: 95.0, MarginFraction: 0.15, TargetUtilization: 1.0, BucketSize: 1048576}
	namespaces           = defaultNamespaces
	cpuConfig            = defaultCPUConfig
	memoryConfig         = defaultMemoryConfig
	oomBumpRatio         = 1.2
	defaultFrequency     = 3 * time.Minute
	cpuSampleInterval    = 1 * time.Minute
	cpuHistoryLength     = 24 * time.Hour
	memorySampleInterval = 1 * time.Minute
	memoryHistoryLength  = 24 * time.Hour
	lookbackDuration     = 1 * time.Hour
	autoApply            = false
	oomProtection        = true
	// prometheusURL                 = "http://prometheus-service.monitoring.svc.cluster.local:8080"
	prometheusURL                 = "http://10.97.28.239:8080"
	controllerMetrics             = metrics.NewMetrics("resource_adjustment_operator")
	ProportionalGain              = 8.0
	IntegralGain                  = 0.3
	DerivativeGain                = 1.0
	AntiWindUpGain                = 1.0
	IntegralDischargeTimeConstant = 1.0
	LowPassTimeConstant           = 2 * time.Second
	MaxOutput                     = 1.01
	MinOutput                     = -2.15
)

type ContainerState struct {
	Namespace     string
	PodName       string
	ContainerName string
	PIDController *pid.AntiWindupController
	Frequency     time.Duration
	LastUpdated   time.Time
	UpdateChan    chan struct{} // Channel to signal frequency updates
	ContainerQuit chan bool
	Reconciling   bool // Flag to indicate if reconciliation is in progress
}

type Recommender struct {
	sync.Mutex
	ContainerStates map[string]*ContainerState // Key: "namespace/pod/container"
	Metrics         *metrics.Metrics
}

type StatusUpdate struct {
	sync.Mutex
}

func NewRecommender(metrics *metrics.Metrics) *Recommender {
	return &Recommender{
		ContainerStates: make(map[string]*ContainerState),
		Metrics:         metrics,
	}
}

type HistogramBucket struct {
	Start  float64
	End    float64
	Count  int
	Values []float64
	Weight float64
}

// ResourceAdjustmentPolicyReconciler reconciles a ResourceAdjustmentPolicy object
type ResourceAdjustmentPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i, v := range a {
		if v != b[i] {
			return false
		}
	}
	return true
}

// +kubebuilder:rbac:groups=apps.apps.devzero.io,resources=resourceadjustmentpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.apps.devzero.io,resources=resourceadjustmentpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.apps.devzero.io,resources=resourceadjustmentpolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods/resize,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the ResourceAdjustmentPolicy object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.18.4/pkg/reconcile
// Reconcile contains the reconciliation logic for the ResourceAdjustmentPolicy
func (r *ResourceAdjustmentPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := k8log.FromContext(ctx)

	// Fetch the ResourceAdjustmentPolicy instance
	var policy v1.ResourceAdjustmentPolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		log.Error(err, "Failed to fetch ResourceAdjustmentPolicy")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Check if the CRD has the "purpose=status-update" label
	if value, exists := policy.Labels["purpose"]; exists && value == "status-update" {
		log.Info("Skipping reconsile since this is a status update CRD", "policy", policy.Name)
		return ctrl.Result{}, nil // Skip reconciliation
	}

	// Update namespaces
	if len(policy.Spec.TargetSelector.Namespaces) > 0 && policy.Spec.TargetSelector.Namespaces[0] != "" {
		if !equal(policy.Spec.TargetSelector.Namespaces, namespaces) {
			namespaces = policy.Spec.TargetSelector.Namespaces
			log.Info("Updated namespaces", "value", namespaces)
		}
	}

	if policy.Spec.Policies.AutoApply != autoApply {
		autoApply = policy.Spec.Policies.AutoApply
		log.Info("Updated AutoApply", "value", autoApply)
	}

	if policy.Spec.Policies.LookbackDuration != "" {
		if duration, err := time.ParseDuration(policy.Spec.Policies.LookbackDuration); err != nil {
			log.Error(err, "Error parsing LookbackDuration")
		} else if duration != lookbackDuration {
			lookbackDuration = duration
			log.Info("Updated LookbackDuration", "value", lookbackDuration)
		}
	}

	if policy.Spec.Policies.PromURL != "" && policy.Spec.Policies.PromURL != prometheusURL {
		prometheusURL = policy.Spec.Policies.PromURL
		log.Info("Updated Prometheus URL", "value", prometheusURL)
		r.restartRecommender(ctx)
	}

	// Update Frequency
	if policy.Spec.Policies.Frequency != "" {
		if frequency, err := time.ParseDuration(policy.Spec.Policies.Frequency); err != nil {
			log.Error(err, "Error parsing Frequency")
		} else if frequency != defaultFrequency {
			defaultFrequency = frequency
			log.Info("Updated Frequency", "value", defaultFrequency)
			r.restartRecommender(ctx)
		}
	}

	// Update PID Controller configurations
	if policy.Spec.Policies.PidController.PropertionalGain != "" {
		if gain, err := strconv.ParseFloat(policy.Spec.Policies.PidController.PropertionalGain, 64); err != nil {
			log.Error(err, "Error parsing PID Controller PropertionalGain")
		} else if gain != ProportionalGain {
			ProportionalGain = gain
			log.Info("Updated PID Controller PropertionalGain", "value", ProportionalGain)
			r.restartRecommender(ctx)
		}
	}

	if policy.Spec.Policies.PidController.IntegralGain != "" {
		if gain, err := strconv.ParseFloat(policy.Spec.Policies.PidController.IntegralGain, 64); err != nil {
			log.Error(err, "Error parsing PID Controller IntegralGain")
		} else if gain != IntegralGain {
			IntegralGain = gain
			log.Info("Updated PID Controller IntegralGain", "value", IntegralGain)
			r.restartRecommender(ctx)
		}
	}

	if policy.Spec.Policies.PidController.DerivativeGain != "" {
		if gain, err := strconv.ParseFloat(policy.Spec.Policies.PidController.DerivativeGain, 64); err != nil {
			log.Error(err, "Error parsing PID Controller DerivativeGain")
		} else if gain != DerivativeGain {
			DerivativeGain = gain
			log.Info("Updated PID Controller DerivativeGain", "value", DerivativeGain)
			r.restartRecommender(ctx)
		}
	}

	if policy.Spec.Policies.PidController.AntiWindUpGain != "" {
		if gain, err := strconv.ParseFloat(policy.Spec.Policies.PidController.AntiWindUpGain, 64); err != nil {
			log.Error(err, "Error parsing PID Controller AntiWindUpGain")
		} else if gain != AntiWindUpGain {
			AntiWindUpGain = gain
			log.Info("Updated PID Controller AntiWindUpGain", "value", AntiWindUpGain)
			r.restartRecommender(ctx)
		}
	}

	if policy.Spec.Policies.PidController.IntegralDischargeTimeConstant != "" {
		if constant, err := strconv.ParseFloat(policy.Spec.Policies.PidController.IntegralDischargeTimeConstant, 64); err != nil {
			log.Error(err, "Error parsing PID Controller IntegralDischargeTimeConstant")
		} else if constant != IntegralDischargeTimeConstant {
			IntegralDischargeTimeConstant = constant
			log.Info("Updated PID Controller IntegralDischargeTimeConstant", "value", IntegralDischargeTimeConstant)
			r.restartRecommender(ctx)
		}
	}

	if policy.Spec.Policies.PidController.LowPassTimeConstant != "" {
		if duration, err := time.ParseDuration(policy.Spec.Policies.PidController.LowPassTimeConstant); err != nil {
			log.Error(err, "Error parsing PID Controller LowPassTimeConstant")
		} else if duration != LowPassTimeConstant {
			LowPassTimeConstant = duration
			log.Info("Updated PID Controller LowPassTimeConstant", "value", LowPassTimeConstant)
			r.restartRecommender(ctx)
		}
	}

	if policy.Spec.Policies.PidController.MaxOutput != "" {
		if output, err := strconv.ParseFloat(policy.Spec.Policies.PidController.MaxOutput, 64); err != nil {
			log.Error(err, "Error parsing PID Controller MaxOutput")
		} else if output != MaxOutput {
			MaxOutput = output
			log.Info("Updated PID Controller MaxOutput", "value", MaxOutput)
			r.restartRecommender(ctx)
		}
	}

	if policy.Spec.Policies.PidController.MinOutput != "" {
		if output, err := strconv.ParseFloat(policy.Spec.Policies.PidController.MinOutput, 64); err != nil {
			log.Error(err, "Error parsing PID Controller MinOutput")
		} else if output != MinOutput {
			MinOutput = output
			log.Info("Updated PID Controller MinOutput", "value", MinOutput)
			r.restartRecommender(ctx)
		}
	}

	// Update CPU Recommendation configurations
	if policy.Spec.Policies.CPURecommendation.SampleInterval != "" {
		if duration, err := time.ParseDuration(policy.Spec.Policies.CPURecommendation.SampleInterval); err != nil {
			log.Error(err, "Error parsing CPURecommendation.SampleInterval")
		} else if duration != cpuSampleInterval {
			cpuSampleInterval = duration
			log.Info("Updated CPURecommendation.SampleInterval", "value", cpuSampleInterval)
		}
	}

	if policy.Spec.Policies.CPURecommendation.HistoryLength != "" {
		if historyLength, err := time.ParseDuration(policy.Spec.Policies.CPURecommendation.HistoryLength); err != nil {
			log.Error(err, "Error parsing CPURecommendation.HistoryLength")
		} else if historyLength != cpuHistoryLength {
			cpuHistoryLength = historyLength
			log.Info("Updated CPURecommendation.HistoryLength", "value", cpuHistoryLength)
		}
	}

	if policy.Spec.Policies.CPURecommendation.RequestPercentile != "" {
		if requestPercentile, err := strconv.ParseFloat(policy.Spec.Policies.CPURecommendation.RequestPercentile, 64); err != nil {
			log.Error(err, "Error parsing CPURecommendation.RequestPercentile")
		} else if requestPercentile != cpuConfig.RequestPercentile {
			cpuConfig.RequestPercentile = requestPercentile
			log.Info("Updated CPURecommendation.RequestPercentile", "value", fmt.Sprintf("%.2f", cpuConfig.RequestPercentile))
		}
	}

	if policy.Spec.Policies.CPURecommendation.MarginFraction != "" {
		if marginFraction, err := strconv.ParseFloat(policy.Spec.Policies.CPURecommendation.MarginFraction, 64); err != nil {
			log.Error(err, "Error parsing CPURecommendation.MarginFraction")
		} else if marginFraction != cpuConfig.MarginFraction {
			cpuConfig.MarginFraction = marginFraction
			log.Info("Updated CPURecommendation.MarginFraction", "value", fmt.Sprintf("%.2f", cpuConfig.MarginFraction))
		}
	}

	if policy.Spec.Policies.CPURecommendation.TargetUtilization != "" {
		if targetUtilization, err := strconv.ParseFloat(policy.Spec.Policies.CPURecommendation.TargetUtilization, 64); err != nil {
			log.Error(err, "Error parsing CPURecommendation.TargetUtilization")
		} else if targetUtilization != cpuConfig.TargetUtilization {
			cpuConfig.TargetUtilization = targetUtilization
			log.Info("Updated CPURecommendation.TargetUtilization", "value", fmt.Sprintf("%.2f", cpuConfig.TargetUtilization))
		}
	}

	// Update Memory Recommendation configurations
	if policy.Spec.Policies.MemoryRecommendation.SampleInterval != "" {
		if duration, err := time.ParseDuration(policy.Spec.Policies.MemoryRecommendation.SampleInterval); err != nil {
			log.Error(err, "Error parsing MemoryRecommendation.SampleInterval")
		} else if duration != memorySampleInterval {
			memorySampleInterval = duration
			log.Info("Updated MemoryRecommendation.SampleInterval", "value", memorySampleInterval)
		}
	}

	if policy.Spec.Policies.MemoryRecommendation.HistoryLength != "" {
		if historyLength, err := time.ParseDuration(policy.Spec.Policies.MemoryRecommendation.HistoryLength); err != nil {
			log.Error(err, "Error parsing MemoryRecommendation.HistoryLength")
		} else if historyLength != memoryHistoryLength {
			memoryHistoryLength = historyLength
			log.Info("Updated MemoryRecommendation.HistoryLength", "value", memoryHistoryLength)
		}
	}

	if policy.Spec.Policies.MemoryRecommendation.RequestPercentile != "" {
		if requestPercentile, err := strconv.ParseFloat(policy.Spec.Policies.MemoryRecommendation.RequestPercentile, 64); err != nil {
			log.Error(err, "Error parsing MemoryRecommendation.RequestPercentile")
		} else if requestPercentile != memoryConfig.RequestPercentile {
			memoryConfig.RequestPercentile = requestPercentile
			log.Info("Updated MemoryRecommendation.RequestPercentile", "value", fmt.Sprintf("%.2f", memoryConfig.RequestPercentile))
		}
	}

	if policy.Spec.Policies.MemoryRecommendation.MarginFraction != "" {
		if marginFraction, err := strconv.ParseFloat(policy.Spec.Policies.MemoryRecommendation.MarginFraction, 64); err != nil {
			log.Error(err, "Error parsing MemoryRecommendation.MarginFraction")
		} else if marginFraction != memoryConfig.MarginFraction {
			memoryConfig.MarginFraction = marginFraction
			log.Info("Updated MemoryRecommendation.MarginFraction", "value", fmt.Sprintf("%.2f", memoryConfig.MarginFraction))
		}
	}

	if policy.Spec.Policies.MemoryRecommendation.TargetUtilization != "" {
		if targetUtilization, err := strconv.ParseFloat(policy.Spec.Policies.MemoryRecommendation.TargetUtilization, 64); err != nil {
			log.Error(err, "Error parsing MemoryRecommendation.TargetUtilization")
		} else if targetUtilization != memoryConfig.TargetUtilization {
			memoryConfig.TargetUtilization = targetUtilization
			log.Info("Updated MemoryRecommendation.TargetUtilization", "value", fmt.Sprintf("%.2f", memoryConfig.TargetUtilization))
		}
	}

	if oomProtection != policy.Spec.Policies.MemoryRecommendation.OOMProtection {
		oomProtection = policy.Spec.Policies.MemoryRecommendation.OOMProtection
		log.Info("Updated OOMProtection", "value", oomProtection)
	}

	if oomBumpRatioStr := policy.Spec.Policies.MemoryRecommendation.OOMBumpRatio; oomBumpRatioStr != "" {
		if ratio, err := strconv.ParseFloat(oomBumpRatioStr, 64); err != nil {
			log.Error(err, "Error parsing MemoryRecommendation.OOMBumpRatio")
		} else if ratio != oomBumpRatio {
			oomBumpRatio = ratio
			log.Info("Updated MemoryRecommendation.OOMBumpRatio", "value", fmt.Sprintf("%.2f", oomBumpRatio))
		}
	}

	return ctrl.Result{}, nil
}

type PrometheusClient struct {
	api pv1.API
}

func NewPrometheusClient(url string) (*PrometheusClient, error) {
	client, err := api.NewClient(api.Config{Address: url})
	if err != nil {
		return nil, fmt.Errorf("failed to create Prometheus client: %v", err)
	}
	return &PrometheusClient{api: pv1.NewAPI(client)}, nil
}

func (pc *PrometheusClient) GetMetricsRange(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]float64, error) {
	result, _, err := pc.api.QueryRange(ctx, query, pv1.Range{Start: start, End: end, Step: step})
	if err != nil {
		return nil, err
	}

	matrix, ok := result.(model.Matrix)
	if !ok {
		return nil, fmt.Errorf("unexpected result type")
	}

	var values []float64
	for _, stream := range matrix {
		for _, sample := range stream.Values {
			values = append(values, float64(sample.Value))
		}
	}
	return values, nil
}

func (pc *PrometheusClient) QueryAtTime(query string, t time.Time) (float64, error) {
	result, _, err := pc.api.Query(context.Background(), query, t)
	if err != nil {
		return 0, err
	}

	vector, ok := result.(model.Vector)
	if !ok || len(vector) == 0 {
		return 0, fmt.Errorf("no metric value found at specified time")
	}
	return float64(vector[0].Value), nil
}

type Bucket struct {
	MinBoundary float64
	Weight      float64
	Count       int
	Values      []float64
}

type ResourceConfig struct {
	RequestPercentile float64
	MarginFraction    float64
	TargetUtilization float64
	BucketSize        float64
}

type ResourceRecommender struct {
	config      ResourceConfig
	Buckets     []Bucket
	TotalWeight float64
}

func NewResourceRecommender(config ResourceConfig) *ResourceRecommender {
	return &ResourceRecommender{
		config:  config,
		Buckets: make([]Bucket, 0),
	}
}

func (r *ResourceRecommender) addValue(value float64) {
	bucketIndex := int(value / r.config.BucketSize)

	// Ensure we have enough buckets
	for len(r.Buckets) <= bucketIndex {
		r.Buckets = append(r.Buckets, Bucket{
			MinBoundary: float64(len(r.Buckets)) * r.config.BucketSize,
			Weight:      0,
			Count:       0,
			Values:      make([]float64, 0),
		})
	}

	// Update bucket
	r.Buckets[bucketIndex].Weight += value
	r.Buckets[bucketIndex].Count++
	r.Buckets[bucketIndex].Values = append(r.Buckets[bucketIndex].Values, value)
	r.TotalWeight += value
}

func (r *ResourceRecommender) ProcessValues(values []float64) {
	// Reset state
	r.Buckets = make([]Bucket, 0)
	r.TotalWeight = 0

	// Process each value
	for _, value := range values {
		r.addValue(value)
	}
}

func (r *ResourceRecommender) getPercentile(percentile float64) float64 {
	if len(r.Buckets) == 0 {
		return 0
	}

	targetWeight := (percentile / 100.0) * r.TotalWeight
	accumulatedWeight := 0.0

	for i, bucket := range r.Buckets {
		accumulatedWeight += bucket.Weight
		if accumulatedWeight >= targetWeight {
			// Return the minimum boundary of the next bucket if available
			if i+1 < len(r.Buckets) {
				return r.Buckets[i+1].MinBoundary
			}
			return bucket.MinBoundary
		}
	}

	// If we haven't reached the target weight, return the last bucket's boundary
	if len(r.Buckets) > 0 {
		return r.Buckets[len(r.Buckets)-1].MinBoundary
	}
	return 0
}

func (r *ResourceRecommender) GetRecommendation(valueType recommendationType, values []float64, oomMemory float64) (float64, float64) {
	if len(values) == 0 {
		return 0, 0
	}

	r.ProcessValues(values)

	baseRecommendation := r.getPercentile(r.config.RequestPercentile)

	if oomMemory > 0 && valueType == memoryRecommendation {
		baseRecommendation = math.Max(
			math.Max(oomMemory+100*1024*1024, oomMemory*oomBumpRatio),
			baseRecommendation,
		)
	}

	var adjustedRecommendation float64
	marginAdjusted := baseRecommendation * (1 + r.config.MarginFraction)
	adjustedRecommendation = marginAdjusted / r.config.TargetUtilization

	// Round to one decimal place
	adjustedRecommendation = math.Round(adjustedRecommendation*10000) / 10000

	return adjustedRecommendation, baseRecommendation
}

func (re *Recommender) checkResourceAdjustment(container corev1.Container, recommendedCPU, recommendedMemory float64) (bool, bool) {
	log := k8log.FromContext(context.Background())

	log.Info("Starting resource adjustment check",
		"container", container.Name,
		"recommendedCPU", recommendedCPU,
		"recommendedMemory", recommendedMemory)

	cpuRequest := float64(container.Resources.Requests.Cpu().MilliValue()) / 1000
	log.Info("Current CPU request",
		"container", container.Name,
		"cpuRequest", cpuRequest)

	memoryRequest := float64(container.Resources.Requests.Memory().Value()) / 1048576
	log.Info("Current Memory request",
		"container", container.Name,
		"memoryRequestMi", memoryRequest)

	cpuNeedsAdjustment := false
	memoryNeedsAdjustment := false

	if cpuRequest > 0 {
		cpuDiff := math.Abs((recommendedCPU-cpuRequest)/cpuRequest) * 100
		log.Info("CPU difference calculated",
			"container", container.Name,
			"cpuDiff", cpuDiff,
			"currentRequest", cpuRequest,
			"recommendation", recommendedCPU)

		if cpuDiff >= 5 {
			cpuNeedsAdjustment = true
			log.Info("CPU adjustment needed: recommendation is ≥5% greater than request",
				"container", container.Name,
				"cpuDiff", cpuDiff)
		}

		if recommendedCPU <= (cpuRequest * 0.1) {
			cpuNeedsAdjustment = true
			log.Info("CPU adjustment needed: recommendation is ≤10% of request",
				"container", container.Name,
				"recommendedCPU", recommendedCPU,
				"tenPercentOfRequest", cpuRequest*0.1)
		}
	}

	if memoryRequest > 0 {
		memoryDiff := math.Abs((recommendedMemory-memoryRequest)/memoryRequest) * 100
		log.Info("Memory difference calculated",
			"container", container.Name,
			"memoryDiff", memoryDiff,
			"currentRequest", memoryRequest,
			"recommendation", recommendedMemory)

		if memoryDiff >= 5 {
			memoryNeedsAdjustment = true
			log.Info("Memory adjustment needed: recommendation is ≥5% greater than request",
				"container", container.Name,
				"memoryDiff", memoryDiff)
		}

		if recommendedMemory <= (memoryRequest * 0.1) {
			memoryNeedsAdjustment = true
			log.Info("Memory adjustment needed: recommendation is ≤10% of request",
				"container", container.Name,
				"recommendedMemory", recommendedMemory,
				"tenPercentOfRequest", memoryRequest*0.1)
		}
	}

	log.Info("Resource adjustment check completed",
		"container", container.Name,
		"cpuNeedsAdjustment", cpuNeedsAdjustment,
		"memoryNeedsAdjustment", memoryNeedsAdjustment)

	return cpuNeedsAdjustment, memoryNeedsAdjustment
}

func (re *Recommender) checkForOOMEvents(pod corev1.Pod, containerName string) float64 {
	ctx := context.TODO()
	log := k8log.FromContext(ctx)
	for _, containerStatus := range pod.Status.ContainerStatuses {
		if containerStatus.Name == containerName && containerStatus.LastTerminationState.Terminated != nil {
			terminated := containerStatus.LastTerminationState.Terminated
			if terminated.Reason == "OOMKilled" && time.Since(terminated.FinishedAt.Time) <= 7*time.Hour {
				oomMemory := re.getMemoryUsageAtTermination(pod.Namespace, containerName, terminated.FinishedAt.Time)
				log.Info("Found OOM event", "podName", pod.Name, "containerName", containerName, "oomMemory", oomMemory)
				return oomMemory
			}
		}
	}
	return 0
}

func (re *Recommender) getMemoryUsageAtTermination(namespace, containerName string, terminationTime time.Time) float64 {
	ctx := context.TODO()
	log := k8log.FromContext(ctx)
	query := fmt.Sprintf(`container_memory_working_set_bytes{namespace="%s", container="%s"}`, namespace, containerName)
	promClient, err := NewPrometheusClient(prometheusURL)
	if err != nil {
		log.Error(err, "Failed to create Prometheus client")
	}
	memoryUsage, err := promClient.QueryAtTime(query, terminationTime)
	if err != nil {
		log.Error(err, "Error fetching memory usage at termination", "namespace", namespace, "containerName", containerName, "terminationTime", terminationTime)
		return 0
	}
	return memoryUsage
}

func (re *Recommender) getAllocatedResources(container *corev1.Container) (float64, float64) {
	cpuRequest := container.Resources.Requests.Cpu()
	if cpuRequest.IsZero() {
		cpuRequest = container.Resources.Limits.Cpu()
	}
	allocatedCPU := float64(cpuRequest.MilliValue()) / 1000.0

	memoryRequest := container.Resources.Requests.Memory()
	if memoryRequest.IsZero() {
		memoryRequest = container.Resources.Limits.Memory()
	}
	allocatedMemory := float64(memoryRequest.Value())

	return allocatedCPU, allocatedMemory
}

// TODO: Find the perfect values for the PID controller
func NewContainerState(namespace, podName, containerName string) *ContainerState {
	return &ContainerState{
		Namespace:     namespace,
		PodName:       podName,
		ContainerName: containerName,
		PIDController: &pid.AntiWindupController{
			Config: pid.AntiWindupControllerConfig{
				ProportionalGain:              ProportionalGain, // Need to tune these values correctly
				IntegralGain:                  IntegralGain,     // Look at the bayes theroem
				DerivativeGain:                DerivativeGain,
				AntiWindUpGain:                AntiWindUpGain,
				IntegralDischargeTimeConstant: IntegralDischargeTimeConstant,
				LowPassTimeConstant:           LowPassTimeConstant,
				MaxOutput:                     MaxOutput,
				MinOutput:                     MinOutput,
			},
		},
		Frequency:     defaultFrequency,
		UpdateChan:    make(chan struct{}, 1),
		ContainerQuit: make(chan bool),
		Reconciling:   false,
	}
}

func (re *Recommender) calculateError(log logr.Logger, pod *corev1.Pod, container *corev1.Container) (float64, float64, float64, float64, error) {

	currentCPU, currentMemory, _, err := re.fetchCurrentMetrics(context.Background(), log, pod, container, frequencyCalculation)
	if err != nil {
		return 0, 0, 0, 0, err
	}

	allocatedCPU, allocatedMemory := re.getAllocatedResources(container)

	cpuUtilization := currentCPU / allocatedCPU
	memoryUtilization := currentMemory / allocatedMemory
	log.Info("calculate error", "cuurentCPU", currentCPU, "allocatedCPU", allocatedCPU, "cpuUtilization", cpuUtilization)
	log.Info("calculate error", "cuurentMemory", currentMemory, "allocatedMemory", allocatedMemory, "memoryUtilization", memoryUtilization)

	// Define target utilizations (e.g., 85% for CPU, 85% for memory)
	maxTargetCPUUtilization := 0.85
	minTargetCPUUtilization := 0.80
	maxTargetMemoryUtilization := 0.85
	minTargetMemoryUtilization := 0.80

	var cpuError, memoryError float64
	if minTargetCPUUtilization <= cpuUtilization && cpuUtilization <= maxTargetCPUUtilization {
		cpuError = 0
	} else {
		if cpuUtilization < minTargetCPUUtilization {
			cpuError = minTargetCPUUtilization - cpuUtilization
		} else {
			cpuError = maxTargetCPUUtilization - cpuUtilization
		}
	}

	if minTargetMemoryUtilization <= memoryUtilization && memoryUtilization <= maxTargetMemoryUtilization {
		memoryError = 0
	} else {
		if memoryUtilization < minTargetMemoryUtilization {
			memoryError = minTargetMemoryUtilization - memoryUtilization
		} else {
			memoryError = maxTargetMemoryUtilization - memoryUtilization
		}
	}

	return cpuError, memoryError, cpuUtilization, memoryUtilization, nil
}

func (re *Recommender) monitorFrequency(r *ResourceAdjustmentPolicyReconciler, state *ContainerState) {
	log := k8log.FromContext(context.Background())
	threshold := 0.2 // Threshold for immediate reconciliation (e.g., 20% change)
	prvTime := time.Now()

	for {
		select {
		case <-quit:
			return
		case <-state.ContainerQuit:
			return
		case <-time.After(5 * time.Second):
			pod := &corev1.Pod{}
			if err := r.Client.Get(context.Background(), types.NamespacedName{Namespace: state.Namespace, Name: state.PodName}, pod); err != nil {
				log.Error(err, "Failed to fetch pod", "pod", state.PodName)
				continue
			}

			var container *corev1.Container
			for _, c := range pod.Spec.Containers {
				if c.Name == state.ContainerName {
					container = &c
					break
				}
			}
			if container == nil {
				log.Info("Container not found", "pod", state.PodName, "container", state.ContainerName)
				continue
			}

			// Calculate CPU and memory errors and utilizations
			cpuError, memoryError, cpuUtilization, memoryUtilization, err := re.calculateError(log, pod, container)
			if err != nil {
				continue
			}
			log.Info("Calculated errors and utilizations", "pod", state.PodName, "container", state.ContainerName, "cpuError", cpuError, "memoryError", memoryError, "cpuUtilization", cpuUtilization, "memoryUtilization", memoryUtilization)

			// Use the larger error (CPU or memory) to determine the frequency
			var error float64
			if (cpuError < 0 && memoryError < 0) || (cpuError > 0 && memoryError > 0) {
				if math.Abs(cpuError) > math.Abs(memoryError) {
					error = cpuError
				} else {
					error = memoryError
				}
			} else {
				if cpuError < 0 {
					if math.Abs(cpuError)*2 > memoryError {
						error = cpuError
					} else {
						error = memoryError
					}
				} else {
					if math.Abs(memoryError)*2 > cpuError {
						error = memoryError
					} else {
						error = cpuError
					}
				}
			}
			log.Info("Final error calculated", "pod", state.PodName, "container", state.ContainerName, "error", error)

			state.PIDController.Update(pid.AntiWindupControllerInput{
				ReferenceSignal:   0,
				ActualSignal:      error,
				FeedForwardSignal: 0.0,
				SamplingInterval:  time.Since(prvTime),
			})
			prvTime = time.Now()

			normalizedControlSignal := normalizeControlSignal(state.PIDController.State.ControlSignal)

			newFrequency := calculateFrequency(normalizedControlSignal)

			log.Info("Control signal calculated", "pod", state.PodName, "container", state.ContainerName, "controlSignal", state.PIDController.State.ControlSignal, "newFrequency", newFrequency)

			frequencyChange := math.Abs(float64(newFrequency-state.Frequency)) / float64(state.Frequency)
			if frequencyChange > threshold {
				state.Frequency = newFrequency
				state.UpdateChan <- struct{}{} // Signal the main loop to reconcile immediately
			} else {
				state.Frequency = newFrequency
			}
		}
	}
}

func normalizeControlSignal(controlSignal float64) float64 {
	// Define maximum expected control signal magnitudes for each direction
	maxControlSignalPositive := 1.01 // Observed max for negative errors (e.g., error=-0.10)
	maxControlSignalNegative := 2.15 // Observed max for positive errors (e.g., error=+0.20)

	var normalized float64
	if controlSignal >= 0 {
		normalized = controlSignal / maxControlSignalPositive
	} else {
		normalized = controlSignal / maxControlSignalNegative
	}

	if normalized < -1.0 {
		normalized = -1.0
	} else if normalized > 1.0 {
		normalized = 1.0
	}

	return normalized
}

func calculateFrequency(normalizedControlSignal float64) time.Duration {
	scaledSignal := math.Abs(normalizedControlSignal)

	frequencyRange := float64(maxFrequency - minFrequency)

	newFrequency := maxFrequency - time.Duration(frequencyRange*scaledSignal)

	if newFrequency < minFrequency {
		newFrequency = minFrequency
	} else if newFrequency > maxFrequency {
		newFrequency = maxFrequency
	}

	return newFrequency
}

func (re *Recommender) runContainerLoop(ctx context.Context, r *ResourceAdjustmentPolicyReconciler, state *ContainerState, policy *v1.ResourceAdjustmentPolicy, pod *corev1.Pod, container *corev1.Container) {
	log := k8log.FromContext(ctx)

	for {
		select {
		case <-quit:
			return
		case <-state.ContainerQuit:
			return
		case <-state.UpdateChan:
			// Immediate reconciliation triggered
			log.Info("Immediate reconciliation triggered for container",
				"pod", state.PodName,
				"container", state.ContainerName,
				"newFrequency", state.Frequency)
			re.processContainer(ctx, r, log, policy, pod, container)
		case <-time.After(state.Frequency):
			// Regular reconciliation based on current frequency
			log.Info("Regular reconciliation for container",
				"pod", state.PodName,
				"container", state.ContainerName,
				"frequency", state.Frequency)
			re.processContainer(ctx, r, log, policy, pod, container)
		}
	}
}

func (r *ResourceAdjustmentPolicyReconciler) restartRecommender(ctx context.Context) {
	quit <- true
	log := k8log.FromContext(ctx)
	log.Info("Restarting the resource recommender")
	quit = make(chan bool)
	recommender := NewRecommender(controllerMetrics)
	go recommender.runRecommender(r)
}

func (re *Recommender) runRecommender(r *ResourceAdjustmentPolicyReconciler) {
	log := k8log.FromContext(context.Background())
	ticker := time.NewTicker(1 * time.Minute) // Global ticker to periodically check for new containers
	defer ticker.Stop()
	ctx := context.TODO()

	for {
		select {
		case <-quit:
			log.Info("Stopping the resource recommender")
			return
		case <-ticker.C:
			log.Info("Fetching ResourceAdjustmentPolicies")

			// Fetch all namespaces dynamically
			namespaceList := &corev1.NamespaceList{}
			if err := r.Client.List(ctx, namespaceList); err != nil {
				log.Error(err, "Failed to fetch namespaces")
				continue
			}

			// Convert fetched namespaces into a set (map for fast lookups)
			clusterNamespaces := make(map[string]struct{})
			for _, ns := range namespaceList.Items {
				clusterNamespaces[ns.Name] = struct{}{}
			}

			// Fetch the ResourceAdjustmentPolicy list
			policyList := &v1.ResourceAdjustmentPolicyList{}
			if err := r.List(ctx, policyList); err != nil {
				log.Error(err, "Failed to list ResourceAdjustmentPolicies")
				return
			}

			log.Info("Found ResourceAdjustmentPolicies", "count", len(policyList.Items))

			activeNamespaces := make(map[string]struct{})

			for namespace := range clusterNamespaces {
				if !isNamespaceAllowed(namespace) {
					re.cleanupNamespace(namespace, log)
					continue
				}

				activeNamespaces[namespace] = struct{}{} // Mark as active

				// Fetch the ResourceAdjustmentPolicy for this namespace
				var namespacePolicy v1.ResourceAdjustmentPolicy
				policyName := fmt.Sprintf("devzero-balance-recommender-%s", namespace)
				policyKey := types.NamespacedName{Name: policyName, Namespace: namespace}

				if err := r.Get(ctx, policyKey, &namespacePolicy); err != nil {
					if errors.IsNotFound(err) {
						// Create a new policy if it doesn’t exist
						namespacePolicy = v1.ResourceAdjustmentPolicy{
							ObjectMeta: metav1.ObjectMeta{
								Name:      policyName,
								Namespace: namespace,
								Labels: map[string]string{
									"purpose": "status-update",
								},
							},
							Spec:   policyList.Items[0].Spec, // Assuming a global policy
							Status: policyList.Items[0].Status,
						}
						if err := r.Create(ctx, &namespacePolicy); err != nil {
							log.Error(err, "Failed to create ResourceAdjustmentPolicy")
						}
						log.Info("Created new ResourceAdjustmentPolicy", "namespace", namespace)
					} else {
						log.Error(err, "Failed to fetch ResourceAdjustmentPolicy")
					}
				}

				// Fetch pods in this namespace
				pods := &corev1.PodList{}
				if err := r.Client.List(ctx, pods, client.InNamespace(namespace)); err != nil {
					log.Error(err, "Failed to fetch pods", "namespace", namespace)
					continue
				}

				// Process pods
				for _, pod := range pods.Items {
					if pod.Status.Phase != corev1.PodRunning {
						continue
					}

					for _, container := range pod.Spec.Containers {
						key := fmt.Sprintf("%s/%s/%s", namespace, pod.Name, container.Name)

						if r.isPodExcluded(&namespacePolicy, pod.Namespace, pod.Name) {
							log.Info("Pod excluded", "podName", pod.Name, "namespace", pod.Namespace)
						} else {
							log.Info("Pod not excluded", "podName", pod.Name, "exclusion", namespacePolicy.Spec.Exclusions.ExcludedPods)
						}

						re.Lock()
						_, exists := re.ContainerStates[key]
						if !exists && !r.isPodExcluded(&namespacePolicy, pod.Namespace, pod.Name) {
							// Initialize state for new container
							state := NewContainerState(namespace, pod.Name, container.Name)
							re.ContainerStates[key] = state

							// Start the frequency monitor goroutine
							go re.monitorFrequency(r, state)

							// Start the container control loop
							go re.runContainerLoop(ctx, r, state, &namespacePolicy, &pod, &container)
						} else if exists && r.isPodExcluded(&namespacePolicy, pod.Namespace, pod.Name) {
							// Stop monitoring if the pod is now excluded
							log.Info("Stopping monitoring for newly excluded pod", "podName", pod.Name, "namespace", namespace)

							close(re.ContainerStates[key].ContainerQuit)
							close(re.ContainerStates[key].UpdateChan)

							delete(re.ContainerStates, key)
						}
						re.Unlock()
					}
				}
			}

			// Stop monitoring namespaces that are no longer active
			for namespace := range re.ContainerStates {
				ns := getNamespaceFromKey(namespace)
				if _, exists := activeNamespaces[ns]; !exists {
					re.cleanupNamespace(ns, log)
				}
			}
			re.cleanupDeletedContainers(r, log)
		}
	}
}

func (re *Recommender) cleanupDeletedContainers(r *ResourceAdjustmentPolicyReconciler, log logr.Logger) {
	re.Lock()
	defer re.Unlock()

	ctx := context.TODO()

	pods := &corev1.PodList{}
	if err := r.Client.List(ctx, pods); err != nil {
		log.Error(err, "Failed to fetch all pods for cleanup")
		return
	}

	// Create a set of all active containers in the cluster
	activeContainers := make(map[string]struct{})
	for _, pod := range pods.Items {
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}
		for _, container := range pod.Spec.Containers {
			key := fmt.Sprintf("%s/%s/%s", pod.Namespace, pod.Name, container.Name)
			activeContainers[key] = struct{}{}
		}
	}

	// Stop monitoring for containers that no longer exist
	for key, state := range re.ContainerStates {
		if _, exists := activeContainers[key]; !exists {
			log.Info("Stopping monitoring for deleted container", "containerKey", key)

			close(state.ContainerQuit)
			close(state.UpdateChan)

			delete(re.ContainerStates, key)
		}
	}
}

func isNamespaceAllowed(namespace string) bool {
	for _, ns := range namespaces {
		if ns == namespace {
			return true
		}
	}
	return false
}

func (re *Recommender) cleanupNamespace(namespace string, log logr.Logger) {
	re.Lock()
	defer re.Unlock()

	log.Info("Stopping monitoring for namespace", "namespace", namespace)

	for key, state := range re.ContainerStates {
		ns := getNamespaceFromKey(key)
		if ns == namespace {
			close(state.ContainerQuit)
			close(state.UpdateChan)
			delete(re.ContainerStates, key)
		}
	}
}

func getNamespaceFromKey(key string) string {
	parts := strings.Split(key, "/")
	if len(parts) > 0 {
		return parts[0]
	}
	return ""
}

func (r *ResourceAdjustmentPolicyReconciler) isPodExcluded(policy *v1.ResourceAdjustmentPolicy, namespace, podName string) bool {
	for _, excludedPod := range policy.Spec.Exclusions.ExcludedPods {
		if excludedPod.Namespace == namespace && excludedPod.PodName == podName {
			return true
		}
	}
	return false
}

func (re *Recommender) processContainer(ctx context.Context, r *ResourceAdjustmentPolicyReconciler, log logr.Logger, policy *v1.ResourceAdjustmentPolicy, pod *corev1.Pod, container *corev1.Container) {
	log.Info("Processing container", "podName", pod.Name, "containerName", container.Name)

	currentCPUValue, currentMemoryValue, oomMemory, err := re.fetchCurrentMetrics(ctx, log, pod, container, currentUsage)
	if err != nil {
		log.Error(err, "Failed to get current metrics", "container", container.Name, "defaultCPUValue", currentCPUValue, "defaultMemoryValue", currentMemoryValue)
		return
	}

	recommendedCPU, baseCPU, recommendedMemory, baseMemory := re.calculateRecommendations(ctx, log, pod, container, oomMemory)

	re.adjustRecommendations(&recommendedCPU, &baseCPU, &recommendedMemory, &baseMemory)

	// Expose recommendations to Prometheus
	re.Metrics.CpuRecommendation.With(prometheus.Labels{
		"namespace": pod.Namespace,
		"pod":       pod.Name,
		"container": container.Name,
	}).Set(recommendedCPU)

	re.Metrics.MemoryRecommendation.With(prometheus.Labels{
		"namespace": pod.Namespace,
		"pod":       pod.Name,
		"container": container.Name,
	}).Set(recommendedMemory)

	// Expose PID controller state
	state := re.ContainerStates[fmt.Sprintf("%s/%s/%s", pod.Namespace, pod.Name, container.Name)]
	if state != nil {
		re.Metrics.ControlSignal.With(prometheus.Labels{
			"namespace": pod.Namespace,
			"pod":       pod.Name,
			"container": container.Name,
		}).Set(state.PIDController.State.ControlSignal)

		re.Metrics.IntegralTerm.With(prometheus.Labels{
			"namespace": pod.Namespace,
			"pod":       pod.Name,
			"container": container.Name,
		}).Set(state.PIDController.State.ControlErrorIntegral)

		re.Metrics.DerivativeTerm.With(prometheus.Labels{
			"namespace": pod.Namespace,
			"pod":       pod.Name,
			"container": container.Name,
		}).Set(state.PIDController.State.ControlErrorDerivative)

		// Expose reconciliation frequency
		re.Metrics.ReconciliationFrequency.With(prometheus.Labels{
			"namespace": pod.Namespace,
			"pod":       pod.Name,
			"container": container.Name,
		}).Set(state.Frequency.Seconds())
	}

	status := re.createContainerStatus(ctx, r, log, pod, container, currentCPUValue, currentMemoryValue, baseCPU, recommendedCPU, baseMemory, recommendedMemory)
	if autoApply && (status.CPURecommendation.NeedToApply || status.MemoryRecommendation.NeedToApply) {
		if err := re.applyRecommendations(ctx, r, pod, status); err != nil {
			log.Error(err, "Failed to apply recommendations", "podName", pod.Name, "containerName", container.Name)
		}
	}

	StatusUpdater.updateContainerStatus(ctx, log, r, policy, status)
}

func (s *StatusUpdate) updateContainerStatus(ctx context.Context, log logr.Logger, r *ResourceAdjustmentPolicyReconciler, policy *v1.ResourceAdjustmentPolicy, status v1.ContainerStatus) {
	s.Lock()
	defer s.Unlock()

	err := retry.OnError(
		retry.DefaultRetry,
		func(err error) bool {
			return errors.IsConflict(err)
		},
		func() error {
			if err := r.Get(ctx, types.NamespacedName{Namespace: policy.Namespace, Name: policy.Name}, policy); err != nil {
				return err
			}

			patch := client.MergeFrom(policy.DeepCopy())
			policy.Status.Containers = append(policy.Status.Containers, status)

			// Trim the list to keep only the last 500 records
			if len(policy.Status.Containers) > 500 {
				policy.Status.Containers = policy.Status.Containers[len(policy.Status.Containers)-500:]
			}

			return r.Status().Patch(ctx, policy, patch)
		},
	)

	if err != nil {
		log.Error(err, "Failed to update status for policy", "policyName", policy.Name)
	} else {
		log.Info("Successfully updated status for policy", "policyName", policy.Name)
	}
}

func (re *Recommender) fetchCurrentMetrics(ctx context.Context, log logr.Logger, pod *corev1.Pod, container *corev1.Container, purpose purpose) (float64, float64, float64, error) {
	promClient, err := NewPrometheusClient(prometheusURL)
	if err != nil {
		log.Error(err, "Failed to create Prometheus client")
		return 0, 0, 0, err
	}

	var currentCPUQuery string
	if purpose == currentUsage {
		currentCPUQuery = fmt.Sprintf(`max(
			rate(container_cpu_usage_seconds_total{namespace="%s",container="%s"}[1m])
		) by (container)`, pod.Namespace, container.Name)
	} else if purpose == frequencyCalculation {
		// TODO: find a way to calculate the time frame here (e.g., 10m)
		currentCPUQuery = fmt.Sprintf(`max(
        rate(container_cpu_usage_seconds_total{namespace="%s",container="%s"}[10m])
    ) by (container)`, pod.Namespace, container.Name)
	}

	currentCPUValue, err := promClient.QueryAtTime(currentCPUQuery, time.Now())
	if err != nil {
		log.Error(err, "Failed to fetch current CPU metrics", "podName", pod.Name, "containerName", container.Name)
		return 0, 0, 0, err
	}

	currentMemoryQuery := fmt.Sprintf(`max(
        container_memory_working_set_bytes{namespace="%s", container="%s"}
    ) by (container)`, pod.Namespace, container.Name)
	currentMemoryValue, err := promClient.QueryAtTime(currentMemoryQuery, time.Now())
	if err != nil {
		log.Error(err, "Failed to fetch current memory metrics", "podName", pod.Name, "containerName", container.Name)
		return 0, 0, 0, err
	}

	oomMemory := 0.0
	if oomProtection {
		oomMemory = re.checkForOOMEvents(*pod, container.Name)
	}

	return currentCPUValue, currentMemoryValue, oomMemory, nil
}

func (re *Recommender) calculateRecommendations(
	ctx context.Context,
	log logr.Logger,
	pod *corev1.Pod,
	container *corev1.Container,
	oomMemory float64,
) (float64, float64, float64, float64) {
	promClient, err := NewPrometheusClient(prometheusURL)
	if err != nil {
		log.Error(err, "Failed to create Prometheus client")
		return 0, 0, 0, 0
	}

	cpuQuery := fmt.Sprintf(`max(
        rate(container_cpu_usage_seconds_total{namespace="%s", container="%s"}[%s])
    ) by (container)`, pod.Namespace, container.Name, cpuSampleInterval.String())
	cpuValues, err := promClient.GetMetricsRange(ctx, cpuQuery, time.Now().Add(-cpuHistoryLength), time.Now(), cpuSampleInterval)
	if err != nil {
		log.Error(err, "Failed to fetch CPU metrics range", "podName", pod.Name, "containerName", container.Name)
		return 0, 0, 0, 0
	}

	cpuRecommender := NewResourceRecommender(cpuConfig)
	recommendedCPU, baseCPU := cpuRecommender.GetRecommendation(cpuRecommendation, cpuValues, oomMemory)
	log.Info("Computed CPU recommendations", "podName", pod.Name, "containerName", container.Name, "baseCPU", baseCPU, "recommendedCPU", recommendedCPU)

	memoryQuery := fmt.Sprintf(`max(
        container_memory_working_set_bytes{namespace="%s", container="%s"}
    ) by (container)`, pod.Namespace, container.Name)
	memoryValues, err := promClient.GetMetricsRange(ctx, memoryQuery, time.Now().Add(-memoryHistoryLength), time.Now(), memorySampleInterval)
	if err != nil {
		log.Error(err, "Failed to fetch memory metrics range", "podName", pod.Name, "containerName", container.Name)
		return 0, 0, 0, 0
	}

	memoryRecommender := NewResourceRecommender(memoryConfig)
	recommendedMemory, baseMemory := memoryRecommender.GetRecommendation(memoryRecommendation, memoryValues, oomMemory)
	log.Info("Computed memory recommendations", "podName", pod.Name, "containerName", container.Name, "baseMemory", baseMemory, "recommendedMemory", recommendedMemory)

	return recommendedCPU, baseCPU, recommendedMemory, baseMemory
}

func (re *Recommender) adjustRecommendations(
	recommendedCPU, baseCPU, recommendedMemory, baseMemory *float64,
) {
	if *recommendedCPU < minCPURecommendation {
		*baseCPU = minCPURecommendation
		*recommendedCPU = minCPURecommendation + adjustedCPUBuffer
	}

	if *recommendedMemory < minMemoryRecommendation {
		*baseMemory = minMemoryRecommendation
		*recommendedMemory = minMemoryRecommendation + adjustedMemoryBuffer
	}
}

func (re *Recommender) createContainerStatus(
	ctx context.Context,
	r *ResourceAdjustmentPolicyReconciler,
	log logr.Logger,
	pod *corev1.Pod,
	container *corev1.Container,
	currentCPUValue, currentMemoryValue, baseCPU, recommendedCPU, baseMemory, recommendedMemory float64,
) v1.ContainerStatus {

	cpuNeedsAdjustment, memoryNeedsAdjustment := re.checkResourceAdjustment(
		*container,
		recommendedCPU,
		recommendedMemory,
	)

	var nodeSelectionResult v1.NodeSelectionResult
	var err error
	if !cpuNeedsAdjustment && !memoryNeedsAdjustment {
		nodeSelectionResult = v1.NodeSelectionResult{NeedsMigration: false, TargetNode: pod.Spec.NodeName}
	} else {
		nodeSelectionResult, err = re.nodeSelection(ctx, log, r, pod, recommendedCPU, recommendedMemory)
	}
	if err != nil {
		log.Error(err, "Failed to select target node for migration", "podName", pod.Name, "containerName", container.Name)
	}

	log.Info("Current cpu request", "container", container.Name, "cpuRequest", float64(container.Resources.Requests.Cpu().MilliValue())/1000.0)

	return v1.ContainerStatus{
		Namespace:           pod.Namespace,
		PodName:             pod.Name,
		ContainerName:       container.Name,
		LastUpdated:         metav1.Now(),
		NodeSelectionResult: nodeSelectionResult,
		CurrentCPU: v1.ResourceUsage{
			Request: container.Resources.Requests.Cpu().String(),
			Limit:   container.Resources.Limits.Cpu().String(),
			// Limit: strconv.FormatInt(container.Resources.Limits.Cpu().MilliValue(), 10),
			Usage: fmt.Sprintf("%.3f cores", currentCPUValue),
		},
		CurrentMemory: v1.ResourceUsage{
			Request: container.Resources.Requests.Memory().String(),
			Limit:   container.Resources.Limits.Memory().String(),
			Usage:   fmt.Sprintf("%d bytes (%.0f Mi)", int64(currentMemoryValue), float64(int64(currentMemoryValue))/1048576),
		},
		CPURecommendation: v1.RecommendationDetails{
			BaseRecommendation:     fmt.Sprintf("%.3f", baseCPU),
			AdjustedRecommendation: fmt.Sprintf("%.3f", recommendedCPU),
			NeedToApply:            cpuNeedsAdjustment,
		},
		MemoryRecommendation: v1.RecommendationDetails{
			BaseRecommendation:     fmt.Sprintf("%.2f", baseMemory/1048576),
			AdjustedRecommendation: fmt.Sprintf("%.2f", recommendedMemory/1048576),
			NeedToApply:            memoryNeedsAdjustment,
		},
	}
}

// TODO: Find a way to check the feature gate
func (re *Recommender) checkFeatureGate(ctx context.Context) (bool, error) {
	// // Create a Kubernetes client
	// config, err := rest.InClusterConfig()
	// if err != nil {
	// 	return false, fmt.Errorf("failed to create in-cluster config: %v", err)
	// }

	// clientset, err := kubernetes.NewForConfig(config)
	// if err != nil {
	// 	return false, fmt.Errorf("failed to create clientset: %v", err)
	// }

	// // Fetch the kube-apiserver pod to check its feature gates
	// pods, err := clientset.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{
	// 	LabelSelector: "component=kube-apiserver",
	// })
	// if err != nil {
	// 	return false, fmt.Errorf("failed to list kube-apiserver pods: %v", err)
	// }

	// if len(pods.Items) == 0 {
	// 	return false, fmt.Errorf("no kube-apiserver pods found")
	// }

	// for _, pod := range pods.Items {
	// 	for _, container := range pod.Spec.Containers {
	// 		for _, arg := range container.Args {
	// 			if arg == "--feature-gates=InPlacePodVerticalScaling=true" {
	// 				return true, nil
	// 			}
	// 		}
	// 	}
	// }

	// return false, nil
	return true, nil
}

func (re *Recommender) applyRecommendations(ctx context.Context, r *ResourceAdjustmentPolicyReconciler, pod *corev1.Pod, status v1.ContainerStatus) error {
	log := k8log.FromContext(ctx)

	enabled, err := re.checkFeatureGate(ctx)
	if err != nil {
		return fmt.Errorf("failed to check feature gate: %v", err)
	}
	if !enabled {
		return fmt.Errorf("InPlacePodVerticalScaling feature gate is not enabled")
	}

	qosClass := pod.Status.QOSClass
	log.Info("Current QoS class", "podName", pod.Name, "qosClass", qosClass)

	updatedContainers := make([]corev1.Container, len(pod.Spec.Containers))
	copy(updatedContainers, pod.Spec.Containers)

	for i, container := range updatedContainers {
		if container.Name == status.ContainerName {
			if status.CPURecommendation.NeedToApply {
				cpuRequest, err := resource.ParseQuantity(status.CPURecommendation.AdjustedRecommendation)
				if err != nil {
					return fmt.Errorf("failed to parse CPU recommendation (Request): %v", err)
				}

				updatedContainers[i].Resources.Requests[corev1.ResourceCPU] = cpuRequest

				if qosClass == corev1.PodQOSGuaranteed {
					updatedContainers[i].Resources.Limits[corev1.ResourceCPU] = cpuRequest
				} else if qosClass == corev1.PodQOSBurstable {
					// For Burstable QoS, set a higher limit (e.g., 1.5x request)
					cpuLimit := cpuRequest.DeepCopy()
					cpuLimit.SetMilli(cpuRequest.MilliValue() * 3 / 2) // 1.5x request
					updatedContainers[i].Resources.Limits[corev1.ResourceCPU] = cpuLimit
				}
			}

			if status.MemoryRecommendation.NeedToApply {
				memoryRequestMi, err := strconv.ParseFloat(status.MemoryRecommendation.AdjustedRecommendation, 64)
				if err != nil {
					return fmt.Errorf("failed to parse memory recommendation (Request): %v", err)
				}
				memoryRequest := resource.NewQuantity(int64(memoryRequestMi*1048576), resource.BinarySI)

				updatedContainers[i].Resources.Requests[corev1.ResourceMemory] = *memoryRequest

				if qosClass == corev1.PodQOSGuaranteed {
					updatedContainers[i].Resources.Limits[corev1.ResourceMemory] = *memoryRequest
				} else if qosClass == corev1.PodQOSBurstable {
					// For Burstable QoS, set a higher limit (e.g., 1.5x request)
					memoryLimit := memoryRequest.DeepCopy()
					memoryLimit.Set(memoryRequest.Value() * 3 / 2) // 1.5x request
					updatedContainers[i].Resources.Limits[corev1.ResourceMemory] = memoryLimit
				}
			}
		}
	}

	patch := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
		Spec: corev1.PodSpec{
			Containers: updatedContainers,
		},
	}

	if err := r.Client.SubResource("resize").Update(ctx, patch); err != nil {
		return fmt.Errorf("failed to update pod resources via /resize subresource: %v", err)
	}

	// Start watching the resize status
	go re.watchResizeStatus(ctx, r, status)

	return nil
}

func (re *Recommender) watchResizeStatus(ctx context.Context, r *ResourceAdjustmentPolicyReconciler, status v1.ContainerStatus) {
	log := k8log.FromContext(ctx)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pod := &corev1.Pod{}
			err := r.Get(ctx, types.NamespacedName{
				Namespace: status.Namespace,
				Name:      status.PodName,
			}, pod)
			if err != nil {
				log.Error(err, "Failed to get pod while watching resize status")
				return
			}

			for _, containerStatus := range pod.Status.ContainerStatuses {
				if containerStatus.Name == status.ContainerName {
					resizeStatus := containerStatus.Resources.Requests
					if resizeStatus != nil {
						log.Info("Current resize status",
							"podName", status.PodName,
							"containerName", status.ContainerName,
							"status", resizeStatus)

						if _, deferred := resizeStatus["ResourceResizeStatusDeferred"]; deferred {
							log.Info("Resource update is deferred",
								"podName", status.PodName,
								"containerName", status.ContainerName)
							return
						}
						if _, infeasible := resizeStatus["ResourceResizeStatusInfeasible"]; infeasible {
							log.Info("Resource update is infeasible",
								"podName", status.PodName,
								"containerName", status.ContainerName)
							return
						}
						log.Info("Resource update completed successfully",
							"podName", status.PodName,
							"containerName", status.ContainerName)
						return
					}
				}
			}
		}
	}
}

// NodeInfo holds the current state and capacity of a node
type NodeInfo struct {
	Name               string
	AllocatableCPU     float64
	AllocatableMemory  float64
	CurrentCPUUsage    float64
	CurrentMemoryUsage float64
	Score              float64
}

func (re *Recommender) nodeSelection(
	ctx context.Context,
	log logr.Logger,
	r *ResourceAdjustmentPolicyReconciler,
	pod *corev1.Pod,
	recommendedCPU, recommendedMemory float64,
) (v1.NodeSelectionResult, error) {
	canStayOnCurrentNode, err := re.canNodeAccommodateResources(ctx, log, r, pod.Spec.NodeName,
		recommendedCPU, recommendedMemory)
	if err != nil {
		return v1.NodeSelectionResult{}, err
	}

	if canStayOnCurrentNode {
		return v1.NodeSelectionResult{NeedsMigration: false, TargetNode: pod.Spec.NodeName}, nil
	}

	targetNode, err := re.findSuitableNode(ctx, log, r, recommendedCPU, recommendedMemory, pod)
	if err != nil {
		return v1.NodeSelectionResult{}, err
	}

	return v1.NodeSelectionResult{
		NeedsMigration: true,
		TargetNode:     targetNode,
	}, nil
}

func (re *Recommender) canNodeAccommodateResources(ctx context.Context, log logr.Logger, r *ResourceAdjustmentPolicyReconciler, nodeName string, cpuRequest, memoryRequest float64) (bool, error) {
	nodeMetrics, err := re.getNodeMetrics(ctx, log, r, nodeName)
	if err != nil {
		return false, fmt.Errorf("failed to get node metrics: %w", err)
	}
	log.Info("Node metrics", "nodeName", nodeName, "nodeMetrics", nodeMetrics)

	projectedCPUUsage := nodeMetrics.CurrentCPUUsage + cpuRequest
	log.Info("Projected CPU usage", "nodeName", nodeName, "projectedCPUUsage", projectedCPUUsage)
	projectedMemoryUsage := nodeMetrics.CurrentMemoryUsage + memoryRequest
	log.Info("Projected memory usage", "nodeName", nodeName, "projectedMemoryUsage", projectedMemoryUsage)

	cpuFits := projectedCPUUsage <= (nodeMetrics.AllocatableCPU * 0.85) // 85% threshold
	memoryFits := projectedMemoryUsage <= (nodeMetrics.AllocatableMemory * 0.85)

	return cpuFits && memoryFits, nil
}

func (re *Recommender) getNodeMetrics(ctx context.Context, log logr.Logger, r *ResourceAdjustmentPolicyReconciler, nodeName string) (*NodeInfo, error) {
	node := &corev1.Node{}
	err := r.Get(ctx, types.NamespacedName{Name: nodeName}, node)
	if err != nil {
		return nil, err
	}

	promClient, err := NewPrometheusClient(prometheusURL)
	if err != nil {
		return nil, err
	}

	cpuQuery := fmt.Sprintf(`avg(rate(node_cpu_seconds_total{mode="idle", node="%s"}[5m])) by (instance)`, nodeName)
	memQuery := fmt.Sprintf(`avg(node_memory_MemTotal_bytes{node="%s"} - node_memory_MemAvailable_bytes{node="%s"}) by (instance)`, nodeName, nodeName)

	cpuUsage, err := promClient.QueryAtTime(cpuQuery, time.Now())
	if err != nil {
		return nil, err
	}
	log.Info("CPU usage", "nodeName", nodeName, "cpuUsage", cpuUsage)

	memUsage, err := promClient.QueryAtTime(memQuery, time.Now())
	if err != nil {
		return nil, err
	}
	log.Info("Memory usage", "nodeName", nodeName, "memUsage", memUsage)

	return &NodeInfo{
		Name:               nodeName,
		AllocatableCPU:     float64(node.Status.Allocatable.Cpu().Value()),
		AllocatableMemory:  float64(node.Status.Allocatable.Memory().Value()),
		CurrentCPUUsage:    cpuUsage,
		CurrentMemoryUsage: memUsage,
	}, nil
}

func (re *Recommender) findSuitableNode(ctx context.Context, log logr.Logger, r *ResourceAdjustmentPolicyReconciler, cpuRequest, memoryRequest float64, pod *corev1.Pod) (string, error) {
	nodes := &corev1.NodeList{}
	err := r.List(ctx, nodes)
	if err != nil {
		return "", err
	}

	var candidateNodes []*NodeInfo

	for _, node := range nodes.Items {
		if node.Name == pod.Spec.NodeName {
			continue
		}

		nodeMetrics, err := re.getNodeMetrics(ctx, log, r, node.Name)
		if err != nil {
			continue
		}

		if !re.canNodeAccommodatePod(nodeMetrics, cpuRequest, memoryRequest) {
			continue
		}

		nodeMetrics.Score = re.scoreNode(nodeMetrics, cpuRequest, memoryRequest)
		candidateNodes = append(candidateNodes, nodeMetrics)
	}

	if len(candidateNodes) == 0 {
		log.Info("No suitable nodes found", "podName", pod.Name)
		return pod.Spec.NodeName, nil
		// return "", fmt.Errorf("no suitable nodes found")
	}

	sort.Slice(candidateNodes, func(i, j int) bool {
		return candidateNodes[i].Score > candidateNodes[j].Score
	})

	return candidateNodes[0].Name, nil
}

func (re *Recommender) canNodeAccommodatePod(node *NodeInfo, cpuRequest, memoryRequest float64) bool {
	cpuAvailable := node.AllocatableCPU - node.CurrentCPUUsage
	memoryAvailable := node.AllocatableMemory - node.CurrentMemoryUsage

	// Include buffer (15% safety margin)
	const safetyMargin = 0.85

	return cpuRequest <= (cpuAvailable*safetyMargin) &&
		memoryRequest <= (memoryAvailable*safetyMargin)
}

func (re *Recommender) scoreNode(node *NodeInfo, cpuRequest, memoryRequest float64) float64 {
	projectedCPUUtil := (node.CurrentCPUUsage + cpuRequest) / node.AllocatableCPU
	projectedMemUtil := (node.CurrentMemoryUsage + memoryRequest) / node.AllocatableMemory

	balanceFactor := 1 - math.Abs(projectedCPUUtil-projectedMemUtil)

	utilizationScore := 1 - ((projectedCPUUtil + projectedMemUtil) / 2)

	return (balanceFactor * 0.4) + (utilizationScore * 0.6)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ResourceAdjustmentPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	quit = make(chan bool)

	// Run the recommender as a background goroutine
	recommender := NewRecommender(controllerMetrics)
	go recommender.runRecommender(r)

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.ResourceAdjustmentPolicy{}).
		Complete(r)
}
