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
	"reflect"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	monitoringv1 "github.com/devzero-inc/zxporter/api/v1"
	metricsv1 "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/devzero-inc/zxporter/internal/collector"
	"github.com/devzero-inc/zxporter/internal/transport"
	"github.com/devzero-inc/zxporter/internal/util"
	"k8s.io/client-go/kubernetes"
)

// CollectionPolicyReconciler reconciles a CollectionPolicy object
type CollectionPolicyReconciler struct {
	client.Client
	Scheme            *runtime.Scheme
	Log               logr.Logger
	K8sClient         *kubernetes.Clientset
	CollectionManager *collector.CollectionManager
	Sender            transport.Sender
	IsRunning         bool
	CurrentPolicyHash string
	CurrentConfig     *PolicyConfig
	LastEnvCheckTime  time.Time
	EnvCheckInterval  time.Duration
	EnvConfig         *util.EnvPolicyConfig
	RestartInProgress bool
}

// PolicyConfig holds the current configuration
type PolicyConfig struct {
	TargetNamespaces    []string
	ExcludedNamespaces  []string
	ExcludedPods        []collector.ExcludedPod
	ExcludedNodes       []string
	PulseURL            string
	UpdateInterval      time.Duration
	NodeMetricsInterval time.Duration
	BufferSize          int
}

//+kubebuilder:rbac:groups=monitoring.devzero.io,resources=collectionpolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=monitoring.devzero.io,resources=collectionpolicies/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=monitoring.devzero.io,resources=collectionpolicies/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=pods/status,verbs=get
//+kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=nodes/status,verbs=get

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *CollectionPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling CollectionPolicy", "request", req)

	// Check if we should reload env config (every 5 minutes)
	if time.Since(r.LastEnvCheckTime) > r.EnvCheckInterval {
		r.EnvConfig = util.LoadEnvPolicyConfig(logger)
		r.LastEnvCheckTime = time.Now()
		logger.Info("Reloaded environment configuration")
	}

	// Fetch the CollectionPolicy instance
	var policy monitoringv1.CollectionPolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Create a new config object from the policy and environment
	newConfig, configChanged := r.createNewConfig(&policy, logger)

	// Check if we need to restart collectors due to config change
	if configChanged && r.IsRunning {
		logger.Info("Configuration changed, restarting collectors")
		return r.restartCollectors(ctx, newConfig)
	}

	// Initialize collection system if not already running
	if !r.IsRunning {
		logger.Info("Collection system not running, initializing")
		return r.initializeCollectors(ctx, newConfig)
	}

	// No changes needed
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// createNewConfig creates a new config by merging policy and environment variables
func (r *CollectionPolicyReconciler) createNewConfig(policy *monitoringv1.CollectionPolicy, logger logr.Logger) (*PolicyConfig, bool) {
	// Convert excluded pods from policy format
	var excludedPods []collector.ExcludedPod
	for _, pod := range policy.Spec.Exclusions.ExcludedPods {
		excludedPods = append(excludedPods, collector.ExcludedPod{
			Namespace: pod.Namespace,
			Name:      pod.PodName,
		})
	}

	// Get policy values
	targetNamespaces := policy.Spec.TargetSelector.Namespaces
	excludedNamespaces := policy.Spec.Exclusions.ExcludedNamespaces
	excludedNodes := policy.Spec.Exclusions.ExcludedNodes
	pulseURL := policy.Spec.Policies.PulseURL
	frequencyStr := policy.Spec.Policies.Frequency
	bufferSize := policy.Spec.Policies.BufferSize

	// Merge with environment config
	targetNamespaces, excludedNamespaces, excludedNodes, pulseURL, frequency, bufferSize :=
		r.EnvConfig.MergeWithCRPolicy(
			targetNamespaces,
			excludedNamespaces,
			excludedNodes,
			pulseURL,
			frequencyStr,
			bufferSize,
		)

	// Use default if frequency is not set
	if frequency <= 0 {
		frequency = 10 * time.Second
	}

	// Set node metrics interval (6x regular interval but minimum 60s)
	nodeMetricsInterval := frequency * 6
	if nodeMetricsInterval < 60*time.Second {
		nodeMetricsInterval = 60 * time.Second
	}

	// Create the new config
	newConfig := &PolicyConfig{
		TargetNamespaces:    targetNamespaces,
		ExcludedNamespaces:  excludedNamespaces,
		ExcludedPods:        excludedPods,
		ExcludedNodes:       excludedNodes,
		PulseURL:            pulseURL,
		UpdateInterval:      frequency,
		NodeMetricsInterval: nodeMetricsInterval,
		BufferSize:          bufferSize,
	}

	// Check if config has changed
	configChanged := false
	if r.CurrentConfig == nil {
		configChanged = true
	} else {
		configChanged = !reflect.DeepEqual(r.CurrentConfig, newConfig)

		if configChanged {
			logger.Info("Configuration changed",
				"old", fmt.Sprintf("%+v", r.CurrentConfig),
				"new", fmt.Sprintf("%+v", newConfig))
		}
	}

	return newConfig, configChanged
}

