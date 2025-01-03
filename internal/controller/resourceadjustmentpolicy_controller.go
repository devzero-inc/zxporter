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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	k8log "sigs.k8s.io/controller-runtime/pkg/log"
)

// Global configurations
var (
	Recommender          recommender
	defaultNamespaces    = []string{"default"}
	policy_list          = &v1.ResourceAdjustmentPolicyList{}
	defaultCPUConfig     = ResourceConfig{RequestPercentile: 0.95, MarginFraction: 0.15, TargetUtilization: 1.0, BucketSize: 0.1}
	defaultMemoryConfig  = ResourceConfig{RequestPercentile: 0.95, MarginFraction: 0.15, TargetUtilization: 1.0, BucketSize: 1048576}
	namespaces           = defaultNamespaces
	cpuConfig            = defaultCPUConfig
	memoryConfig         = defaultMemoryConfig
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

type ResourceConfig struct {
	RequestPercentile float64
	MarginFraction    float64
	TargetUtilization float64
	BucketSize        float64
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
	// policy := global_policy.Items[0]
	var policy v1.ResourceAdjustmentPolicy
	// if err := r.Get(ctx, req.NamespacedName, policy); err != nil {
	// 	log.Error(err, "Failed to fetch ResourceAdjustmentPolicy")
	// 	return ctrl.Result{}, client.IgnoreNotFound(err)
	// }

	// Update global configurations if specified in the CRD
	if len(policy.Spec.TargetSelector.Namespaces) > 0 && policy.Spec.TargetSelector.Namespaces[0] != "" {
		namespaces = policy.Spec.TargetSelector.Namespaces
		log.Info("Updated namespaces", "value", namespaces)
	}
	if policy.Spec.Policies.CPURecommendation.SampleInterval != "" {
		duration, err := time.ParseDuration(policy.Spec.Policies.CPURecommendation.SampleInterval)
		if err != nil {
			log.Error(err, "Error parsing CPURecommendation.SampleInterval")
		} else {
			cpuSampleInterval = duration
			log.Info("Updated CPURecommendation.SampleInterval", "value", cpuSampleInterval)
		}

		historyLength, err := time.ParseDuration(policy.Spec.Policies.CPURecommendation.HistoryLength)
		if err != nil {
			log.Error(err, "Error parsing CPURecommendation.HistoryLength")
		} else {
			cpuHistoryLength = historyLength
			log.Info("Updated CPURecommendation.HistoryLength", "value", cpuHistoryLength)
		}

		// Parse CPU Recommendation fields
		requestPercentile, err := strconv.ParseFloat(policy.Spec.Policies.CPURecommendation.RequestPercentile, 64)
		if err != nil {
			log.Error(err, "Error parsing CPURecommendation.RequestPercentile")
		} else {
			cpuConfig.RequestPercentile = requestPercentile
			log.Info("Updated CPURecommendation.RequestPercentile", "value", fmt.Sprintf("%.2f", cpuConfig.RequestPercentile))
		}

		marginFraction, err := strconv.ParseFloat(policy.Spec.Policies.CPURecommendation.MarginFraction, 64)
		if err != nil {
			log.Error(err, "Error parsing CPURecommendation.MarginFraction")
		} else {
			cpuConfig.MarginFraction = marginFraction
			log.Info("Updated CPURecommendation.MarginFraction", "value", fmt.Sprintf("%.2f", cpuConfig.MarginFraction))
		}

		targetUtilization, err := strconv.ParseFloat(policy.Spec.Policies.CPURecommendation.TargetUtilization, 64)
		if err != nil {
			log.Error(err, "Error parsing CPURecommendation.TargetUtilization")
		} else {
			cpuConfig.TargetUtilization = targetUtilization
			log.Info("Updated CPURecommendation.TargetUtilization", "value", fmt.Sprintf("%.2f", cpuConfig.TargetUtilization))
		}
	}

	if policy.Spec.Policies.MemoryRecommendation.SampleInterval != "" {
		duration, err := time.ParseDuration(policy.Spec.Policies.MemoryRecommendation.SampleInterval)
		if err != nil {
			log.Error(err, "Error parsing MemoryRecommendation.SampleInterval")
		} else {
			memorySampleInterval = duration
			log.Info("Updated MemoryRecommendation.SampleInterval", "value", memorySampleInterval)
		}

		historyLength, err := time.ParseDuration(policy.Spec.Policies.MemoryRecommendation.HistoryLength)
		if err != nil {
			log.Error(err, "Error parsing MemoryRecommendation.HistoryLength")
		} else {
			memoryHistoryLength = historyLength
			log.Info("Updated MemoryRecommendation.HistoryLength", "value", memoryHistoryLength)
		}

		// Parse Memory Recommendation fields
		requestPercentile, err := strconv.ParseFloat(policy.Spec.Policies.MemoryRecommendation.RequestPercentile, 64)
		if err != nil {
			log.Error(err, "Error parsing MemoryRecommendation.RequestPercentile")
		} else {
			memoryConfig.RequestPercentile = requestPercentile
			log.Info("Updated MemoryRecommendation.RequestPercentile", "value", fmt.Sprintf("%.2f", memoryConfig.RequestPercentile))
		}

		marginFraction, err := strconv.ParseFloat(policy.Spec.Policies.MemoryRecommendation.MarginFraction, 64)
		if err != nil {
			log.Error(err, "Error parsing MemoryRecommendation.MarginFraction")
		} else {
			memoryConfig.MarginFraction = marginFraction
			log.Info("Updated MemoryRecommendation.MarginFraction", "value", fmt.Sprintf("%.2f", memoryConfig.MarginFraction))
		}

		targetUtilization, err := strconv.ParseFloat(policy.Spec.Policies.MemoryRecommendation.TargetUtilization, 64)
		if err != nil {
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

type ResourceRecommender struct {
	config      ResourceConfig
	buckets     map[int]*HistogramBucket
	totalWeight float64
}

func NewResourceRecommender(config ResourceConfig) *ResourceRecommender {
	return &ResourceRecommender{
		config:  config,
		buckets: make(map[int]*HistogramBucket),
	}
}

func (r *ResourceRecommender) ProcessValues(values []float64) {
	r.buckets = make(map[int]*HistogramBucket)
	r.totalWeight = 0

	for _, v := range values {
		bucketIndex := int(v / r.config.BucketSize)
		if _, exists := r.buckets[bucketIndex]; !exists {
			r.buckets[bucketIndex] = &HistogramBucket{
				Start:  float64(bucketIndex) * r.config.BucketSize,
				End:    float64(bucketIndex+1) * r.config.BucketSize,
				Values: make([]float64, 0),
			}
		}
		r.buckets[bucketIndex].Count++
		r.buckets[bucketIndex].Values = append(r.buckets[bucketIndex].Values, v)
		r.buckets[bucketIndex].Weight += v
		r.totalWeight += v
	}
}

func (r *ResourceRecommender) CalculatePercentileRecommendation() float64 {
	if len(r.buckets) == 0 {
		return 0
	}

	targetWeight := r.config.RequestPercentile * r.totalWeight // Using 95th percentile as per algorithm
	cumulativeWeight := 0.0
	maxBucketIndex := 0

	for idx := range r.buckets {
		if idx > maxBucketIndex {
			maxBucketIndex = idx
		}
	}

	for i := 0; i <= maxBucketIndex; i++ {
		if bucket, exists := r.buckets[i]; exists {
			cumulativeWeight += bucket.Weight
			if cumulativeWeight >= targetWeight {
				return math.Ceil((float64(i)+1)*r.config.BucketSize*10) / 10
			}
		}
	}

	return math.Ceil((float64(maxBucketIndex)+1)*r.config.BucketSize*10) / 10
}

func (r *ResourceRecommender) GetRecommendation(values []float64) (float64, float64) {
	if len(values) == 0 {
		return 0, 0
	}

	r.ProcessValues(values)

	// Step 1: Calculate percentile-based recommendation
	baseRecommendation := r.CalculatePercentileRecommendation()

	// Step 2: Apply margin
	marginAdjusted := baseRecommendation * (1 + r.config.MarginFraction)

	// Step 3: Apply target utilization
	utilizationAdjusted := marginAdjusted / r.config.TargetUtilization

	return math.Ceil(utilizationAdjusted*10) / 10, baseRecommendation
}

// runRecommender periodically fetches metrics, computes recommendations, and updates the CRD status
func (re *recommender) runRecommender(r *ResourceAdjustmentPolicyReconciler) {
	ticker := time.NewTicker(3 * time.Minute) // Use the global cpuSampleInterval as the ticker duration
	defer ticker.Stop()

	ctx := context.TODO()
	log := k8log.FromContext(ctx)

	log.Info("Starting the resource recommender")

	re.Lock()
	defer re.Unlock()

	for range ticker.C {
		ctx := context.Background()
		log.Info("Fetching ResourceAdjustmentPolicies")
		if err := r.List(ctx, policy_list); err != nil {
			log.Error(err, "Failed to list ResourceAdjustmentPolicies")
			continue
		}
		log.Info("Found ResourceAdjustmentPolicies", "count", len(policy_list.Items))

		for _, policy := range policy_list.Items {
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

						// Fetch metrics
						promClient, err := NewPrometheusClient(prometheusURL)
						if err != nil {
							log.Error(err, "Failed to create Prometheus client")
							continue
						}

						// Get current CPU usage
						currentCPUQuery := fmt.Sprintf(`rate(container_cpu_usage_seconds_total{pod="%s",container="%s"}[%s])`,
							pod.Name, container.Name, cpuSampleInterval.String())
						currentCPUValue, err := promClient.GetCurrentMetric(ctx, currentCPUQuery)
						if err != nil {
							log.Error(err, "Failed to fetch current CPU metrics", "podName", pod.Name, "containerName", container.Name)
							continue
						}
						log.Info("Fetched current CPU metrics", "podName", pod.Name, "containerName", container.Name, "currentCPUValue", currentCPUValue)

						// Get current memory usage
						currentMemoryQuery := fmt.Sprintf(`rate(container_memory_usage_bytes{pod="%s",container="%s"}[%s])`,
							pod.Name, container.Name, memorySampleInterval.String())
						currentMemoryValue, err := promClient.GetCurrentMetric(ctx, currentMemoryQuery)
						if err != nil {
							log.Error(err, "Failed to fetch current memory metrics", "podName", pod.Name, "containerName", container.Name)
							continue
						}
						log.Info("Fetched current memory metrics", "podName", pod.Name, "containerName", container.Name, "currentMemoryValue", currentMemoryValue)

						// Get CPU metrics range for recommendation
						cpuQuery := fmt.Sprintf(`rate(container_cpu_usage_seconds_total{pod="%s",container="%s"}[%s])`,
							pod.Name, container.Name, cpuSampleInterval.String())
						cpuValues, err := promClient.GetMetricsRange(ctx, cpuQuery, time.Now().Add(-cpuHistoryLength), time.Now(), cpuSampleInterval)
						if err != nil {
							log.Error(err, "Failed to fetch CPU metrics range", "podName", pod.Name, "containerName", container.Name)
							continue
						}
						cpuRecommender := NewResourceRecommender(cpuConfig)
						recommendedCPU, baseCPU := cpuRecommender.GetRecommendation(cpuValues)
						log.Info("Computed CPU recommendations", "podName", pod.Name, "containerName", container.Name, "baseCPU", baseCPU, "recommendedCPU", recommendedCPU)

						// Get memory metrics range for recommendation
						memoryQuery := fmt.Sprintf(`rate(container_memory_usage_bytes{pod="%s",container="%s"}[%s])`,
							pod.Name, container.Name, memorySampleInterval.String())
						memoryValues, err := promClient.GetMetricsRange(ctx, memoryQuery, time.Now().Add(-memoryHistoryLength), time.Now(), memorySampleInterval)
						if err != nil {
							log.Error(err, "Failed to fetch memory metrics range", "podName", pod.Name, "containerName", container.Name)
							continue
						}
						memoryRecommender := NewResourceRecommender(memoryConfig)
						recommendedMemory, baseMemory := memoryRecommender.GetRecommendation(memoryValues)
						log.Info("Computed memory recommendations", "podName", pod.Name, "containerName", container.Name, "baseMemory", baseMemory, "recommendedMemory", recommendedMemory)

						// Update status
						status := v1.ContainerStatus{
							Namespace:     namespace,
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
								BaseRecommendation:     fmt.Sprintf("%.2f", baseCPU),
								AdjustedRecommendation: fmt.Sprintf("%.2f", recommendedCPU),
							},
							MemoryRecommendation: v1.RecommendationDetails{
								BaseRecommendation:     fmt.Sprintf("%.2f", baseMemory/1048576),
								AdjustedRecommendation: fmt.Sprintf("%.2f", recommendedMemory/104857),
							},
						}
						policy.Status.Containers = append(policy.Status.Containers, status)
						log.Info("Updated policy status for container", "policyName", policy.Name, "podName", pod.Name, "containerName", container.Name)
					}

					// Update CRD status
					log.Info("Updating status for policy", "policyName", policy.Name)
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

// SetupWithManager sets up the controller with the Manager.
func (r *ResourceAdjustmentPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Run the recommender as a background goroutine
	go Recommender.runRecommender(r)

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.ResourceAdjustmentPolicy{}).
		Complete(r)
}
