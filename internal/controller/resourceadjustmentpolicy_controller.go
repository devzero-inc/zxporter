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
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/devzero-inc/zxporter/api/v1"
	"github.com/devzero-inc/zxporter/internal/controller/pid"

	gen "github.com/devzero-inc/services/pulse/gen/api/v1"
	genconnect "github.com/devzero-inc/services/pulse/gen/api/v1/apiv1connect"
	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/api"
	pv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	k8s_errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	metricsv1 "k8s.io/metrics/pkg/client/clientset/versioned"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	k8log "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	minFrequency = 30 * time.Second
	maxFrequency = 20 * time.Minute
	crNamespace  = "devzero"
)

const (
	// _ENV_PULSE_URL is the URL that this controller will use to export data to pulse.
	_ENV_PULSE_URL = "PULSE_URL"

	// _ENV_PULSE_API_TOKEN is the token Pulse will use to identify which controller is sending it data.
	_ENV_PULSE_API_TOKEN = "PULSE_API_TOKEN"
)

// Global configurations
var (
	StatusUpdater       StatusUpdate
	quit                chan bool
	defaultCPUConfig    = ResourceConfig{RequestPercentile: 95.0, MarginFraction: 0.15, TargetUtilization: 1.0, BucketSize: 0.001}
	defaultMemoryConfig = ResourceConfig{RequestPercentile: 95.0, MarginFraction: 0.15, TargetUtilization: 1.0, BucketSize: 1048576}
	cpuConfig           = defaultCPUConfig
	memoryConfig        = defaultMemoryConfig
	oomBumpRatio        = 1.2
	defaultFrequency    = 3 * time.Minute

	// seems to be unused?
	cpuSampleInterval    = 1 * time.Minute
	cpuHistoryLength     = 24 * time.Hour
	memorySampleInterval = 1 * time.Minute
	memoryHistoryLength  = 24 * time.Hour
	lookbackDuration     = 1 * time.Hour

	autoApply     = false
	oomProtection = true
	pulseUrl      = "http://host.docker.internal:9990"
	pulseClient   = NewPulseClient(pulseUrl)

	ProportionalGain              = 8.0
	IntegralGain                  = 0.3
	DerivativeGain                = 1.0
	AntiWindUpGain                = 1.0
	IntegralDischargeTimeConstant = 1.0
	LowPassTimeConstant           = 2 * time.Second
	MaxOutput                     = 1.01
	MinOutput                     = -2.15
)

type ResourceConfig struct {
	RequestPercentile float64
	MarginFraction    float64
	TargetUtilization float64
	BucketSize        float64
}

// PulseClient struct for handling gRPC requests
type PulseClient struct {
	recommendationClient genconnect.RecommendationServiceClient
	usageClient          genconnect.ResourceUsageServiceClient
}

// NewPulseClient initializes Pulse gRPC clients
func NewPulseClient(pulseBaseURL string) *PulseClient {
	return &PulseClient{
		recommendationClient: genconnect.NewRecommendationServiceClient(http.DefaultClient, pulseBaseURL),
		usageClient:          genconnect.NewResourceUsageServiceClient(http.DefaultClient, pulseBaseURL),
	}
}

// Initialize Kubernetes Metrics API client
func getMetricsClient() (*metricsv1.Clientset, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	metricsClient, err := metricsv1.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return metricsClient, nil
}

// Fetch real-time CPU & Memory usage
func fetchContainerUsage(metricsClient *metricsv1.Clientset, podNamespace, podName, containerName string) (int64, int64, error) {
	podMetrics, err := metricsClient.MetricsV1beta1().PodMetricses(podNamespace).Get(context.TODO(), podName, metav1.GetOptions{})
	if err != nil {
		return 0, 0, fmt.Errorf("failed to fetch pod metrics: %v", err)
	}

	for _, container := range podMetrics.Containers {
		if container.Name == containerName {
			cpuUsage := container.Usage.Cpu().MilliValue()
			memUsage := container.Usage.Memory().Value()
			return cpuUsage, memUsage, nil
		}
	}
	return 0, 0, fmt.Errorf("metrics not found for container %s", containerName)
}

