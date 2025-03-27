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
	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	monitoringv1 "github.com/devzero-inc/zxporter/api/v1"
	metricsv1 "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/devzero-inc/zxporter/internal/collector"
	"github.com/devzero-inc/zxporter/internal/collector/provider"
	"github.com/devzero-inc/zxporter/internal/transport"
	"github.com/devzero-inc/zxporter/internal/util"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// CollectionPolicyReconciler reconciles a CollectionPolicy object
type CollectionPolicyReconciler struct {
	client.Client
	Scheme            *runtime.Scheme
	Log               logr.Logger
	K8sClient         *kubernetes.Clientset
	DynamicClient     *dynamic.DynamicClient
	DiscoveryClient   *discovery.DiscoveryClient
	ApiExtensions     *apiextensionsclientset.Clientset
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
	TargetNamespaces               []string
	ExcludedNamespaces             []string
	ExcludedPods                   []collector.ExcludedPod
	ExcludedDeployments            []collector.ExcludedDeployment
	ExcludedStatefulSets           []collector.ExcludedStatefulSet
	ExcludedDaemonSets             []collector.ExcludedDaemonSet
	ExcludedServices               []collector.ExcludedService
	ExcludedConfigMaps             []collector.ExcludedConfigMap
	ExcludedPVCs                   []collector.ExcludedPVC
	ExcludedEvents                 []collector.ExcludedEvent
	ExcludedJobs                   []collector.ExcludedJob
	ExcludedCronJobs               []collector.ExcludedCronJob
	ExcludedReplicationControllers []collector.ExcludedReplicationController
	ExcludedIngresses              []collector.ExcludedIngress
	ExcludedNetworkPolicies        []collector.ExcludedNetworkPolicy
	ExcludedEndpoints              []collector.ExcludedEndpoint
	ExcludedServiceAccounts        []collector.ExcludedServiceAccount
	ExcludedLimitRanges            []collector.ExcludedLimitRange
	ExcludedResourceQuotas         []collector.ExcludedResourceQuota
	ExcludedHPAs                   []collector.ExcludedHPA
	ExcludedVPAs                   []collector.ExcludedVPA
	ExcludedRoles                  []collector.ExcludedRole
	ExcludedRoleBindings           []collector.ExcludedRoleBinding
	ExcludedClusterRoles           []string
	ExcludedClusterRoleBindings    []string
	ExcludedPDBs                   []collector.ExcludedPDB
	ExcludedPSPs                   []string
	ExcludedCRDs                   []string
	ExcludedCustomResources        []collector.ExcludedCustomResource
	ExcludedSecrets                []collector.ExcludedSecret
	ExcludedStorageClasses         []string
	ExcludedPVs                    []string
	ExcludedIngressClasses         []string
	ExcludedNodes                  []string
	ExcludedCRDGroups              []string
	WatchedCRDs                    []string

	PulseURL            string
	UpdateInterval      time.Duration
	NodeMetricsInterval time.Duration
	BufferSize          int
	MaskSecretData      bool
}

//+kubebuilder:rbac:groups=monitoring.devzero.io,resources=collectionpolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=monitoring.devzero.io,resources=collectionpolicies/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=monitoring.devzero.io,resources=collectionpolicies/finalizers,verbs=update

// Core API Group resources
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=pods/status,verbs=get
//+kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=nodes/status,verbs=get
//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=events,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=limitranges,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=resourcequotas,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=replicationcontrollers,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=persistentvolumes,verbs=get;list;watch

// Apps API Group resources
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
//+kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch
//+kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch
//+kubebuilder:rbac:groups=apps,resources=replicasets,verbs=get;list;watch

// Batch API Group resources
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch
//+kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch

// Metrics API Group resources
//+kubebuilder:rbac:groups=metrics.k8s.io,resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups=metrics.k8s.io,resources=nodes,verbs=get;list;watch

// Networking API Group resources
//+kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch
//+kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch
//+kubebuilder:rbac:groups=networking.k8s.io,resources=ingressclasses,verbs=get;list;watch

