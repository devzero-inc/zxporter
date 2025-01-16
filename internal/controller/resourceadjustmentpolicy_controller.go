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
	"strconv"
	"sync"
	"time"

	v1 "github.com/devzero-inc/resource-adjustment-operator/api/v1"
	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/api"
	pv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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

type recommendationType int

const (
	cpuRecommendation recommendationType = iota
	memoryRecommendation
)

// Global configurations
var (
	Recommender          recommender
	defaultNamespaces    = []string{"default"}
	quit                 chan bool
	defaultCPUConfig     = ResourceConfig{RequestPercentile: 0.95, MarginFraction: 0.15, TargetUtilization: 1.0, BucketSize: 0.001}
	defaultMemoryConfig  = ResourceConfig{RequestPercentile: 0.95, MarginFraction: 0.15, TargetUtilization: 1.0, BucketSize: 1048576}
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
	prometheusURL        = "http://prometheus-service.monitoring.svc.cluster.local:8080"
)

type recommender struct {
	sync.Mutex
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

// +kubebuilder:rbac:groups=apps.apps.devzero.io,resources=resourceadjustmentpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.apps.devzero.io,resources=resourceadjustmentpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.apps.devzero.io,resources=resourceadjustmentpolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods/resize,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

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

	// Update namespaces
	if len(policy.Spec.TargetSelector.Namespaces) > 0 && policy.Spec.TargetSelector.Namespaces[0] != "" {
		namespaces = policy.Spec.TargetSelector.Namespaces
		log.Info("Updated namespaces", "value", namespaces)
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

	// Update Frequency
	if policy.Spec.Policies.Frequency != "" {
		if frequency, err := time.ParseDuration(policy.Spec.Policies.Frequency); err != nil {
			log.Error(err, "Error parsing Frequency")
		} else if frequency != defaultFrequency {
			defaultFrequency = frequency
			log.Info("Updated Frequency", "value", defaultFrequency)
			r.restartRecommender()
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

func (re *recommender) checkResourceAdjustment(container corev1.Container, recommendedCPU, recommendedMemory float64) (bool, bool) {
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

func (re *recommender) checkForOOMEvents(pod corev1.Pod, containerName string) float64 {
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

func (re *recommender) getMemoryUsageAtTermination(namespace, containerName string, terminationTime time.Time) float64 {
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

func (r *ResourceAdjustmentPolicyReconciler) restartRecommender() {
	quit <- true
	quit = make(chan bool)
	go Recommender.runRecommender(r)
}

func (re *recommender) runRecommender(r *ResourceAdjustmentPolicyReconciler) {
	ticker := time.NewTicker(defaultFrequency)
	defer ticker.Stop()

	ctx := context.TODO()
	log := k8log.FromContext(ctx)

	log.Info("Starting the resource recommender")

	re.Lock()
	defer re.Unlock()

	for {
		select {
		case <-quit:
			log.Info("Stopping the resource recommender")
			return
		case <-ticker.C:
			re.processPolicies(ctx, r, log)
		}
	}
}

func (re *recommender) processPolicies(ctx context.Context, r *ResourceAdjustmentPolicyReconciler, log logr.Logger) {
	log.Info("Fetching ResourceAdjustmentPolicies")
	policyList := &v1.ResourceAdjustmentPolicyList{}
	if err := r.List(ctx, policyList); err != nil {
		log.Error(err, "Failed to list ResourceAdjustmentPolicies")
		return
	}
	log.Info("Found ResourceAdjustmentPolicies", "count", len(policyList.Items))

	for _, policy := range policyList.Items {
		for _, namespace := range namespaces {
			re.processNamespace(ctx, r, log, &policy, namespace)
		}
	}
}

func (re *recommender) processNamespace(ctx context.Context, r *ResourceAdjustmentPolicyReconciler, log logr.Logger, policy *v1.ResourceAdjustmentPolicy, namespace string) {
	log.Info("Processing ResourceAdjustmentPolicy", "policyName", policy.Name, "namespace", namespace)

	pods := &corev1.PodList{}
	log.Info("Fetching pods for namespace", "namespace", namespace)
	if err := r.Client.List(ctx, pods, client.InNamespace(namespace)); err != nil {
		log.Error(err, "Failed to fetch pods", "namespace", namespace)
		return
	}
	log.Info("Found pods in namespace", "namespace", namespace, "podCount", len(pods.Items))

	for _, pod := range pods.Items {
		re.processPod(ctx, r, log, policy, &pod)
	}
}

func (re *recommender) processPod(ctx context.Context, r *ResourceAdjustmentPolicyReconciler, log logr.Logger, policy *v1.ResourceAdjustmentPolicy, pod *corev1.Pod) {
	log.Info("Processing pod", "podName", pod.Name)

	for _, container := range pod.Spec.Containers {
		re.processContainer(ctx, r, log, policy, pod, &container)
	}

	if err := r.Status().Update(ctx, policy); err != nil {
		log.Error(err, "Failed to update status for policy", "policyName", policy.Name)
	} else {
		log.Info("Successfully updated status for policy", "policyName", policy.Name)
	}
}

func (re *recommender) processContainer(ctx context.Context, r *ResourceAdjustmentPolicyReconciler, log logr.Logger, policy *v1.ResourceAdjustmentPolicy, pod *corev1.Pod, container *corev1.Container) {
	log.Info("Processing container", "podName", pod.Name, "containerName", container.Name)

	currentCPUValue, currentMemoryValue, oomMemory, err := re.fetchCurrentMetrics(ctx, log, pod, container)
	if err != nil {
		return
	}

	recommendedCPU, baseCPU, recommendedMemory, baseMemory := re.calculateRecommendations(ctx, log, pod, container, oomMemory)

	re.adjustRecommendations(&recommendedCPU, &baseCPU, &recommendedMemory, &baseMemory)

	status := re.createContainerStatus(pod, container, currentCPUValue, currentMemoryValue, baseCPU, recommendedCPU, baseMemory, recommendedMemory)
	if autoApply && (status.CPURecommendation.NeedToApply || status.MemoryRecommendation.NeedToApply) {
		if err := re.applyRecommendations(ctx, r, status); err != nil {
			log.Error(err, "Failed to apply recommendations", "podName", pod.Name, "containerName", container.Name)
		}
	}

	policy.Status.Containers = append(policy.Status.Containers, status)
	log.Info("Updated policy status for container", "policyName", policy.Name, "podName", pod.Name, "containerName", container.Name)
}

func (re *recommender) fetchCurrentMetrics(ctx context.Context, log logr.Logger, pod *corev1.Pod, container *corev1.Container) (float64, float64, float64, error) {
	promClient, err := NewPrometheusClient(prometheusURL)
	if err != nil {
		log.Error(err, "Failed to create Prometheus client")
		return 0, 0, 0, err
	}

	currentCPUQuery := fmt.Sprintf(`max(
        rate(container_cpu_usage_seconds_total{namespace="%s",container="%s"}[%s])
    ) by (container)`, pod.Namespace, container.Name, cpuSampleInterval.String())
	currentCPUValue, err := promClient.QueryAtTime(currentCPUQuery, time.Now())
	if err != nil {
		log.Error(err, "Failed to fetch current CPU metrics", "podName", pod.Name, "containerName", container.Name)
		return 0, 0, 0, err
	}
	log.Info("Fetched current CPU metrics", "podName", pod.Name, "containerName", container.Name, "currentCPUValue", currentCPUValue)

	currentMemoryQuery := fmt.Sprintf(`max(
        container_memory_working_set_bytes{namespace="%s", container="%s"}
    ) by (container)`, pod.Namespace, container.Name)
	currentMemoryValue, err := promClient.QueryAtTime(currentMemoryQuery, time.Now())
	if err != nil {
		log.Error(err, "Failed to fetch current memory metrics", "podName", pod.Name, "containerName", container.Name)
		return 0, 0, 0, err
	}
	log.Info("Fetched current memory metrics", "podName", pod.Name, "containerName", container.Name, "currentMemoryValue", currentMemoryValue)

	oomMemory := 0.0
	if oomProtection {
		oomMemory = re.checkForOOMEvents(*pod, container.Name)
		log.Info("OOM memory", "podName", pod.Name, "containerName", container.Name, "oomMemory", oomMemory)
	}

	return currentCPUValue, currentMemoryValue, oomMemory, nil
}

func (re *recommender) calculateRecommendations(
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

func (re *recommender) adjustRecommendations(
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

func (re *recommender) createContainerStatus(
	pod *corev1.Pod,
	container *corev1.Container,
	currentCPUValue, currentMemoryValue, baseCPU, recommendedCPU, baseMemory, recommendedMemory float64,
) v1.ContainerStatus {

	cpuNeedsAdjustment, memoryNeedsAdjustment := re.checkResourceAdjustment(
		*container,
		baseCPU,
		baseMemory,
	)

	return v1.ContainerStatus{
		Namespace:     pod.Namespace,
		PodName:       pod.Name,
		ContainerName: container.Name,
		LastUpdated:   metav1.Now(),
		CurrentCPU: v1.ResourceUsage{
			Limit: container.Resources.Limits.Cpu().String(),
			Usage: fmt.Sprintf("%.3f cores", currentCPUValue),
		},
		CurrentMemory: v1.ResourceUsage{
			Limit: container.Resources.Limits.Memory().String(),
			Usage: fmt.Sprintf("%d bytes (%.0f Mi)", int64(currentMemoryValue), float64(int64(currentMemoryValue))/1048576),
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
func (re *recommender) checkFeatureGate(ctx context.Context) (bool, error) {
	return true, nil
}

func (re *recommender) applyRecommendations(ctx context.Context, r *ResourceAdjustmentPolicyReconciler, status v1.ContainerStatus) error {
	log := k8log.FromContext(ctx)

	// Check if InPlacePodVerticalScaling feature gate is enabled
	featureGate, err := re.checkFeatureGate(ctx)
	if err != nil {
		return fmt.Errorf("failed to check feature gate: %v", err)
	}
	if !featureGate {
		log.Info("InPlacePodVerticalScaling feature gate is not enabled, skipping resource update")
		return nil
	}

	pod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: status.Namespace,
		Name:      status.PodName,
	}, pod); err != nil {
		return fmt.Errorf("failed to get pod: %v", err)
	}

	// Prepare the patch for the /resize subresource
	updatedContainers := make([]corev1.Container, len(pod.Spec.Containers))
	copy(updatedContainers, pod.Spec.Containers)

	for i, container := range updatedContainers {
		if container.Name == status.ContainerName {
			cpuRecommendation := status.CPURecommendation
			if cpuRecommendation.NeedToApply {
				cpuLimitQuantity, err := resource.ParseQuantity(cpuRecommendation.AdjustedRecommendation)
				if err != nil {
					return fmt.Errorf("failed to parse CPU recommendation (Limit): %v", err)
				}
				cpuRequestQuantity, err := resource.ParseQuantity(cpuRecommendation.BaseRecommendation)
				if err != nil {
					return fmt.Errorf("failed to parse CPU recommendation (Request): %v", err)
				}
				updatedContainers[i].Resources.Limits[corev1.ResourceCPU] = cpuLimitQuantity
				updatedContainers[i].Resources.Requests[corev1.ResourceCPU] = cpuRequestQuantity
			}

			memoryRecommendation := status.MemoryRecommendation
			if memoryRecommendation.NeedToApply {
				memoryLimitMi, err := strconv.ParseFloat(memoryRecommendation.AdjustedRecommendation, 64)
				if err != nil {
					return fmt.Errorf("failed to parse memory recommendation (Limit): %v", err)
				}
				memoryRequestMi, err := strconv.ParseFloat(memoryRecommendation.BaseRecommendation, 64)
				if err != nil {
					return fmt.Errorf("failed to parse memory recommendation (Request): %v", err)
				}
				memoryLimitQuantity := resource.NewQuantity(int64(memoryLimitMi*1048576), resource.BinarySI)
				memoryRequestQuantity := resource.NewQuantity(int64(memoryRequestMi*1048576), resource.BinarySI)
				updatedContainers[i].Resources.Requests[corev1.ResourceMemory] = *memoryRequestQuantity
				updatedContainers[i].Resources.Limits[corev1.ResourceMemory] = *memoryLimitQuantity
			}
		}
	}

	// Create a patch for the /resize subresource
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

func (re *recommender) watchResizeStatus(ctx context.Context, r *ResourceAdjustmentPolicyReconciler, status v1.ContainerStatus) {
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

			// Check resize status
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

// SetupWithManager sets up the controller with the Manager.
func (r *ResourceAdjustmentPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	quit = make(chan bool)
	// Run the recommender as a background goroutine
	go Recommender.runRecommender(r)

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.ResourceAdjustmentPolicy{}).
		Complete(r)
}