// restartCollectors stops existing collectors and starts new ones with updated config
func (r *CollectionPolicyReconciler) restartCollectors(ctx context.Context, config *PolicyConfig) (ctrl.Result, error) {
	if r.RestartInProgress {
		// Avoid concurrent restarts
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	r.RestartInProgress = true
	logger := r.Log.WithName("restart")
	logger.Info("Restarting collectors with new configuration")

	// Stop existing collectors
	if r.CollectionManager != nil {
		logger.Info("Stopping existing collectors")
		if err := r.CollectionManager.StopAll(); err != nil {
			logger.Error(err, "Error stopping collection manager")
		}
	}

	// Reset state
	r.IsRunning = false
	r.CollectionManager = nil
	r.CurrentConfig = nil

	// Initialize with new config
	result, err := r.initializeCollectors(ctx, config)
	r.RestartInProgress = false

	return result, err
}

// initializeCollectors sets up and starts the collectors based on policy
func (r *CollectionPolicyReconciler) initializeCollectors(ctx context.Context, config *PolicyConfig) (ctrl.Result, error) {
	logger := r.Log.WithName("initialize")
	logger.Info("Initializing collectors", "config", fmt.Sprintf("%+v", config))

	// Create collection config
	collectionConfig := &collector.CollectionConfig{
		Namespaces:         config.TargetNamespaces,
		ExcludedNamespaces: config.ExcludedNamespaces,
		ExcludedPods:       config.ExcludedPods,
		BufferSize:         config.BufferSize,
	}

	// Create collection manager
	r.CollectionManager = collector.NewCollectionManager(
		collectionConfig,
		r.K8sClient,
		logger.WithName("collection-manager"),
	)

	// Create metrics client for container resource usage
	clientConfig := ctrl.GetConfigOrDie()
	metricsClient, err := metricsv1.NewForConfig(clientConfig)
	if err != nil {
		logger.Error(err, "Failed to create metrics client, container resource collection will be disabled")
	}

	// Create and register pod collector
	podCollector := collector.NewPodCollector(
		r.K8sClient,
		config.TargetNamespaces,
		config.ExcludedPods,
		logger,
	)

	if err := r.CollectionManager.RegisterCollector(podCollector); err != nil {
		logger.Error(err, "Failed to register pod collector")
		return ctrl.Result{}, err
	}

	// Create and register container resource collector if metrics client is available
	containerResourceCollector := collector.NewContainerResourceCollector(
		r.K8sClient,
		metricsClient,
		config.TargetNamespaces,
		config.ExcludedPods,
		config.UpdateInterval,
		logger,
	)

	if err := r.CollectionManager.RegisterCollector(containerResourceCollector); err != nil {
		logger.Error(err, "Failed to register container resource collector")
		return ctrl.Result{}, err
	}

	// Create and register node collector
	nodeCollector := collector.NewNodeCollector(
		r.K8sClient,
		metricsClient,
		config.ExcludedNodes,
		config.UpdateInterval,
		logger,
	)

	if err := r.CollectionManager.RegisterCollector(nodeCollector); err != nil {
		logger.Error(err, "Failed to register node collector")
		return ctrl.Result{}, err
	}

	// Create and register node metrics collector
	nodeMetricsCollector := collector.NewNodeMetricsCollector(
		r.K8sClient,
		config.ExcludedNodes,
		config.NodeMetricsInterval,
		logger,
	)

	if err := r.CollectionManager.RegisterCollector(nodeMetricsCollector); err != nil {
		logger.Error(err, "Failed to register node metrics collector")
		return ctrl.Result{}, err
	}

	// Create Pulse client with configured URL if provided
	var pulseClient transport.PulseClient
	if config.PulseURL != "" {
		// 	pulseClient = transport.NewPulseClient(config.PulseURL, logger)
		// } else {
		// Use simple client for testing if no URL provided
		pulseClient = transport.NewSimplePulseClient(logger)
	}

	// Create and configure sender
	r.Sender = transport.NewDirectPulseSender(pulseClient, logger)

	// Start the collection manager
	if err := r.CollectionManager.StartAll(ctx); err != nil {
		logger.Error(err, "Failed to start collection manager")
		return ctrl.Result{}, err
	}

	// Start processing collected resources
	go r.processCollectedResources(ctx)

	// Update current config
	r.CurrentConfig = config
	r.IsRunning = true

	logger.Info("Successfully started collectors with new configuration")

	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// processCollectedResources reads from collection channel and forwards to sender
func (r *CollectionPolicyReconciler) processCollectedResources(ctx context.Context) {
	logger := r.Log.WithName("processor")
	logger.Info("Starting to process collected resources")

	resourceChan := r.CollectionManager.GetCombinedChannel()

	for {
		select {
		case <-ctx.Done():
			logger.Info("Context done, stopping processor")
			return
		case resource, ok := <-resourceChan:
			if !ok {
				logger.Info("Resource channel closed, stopping processor")
				return
			}

			// Send the raw resource directly to Pulse
			if err := r.Sender.Send(ctx, resource.ResourceType, resource.Object); err != nil {
				logger.Error(err, "Failed to send resource to Pulse",
					"resourceType", resource.ResourceType,
					"eventType", resource.EventType,
					"key", resource.Key)
			} else {
				logger.V(4).Info("Sent resource to Pulse",
					"resourceType", resource.ResourceType,
					"eventType", resource.EventType,
					"key", resource.Key)
			}
		}
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *CollectionPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Set up basic components
	r.Log = util.NewLogger("controller")
	r.EnvCheckInterval = 5 * time.Minute
	r.LastEnvCheckTime = time.Now()
	r.EnvConfig = util.LoadEnvPolicyConfig(r.Log)
	r.RestartInProgress = false

	// Create a Kubernetes clientset
	config := mgr.GetConfig()
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create kubernetes clientset: %w", err)
	}
	r.K8sClient = clientset

	return ctrl.NewControllerManagedBy(mgr).
		For(&monitoringv1.CollectionPolicy{}).
		Complete(r)
}