// RBAC API Group resources
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles,verbs=get;list;watch
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=get;list;watch
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=endpoints,verbs=get;list;watch

// Autoscaling API Group resources
//+kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch
//+kubebuilder:rbac:groups=autoscaling.k8s.io,resources=verticalpodautoscalers,verbs=get;list;watch

// Policy API Group resources
//+kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch

// Storage API Group resources
//+kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch

// API Extensions resources
//+kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *CollectionPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling CollectionPolicy", "request", req)

	// TODO: call pulse to get the update the configs, dont collect config from ENV in reconcile
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

// initializeCollectors coordinates the setup and startup of all collectors
func (r *CollectionPolicyReconciler) initializeCollectors(ctx context.Context, config *PolicyConfig) (ctrl.Result, error) {
	logger := r.Log.WithName("initialize")
	logger.Info("Initializing collectors", "config", fmt.Sprintf("%+v", config))

	// Setup collection manager and basic services
	if err := r.setupCollectionManager(ctx, config, logger); err != nil {
		return ctrl.Result{}, err
	}

	// First register and start only the cluster collector
	clusterCollector, err := r.setupClusterCollector(ctx, logger, config)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Wait for cluster data to be sent to Pulse
	registrationResult := r.waitForClusterRegistration(ctx, logger)
	if registrationResult != nil {
		// Clean up and return the error result
		r.cleanupOnFailure(logger)
		return *registrationResult, fmt.Errorf("failed to register cluster with Pulse")
	}

	// Register and start all other collectors
	if err := r.setupAllCollectors(ctx, logger, config, clusterCollector); err != nil {
		return ctrl.Result{}, err
	}

	// Start processing collected resources
	go r.processCollectedResources(ctx)

	// Update current config
	r.CurrentConfig = config
	r.IsRunning = true

	logger.Info("Successfully started all collectors")
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// setupCollectionManager initializes the collection manager and sender
func (r *CollectionPolicyReconciler) setupCollectionManager(ctx context.Context, config *PolicyConfig, logger logr.Logger) error {
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

	// Create pulse client and sender
	var pulseClient transport.PulseClient
	if config.PulseURL != "" {
		pulseClient = transport.NewPulseClient(config.PulseURL, logger)
		logger.Info("Created Pulse client with configured URL", "url", config.PulseURL)
	} else {
		pulseClient = transport.NewSimplePulseClient(logger)
		logger.Info("Created simple (logging) Pulse client because no URL was configured")
	}

	// Create buffered sender with default options
	senderOptions := transport.DefaultBufferedSenderOptions()
	if config.BufferSize > 0 {
		senderOptions.MaxBufferSize = config.BufferSize
	}

	r.Sender = transport.NewBufferedSender(pulseClient, logger, senderOptions)

	// Start the sender
	if err := r.Sender.Start(ctx); err != nil {
		logger.Error(err, "Failed to start sender")
		return err
	}

	return nil
}

// setupClusterCollector creates and starts just the cluster collector
func (r *CollectionPolicyReconciler) setupClusterCollector(ctx context.Context, logger logr.Logger, config *PolicyConfig) (collector.ResourceCollector, error) {
	logger.Info("First registering and starting cluster collector")

	// Create metrics client
	clientConfig := ctrl.GetConfigOrDie()
	metricsClient, err := metricsv1.NewForConfig(clientConfig)
	if err != nil {
		logger.Error(err, "Failed to create metrics client")
	}

	// Create and register cluster collector with provider detection
	providerDetector := provider.NewDetector(logger, r.K8sClient)
	detectedProvider, err := providerDetector.DetectProvider(ctx)
	if err != nil {
		logger.Error(err, "Failed to detect cloud provider, collector will have limited functionality")
	}

	// Create the cluster collector
	clusterCollector := collector.NewClusterCollector(
		r.K8sClient,
		metricsClient,
		detectedProvider,
		30*time.Second, // Regular interval is 30 minutes
		logger,
	)

	// Register only the cluster collector initially
	if err := r.CollectionManager.RegisterCollector(clusterCollector); err != nil {
		logger.Error(err, "Failed to register cluster collector")
		return nil, err
	}

	// Start the collection manager with just the cluster collector
	if err := r.CollectionManager.StartAll(ctx); err != nil {
		logger.Error(err, "Failed to start collection manager")
		return nil, err
	}

	return clusterCollector, nil
}

// waitForClusterRegistration waits for cluster data to be sent to Pulse
func (r *CollectionPolicyReconciler) waitForClusterRegistration(ctx context.Context, logger logr.Logger) *ctrl.Result {
	clusterRegisteredCh := make(chan struct{})
	clusterFailedCh := make(chan struct{})

	// TODO: make it as config
	clusterTimeoutCh := time.After(2 * time.Minute) // Timeout after 2 minutes

	go r.monitorClusterRegistration(ctx, logger, clusterRegisteredCh, clusterFailedCh)

	// Wait for registration to complete, fail, or timeout
	select {
	case <-clusterRegisteredCh:
		logger.Info("Cluster registration completed, proceeding with other collectors")
		return nil
	case <-clusterFailedCh:
		logger.Error(nil, "Cluster data registration failed after maximum attempts, operator entering error state")
		return &ctrl.Result{Requeue: true, RequeueAfter: 1 * time.Minute}
	case <-clusterTimeoutCh:
		logger.Info("Timeout waiting for cluster data")
		return &ctrl.Result{Requeue: true, RequeueAfter: 1 * time.Minute}
	case <-ctx.Done():
		logger.Info("Context cancelled while waiting for cluster data")
		return &ctrl.Result{Requeue: true, RequeueAfter: 1 * time.Minute}
	}
}

// monitorClusterRegistration watches for cluster data and sends it to Pulse
func (r *CollectionPolicyReconciler) monitorClusterRegistration(
	ctx context.Context,
	logger logr.Logger,
	clusterRegisteredCh chan<- struct{},
	clusterFailedCh chan<- struct{},
) {
	logger.Info("Waiting for cluster data to be sent to Pulse")
	resourceChan := r.CollectionManager.GetCombinedChannel()

	attemptCount := 0
	maxAttempts := 5 // Max attempts before failing

	for {
		select {
		case resource, ok := <-resourceChan:
			if !ok {
				// Channel closed unexpectedly
				logger.Error(nil, "Resource channel closed while waiting for cluster data")
				close(clusterFailedCh)
				return
			}

			// Only process cluster resources at this stage
			if resource.ResourceType == collector.Cluster {
				attemptCount++
				logger.Info("Received cluster data, sending to Pulse",
					"key", resource.Key,
					"attempt", attemptCount)

				// Send the cluster data to Pulse
				err := r.Sender.Send(ctx, resource)
				if err != nil {
					logger.Error(err, "Failed to send cluster data to Pulse",
						"attempt", attemptCount)

					if attemptCount >= maxAttempts {
						logger.Error(nil, "Failed to send cluster data after maximum attempts",
							"maxAttempts", maxAttempts)
						close(clusterFailedCh)
						return
					}
					// Continue waiting for next attempt
				} else {
					logger.Info("Successfully sent cluster data to Pulse")
					// Signal that cluster registration is complete
					close(clusterRegisteredCh)
					return
				}
			}

		case <-ctx.Done():
			logger.Info("Context cancelled while monitoring cluster registration")
			close(clusterFailedCh)
			return
		}
	}
}

// cleanupOnFailure stops all components and resets state on failure
func (r *CollectionPolicyReconciler) cleanupOnFailure(logger logr.Logger) {
	// Clean up resources
	if r.CollectionManager != nil {
		if err := r.CollectionManager.StopAll(); err != nil {
			logger.Error(err, "Error stopping collection manager during failure")
		}
	}
	if r.Sender != nil {
		if err := r.Sender.Stop(); err != nil {
			logger.Error(err, "Error stopping sender during failure")
		}
	}

	// Reset state
	r.IsRunning = false
	r.CollectionManager = nil
	r.CurrentConfig = nil

	logger.Info("Cleaned up resources after failure")
}

// setupAllCollectors registers and starts all collectors
func (r *CollectionPolicyReconciler) setupAllCollectors(
	ctx context.Context,
	logger logr.Logger,
	config *PolicyConfig,
	clusterCollector collector.ResourceCollector,
) error {
	logger.Info("Now registering and starting all collectors")

	// Stop the collection manager to reconfigure with all collectors
	if err := r.CollectionManager.StopAll(); err != nil {
		logger.Error(err, "Error stopping collection manager")
	}

	// Create metrics client if needed for resource collectors
	clientConfig := ctrl.GetConfigOrDie()
	metricsClient, err := metricsv1.NewForConfig(clientConfig)
	if err != nil {
		logger.Error(err, "Failed to create metrics client, some collectors may be limited")
	}

	// Register all other collectors
	if err := r.registerResourceCollectors(logger, config, metricsClient); err != nil {
		return err
	}

	// Start the collection manager with all collectors
	if err := r.CollectionManager.StartAll(ctx); err != nil {
		logger.Error(err, "Failed to start collection manager with all collectors")
		return err
	}

	return nil
}

// registerResourceCollectors creates and registers all resource collectors
func (r *CollectionPolicyReconciler) registerResourceCollectors(
	logger logr.Logger,
	config *PolicyConfig,
	metricsClient *metricsv1.Clientset,
) error {

	// List of collectors to register
	collectors := []struct {
		collector collector.ResourceCollector
		name      collector.ResourceType
	}{
		{
			collector: collector.NewEndpointCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedEndpoints,
				logger,
			),
			name: collector.Endpoints,
		},
		{
			collector: collector.NewServiceAccountCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedServiceAccounts,
				logger,
			),
			name: collector.ServiceAccount,
		},
		{
			collector: collector.NewLimitRangeCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedLimitRanges,
				logger,
			),
			name: collector.LimitRange,
		},
		{
			collector: collector.NewResourceQuotaCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedResourceQuotas,
				logger,
			),
			name: collector.ResourceQuota,
		},
		{
			collector: collector.NewPersistentVolumeCollector(
				r.K8sClient,
				config.ExcludedPVs,
				logger,
			),
			name: collector.PersistentVolume,
		},
		{
			collector: collector.NewPodCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedPods,
				logger,
			),
			name: collector.Pod,
		},
		{
			collector: collector.NewDeploymentCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedDeployments,
				logger,
			),
			name: collector.Deployment,
		},
		{
			collector: collector.NewStatefulSetCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedStatefulSets,
				logger,
			),
			name: collector.StatefulSet,
		},
		{
			collector: collector.NewDaemonSetCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedDaemonSets,
				logger,
			),
			name: collector.DaemonSet,
		},
		{
			collector: collector.NewNamespaceCollector(
				r.K8sClient,
				config.ExcludedNamespaces,
				logger,
			),
			name: collector.Namespace,
		},
		{
			collector: collector.NewConfigMapCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedConfigMaps,
				logger,
			),
			name: collector.ConfigMap,
		},
		{
			collector: collector.NewReplicationControllerCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedReplicationControllers,
				logger,
			),
			name: collector.ReplicationController,
		},
		{
			collector: collector.NewIngressCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedIngresses,
				logger,
			),
			name: collector.Ingress,
		},
		{
			collector: collector.NewIngressClassCollector(
				r.K8sClient,
				config.ExcludedIngressClasses,
				logger,
			),
			name: collector.IngressClass,
		},
		{
			collector: collector.NewPersistentVolumeClaimCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedPVCs,
				logger,
			),
			name: collector.PersistentVolumeClaim,
		},
		{
			collector: collector.NewEventCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedEvents,
				10,
				10*time.Minute,
				logger,
			),
			name: collector.Event,
		},
		{
			collector: collector.NewServiceCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedServices,
				logger,
			),
			name: collector.Service,
		},
		{
			collector: collector.NewJobCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedJobs,
				logger,
			),
			name: collector.Job,
		},
		{
			collector: collector.NewNetworkPolicyCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedNetworkPolicies,
				logger,
			),
			name: collector.NetworkPolicy,
		},
		{
			collector: collector.NewCronJobCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedCronJobs,
				logger,
			),
			name: collector.CronJob,
		},
		{
			collector: collector.NewContainerResourceCollector(
				r.K8sClient,
				metricsClient,
				config.TargetNamespaces,
				config.ExcludedPods,
				config.UpdateInterval,
				logger,
			),
			name: collector.ContainerResource,
		},
		{
			collector: collector.NewNodeCollector(
				r.K8sClient,
				metricsClient,
				config.ExcludedNodes,
				config.UpdateInterval,
				logger,
			),
			name: collector.Node,
		},
		{
			collector: collector.NewNodeMetricsCollector(
				r.K8sClient,
				config.ExcludedNodes,
				config.NodeMetricsInterval,
				logger,
			),
			name: collector.NodeResource,
		},
		{
			collector: collector.NewRoleCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedRoles,
				logger,
			),
			name: collector.Role,
		},
		{
			collector: collector.NewRoleBindingCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedRoleBindings,
				logger,
			),
			name: collector.RoleBinding,
		},
		{
			collector: collector.NewClusterRoleCollector(
				r.K8sClient,
				config.ExcludedClusterRoles,
				logger,
			),
			name: collector.ClusterRole,
		},
		{
			collector: collector.NewClusterRoleBindingCollector(
				r.K8sClient,
				config.ExcludedClusterRoleBindings,
				logger,
			),
			name: collector.ClusterRoleBinding,
		},
		{
			collector: collector.NewHorizontalPodAutoscalerCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedHPAs,
				logger,
			),
			name: collector.HorizontalPodAutoscaler,
		},
		{
			collector: collector.NewVerticalPodAutoscalerCollector(
				r.DynamicClient,
				config.TargetNamespaces,
				config.ExcludedVPAs,
				logger,
			),
			name: collector.VerticalPodAutoscaler,
		},
		{
			collector: collector.NewPodDisruptionBudgetCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedPDBs,
				logger,
			),
			name: collector.PodDisruptionBudget,
		},
		{
			collector: collector.NewCRDCollector(
				r.ApiExtensions,
				config.ExcludedCRDs,
				logger,
			),
			name: collector.CustomResourceDefinition,
		},
		{
			collector: collector.NewCustomResourceCollector(
				r.ApiExtensions,
				r.DiscoveryClient,
				r.DynamicClient,
				collector.CustomResourceCollectorConfig{},
				logger,
			),
			name: collector.CustomResource,
		},
		{
			collector: collector.NewSecretCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedSecrets,
				config.MaskSecretData,
				logger,
			),
			name: collector.Secret,
		},
		{
			collector: collector.NewStorageClassCollector(
				r.K8sClient,
				config.ExcludedStorageClasses,
				logger,
			),
			name: collector.StorageClass,
		},
	}

	// Register all collectors
	for _, c := range collectors {
		if c.collector.IsAvailable(context.Background()) {
			logger.Info("Registering collector", "name", c.name.String())
			if err := r.CollectionManager.RegisterCollector(c.collector); err != nil {
				logger.Error(err, "Failed to register collector", "collector", c.name)
			} else {
				logger.Info("Registered collector", "collector", c.name)
			}
		}
	}

	return nil
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
			if err := r.Sender.Send(ctx, resource); err != nil {
				logger.Error(err, "Failed to send resource to Pulse",
					"resourceType", resource.ResourceType,
					"eventType", resource.EventType,
					"key", resource.Key)
			} else {
				logger.Info("Sent resource to Pulse",
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