type ContainerState struct {
	Namespace        string
	PodName          string
	ContainerName    string
	PIDController    *pid.AntiWindupController
	Frequency        time.Duration
	LastUpdated      time.Time
	UpdateChan       chan struct{} // Channel to signal frequency updates
	ContainerQuit    chan bool
	UsageMonitorQuit chan bool // Channel to stop usage monitoring loop
	Reconciling      bool      // Flag to indicate if reconciliation is in progress
}

type Recommender struct {
	sync.Mutex
	ContainerStates map[string]*ContainerState // Key: "namespace/pod/container"
}

type StatusUpdate struct {
	sync.Mutex
}

func NewRecommender() *Recommender {
	return &Recommender{
		ContainerStates: make(map[string]*ContainerState),
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
// +kubebuilder:rbac:groups=metrics.k8s.io,resources=pods,verbs=get;list;watch

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

	// We only use zxporter-resourceadjustmentpolicy-cr in zxporter-system
	if policy.Namespace != crNamespace {
		log.Info("Skipping reconsile for CR", "policy", policy.Name, "namespace", policy.Namespace)
		return ctrl.Result{}, nil // Skip reconciliation
	}

	// // Update namespaces
	// if len(policy.Spec.TargetSelector.Namespaces) > 0 && policy.Spec.TargetSelector.Namespaces[0] != "" {
	// 	if !equal(policy.Spec.TargetSelector.Namespaces, namespaces) {
	// 		namespaces = policy.Spec.TargetSelector.Namespaces
	// 		log.Info("Updated namespaces", "value", namespaces)
	// 	}
	// }

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

	// if policy.Spec.Policies.PromURL != "" && policy.Spec.Policies.PromURL != prometheusURL {
	// 	prometheusURL = policy.Spec.Policies.PromURL
	// 	log.Info("Updated Prometheus URL", "value", prometheusURL)
	// 	r.restartRecommender(ctx)
	// }

	if policy.Spec.Policies.PulseURL != "" && policy.Spec.Policies.PulseURL != pulseUrl {
		pulseUrl = policy.Spec.Policies.PulseURL
		pulseClient = NewPulseClient(pulseUrl)
		log.Info("Updated Pulse URL", "value", pulseUrl)
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

func (re *Recommender) checkForOOMEvents(pod corev1.Pod, containerName string) *timestamppb.Timestamp {
	ctx := context.TODO()
	log := k8log.FromContext(ctx)
	for _, containerStatus := range pod.Status.ContainerStatuses {
		if containerStatus.Name == containerName && containerStatus.LastTerminationState.Terminated != nil {
			terminated := containerStatus.LastTerminationState.Terminated
			if terminated.Reason == "OOMKilled" && time.Since(terminated.FinishedAt.Time) <= 7*time.Hour {
				log.Info("Found OOM event", "podName", pod.Name, "containerName", containerName, "oomTime", terminated.FinishedAt.Time)
				return timestamppb.New(terminated.FinishedAt.Time)
			}
		}
	}
	return nil
}

func (re *Recommender) getAllocatedResources(container *corev1.Container) (float64, float64) {
	cpuRequest := container.Resources.Requests.Cpu()
	if cpuRequest.IsZero() {
		cpuRequest = container.Resources.Limits.Cpu()
	}
	allocatedCPU := float64(cpuRequest.MilliValue())

	memoryRequest := container.Resources.Requests.Memory()
	if memoryRequest.IsZero() {
		memoryRequest = container.Resources.Limits.Memory()
	}
	allocatedMemory := float64(memoryRequest.Value())

	return allocatedCPU, allocatedMemory
}

func NewContainerState(namespace, podName, containerName string) *ContainerState {
	return &ContainerState{
		Namespace:     namespace,
		PodName:       podName,
		ContainerName: containerName,
		PIDController: &pid.AntiWindupController{
			// TODO: consider finding an optimal tuning path for these values
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

type errorStates struct {
	cpuError, cpuUtilization       float64
	memoryError, memoryUtilization float64
}

func (re *Recommender) calculateError(log logr.Logger, pod *corev1.Pod, container *corev1.Container) (*errorStates, error) {
	metricsClient, err := getMetricsClient()
	if err != nil {
		log.Error(err, "Failed to create Kubernetes Metrics client")
		return nil, errors.New("this should really not be an error")
	}

	currentCPU, currentMemory, err := fetchContainerUsage(metricsClient, pod.Namespace, pod.Name, container.Name)
	if err != nil {
		return nil, err
	}

	allocatedCPU, allocatedMemory := re.getAllocatedResources(container)

	cpuUtilization := float64(currentCPU) / allocatedCPU
	memoryUtilization := float64(currentMemory) / allocatedMemory
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

	return &errorStates{
		cpuError:          cpuError,
		cpuUtilization:    cpuUtilization,
		memoryError:       memoryError,
		memoryUtilization: memoryUtilization,
	}, nil
}

func (re *Recommender) monitorFrequency(r *ResourceAdjustmentPolicyReconciler, state *ContainerState) {
	ctx := context.Background()
	log := k8log.FromContext(ctx)
	threshold := 0.2 // Threshold for immediate reconciliation (e.g., 20% change)
	prvTime := time.Now()

	for {
		select {
		case <-quit:
			return
		case <-state.ContainerQuit:
			return
		case <-time.After(5 * time.Second):
			key := types.NamespacedName{Namespace: state.Namespace, Name: state.PodName}
			pod := corev1.Pod{}
			if err := r.Client.Get(ctx, key, &pod); err != nil {
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
			errStates, err := re.calculateError(log, &pod, container)
			if err != nil {
				continue
			}
			log.Info("Calculated errors and utilizations",
				"pod", state.PodName,
				"container", state.ContainerName,
				"cpuError", errStates.cpuError,
				"memoryError", errStates.memoryError,
				"cpuUtilization", errStates.cpuUtilization,
				"memoryUtilization", errStates.memoryUtilization,
			)

			// Use the larger error (CPU or memory) to determine the frequency
			var errVal float64
			if (errStates.cpuError < 0 && errStates.memoryError < 0) || (errStates.cpuError > 0 && errStates.memoryError > 0) {
				if math.Abs(errStates.cpuError) > math.Abs(errStates.memoryError) {
					errVal = errStates.cpuError
				} else {
					errVal = errStates.memoryError
				}
			} else {
				if errStates.cpuError < 0 {
					if math.Abs(errStates.cpuError)*2 > errStates.memoryError {
						errVal = errStates.cpuError
					} else {
						errVal = errStates.memoryError
					}
				} else {
					if math.Abs(errStates.memoryError)*2 > errStates.cpuError {
						errVal = errStates.memoryError
					} else {
						errVal = errStates.cpuError
					}
				}
			}
			log.Info(
				"Final error calculated",
				"pod", state.PodName,
				"container", state.ContainerName,
				"error", errVal,
			)

			state.PIDController.Update(pid.AntiWindupControllerInput{
				ReferenceSignal:   0,
				ActualSignal:      errVal,
				FeedForwardSignal: 0.0,
				SamplingInterval:  time.Since(prvTime),
			})
			prvTime = time.Now()

			normalizedControlSignal := normalizeControlSignal(state.PIDController.State.ControlSignal)

			newFrequency := calculateFrequency(normalizedControlSignal)

			log.Info(
				"Control signal calculated",
				"pod", state.PodName,
				"container", state.ContainerName,
				"controlSignal", state.PIDController.State.ControlSignal,
				"newFrequency", newFrequency,
			)

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

// func (re *Recommender) runContainerLoop(ctx context.Context, r *ResourceAdjustmentPolicyReconciler, state *ContainerState, policy *v1.ResourceAdjustmentPolicy, pod *corev1.Pod, container *corev1.Container) {
// 	log := k8log.FromContext(ctx)

// 	for {
// 		select {
// 		case <-quit:
// 			return
// 		case <-state.ContainerQuit:
// 			return
// 		case <-state.UpdateChan:
// 			// Immediate reconciliation triggered
// 			log.Info("Immediate reconciliation triggered for container",
// 				"pod", state.PodName,
// 				"container", state.ContainerName,
// 				"newFrequency", state.Frequency)
// 			re.processContainer(ctx, r, log, policy, pod, container)
// 		case <-time.After(state.Frequency):
// 			// Regular reconciliation based on current frequency
// 			log.Info("Regular reconciliation for container",
// 				"pod", state.PodName,
// 				"container", state.ContainerName,
// 				"frequency", state.Frequency)
// 			re.processContainer(ctx, r, log, policy, pod, container)
// 		}
// 	}
// }

func (re *Recommender) runUsageMonitorLoop(ctx context.Context, r *ResourceAdjustmentPolicyReconciler, state *ContainerState, pod *corev1.Pod, container *corev1.Container) {
	log := k8log.FromContext(ctx)
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	metricsClient, err := getMetricsClient()
	if err != nil {
		log.Error(err, "Failed to create Kubernetes Metrics client")
		return
	}

	for {
		select {
		case <-state.UsageMonitorQuit:
			log.Info("Stopping usage monitor for container", "pod", pod.Name, "container", container.Name)
			return
		case <-ticker.C:
			log.Info("Fetching real-time resource usage for container", "pod", pod.Name, "container", container.Name)
			currentCPUUsage, currentMemoryUsage, err := fetchContainerUsage(metricsClient, pod.Namespace, pod.Name, container.Name)
			if err != nil {
				log.Error(err, "Failed to fetch real-time resource usage", "pod", pod.Name, "container", container.Name)
				continue
			}

			// Send usage data to Pulse
			go re.sendResourceUsageToPulse(log, pod, container, currentCPUUsage, currentMemoryUsage)
		}
	}
}

func (re *Recommender) sendResourceUsageToPulse(log logr.Logger, pod *corev1.Pod, container *corev1.Container, cpuUsage, memoryUsage int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Fetch labels from the Pod
	createdByUser := pod.Labels["meta.devzero.io/created-by-user"]       // Will be "" if missing
	organizationID := pod.Labels["meta.devzero.io/organization-id"]      // Will be "" if missing
	virtualClusterID := pod.Labels["meta.devzero.io/virtual-cluster-id"] // Will be "" if missing
	workloadID := pod.Labels["meta.devzero.io/workload-id"]              // Will be "" if missing
	workloadName := pod.Labels["meta.devzero.io/workload-name"]          // Will be "" if missing

	// Create the ResourceUsage message
	usage := &gen.ResourceUsage{
		ContainerId:        fmt.Sprintf("%s-%s", pod.Name, container.Name),
		ContainerName:      container.Name,
		PodName:            pod.Name,
		Namespace:          pod.Namespace,
		NodeName:           pod.Spec.NodeName,
		CpuUsageMillicores: cpuUsage,
		MemoryUsageBytes:   memoryUsage,
		Timestamp:          timestamppb.Now(),
		CreatedByUser:      createdByUser,
		OrganizationId:     organizationID,
		VirtualClusterId:   virtualClusterID,
		WorkloadId:         workloadID,
		WorkloadName:       workloadName,
	}

	// Create the request
	req := connect.NewRequest(&gen.SendResourceUsageRequest{
		Usages: []*gen.ResourceUsage{usage},
	})

	// Send the request
	_, err := pulseClient.usageClient.SendResourceUsage(ctx, req)
	if err != nil {
		log.Error(err, "Failed to send resource usage to Pulse", "podName", pod.Name, "containerName", container.Name)
	} else {
		log.Info("Successfully sent resource usage to Pulse", "podName", pod.Name, "containerName", container.Name)
	}
}

func (r *ResourceAdjustmentPolicyReconciler) restartRecommender(ctx context.Context) {
	quit <- true
	log := k8log.FromContext(ctx)
	log.Info("Restarting the resource recommender")
	quit = make(chan bool)
	recommender := NewRecommender()
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

				// Fetch the ResourceAdjustmentPolicy from the zxporter-system namespace
				var policy v1.ResourceAdjustmentPolicy
				policyKey := types.NamespacedName{Namespace: crNamespace}

				if err := r.Get(ctx, policyKey, &policy); err != nil {
					if k8s_errors.IsNotFound(err) {
						log.Error(err, "ResourceAdjustmentPolicy not found in zxporter-system namespace")
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

						if r.isPodExcluded(&policy, pod.Namespace, pod.Name) {
							log.Info("Pod excluded", "podName", pod.Name, "namespace", pod.Namespace)
						} else {
							log.Info("Pod not excluded", "podName", pod.Name, "exclusion", policy.Spec.Exclusions.ExcludedPods)
						}

						re.Lock()
						_, exists := re.ContainerStates[key]
						if !exists && !r.isPodExcluded(&policy, pod.Namespace, pod.Name) {
							// Initialize state for new container
							state := NewContainerState(namespace, pod.Name, container.Name)
							re.ContainerStates[key] = state

							// Start the frequency monitor goroutine
							go re.monitorFrequency(r, state)

							// Start usage monitoring
							go re.runUsageMonitorLoop(ctx, r, state, &pod, &container)

							// // Start the container control loop
							// go re.runContainerLoop(ctx, r, state, &policy, &pod, &container)

						} else if exists && r.isPodExcluded(&policy, pod.Namespace, pod.Name) {
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
			close(state.UsageMonitorQuit)
			close(state.UpdateChan)

			delete(re.ContainerStates, key)
		}
	}
}

// var namespaceRegex = regexp.MustCompile(`^dz--([a-z0-9]{20})--(cluster-[a-z0-9]{12})--([a-z0-9]{15})$`)

func isNamespaceAllowed(namespace string) bool {
	regexPattern := os.Getenv("NAMESPACE_REGEX")
	if regexPattern == "" {
		// Default regex pattern
		regexPattern = `^dz--([a-z0-9]{20})--(cluster-[a-z0-9]{12})--([a-z0-9]{15})$`
	}
	namespaceRegex := regexp.MustCompile(regexPattern)
	return namespaceRegex.MatchString(namespace)
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

// func (re *Recommender) processContainer(ctx context.Context, r *ResourceAdjustmentPolicyReconciler, log logr.Logger, policy *v1.ResourceAdjustmentPolicy, pod *corev1.Pod, container *corev1.Container) {
// 	log.Info("Processing container", "podName", pod.Name, "containerName", container.Name)

// 	// Check for OOM events
// 	oomTime := re.checkForOOMEvents(*pod, container.Name)

// 	// Fetch labels from the Pod
// 	createdByUser := pod.Labels["meta.devzero.io/created-by-user"]
// 	organizationID := pod.Labels["meta.devzero.io/organization-id"]
// 	virtualClusterID := pod.Labels["meta.devzero.io/virtual-cluster-id"]
// 	workloadID := pod.Labels["meta.devzero.io/workload-id"]
// 	workloadName := pod.Labels["meta.devzero.io/workload-name"]

// 	// Create the recommendation request
// 	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
// 	defer cancel()

// 	req := connect.NewRequest(&gen.GetRecommendationRequest{
// 		ContainerId:                fmt.Sprintf("%s-%s", pod.Name, container.Name),
// 		ContainerName:              container.Name,
// 		PodName:                    pod.Name,
// 		NodeName:                   pod.Spec.NodeName,
// 		CpuRequest:                 container.Resources.Requests.Cpu().MilliValue(),
// 		MemoryRequest:              container.Resources.Requests.Memory().Value(),
// 		CpuLimit:                   container.Resources.Limits.Cpu().MilliValue(),
// 		MemoryLimit:                container.Resources.Limits.Memory().Value(),
// 		CpuRequestPercentile:       cpuConfig.RequestPercentile,
// 		CpuMarginFraction:          cpuConfig.MarginFraction,
// 		CpuTargetUtilization:       cpuConfig.TargetUtilization,
// 		MemoryRequestPercentile:    memoryConfig.RequestPercentile,
// 		MemoryMarginFraction:       memoryConfig.MarginFraction,
// 		MemoryTargetUtilization:    memoryConfig.TargetUtilization,
// 		CpuHistoryLengthSeconds:    86400,
// 		MemoryHistoryLengthSeconds: 86400,
// 		OomBumpRatio:               oomBumpRatio,
// 		OomProtection:              oomProtection,
// 		OomTime:                    oomTime,
// 		CreatedByUser:              createdByUser,
// 		OrganizationId:             organizationID,
// 		VirtualClusterId:           virtualClusterID,
// 		WorkloadId:                 workloadID,
// 		WorkloadName:               workloadName,
// 	})

// 	// Send the recommendation request
// 	_, err := pulseClient.recommendationClient.GetRecommendation(ctx, req)
// 	if err != nil {
// 		log.Error(err, "❌ Failed to send recommendation generation request")
// 	}
// }

// SetupWithManager sets up the controller with the Manager.
func (r *ResourceAdjustmentPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	quit = make(chan bool)

	// Run the recommender as a background goroutine
	recommender := NewRecommender()
	go recommender.runRecommender(r)

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.ResourceAdjustmentPolicy{}).
		Complete(r)
}
