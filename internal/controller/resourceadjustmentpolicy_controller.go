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
	"github.com/prometheus/client_golang/api"
	pv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	k8log "sigs.k8s.io/controller-runtime/pkg/log"
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
	defaultFrequency     = 3 * time.Minute
	cpuSampleInterval    = 1 * time.Minute
	cpuHistoryLength     = 24 * time.Hour
	memorySampleInterval = 1 * time.Minute
	memoryHistoryLength  = 24 * time.Hour
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

// type ResourceConfig struct {
// 	RequestPercentile float64
// 	MarginFraction    float64
// 	TargetUtilization float64
// 	BucketSize        float64
// }

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
		} else {
			cpuSampleInterval = duration
			log.Info("Updated CPURecommendation.SampleInterval", "value", cpuSampleInterval)
		}
	}

	if policy.Spec.Policies.CPURecommendation.HistoryLength != "" {
		if historyLength, err := time.ParseDuration(policy.Spec.Policies.CPURecommendation.HistoryLength); err != nil {
			log.Error(err, "Error parsing CPURecommendation.HistoryLength")
		} else {
			cpuHistoryLength = historyLength
			log.Info("Updated CPURecommendation.HistoryLength", "value", cpuHistoryLength)
		}
	}

	if policy.Spec.Policies.CPURecommendation.RequestPercentile != "" {
		if requestPercentile, err := strconv.ParseFloat(policy.Spec.Policies.CPURecommendation.RequestPercentile, 64); err != nil {
			log.Error(err, "Error parsing CPURecommendation.RequestPercentile")
		} else {
			cpuConfig.RequestPercentile = requestPercentile
			log.Info("Updated CPURecommendation.RequestPercentile", "value", fmt.Sprintf("%.2f", cpuConfig.RequestPercentile))
		}
	}

	if policy.Spec.Policies.CPURecommendation.MarginFraction != "" {
		if marginFraction, err := strconv.ParseFloat(policy.Spec.Policies.CPURecommendation.MarginFraction, 64); err != nil {
			log.Error(err, "Error parsing CPURecommendation.MarginFraction")
		} else {
			cpuConfig.MarginFraction = marginFraction
			log.Info("Updated CPURecommendation.MarginFraction", "value", fmt.Sprintf("%.2f", cpuConfig.MarginFraction))
		}
	}

	if policy.Spec.Policies.CPURecommendation.TargetUtilization != "" {
		if targetUtilization, err := strconv.ParseFloat(policy.Spec.Policies.CPURecommendation.TargetUtilization, 64); err != nil {
			log.Error(err, "Error parsing CPURecommendation.TargetUtilization")
		} else {
			cpuConfig.TargetUtilization = targetUtilization
			log.Info("Updated CPURecommendation.TargetUtilization", "value", fmt.Sprintf("%.2f", cpuConfig.TargetUtilization))
		}
	}

	// Update Memory Recommendation configurations
	if policy.Spec.Policies.MemoryRecommendation.SampleInterval != "" {
		if duration, err := time.ParseDuration(policy.Spec.Policies.MemoryRecommendation.SampleInterval); err != nil {
			log.Error(err, "Error parsing MemoryRecommendation.SampleInterval")
		} else {
			memorySampleInterval = duration
			log.Info("Updated MemoryRecommendation.SampleInterval", "value", memorySampleInterval)
		}
	}

	if policy.Spec.Policies.MemoryRecommendation.HistoryLength != "" {
		if historyLength, err := time.ParseDuration(policy.Spec.Policies.MemoryRecommendation.HistoryLength); err != nil {
			log.Error(err, "Error parsing MemoryRecommendation.HistoryLength")
		} else {
			memoryHistoryLength = historyLength
			log.Info("Updated MemoryRecommendation.HistoryLength", "value", memoryHistoryLength)
		}
	}

	if policy.Spec.Policies.MemoryRecommendation.RequestPercentile != "" {
		if requestPercentile, err := strconv.ParseFloat(policy.Spec.Policies.MemoryRecommendation.RequestPercentile, 64); err != nil {
			log.Error(err, "Error parsing MemoryRecommendation.RequestPercentile")
		} else {
			memoryConfig.RequestPercentile = requestPercentile
			log.Info("Updated MemoryRecommendation.RequestPercentile", "value", fmt.Sprintf("%.2f", memoryConfig.RequestPercentile))
		}
	}

	if policy.Spec.Policies.MemoryRecommendation.MarginFraction != "" {
		if marginFraction, err := strconv.ParseFloat(policy.Spec.Policies.MemoryRecommendation.MarginFraction, 64); err != nil {
			log.Error(err, "Error parsing MemoryRecommendation.MarginFraction")
		} else {
			memoryConfig.MarginFraction = marginFraction
			log.Info("Updated MemoryRecommendation.MarginFraction", "value", fmt.Sprintf("%.2f", memoryConfig.MarginFraction))
		}
	}

	if policy.Spec.Policies.MemoryRecommendation.TargetUtilization != "" {
		if targetUtilization, err := strconv.ParseFloat(policy.Spec.Policies.MemoryRecommendation.TargetUtilization, 64); err != nil {
			log.Error(err, "Error parsing MemoryRecommendation.TargetUtilization")
		} else {
			memoryConfig.TargetUtilization = targetUtilization
			log.Info("Updated MemoryRecommendation.TargetUtilization", "value", fmt.Sprintf("%.2f", memoryConfig.TargetUtilization))
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

func (pc *PrometheusClient) GetCurrentMetric(ctx context.Context, query string) (float64, error) {
	result, _, err := pc.api.Query(ctx, query, time.Now())
	if err != nil {
		return 0, err
	}

	vector, ok := result.(model.Vector)
	if !ok || len(vector) == 0 {
		return 0, fmt.Errorf("no current metric value found")
	}
	return float64(vector[0].Value), nil
}

// type ResourceRecommender struct {
// 	config      ResourceConfig
// 	buckets     map[int]*HistogramBucket
// 	totalWeight float64
// }

// func NewResourceRecommender(config ResourceConfig) *ResourceRecommender {
// 	return &ResourceRecommender{
// 		config:  config,
// 		buckets: make(map[int]*HistogramBucket),
// 	}
// }

// func (r *ResourceRecommender) ProcessValues(values []float64) {
// 	r.buckets = make(map[int]*HistogramBucket)
// 	r.totalWeight = 0

// 	for _, v := range values {
// 		bucketIndex := int(v / r.config.BucketSize)
// 		if _, exists := r.buckets[bucketIndex]; !exists {
// 			r.buckets[bucketIndex] = &HistogramBucket{
// 				Start:  float64(bucketIndex) * r.config.BucketSize,
// 				End:    float64(bucketIndex+1) * r.config.BucketSize,
// 				Values: make([]float64, 0),
// 			}
// 		}
// 		r.buckets[bucketIndex].Count++
// 		r.buckets[bucketIndex].Values = append(r.buckets[bucketIndex].Values, v)
// 		r.buckets[bucketIndex].Weight += v
// 		r.totalWeight += v
// 	}
// }

// func (r *ResourceRecommender) CalculatePercentileRecommendation() float64 {
// 	if len(r.buckets) == 0 {
// 		return 0
// 	}

// 	targetWeight := r.config.RequestPercentile * r.totalWeight
// 	cumulativeWeight := 0.0
// 	maxBucketIndex := 0

// 	for idx := range r.buckets {
// 		if idx > maxBucketIndex {
// 			maxBucketIndex = idx
// 		}
// 	}

// 	for i := 0; i <= maxBucketIndex; i++ {
// 		if bucket, exists := r.buckets[i]; exists {
// 			cumulativeWeight += bucket.Weight
// 			if cumulativeWeight >= targetWeight {
// 				// Return the minimum boundary of the next bucket
// 				// This is exactly what the algorithm specifies
// 				return float64(i+1) * r.config.BucketSize
// 			}
// 		}
// 	}

// 	return float64(maxBucketIndex+1) * r.config.BucketSize
// }

// func (r *ResourceRecommender) GetRecommendation(values []float64) (float64, float64) {
// 	if len(values) == 0 {
// 		return 0, 0
// 	}

// 	r.ProcessValues(values)

// 	baseRecommendation := r.CalculatePercentileRecommendation()

// 	marginAdjusted := baseRecommendation * (1 + r.config.MarginFraction)

// 	utilizationAdjusted := marginAdjusted / r.config.TargetUtilization

// 	return math.Ceil(utilizationAdjusted*10) / 10, baseRecommendation
// }

// New bucket structure
type Bucket struct {
	MinBoundary float64
	Weight      float64
	Count       int
	Values      []float64 // Kept for compatibility with existing code
}

type ResourceConfig struct {
	RequestPercentile float64
	MarginFraction    float64
	TargetUtilization float64
	BucketSize        float64
}

// Updated ResourceRecommender structure
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

func (r *ResourceRecommender) GetRecommendation(values []float64) (float64, float64) {
	if len(values) == 0 {
		return 0, 0
	}

	r.ProcessValues(values)

	baseRecommendation := r.getPercentile(r.config.RequestPercentile)

	var adjustedRecommendation float64
	if r.config.BucketSize == 0.001 { // This indicates CPU resource
		marginAdjusted := baseRecommendation * (1 + math.Min(r.config.MarginFraction, 0.5))

		adjustedRecommendation = math.Min(marginAdjusted, baseRecommendation*1.5)

		if baseRecommendation < 0.1 {
			adjustedRecommendation = math.Min(adjustedRecommendation, baseRecommendation*1.3)
		}
	} else {
		marginAdjusted := baseRecommendation * (1 + r.config.MarginFraction)
		adjustedRecommendation = marginAdjusted / r.config.TargetUtilization
	}

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

	// Convert CPU request to cores
	cpuRequest := float64(container.Resources.Requests.Cpu().MilliValue()) / 1000
	log.Info("Current CPU request",
		"container", container.Name,
		"cpuRequest", cpuRequest)

	// Convert Memory request to Mi
	memoryRequest := float64(container.Resources.Requests.Memory().Value()) / 1048576
	log.Info("Current Memory request",
		"container", container.Name,
		"memoryRequestMi", memoryRequest)

	cpuNeedsAdjustment := false
	memoryNeedsAdjustment := false

	// Check CPU
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

	// Check Memory
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
		case <-quit: // Termination signal received
			log.Info("Stopping the resource recommender")
			return
		case <-ticker.C:
			ctx := context.Background()
			log.Info("Fetching ResourceAdjustmentPolicies")
			policyList := &v1.ResourceAdjustmentPolicyList{}
			if err := r.List(ctx, policyList); err != nil {
				log.Error(err, "Failed to list ResourceAdjustmentPolicies")
				continue
			}
			log.Info("Found ResourceAdjustmentPolicies", "count", len(policyList.Items))

			for _, policy := range policyList.Items {
				for _, namespace := range namespaces {
					log.Info("Processing ResourceAdjustmentPolicy", "policyName", policy.Name, "namespace", namespace)

					// Fetch pod details from the namespace
					pods := &corev1.PodList{}
					log.Info("Fetching pods for namespace", "namespace", namespace)
					if err := r.Client.List(ctx, pods, client.InNamespace(namespace)); err != nil {
						log.Error(err, "Failed to fetch pods", "namespace", namespace)
						continue
					}
					log.Info("Found pods in namespace", "namespace", namespace, "podCount", len(pods.Items))

					for _, pod := range pods.Items {
						log.Info("Processing pod", "podName", pod.Name)
						for _, container := range pod.Spec.Containers {
							log.Info("Processing container", "podName", pod.Name, "containerName", container.Name)

							// Fetch current CPU usage
							promClient, err := NewPrometheusClient(prometheusURL)
							if err != nil {
								log.Error(err, "Failed to create Prometheus client")
								continue
							}

							currentCPUQuery := fmt.Sprintf(`max(
                                rate(container_cpu_usage_seconds_total{namespace="%s",container="%s"}[%s])
                            ) by (container)`, pod.Namespace, container.Name, cpuSampleInterval.String())
							currentCPUValue, err := promClient.GetCurrentMetric(ctx, currentCPUQuery)
							if err != nil {
								log.Error(err, "Failed to fetch current CPU metrics", "podName", pod.Name, "containerName", container.Name)
								continue
							}
							log.Info("Fetched current CPU metrics", "podName", pod.Name, "containerName", container.Name, "currentCPUValue", currentCPUValue)

							// Fetch current memory usage
							currentMemoryQuery := fmt.Sprintf(`max(
                                container_memory_working_set_bytes{namespace="%s", container="%s"}
                            ) by (container)`, pod.Namespace, container.Name)
							currentMemoryValue, err := promClient.GetCurrentMetric(ctx, currentMemoryQuery)
							if err != nil {
								log.Error(err, "Failed to fetch current memory metrics", "podName", pod.Name, "containerName", container.Name)
								continue
							}
							log.Info("Fetched current memory metrics", "podName", pod.Name, "containerName", container.Name, "currentMemoryValue", currentMemoryValue)

							// Get CPU metrics range for recommendation
							cpuQuery := fmt.Sprintf(`max(
                                rate(container_cpu_usage_seconds_total{namespace="%s", container="%s"}[%s])
                            ) by (container)`, pod.Namespace, container.Name, cpuSampleInterval.String())
							cpuValues, err := promClient.GetMetricsRange(ctx, cpuQuery, time.Now().Add(-cpuHistoryLength), time.Now(), cpuSampleInterval)
							if err != nil {
								log.Error(err, "Failed to fetch CPU metrics range", "podName", pod.Name, "containerName", container.Name)
								continue
							}
							cpuRecommender := NewResourceRecommender(cpuConfig)
							recommendedCPU, baseCPU := cpuRecommender.GetRecommendation(cpuValues)
							log.Info("Computed CPU recommendations", "podName", pod.Name, "containerName", container.Name, "baseCPU", baseCPU, "recommendedCPU", recommendedCPU)

							// Get memory metrics range for recommendation
							memoryQuery := fmt.Sprintf(`max(
                                container_memory_working_set_bytes{namespace="%s", container="%s"}
                            ) by (container)`, pod.Namespace, container.Name)
							memoryValues, err := promClient.GetMetricsRange(ctx, memoryQuery, time.Now().Add(-memoryHistoryLength), time.Now(), memorySampleInterval)
							if err != nil {
								log.Error(err, "Failed to fetch memory metrics range", "podName", pod.Name, "containerName", container.Name)
								continue
							}
							memoryRecommender := NewResourceRecommender(memoryConfig)
							recommendedMemory, baseMemory := memoryRecommender.GetRecommendation(memoryValues)
							log.Info("Computed memory recommendations", "podName", pod.Name, "containerName", container.Name, "baseMemory", baseMemory, "recommendedMemory", recommendedMemory)

							cpuNeedsAdjustment, memoryNeedsAdjustment := re.checkResourceAdjustment(
								container,
								recommendedCPU,
								recommendedMemory,
							)

							// Update status with new recommendations
							status := v1.ContainerStatus{
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

							if cpuNeedsAdjustment || memoryNeedsAdjustment {
								if err := re.applyRecommendations(ctx, r, status); err != nil {
									log.Error(err, "Failed to apply recommendations",
										"podName", pod.Name,
										"containerName", container.Name)
								}
							}

							policy.Status.Containers = append(policy.Status.Containers, status)
							log.Info("Updated policy status for container", "policyName", policy.Name, "podName", pod.Name, "containerName", container.Name)
						}

						// Update CRD status
						if err := r.Status().Update(ctx, &policy); err != nil {
							log.Error(err, "Failed to update status for policy", "policyName", policy.Name)
						} else {
							log.Info("Successfully updated status for policy", "policyName", policy.Name)
						}
					}
				}
			}
		}
	}
}

func (re *recommender) checkFeatureGate(ctx context.Context) (bool, error) {
	log := k8log.FromContext(ctx)
	log.Info("Checking InPlacePodVerticalScaling feature gate")

	config, err := rest.InClusterConfig()
	if err != nil {
		log.Error(err, "Failed to get cluster config")
		return false, fmt.Errorf("failed to get cluster config: %v", err)
	}

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		log.Error(err, "Failed to create discovery client")
		return false, fmt.Errorf("failed to create discovery client: %v", err)
	}

	featureGates, err := discoveryClient.ServerVersion()
	if err != nil {
		log.Error(err, "Failed to get server version")
		return false, fmt.Errorf("failed to get server version: %v", err)
	}

	// Check if InPlacePodVerticalScaling is enabled in the feature gates
	if featureGates.GitVersion >= "v1.24.0" {
		log.Info("InPlacePodVerticalScaling feature gate is enabled")
		return true, nil
	}

	log.Info("InPlacePodVerticalScaling feature gate is not enabled")
	return false, nil
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

	// Get the pod
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
			// Parse CPU recommendation
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

			// Parse Memory recommendation
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

// Function to watch resize status
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
