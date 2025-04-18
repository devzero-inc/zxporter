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
	ExcludedReplicaSet             []collector.ExcludedReplicaSet
	ExcludedStorageClasses         []string
	ExcludedPVs                    []string
	ExcludedIngressClasses         []string
	ExcludedNodes                  []string
	ExcludedCRDGroups              []string
	WatchedCRDs                    []string
	ExcludedCSINodes               []string
	ExcludedDatadogReplicaSets     []collector.ExcludedDatadogExtendedDaemonSetReplicaSet
	ExcludedArgoRollouts           []collector.ExcludedArgoRollout

	DisabledCollectors []string

	DakrURL                 string
	ClusterToken            string
	PrometheusURL           string
	DisableNetworkIOMetrics bool
	UpdateInterval          time.Duration
	NodeMetricsInterval     time.Duration
	BufferSize              int
	MaskSecretData          bool
}

//+kubebuilder:rbac:groups=devzero.io,resources=collectionpolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=devzero.io,resources=collectionpolicies/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=devzero.io,resources=collectionpolicies/finalizers,verbs=update

// Core API Group resources
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=pods/status,verbs=get
//+kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=nodes/status,verbs=get
//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=events,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=limitranges,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=resourcequotas,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=replicationcontrollers,verbs=get;list;watch
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
//+kubebuilder:rbac:groups=storage.k8s.io,resources=csinodes,verbs=get;list;watch

// Karpenter resources
//+kubebuilder:rbac:groups=karpenter.sh,resources=provisioners,verbs=get;list;watch
//+kubebuilder:rbac:groups=karpenter.sh,resources=machines,verbs=get;list;watch
//+kubebuilder:rbac:groups=karpenter.sh,resources=nodepools,verbs=get;list;watch
//+kubebuilder:rbac:groups=karpenter.sh,resources=nodeclaims,verbs=get;list;watch
//+kubebuilder:rbac:groups=karpenter.k8s.aws,resources=awsnodetemplates,verbs=get;list;watch
//+kubebuilder:rbac:groups=karpenter.k8s.aws,resources=ec2nodeclasses,verbs=get;list;watch

// DataDog resources
//+kubebuilder:rbac:groups=datadoghq.com,resources=extendeddaemonsetreplicasets,verbs=get;list;watch

// Argo Rollouts resources
//+kubebuilder:rbac:groups=argoproj.io,resources=rollouts,verbs=get;list;watch

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
			Name:      pod.Name,
		})
	}

	// Get policy values
	targetNamespaces := policy.Spec.TargetSelector.Namespaces
	excludedNamespaces := policy.Spec.Exclusions.ExcludedNamespaces
	excludedNodes := policy.Spec.Exclusions.ExcludedNodes
	excludedCSINodes := policy.Spec.Exclusions.ExcludedNodes
	dakrURL := policy.Spec.Policies.DakrURL
	clusterToken := policy.Spec.Policies.ClusterToken
	prometheusURL := policy.Spec.Policies.PrometheusURL
	disableNetworkIOMetrics := policy.Spec.Policies.DisableNetworkIOMetrics
	frequencyStr := policy.Spec.Policies.Frequency
	bufferSize := policy.Spec.Policies.BufferSize
	disabledCollectors := policy.Spec.Policies.DisabledCollectors
	var frequency time.Duration

	// Merge with environment config
	targetNamespaces, excludedNamespaces, excludedNodes, dakrURL, frequency, bufferSize, clusterToken =
		r.EnvConfig.MergeWithCRPolicy(
			targetNamespaces,
			excludedNamespaces,
			excludedNodes,
			dakrURL,
			frequencyStr,
			bufferSize,
			clusterToken,
		)

	if frequencyStr != "" {
		if freq, err := time.ParseDuration(frequencyStr); err == nil {
			frequency = freq
		} else {
			logger.Error(err, "Error parsing frequency string")
		}

	}

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
		TargetNamespaces:        targetNamespaces,
		ExcludedNamespaces:      excludedNamespaces,
		ExcludedPods:            excludedPods,
		ExcludedNodes:           excludedNodes,
		ExcludedCSINodes:        excludedCSINodes,
		DakrURL:                 dakrURL,
		ClusterToken:            clusterToken,
		PrometheusURL:           prometheusURL,
		DisableNetworkIOMetrics: disableNetworkIOMetrics,
		UpdateInterval:          frequency,
		NodeMetricsInterval:     nodeMetricsInterval,
		BufferSize:              bufferSize,
		DisabledCollectors:      disabledCollectors,
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

// identifyAffectedCollectors determines which collectors are affected by configuration changes
func (r *CollectionPolicyReconciler) identifyAffectedCollectors(oldConfig, newConfig *PolicyConfig) map[string]bool {
	affectedCollectors := make(map[string]bool)

	// Check namespaces and general settings that affect all collectors
	if !reflect.DeepEqual(oldConfig.TargetNamespaces, newConfig.TargetNamespaces) ||
		!reflect.DeepEqual(oldConfig.ExcludedNamespaces, newConfig.ExcludedNamespaces) ||
		oldConfig.BufferSize != newConfig.BufferSize {
		// These changes affect all collectors, return empty map to signal full restart
		return nil
	}

	// Check exclusion lists that affect specific collectors
	if !reflect.DeepEqual(oldConfig.ExcludedPods, newConfig.ExcludedPods) {
		affectedCollectors["pod"] = true
		affectedCollectors["container_resources"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedDeployments, newConfig.ExcludedDeployments) {
		affectedCollectors["deployment"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedStatefulSets, newConfig.ExcludedStatefulSets) {
		affectedCollectors["statefulset"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedDaemonSets, newConfig.ExcludedDaemonSets) {
		affectedCollectors["daemonset"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedServices, newConfig.ExcludedServices) {
		affectedCollectors["service"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedPVCs, newConfig.ExcludedPVCs) {
		affectedCollectors["persistentvolumeclaim"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedEvents, newConfig.ExcludedEvents) {
		affectedCollectors["event"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedJobs, newConfig.ExcludedJobs) {
		affectedCollectors["job"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedCronJobs, newConfig.ExcludedCronJobs) {
		affectedCollectors["cronjob"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedReplicationControllers, newConfig.ExcludedReplicationControllers) {
		affectedCollectors["replicationcontroller"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedIngresses, newConfig.ExcludedIngresses) {
		affectedCollectors["ingress"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedNetworkPolicies, newConfig.ExcludedNetworkPolicies) {
		affectedCollectors["networkpolicy"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedEndpoints, newConfig.ExcludedEndpoints) {
		affectedCollectors["endpoints"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedServiceAccounts, newConfig.ExcludedServiceAccounts) {
		affectedCollectors["serviceaccount"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedLimitRanges, newConfig.ExcludedLimitRanges) {
		affectedCollectors["limitrange"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedResourceQuotas, newConfig.ExcludedResourceQuotas) {
		affectedCollectors["resourcequota"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedHPAs, newConfig.ExcludedHPAs) {
		affectedCollectors["horizontalpodautoscaler"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedVPAs, newConfig.ExcludedVPAs) {
		affectedCollectors["verticalpodautoscaler"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedRoles, newConfig.ExcludedRoles) {
		affectedCollectors["role"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedRoleBindings, newConfig.ExcludedRoleBindings) {
		affectedCollectors["rolebinding"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedClusterRoles, newConfig.ExcludedClusterRoles) {
		affectedCollectors["clusterrole"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedClusterRoleBindings, newConfig.ExcludedClusterRoleBindings) {
		affectedCollectors["clusterrolebinding"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedPDBs, newConfig.ExcludedPDBs) {
		affectedCollectors["poddisruptionbudget"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedStorageClasses, newConfig.ExcludedStorageClasses) {
		affectedCollectors["storageclass"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedPVs, newConfig.ExcludedPVs) {
		affectedCollectors["persistentvolume"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedIngressClasses, newConfig.ExcludedIngressClasses) {
		affectedCollectors["ingressclass"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedNodes, newConfig.ExcludedNodes) {
		affectedCollectors["node"] = true
		affectedCollectors["noderesource"] = true
	}

	if oldConfig.NodeMetricsInterval != newConfig.NodeMetricsInterval {
		affectedCollectors["noderesource"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedCSINodes, newConfig.ExcludedCSINodes) {
		affectedCollectors["csinode"] = true
	}

	// Check if the special node collectors are affected by the update interval change
	if oldConfig.UpdateInterval != newConfig.UpdateInterval ||
		oldConfig.PrometheusURL != newConfig.PrometheusURL ||
		oldConfig.DisableNetworkIOMetrics != newConfig.DisableNetworkIOMetrics {
		affectedCollectors["node"] = true
		affectedCollectors["container_resources"] = true
	}

	return affectedCollectors
}

// restartCollectors stops existing collectors and starts new ones with updated config
func (r *CollectionPolicyReconciler) restartCollectors(ctx context.Context, newConfig *PolicyConfig) (ctrl.Result, error) {
	if r.RestartInProgress {
		// Avoid concurrent restarts
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	r.RestartInProgress = true
	logger := r.Log.WithName("restart")

	defer func() {
		r.RestartInProgress = false
	}()

	// Check if the DisabledCollectors list has changed
	if !reflect.DeepEqual(r.CurrentConfig.DisabledCollectors, newConfig.DisabledCollectors) {
		logger.Info("Disabled collectors configuration changed, updating affected collectors")
		if err := r.handleDisabledCollectorsChange(ctx, logger, r.CurrentConfig, newConfig); err != nil {
			logger.Error(err, "Error handling disabled collectors change")
			// Continue with other updates despite error
		}
	}

	// Identify which collectors need to be restarted
	affectedCollectors := r.identifyAffectedCollectors(r.CurrentConfig, newConfig)

	// If affectedCollectors is nil, it signals a full restart is needed
	if affectedCollectors == nil {
		logger.Info("Major configuration change detected, performing full restart")

		// Stop all existing collectors
		if r.CollectionManager != nil {
			logger.Info("Stopping all collectors")
			if err := r.CollectionManager.StopAll(); err != nil {
				logger.Error(err, "Error stopping collection manager")
			}
		}

		// Reset state
		r.IsRunning = false
		r.CollectionManager = nil
		r.CurrentConfig = nil

		// Initialize with new config
		return r.initializeCollectors(ctx, newConfig)
	}

	// If no collectors need to be restarted, just update the config
	if len(affectedCollectors) == 0 {
		logger.Info("No collectors affected by this configuration change")
		r.CurrentConfig = newConfig
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}

	logger.Info("Performing selective restart of affected collectors", "affectedCount", len(affectedCollectors))

	// Get collector registry from CollectionManager
	collectorTypes := r.CollectionManager.GetCollectorTypes()

	// Create a mapping of collector type string to ResourceType
	collectorTypeMap := make(map[string]collector.ResourceType)
	for _, resType := range collector.AllResourceTypes() {
		collectorTypeMap[resType.String()] = resType
	}

	// Stop only the affected collectors
	for _, collectorType := range collectorTypes {
		if affected := affectedCollectors[collectorType]; affected {
			logger.Info("Stopping collector due to configuration change", "type", collectorType)
			if err := r.CollectionManager.StopCollector(collectorType); err != nil {
				logger.Error(err, "Error stopping collector", "type", collectorType)
				// Continue with other collectors even if there's an error
			}
		}
	}

	// Update the current config
	r.CurrentConfig = newConfig

	// Now recreate and restart the affected collectors
	clientConfig := ctrl.GetConfigOrDie()
	metricsClient, err := metricsv1.NewForConfig(clientConfig)
	if err != nil {
		logger.Error(err, "Failed to create metrics client, some collectors may be limited")
	}

	// This is a simplified version of registerResourceCollectors but only for affected collectors
	for collectorType := range affectedCollectors {
		var replacedCollector collector.ResourceCollector

		// Recreate the collector with new configuration based on type
		switch collectorType {
		case "pod":
			replacedCollector = collector.NewPodCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedPods,
				logger,
			)
		case "deployment":
			replacedCollector = collector.NewDeploymentCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedDeployments,
				logger,
			)
		case "statefulset":
			replacedCollector = collector.NewStatefulSetCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedStatefulSets,
				logger,
			)
		case "daemonset":
			replacedCollector = collector.NewDaemonSetCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedDaemonSets,
				logger,
			)
		case "service":
			replacedCollector = collector.NewServiceCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedServices,
				logger,
			)
		case "container_resources":
			replacedCollector = collector.NewContainerResourceCollector(
				r.K8sClient,
				metricsClient,
				collector.ContainerResourceCollectorConfig{
					PrometheusURL:           newConfig.PrometheusURL,
					UpdateInterval:          newConfig.UpdateInterval,
					QueryTimeout:            10 * time.Second,
					DisableNetworkIOMetrics: newConfig.DisableNetworkIOMetrics,
				},
				newConfig.TargetNamespaces,
				newConfig.ExcludedPods,
				logger,
			)
		case "node":
			replacedCollector = collector.NewNodeCollector(
				r.K8sClient,
				metricsClient,
				collector.NodeCollectorConfig{
					PrometheusURL:           newConfig.PrometheusURL,
					UpdateInterval:          newConfig.UpdateInterval,
					QueryTimeout:            10 * time.Second,
					DisableNetworkIOMetrics: newConfig.DisableNetworkIOMetrics,
				},
				newConfig.ExcludedNodes,
				logger,
			)
		case "persistentvolumeclaim":
			replacedCollector = collector.NewPersistentVolumeClaimCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedPVCs,
				logger,
			)
		case "persistentvolume":
			replacedCollector = collector.NewPersistentVolumeCollector(
				r.K8sClient,
				newConfig.ExcludedPVs,
				logger,
			)
		case "event":
			replacedCollector = collector.NewEventCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedEvents,
				10,
				10*time.Minute,
				logger,
			)
		case "job":
			replacedCollector = collector.NewJobCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedJobs,
				logger,
			)
		case "cronjob":
			replacedCollector = collector.NewCronJobCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedCronJobs,
				logger,
			)
		case "replicationcontroller":
			replacedCollector = collector.NewReplicationControllerCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedReplicationControllers,
				logger,
			)
		case "ingress":
			replacedCollector = collector.NewIngressCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedIngresses,
				logger,
			)
		case "ingressclass":
			replacedCollector = collector.NewIngressClassCollector(
				r.K8sClient,
				newConfig.ExcludedIngressClasses,
				logger,
			)
		case "networkpolicy":
			replacedCollector = collector.NewNetworkPolicyCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedNetworkPolicies,
				logger,
			)
		case "endpoints":
			replacedCollector = collector.NewEndpointCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedEndpoints,
				logger,
			)
		case "serviceaccount":
			replacedCollector = collector.NewServiceAccountCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedServiceAccounts,
				logger,
			)
		case "limitrange":
			replacedCollector = collector.NewLimitRangeCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedLimitRanges,
				logger,
			)
		case "resourcequota":
			replacedCollector = collector.NewResourceQuotaCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedResourceQuotas,
				logger,
			)
		case "horizontalpodautoscaler":
			replacedCollector = collector.NewHorizontalPodAutoscalerCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedHPAs,
				logger,
			)
		case "verticalpodautoscaler":
			replacedCollector = collector.NewVerticalPodAutoscalerCollector(
				r.DynamicClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedVPAs,
				logger,
			)
		case "role":
			replacedCollector = collector.NewRoleCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedRoles,
				logger,
			)
		case "rolebinding":
			replacedCollector = collector.NewRoleBindingCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedRoleBindings,
				logger,
			)
		case "clusterrole":
			replacedCollector = collector.NewClusterRoleCollector(
				r.K8sClient,
				newConfig.ExcludedClusterRoles,
				logger,
			)
		case "clusterrolebinding":
			replacedCollector = collector.NewClusterRoleBindingCollector(
				r.K8sClient,
				newConfig.ExcludedClusterRoleBindings,
				logger,
			)
		case "poddisruptionbudget":
			replacedCollector = collector.NewPodDisruptionBudgetCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedPDBs,
				logger,
			)
		case "storageclass":
			replacedCollector = collector.NewStorageClassCollector(
				r.K8sClient,
				newConfig.ExcludedStorageClasses,
				logger,
			)
		case "csinode":
			replacedCollector = collector.NewCSINodeCollector(
				r.K8sClient,
				newConfig.ExcludedNodes,
				logger,
			)
		case "karpenter":
			replacedCollector = collector.NewKarpenterCollector(
				r.DynamicClient,
				logger,
			)
		case "datadog":
			replacedCollector = collector.NewDatadogCollector(
				r.DynamicClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedDatadogReplicaSets,
				logger,
			)
		case "argo_rollouts":
			replacedCollector = collector.NewArgoRolloutsCollector(
				r.DynamicClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedArgoRollouts,
				logger,
			)
		default:
			logger.Info("Collector type not handled in selective restart", "type", collectorType)
			continue
		}

		// Check if collector is available
		if !replacedCollector.IsAvailable(ctx) {
			logger.Info("Collector not available, skipping restart", "type", collectorType)
			continue
		}

		// Register the new collector
		if err := r.CollectionManager.RegisterCollector(replacedCollector); err != nil {
			logger.Error(err, "Failed to register collector", "type", collectorType)
			continue
		}

		// Start the collector
		if err := r.CollectionManager.StartCollector(ctx, collectorType); err != nil {
			logger.Error(err, "Failed to start collector", "type", collectorType)
		} else {
			logger.Info("Successfully restarted collector", "type", collectorType)
		}
	}

	logger.Info("Completed selective restart of collectors")
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
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

	// Wait for cluster data to be sent to Dakr
	registrationResult := r.waitForClusterRegistration(ctx, logger)
	if registrationResult != nil {
		// Clean up and return the error result
		r.cleanupOnFailure(logger)
		return *registrationResult, fmt.Errorf("failed to register cluster with Dakr")
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

	// Create dakr client and sender
	var dakrClient transport.DakrClient
	if config.DakrURL != "" {
		dakrClient = transport.NewDakrClient(config.DakrURL, config.ClusterToken, logger)
		logger.Info("Created Dakr client with configured URL", "url", config.DakrURL)
	} else {
		dakrClient = transport.NewSimpleDakrClient(logger)
		logger.Info("Created simple (logging) Dakr client because no URL was configured")
	}

	// Create buffered sender with default options
	senderOptions := transport.DefaultBufferedSenderOptions()
	if config.BufferSize > 0 {
		senderOptions.MaxBufferSize = config.BufferSize
	}

	r.Sender = transport.NewBufferedSender(dakrClient, logger, senderOptions)

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

// waitForClusterRegistration waits for cluster data to be sent to Dakr
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

// monitorClusterRegistration watches for cluster data and sends it to Dakr
func (r *CollectionPolicyReconciler) monitorClusterRegistration(
	ctx context.Context,
	logger logr.Logger,
	clusterRegisteredCh chan<- struct{},
	clusterFailedCh chan<- struct{},
) {
	logger.Info("Waiting for cluster data to be sent to Dakr")
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
				logger.Info("Received cluster data, sending to Dakr",
					"key", resource.Key,
					"attempt", attemptCount)

				// Send the cluster data to Dakr
				_, err := r.Sender.Send(ctx, resource)
				if err != nil {
					logger.Error(err, "Failed to send cluster data to Dakr",
						"attempt", attemptCount)

					if attemptCount >= maxAttempts {
						logger.Error(nil, "Failed to send cluster data after maximum attempts",
							"maxAttempts", maxAttempts)
						close(clusterFailedCh)
						return
					}
					// Continue waiting for next attempt
				} else {
					logger.Info("Successfully sent cluster data to Dakr")
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

	disabledCollectorsMap := make(map[string]bool)
	for _, collectorType := range config.DisabledCollectors {
		disabledCollectorsMap[collectorType] = true
	}

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
				collector.ContainerResourceCollectorConfig{
					PrometheusURL:           config.PrometheusURL,
					UpdateInterval:          config.UpdateInterval,
					QueryTimeout:            10 * time.Second,
					DisableNetworkIOMetrics: config.DisableNetworkIOMetrics,
				},
				config.TargetNamespaces,
				config.ExcludedPods,
				logger,
			),
			name: collector.ContainerResource,
		},
		{
			collector: collector.NewNodeCollector(
				r.K8sClient,
				metricsClient,
				collector.NodeCollectorConfig{
					PrometheusURL:           config.PrometheusURL,
					UpdateInterval:          config.UpdateInterval,
					QueryTimeout:            10 * time.Second,
					DisableNetworkIOMetrics: config.DisableNetworkIOMetrics,
				},
				config.ExcludedNodes,
				logger,
			),
			name: collector.Node,
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
			collector: collector.NewStorageClassCollector(
				r.K8sClient,
				config.ExcludedStorageClasses,
				logger,
			),
			name: collector.StorageClass,
		},
		{
			collector: collector.NewReplicaSetCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedReplicaSet,
				logger,
			),
			name: collector.StorageClass,
		},
		{
			collector: collector.NewCSINodeCollector(
				r.K8sClient,
				config.ExcludedCSINodes,
				logger,
			),
			name: collector.CSINode,
		},
		{
			collector: collector.NewKarpenterCollector(
				r.DynamicClient,
				logger,
			),
			name: collector.Karpenter,
		},
		{
			collector: collector.NewDatadogCollector(
				r.DynamicClient,
				config.TargetNamespaces,
				config.ExcludedDatadogReplicaSets,
				logger,
			),
			name: collector.Datadog,
		},
		{
			collector: collector.NewArgoRolloutsCollector(
				r.DynamicClient,
				config.TargetNamespaces,
				config.ExcludedArgoRollouts,
				logger,
			),
			name: collector.ArgoRollouts,
		},
	}

	// Register all collectors
	for _, c := range collectors {
		if disabledCollectorsMap[c.name.String()] {
			logger.Info("Skipping disabled collector", "type", c.name.String())
			continue
		}
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

			// Send the raw resource directly to Dakr
			if _, err := r.Sender.Send(ctx, resource); err != nil {
				logger.Error(err, "Failed to send resource to Dakr",
					"resourceType", resource.ResourceType,
					"eventType", resource.EventType,
					"key", resource.Key)
			} else {
				logger.Info("Sent resource to Dakr",
					"resourceType", resource.ResourceType,
					"eventType", resource.EventType,
					"key", resource.Key)
			}
		}
	}
}

// Add this method to the CollectionPolicyReconciler to handle disabled collectors changes
func (r *CollectionPolicyReconciler) handleDisabledCollectorsChange(
	ctx context.Context,
	logger logr.Logger,
	oldConfig, newConfig *PolicyConfig,
) error {
	// Create maps for quick lookups
	oldDisabled := make(map[string]bool)
	newDisabled := make(map[string]bool)

	for _, collector := range oldConfig.DisabledCollectors {
		oldDisabled[collector] = true
	}

	for _, collector := range newConfig.DisabledCollectors {
		newDisabled[collector] = true
	}

	// Get all collector types currently registered
	currentCollectors := r.CollectionManager.GetCollectorTypes()

	// Find collectors that need to be deregistered (they were enabled before but now are disabled)
	for _, collectorType := range currentCollectors {
		if newDisabled[collectorType] && !oldDisabled[collectorType] {
			logger.Info("Deregistering newly disabled collector", "type", collectorType)
			if err := r.CollectionManager.DeregisterCollector(collectorType); err != nil {
				logger.Error(err, "Failed to deregister collector", "type", collectorType)
				// Continue with other collectors even if one fails
			}
		}
	}

	// Find collectors that need to be registered (they were disabled before but now are enabled)
	clientConfig := ctrl.GetConfigOrDie()
	metricsClient, err := metricsv1.NewForConfig(clientConfig)
	if err != nil {
		logger.Error(err, "Failed to create metrics client for newly enabled collectors")
	}

	for _, collectorType := range oldConfig.DisabledCollectors {
		// If it was disabled before but not now
		if !newDisabled[collectorType] {
			logger.Info("Registering newly enabled collector", "type", collectorType)

			// Create the collector based on type
			var replacedCollector collector.ResourceCollector

			switch collectorType {
			case "pod":
				replacedCollector = collector.NewPodCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedPods,
					logger,
				)
			case "deployment":
				replacedCollector = collector.NewDeploymentCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedDeployments,
					logger,
				)
			case "statefulset":
				replacedCollector = collector.NewStatefulSetCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedStatefulSets,
					logger,
				)
			case "daemonset":
				replacedCollector = collector.NewDaemonSetCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedDaemonSets,
					logger,
				)
			case "service":
				replacedCollector = collector.NewServiceCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedServices,
					logger,
				)
			case "container_resources":
				replacedCollector = collector.NewContainerResourceCollector(
					r.K8sClient,
					metricsClient,
					collector.ContainerResourceCollectorConfig{
						PrometheusURL:           newConfig.PrometheusURL,
						UpdateInterval:          newConfig.UpdateInterval,
						QueryTimeout:            10 * time.Second,
						DisableNetworkIOMetrics: newConfig.DisableNetworkIOMetrics,
					},
					newConfig.TargetNamespaces,
					newConfig.ExcludedPods,
					logger,
				)
			case "node":
				replacedCollector = collector.NewNodeCollector(
					r.K8sClient,
					metricsClient,
					collector.NodeCollectorConfig{
						PrometheusURL:           newConfig.PrometheusURL,
						UpdateInterval:          newConfig.UpdateInterval,
						QueryTimeout:            10 * time.Second,
						DisableNetworkIOMetrics: newConfig.DisableNetworkIOMetrics,
					},
					newConfig.ExcludedNodes,
					logger,
				)
			case "persistentvolumeclaim":
				replacedCollector = collector.NewPersistentVolumeClaimCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedPVCs,
					logger,
				)
			case "persistentvolume":
				replacedCollector = collector.NewPersistentVolumeCollector(
					r.K8sClient,
					newConfig.ExcludedPVs,
					logger,
				)
			case "event":
				replacedCollector = collector.NewEventCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedEvents,
					10,
					10*time.Minute,
					logger,
				)
			case "job":
				replacedCollector = collector.NewJobCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedJobs,
					logger,
				)
			case "cronjob":
				replacedCollector = collector.NewCronJobCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedCronJobs,
					logger,
				)
			case "replicationcontroller":
				replacedCollector = collector.NewReplicationControllerCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedReplicationControllers,
					logger,
				)
			case "ingress":
				replacedCollector = collector.NewIngressCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedIngresses,
					logger,
				)
			case "ingressclass":
				replacedCollector = collector.NewIngressClassCollector(
					r.K8sClient,
					newConfig.ExcludedIngressClasses,
					logger,
				)
			case "networkpolicy":
				replacedCollector = collector.NewNetworkPolicyCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedNetworkPolicies,
					logger,
				)
			case "endpoints":
				replacedCollector = collector.NewEndpointCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedEndpoints,
					logger,
				)
			case "serviceaccount":
				replacedCollector = collector.NewServiceAccountCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedServiceAccounts,
					logger,
				)
			case "limitrange":
				replacedCollector = collector.NewLimitRangeCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedLimitRanges,
					logger,
				)
			case "resourcequota":
				replacedCollector = collector.NewResourceQuotaCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedResourceQuotas,
					logger,
				)
			case "horizontalpodautoscaler":
				replacedCollector = collector.NewHorizontalPodAutoscalerCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedHPAs,
					logger,
				)
			case "verticalpodautoscaler":
				replacedCollector = collector.NewVerticalPodAutoscalerCollector(
					r.DynamicClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedVPAs,
					logger,
				)
			case "role":
				replacedCollector = collector.NewRoleCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedRoles,
					logger,
				)
			case "rolebinding":
				replacedCollector = collector.NewRoleBindingCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedRoleBindings,
					logger,
				)
			case "clusterrole":
				replacedCollector = collector.NewClusterRoleCollector(
					r.K8sClient,
					newConfig.ExcludedClusterRoles,
					logger,
				)
			case "clusterrolebinding":
				replacedCollector = collector.NewClusterRoleBindingCollector(
					r.K8sClient,
					newConfig.ExcludedClusterRoleBindings,
					logger,
				)
			case "poddisruptionbudget":
				replacedCollector = collector.NewPodDisruptionBudgetCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedPDBs,
					logger,
				)
			case "storageclass":
				replacedCollector = collector.NewStorageClassCollector(
					r.K8sClient,
					newConfig.ExcludedStorageClasses,
					logger,
				)
			case "namespace":
				replacedCollector = collector.NewNamespaceCollector(
					r.K8sClient,
					newConfig.ExcludedNamespaces,
					logger,
				)
			case "csinode":
				replacedCollector = collector.NewCSINodeCollector(
					r.K8sClient,
					newConfig.ExcludedCSINodes,
					logger,
				)
			case "karpenter":
				replacedCollector = collector.NewKarpenterCollector(
					r.DynamicClient,
					logger,
				)
			case "datadog":
				replacedCollector = collector.NewDatadogCollector(
					r.DynamicClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedDatadogReplicaSets,
					logger,
				)
			case "argo_rollouts":
				replacedCollector = collector.NewArgoRolloutsCollector(
					r.DynamicClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedArgoRollouts,
					logger,
				)
			default:
				logger.Info("Unknown collector type, skipping", "type", collectorType)
				continue
			}

			// Register and start the collector
			if err := r.CollectionManager.RegisterCollector(replacedCollector); err != nil {
				logger.Error(err, "Failed to register collector", "type", collectorType)
				continue
			}

			if err := r.CollectionManager.StartCollector(ctx, collectorType); err != nil {
				logger.Error(err, "Failed to start collector", "type", collectorType)
			}
		}
	}

	return nil
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
