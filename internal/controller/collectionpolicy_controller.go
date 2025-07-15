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
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"go.uber.org/zap"
	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	monitoringv1 "github.com/devzero-inc/zxporter/api/v1"
	kedaclient "github.com/kedacore/keda/v2/pkg/generated/clientset/versioned"
	metricsv1 "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/devzero-inc/zxporter/internal/collector"
	"github.com/devzero-inc/zxporter/internal/collector/provider"
	"github.com/devzero-inc/zxporter/internal/collector/snap"
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
	KEDAClient        *kedaclient.Clientset
	K8sClient         *kubernetes.Clientset
	DynamicClient     *dynamic.DynamicClient
	DiscoveryClient   *discovery.DiscoveryClient
	ApiExtensions     *apiextensionsclientset.Clientset
	CollectionManager *collector.CollectionManager
	Sender            transport.DirectSender
	TelemetrySender   *transport.TelemetrySender
	TelemetryMetrics  *collector.TelemetryMetrics
	IsRunning         bool
	CurrentPolicyHash string
	CurrentConfig     *PolicyConfig
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
	ExcludedKedaScaledJobs         []collector.ExcludedScaledJob
	ExcludedKedaScaledObjects      []collector.ExcludedScaledObject

	DisabledCollectors []string

	KubeContextName         string
	DakrURL                 string
	ClusterToken            string
	PrometheusURL           string
	DisableNetworkIOMetrics bool
	DisableGPUMetrics       bool
	UpdateInterval          time.Duration
	NodeMetricsInterval     time.Duration
	BufferSize              int
	MaskSecretData          bool
	NumResourceProcessors   int
}

//+kubebuilder:rbac:groups=devzero.io,resources=collectionpolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=devzero.io,resources=collectionpolicies/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=devzero.io,resources=collectionpolicies/finalizers,verbs=update

// Metric server installation permissions
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=role,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apiregistration.k8s.io,resources=apiservices,verbs=get;list;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=nodes/metrics,verbs=get

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

// CRD API Group resources
//+kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch

// DataDog resources
//+kubebuilder:rbac:groups=datadoghq.com,resources=extendeddaemonsetreplicasets,verbs=get;list;watch

// Argo Rollouts resources
//+kubebuilder:rbac:groups=argoproj.io,resources=rollouts,verbs=get;list;watch

// KEDA resources
//+kubebuilder:rbac:groups=keda.sh,resources=scaledobjects,verbs=get;list;watch
//+kubebuilder:rbac:groups=keda.sh,resources=scaledjobs,verbs=get;list;watch
//+kubebuilder:rbac:groups=keda.sh,resources=triggerauthentications,verbs=get;list;watch
//+kubebuilder:rbac:groups=keda.sh,resources=clustertriggerauthentications,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *CollectionPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling CollectionPolicy", "request", req)

	// Create a new config object from the policy and environment
	newEnvSpec, err := util.LoadCollectionPolicySpecFromEnv()
	if err != nil {
		logger.Error(err, "Error loading ENV varaibles")
	}

	// Create a new config object from the policy and environment
	newConfig, configChanged := r.createNewConfig(&newEnvSpec, logger)

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
func (r *CollectionPolicyReconciler) createNewConfig(envSpec *monitoringv1.CollectionPolicySpec, logger logr.Logger) (*PolicyConfig, bool) {
	// Convert the merged spec to the PolicyConfig format
	newConfig := &PolicyConfig{
		// Target and exclusion basics
		TargetNamespaces:   envSpec.TargetSelector.Namespaces,
		ExcludedNamespaces: envSpec.Exclusions.ExcludedNamespaces,
		ExcludedNodes:      envSpec.Exclusions.ExcludedNodes,
		ExcludedCSINodes:   envSpec.Exclusions.ExcludedNodes, // Same as nodes

		// Policies
		KubeContextName:         envSpec.Policies.KubeContextName,
		DakrURL:                 envSpec.Policies.DakrURL,
		ClusterToken:            envSpec.Policies.ClusterToken,
		PrometheusURL:           envSpec.Policies.PrometheusURL,
		DisableNetworkIOMetrics: envSpec.Policies.DisableNetworkIOMetrics,
		DisableGPUMetrics:       envSpec.Policies.DisableGPUMetrics,
		MaskSecretData:          envSpec.Policies.MaskSecretData,
		DisabledCollectors:      envSpec.Policies.DisabledCollectors,
		BufferSize:              envSpec.Policies.BufferSize,
	}

	if envSpec.Policies.NumResourceProcessors != nil && *envSpec.Policies.NumResourceProcessors > 0 {
		newConfig.NumResourceProcessors = *envSpec.Policies.NumResourceProcessors
	} else {
		newConfig.NumResourceProcessors = 16
	}

	logger.Info("Disabled collectors", "name", newConfig.DisabledCollectors)

	// Parse and set frequency
	frequencyStr := envSpec.Policies.Frequency
	if frequencyStr != "" {
		if freq, err := time.ParseDuration(frequencyStr); err == nil {
			newConfig.UpdateInterval = freq
		} else {
			logger.Error(err, "Error parsing frequency string", "frequency", frequencyStr)
			newConfig.UpdateInterval = 10 * time.Second // Default
		}
	} else {
		newConfig.UpdateInterval = 10 * time.Second // Default
	}

	// Set node metrics interval (6x regular interval but minimum 60s)
	nodeMetricsIntervalStr := envSpec.Policies.NodeMetricsInterval
	if nodeMetricsIntervalStr != "" {
		if interval, err := time.ParseDuration(nodeMetricsIntervalStr); err == nil {
			newConfig.NodeMetricsInterval = interval
		} else {
			logger.Error(err, "Error parsing node metrics interval", "interval", nodeMetricsIntervalStr)
			newConfig.NodeMetricsInterval = max(newConfig.UpdateInterval*6, 60*time.Second)
		}
	} else {
		newConfig.NodeMetricsInterval = max(newConfig.UpdateInterval*6, 60*time.Second)
	}

	// Convert all excluded resources
	// Pods
	for _, pod := range envSpec.Exclusions.ExcludedPods {
		newConfig.ExcludedPods = append(newConfig.ExcludedPods, collector.ExcludedPod{
			Namespace: pod.Namespace,
			Name:      pod.Name,
		})
	}

	// Deployments
	for _, deploy := range envSpec.Exclusions.ExcludedDeployments {
		newConfig.ExcludedDeployments = append(newConfig.ExcludedDeployments, collector.ExcludedDeployment{
			Namespace: deploy.Namespace,
			Name:      deploy.Name,
		})
	}

	// StatefulSets
	for _, sts := range envSpec.Exclusions.ExcludedStatefulSets {
		newConfig.ExcludedStatefulSets = append(newConfig.ExcludedStatefulSets, collector.ExcludedStatefulSet{
			Namespace: sts.Namespace,
			Name:      sts.Name,
		})
	}

	// DaemonSets
	for _, ds := range envSpec.Exclusions.ExcludedDaemonSets {
		newConfig.ExcludedDaemonSets = append(newConfig.ExcludedDaemonSets, collector.ExcludedDaemonSet{
			Namespace: ds.Namespace,
			Name:      ds.Name,
		})
	}

	// Services
	for _, svc := range envSpec.Exclusions.ExcludedServices {
		newConfig.ExcludedServices = append(newConfig.ExcludedServices, collector.ExcludedService{
			Namespace: svc.Namespace,
			Name:      svc.Name,
		})
	}

	// PVCs
	for _, pvc := range envSpec.Exclusions.ExcludedPVCs {
		newConfig.ExcludedPVCs = append(newConfig.ExcludedPVCs, collector.ExcludedPVC{
			Namespace: pvc.Namespace,
			Name:      pvc.Name,
		})
	}

	// Jobs
	for _, job := range envSpec.Exclusions.ExcludedJobs {
		newConfig.ExcludedJobs = append(newConfig.ExcludedJobs, collector.ExcludedJob{
			Namespace: job.Namespace,
			Name:      job.Name,
		})
	}

	// CronJobs
	for _, cron := range envSpec.Exclusions.ExcludedCronJobs {
		newConfig.ExcludedCronJobs = append(newConfig.ExcludedCronJobs, collector.ExcludedCronJob{
			Namespace: cron.Namespace,
			Name:      cron.Name,
		})
	}

	// ReplicationControllers
	for _, rc := range envSpec.Exclusions.ExcludedReplicationControllers {
		newConfig.ExcludedReplicationControllers = append(newConfig.ExcludedReplicationControllers, collector.ExcludedReplicationController{
			Namespace: rc.Namespace,
			Name:      rc.Name,
		})
	}

	// Ingresses
	for _, ing := range envSpec.Exclusions.ExcludedIngresses {
		newConfig.ExcludedIngresses = append(newConfig.ExcludedIngresses, collector.ExcludedIngress{
			Namespace: ing.Namespace,
			Name:      ing.Name,
		})
	}

	// IngressClasses
	newConfig.ExcludedIngressClasses = envSpec.Exclusions.ExcludedIngressClasses

	// NetworkPolicies
	for _, netpol := range envSpec.Exclusions.ExcludedNetworkPolicies {
		newConfig.ExcludedNetworkPolicies = append(newConfig.ExcludedNetworkPolicies, collector.ExcludedNetworkPolicy{
			Namespace: netpol.Namespace,
			Name:      netpol.Name,
		})
	}

	// Endpoints
	for _, ep := range envSpec.Exclusions.ExcludedEndpoints {
		newConfig.ExcludedEndpoints = append(newConfig.ExcludedEndpoints, collector.ExcludedEndpoint{
			Namespace: ep.Namespace,
			Name:      ep.Name,
		})
	}

	// ServiceAccounts
	for _, sa := range envSpec.Exclusions.ExcludedServiceAccounts {
		newConfig.ExcludedServiceAccounts = append(newConfig.ExcludedServiceAccounts, collector.ExcludedServiceAccount{
			Namespace: sa.Namespace,
			Name:      sa.Name,
		})
	}

	// LimitRanges
	for _, lr := range envSpec.Exclusions.ExcludedLimitRanges {
		newConfig.ExcludedLimitRanges = append(newConfig.ExcludedLimitRanges, collector.ExcludedLimitRange{
			Namespace: lr.Namespace,
			Name:      lr.Name,
		})
	}

	// ResourceQuotas
	for _, rq := range envSpec.Exclusions.ExcludedResourceQuotas {
		newConfig.ExcludedResourceQuotas = append(newConfig.ExcludedResourceQuotas, collector.ExcludedResourceQuota{
			Namespace: rq.Namespace,
			Name:      rq.Name,
		})
	}

	// HPAs
	for _, hpa := range envSpec.Exclusions.ExcludedHPAs {
		newConfig.ExcludedHPAs = append(newConfig.ExcludedHPAs, collector.ExcludedHPA{
			Namespace: hpa.Namespace,
			Name:      hpa.Name,
		})
	}

	// VPAs
	for _, vpa := range envSpec.Exclusions.ExcludedVPAs {
		newConfig.ExcludedVPAs = append(newConfig.ExcludedVPAs, collector.ExcludedVPA{
			Namespace: vpa.Namespace,
			Name:      vpa.Name,
		})
	}

	// Roles
	for _, role := range envSpec.Exclusions.ExcludedRoles {
		newConfig.ExcludedRoles = append(newConfig.ExcludedRoles, collector.ExcludedRole{
			Namespace: role.Namespace,
			Name:      role.Name,
		})
	}

	// RoleBindings
	for _, rb := range envSpec.Exclusions.ExcludedRoleBindings {
		newConfig.ExcludedRoleBindings = append(newConfig.ExcludedRoleBindings, collector.ExcludedRoleBinding{
			Namespace: rb.Namespace,
			Name:      rb.Name,
		})
	}

	// ClusterRoles
	newConfig.ExcludedClusterRoles = envSpec.Exclusions.ExcludedClusterRoles

	// ClusterRoleBindings
	newConfig.ExcludedClusterRoleBindings = envSpec.Exclusions.ExcludedClusterRoleBindings

	// PDBs
	for _, pdb := range envSpec.Exclusions.ExcludedPDBs {
		newConfig.ExcludedPDBs = append(newConfig.ExcludedPDBs, collector.ExcludedPDB{
			Namespace: pdb.Namespace,
			Name:      pdb.Name,
		})
	}

	// PSPs
	newConfig.ExcludedPSPs = envSpec.Exclusions.ExcludedPSPs

	// PVs
	newConfig.ExcludedPVs = envSpec.Exclusions.ExcludedPVs

	// StorageClasses
	newConfig.ExcludedStorageClasses = envSpec.Exclusions.ExcludedStorageClasses

	// CRDs
	newConfig.ExcludedCRDs = envSpec.Exclusions.ExcludedCRDs

	// CRDGroups
	newConfig.ExcludedCRDGroups = envSpec.Exclusions.ExcludedCRDGroups

	// KEDA ScaledJob
	for _, scaledJob := range envSpec.Exclusions.ExcludedScaledJobs {
		newConfig.ExcludedKedaScaledJobs = append(newConfig.ExcludedKedaScaledJobs, collector.ExcludedScaledJob{
			Namespace: scaledJob.Namespace,
			Name:      scaledJob.Name,
		})
	}

	// KEDA ScaledObject
	for _, scaledObject := range envSpec.Exclusions.ExcludedScaledObjects {
		newConfig.ExcludedKedaScaledObjects = append(newConfig.ExcludedKedaScaledObjects, collector.ExcludedScaledObject{
			Namespace: scaledObject.Namespace,
			Name:      scaledObject.Name,
		})
	}

	// Events - these are special with more fields
	for _, event := range envSpec.Exclusions.ExcludedEvents {
		newConfig.ExcludedEvents = append(newConfig.ExcludedEvents, collector.ExcludedEvent{
			Namespace: event.Namespace,
			Name:      event.Name,
		})
	}

	// Watched CRDs
	newConfig.WatchedCRDs = envSpec.Policies.WatchedCRDs

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
		affectedCollectors["container_resource"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedDeployments, newConfig.ExcludedDeployments) { // TODO: should depployment influence container and pod collectors?
		affectedCollectors["deployment"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedStatefulSets, newConfig.ExcludedStatefulSets) {
		affectedCollectors["stateful_set"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedReplicaSet, newConfig.ExcludedReplicaSet) {
		affectedCollectors["replica_set"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedDaemonSets, newConfig.ExcludedDaemonSets) {
		affectedCollectors["daemon_set"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedServices, newConfig.ExcludedServices) {
		affectedCollectors["service"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedPVCs, newConfig.ExcludedPVCs) {
		affectedCollectors["persistent_volume_claim"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedEvents, newConfig.ExcludedEvents) {
		affectedCollectors["event"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedJobs, newConfig.ExcludedJobs) {
		affectedCollectors["job"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedCronJobs, newConfig.ExcludedCronJobs) {
		affectedCollectors["cron_job"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedReplicationControllers, newConfig.ExcludedReplicationControllers) {
		affectedCollectors["replication_controller"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedIngresses, newConfig.ExcludedIngresses) {
		affectedCollectors["ingress"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedNetworkPolicies, newConfig.ExcludedNetworkPolicies) {
		affectedCollectors["network_policy"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedEndpoints, newConfig.ExcludedEndpoints) {
		affectedCollectors["endpoints"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedServiceAccounts, newConfig.ExcludedServiceAccounts) {
		affectedCollectors["service_account"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedLimitRanges, newConfig.ExcludedLimitRanges) {
		affectedCollectors["limit_range"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedResourceQuotas, newConfig.ExcludedResourceQuotas) {
		affectedCollectors["resource_quota"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedHPAs, newConfig.ExcludedHPAs) {
		affectedCollectors["horizontal_pod_autoscaler"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedVPAs, newConfig.ExcludedVPAs) {
		affectedCollectors["vertical_pod_autoscaler"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedRoles, newConfig.ExcludedRoles) {
		affectedCollectors["role"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedRoleBindings, newConfig.ExcludedRoleBindings) {
		affectedCollectors["role_binding"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedClusterRoles, newConfig.ExcludedClusterRoles) {
		affectedCollectors["cluster_role"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedClusterRoleBindings, newConfig.ExcludedClusterRoleBindings) {
		affectedCollectors["cluster_role_binding"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedPDBs, newConfig.ExcludedPDBs) {
		affectedCollectors["pod_disruption_budget"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedStorageClasses, newConfig.ExcludedStorageClasses) {
		affectedCollectors["storage_class"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedPVs, newConfig.ExcludedPVs) {
		affectedCollectors["persistent_volume"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedIngressClasses, newConfig.ExcludedIngressClasses) {
		affectedCollectors["ingress_class"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedNodes, newConfig.ExcludedNodes) {
		affectedCollectors["node"] = true
		affectedCollectors["node_resource"] = true
	}

	if oldConfig.NodeMetricsInterval != newConfig.NodeMetricsInterval {
		affectedCollectors["node_resource"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedCSINodes, newConfig.ExcludedCSINodes) {
		affectedCollectors["csi_node"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedKedaScaledJobs, newConfig.ExcludedKedaScaledJobs) {
		affectedCollectors["keda_scaled_job"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedKedaScaledObjects, newConfig.ExcludedKedaScaledObjects) {
		affectedCollectors["keda_scaled_object"] = true
	}

	// Check if the special node collectors are affected by the update interval change
	if oldConfig.UpdateInterval != newConfig.UpdateInterval ||
		oldConfig.PrometheusURL != newConfig.PrometheusURL ||
		oldConfig.DisableNetworkIOMetrics != newConfig.DisableNetworkIOMetrics ||
		oldConfig.DisableGPUMetrics != newConfig.DisableGPUMetrics {
		affectedCollectors["node"] = true
		affectedCollectors["container_resource"] = true
	}

	// Add check for CRD changes
	if !reflect.DeepEqual(oldConfig.ExcludedCRDs, newConfig.ExcludedCRDs) ||
		!reflect.DeepEqual(oldConfig.ExcludedCRDGroups, newConfig.ExcludedCRDGroups) ||
		!reflect.DeepEqual(oldConfig.WatchedCRDs, newConfig.WatchedCRDs) {
		affectedCollectors["custom_resource_definition"] = true
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

	// Check Prometheus availability if URL changed or metrics configuration changed
	if newConfig.PrometheusURL != "" &&
		(r.CurrentConfig.PrometheusURL != newConfig.PrometheusURL ||
			r.CurrentConfig.DisableNetworkIOMetrics != newConfig.DisableNetworkIOMetrics ||
			r.CurrentConfig.DisableGPUMetrics != newConfig.DisableGPUMetrics) {
		logger.Info("Prometheus configuration changed, checking availability", "url", newConfig.PrometheusURL)
		prometheusAvailable := r.waitForPrometheusAvailability(ctx, newConfig.PrometheusURL)
		if !prometheusAvailable {
			logger.Info("Prometheus is not available after waiting, will continue with restart but metrics may be limited")
			// We continue with restart, but log the warning
		} else {
			logger.Info("Prometheus is available, continuing with full metrics collection")
		}
	}

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

	// Restart the telemetry sender if it exists
	if r.TelemetrySender != nil {
		logger.Info("Stopping telemetry sender for restart")
		if err := r.TelemetrySender.Stop(); err != nil {
			logger.Error(err, "Error stopping telemetry sender")
		}

		// Create a new telemetry sender
		r.TelemetrySender = transport.NewTelemetrySender(
			logger.WithName("telemetry"),
			r.Sender.(transport.DakrClient),
			r.TelemetryMetrics,
			15*time.Second, // Send metrics every 15 seconds
		)

		if err := r.TelemetrySender.Start(ctx); err != nil {
			logger.Error(err, "Failed to restart telemetry sender")
		} else {
			logger.Info("Successfully restarted telemetry sender")
		}
	}

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
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			)
		case "deployment":
			replacedCollector = collector.NewDeploymentCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedDeployments,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			)
		case "stateful_set":
			replacedCollector = collector.NewStatefulSetCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedStatefulSets,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			)
		case "replica_set":
			replacedCollector = collector.NewReplicaSetCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedReplicaSet,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			)
		case "daemon_set":
			replacedCollector = collector.NewDaemonSetCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedDaemonSets,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			)
		case "service":
			replacedCollector = collector.NewServiceCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedServices,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			)
		case "container_resource":
			replacedCollector = collector.NewContainerResourceCollector(
				r.K8sClient,
				metricsClient,
				collector.ContainerResourceCollectorConfig{
					PrometheusURL:           newConfig.PrometheusURL,
					UpdateInterval:          newConfig.UpdateInterval,
					QueryTimeout:            10 * time.Second,
					DisableNetworkIOMetrics: newConfig.DisableNetworkIOMetrics,
					DisableGPUMetrics:       newConfig.DisableGPUMetrics,
				},
				newConfig.TargetNamespaces,
				newConfig.ExcludedPods,
				collector.DefaultMaxBatchSize,
				newConfig.UpdateInterval,
				logger,
				r.TelemetryMetrics,
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
					DisableGPUMetrics:       newConfig.DisableGPUMetrics,
				},
				newConfig.ExcludedNodes,
				collector.DefaultMaxBatchSize,
				newConfig.UpdateInterval,
				logger,
				r.TelemetryMetrics,
			)
		case "persistent_volume_claim":
			replacedCollector = collector.NewPersistentVolumeClaimCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedPVCs,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			)
		case "persistent_volume":
			replacedCollector = collector.NewPersistentVolumeCollector(
				r.K8sClient,
				newConfig.ExcludedPVs,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			)
		case "event":
			replacedCollector = collector.NewEventCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedEvents,
				10,             // maxEventsPerType - keeping existing value
				10*time.Minute, // retentionPeriod - keeping existing value
				400,
				collector.DefaultMaxBatchTime,
				logger,
			)
		case "job":
			replacedCollector = collector.NewJobCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedJobs,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			)
		case "cron_job":
			replacedCollector = collector.NewCronJobCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedCronJobs,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			)
		case "replication_controller":
			replacedCollector = collector.NewReplicationControllerCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedReplicationControllers,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			)
		case "ingress":
			replacedCollector = collector.NewIngressCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedIngresses,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			)
		case "ingress_class":
			replacedCollector = collector.NewIngressClassCollector(
				r.K8sClient,
				newConfig.ExcludedIngressClasses,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			)
		case "network_policy":
			replacedCollector = collector.NewNetworkPolicyCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedNetworkPolicies,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			)
		case "endpoints":
			replacedCollector = collector.NewEndpointCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedEndpoints,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			)
		case "service_account":
			replacedCollector = collector.NewServiceAccountCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedServiceAccounts,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			)
		case "limit_range":
			replacedCollector = collector.NewLimitRangeCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedLimitRanges,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			)
		case "resource_quota":
			replacedCollector = collector.NewResourceQuotaCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedResourceQuotas,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			)
		case "horizontal_pod_autoscaler":
			replacedCollector = collector.NewHorizontalPodAutoscalerCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedHPAs,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			)
		case "vertical_pod_autoscaler":
			replacedCollector = collector.NewVerticalPodAutoscalerCollector(
				r.DynamicClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedVPAs,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			)
		case "role":
			replacedCollector = collector.NewRoleCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedRoles,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			)
		case "role_binding":
			replacedCollector = collector.NewRoleBindingCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedRoleBindings,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			)
		case "cluster_role":
			replacedCollector = collector.NewClusterRoleCollector(
				r.K8sClient,
				newConfig.ExcludedClusterRoles,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			)
		case "cluster_role_binding":
			replacedCollector = collector.NewClusterRoleBindingCollector(
				r.K8sClient,
				newConfig.ExcludedClusterRoleBindings,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			)
		case "pod_disruption_budget":
			replacedCollector = collector.NewPodDisruptionBudgetCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedPDBs,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			)
		case "storage_class":
			replacedCollector = collector.NewStorageClassCollector(
				r.K8sClient,
				newConfig.ExcludedStorageClasses,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			)
		case "csi_node":
			replacedCollector = collector.NewCSINodeCollector(
				r.K8sClient,
				newConfig.ExcludedNodes,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			)
		case "karpenter":
			replacedCollector = collector.NewKarpenterCollector(
				r.DynamicClient,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			)
		case "datadog":
			replacedCollector = collector.NewDatadogCollector(
				r.DynamicClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedDatadogReplicaSets,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			)
		case "argo_rollouts":
			replacedCollector = collector.NewArgoRolloutsCollector(
				r.DynamicClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedArgoRollouts,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			)
		case "custom_resource_definition":
			replacedCollector = collector.NewCRDCollector(
				r.ApiExtensions,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			)
		case "keda_scaled_job":
			replacedCollector = collector.NewScaledJobCollector(
				r.KEDAClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedKedaScaledJobs,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			)
		case "keda_scaled_object":
			replacedCollector = collector.NewScaledObjectCollector(
				r.KEDAClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedKedaScaledObjects,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			)
		case "cluster_snapshot":
			replacedCollector = snap.NewClusterSnapshotter(
				r.K8sClient,
				15*time.Minute,
				r.Sender,
				newConfig.TargetNamespaces,
				newConfig.ExcludedPods,
				newConfig.ExcludedNodes,
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

	// Check if Prometheus is available if URL is configured
	if config.PrometheusURL != "" {
		logger.Info("Prometheus URL configured, checking availability", "url", config.PrometheusURL)
		prometheusAvailable := r.waitForPrometheusAvailability(ctx, config.PrometheusURL)
		if !prometheusAvailable {
			logger.Info("Prometheus is not available after waiting, will continue initialization but metrics may be limited")
			// We continue initialization, but log the warning
		} else {
			logger.Info("Prometheus is available, continuing with full metrics collection")
		}
	}

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
	logger.Info("Starting resource processing workers", "count", config.NumResourceProcessors)
	for i := 0; i < config.NumResourceProcessors; i++ {
		workerID := i
		go func() {
			workerLogger := logger.WithValues("resourceProcessorSenderID", workerID)
			r.processCollectedResources(ctx, workerLogger)
		}()
	}

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
		r.TelemetryMetrics,
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

	r.Sender = transport.NewDirectSender(dakrClient, logger)

	// Create and start the telemetry sender
	r.TelemetrySender = transport.NewTelemetrySender(
		logger.WithName("telemetry"),
		dakrClient, // Use the dakrClient directly
		r.TelemetryMetrics,
		15*time.Second, // Send metrics every 15 seconds
	)

	if err := r.TelemetrySender.Start(ctx); err != nil {
		logger.Error(err, "Failed to start telemetry sender")
		// Continue even if telemetry sender fails to start
	} else {
		logger.Info("Started telemetry sender")
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
	providerDetector := provider.NewDetector(logger, r.K8sClient, provider.WithKubeContextName(config.KubeContextName))
	detectedProvider, err := providerDetector.DetectProvider(ctx)
	if err != nil {
		logger.Error(err, "Failed to detect cloud provider, collector will have limited functionality")
	}

	// Create the cluster collector
	clusterCollector := collector.NewClusterCollector(
		r.K8sClient,
		metricsClient,
		detectedProvider,
		30*time.Minute,
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

	select {
	case resources, ok := <-resourceChan:
		if !ok {
			// Channel closed unexpectedly
			logger.Error(nil, "Resource channel closed while waiting for cluster data")
			close(clusterFailedCh)
			return
		}

		// Only process cluster resources at this stage
		if len(resources) == 1 && resources[0].ResourceType == collector.Cluster {
			// Send the cluster data to Dakr
			_, err := r.Sender.Send(ctx, resources[0])
			if err != nil {
				logger.Error(err, "Failed to send cluster data")
				close(clusterFailedCh)
				return
				// Continue waiting for next attempt
			} else {
				logger.Info("Successfully sent cluster data to Dakr")
				close(clusterRegisteredCh)
				return
			}
		} else {
			rLength := len(resources)
			rType := collector.Unknown
			if rLength == 1 {
				rType = resources[0].ResourceType
			}
			logger.Error(fmt.Errorf("non cluster resource received"),
				"unexpected resource encountered when expecting single cluster resource",
				zap.String("resource_type", rType.String()),
				zap.Int("resources_length", rLength),
			)
			close(clusterFailedCh)
			return
		}

	case <-ctx.Done():
		logger.Info("Context cancelled while monitoring cluster registration")
		close(clusterFailedCh)
		return
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

	// Stop telemetry sender if it's running
	if r.TelemetrySender != nil {
		if err := r.TelemetrySender.Stop(); err != nil {
			logger.Error(err, "Error stopping telemetry sender during failure")
		}
		r.TelemetrySender = nil
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
	// Use the shared Prometheus metrics instance from the reconciler

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
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			),
			name: collector.Endpoints,
		},
		{
			collector: collector.NewServiceAccountCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedServiceAccounts,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			),
			name: collector.ServiceAccount,
		},
		{
			collector: collector.NewLimitRangeCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedLimitRanges,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			),
			name: collector.LimitRange,
		},
		{
			collector: collector.NewResourceQuotaCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedResourceQuotas,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			),
			name: collector.ResourceQuota,
		},
		{
			collector: collector.NewPersistentVolumeCollector(
				r.K8sClient,
				config.ExcludedPVs,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			),
			name: collector.PersistentVolume,
		},
		{
			collector: collector.NewPodCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedPods,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			),
			name: collector.Pod,
		},
		{
			collector: collector.NewDeploymentCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedDeployments,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			),
			name: collector.Deployment,
		},
		{
			collector: collector.NewStatefulSetCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedStatefulSets,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			),
			name: collector.StatefulSet,
		},
		{
			collector: collector.NewDaemonSetCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedDaemonSets,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			),
			name: collector.DaemonSet,
		},
		{
			collector: collector.NewNamespaceCollector(
				r.K8sClient,
				config.ExcludedNamespaces,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			),
			name: collector.Namespace,
		},
		{
			collector: collector.NewReplicationControllerCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedReplicationControllers,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			),
			name: collector.ReplicationController,
		},
		{
			collector: collector.NewIngressCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedIngresses,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			),
			name: collector.Ingress,
		},
		{
			collector: collector.NewIngressClassCollector(
				r.K8sClient,
				config.ExcludedIngressClasses,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			),
			name: collector.IngressClass,
		},
		{
			collector: collector.NewPersistentVolumeClaimCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedPVCs,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
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
				400,
				collector.DefaultMaxBatchTime,
				logger,
			),
			name: collector.Event,
		},
		{
			collector: collector.NewServiceCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedServices,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			),
			name: collector.Service,
		},
		{
			collector: collector.NewJobCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedJobs,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			),
			name: collector.Job,
		},
		{
			collector: collector.NewNetworkPolicyCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedNetworkPolicies,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			),
			name: collector.NetworkPolicy,
		},
		{
			collector: collector.NewCronJobCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedCronJobs,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
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
					DisableGPUMetrics:       config.DisableGPUMetrics,
				},
				config.TargetNamespaces,
				config.ExcludedPods,
				collector.DefaultMaxBatchSize,
				config.UpdateInterval,
				logger,
				r.TelemetryMetrics,
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
					DisableGPUMetrics:       config.DisableGPUMetrics,
				},
				config.ExcludedNodes,
				collector.DefaultMaxBatchSize,
				config.UpdateInterval,
				logger,
				r.TelemetryMetrics,
			),
			name: collector.Node,
		},
		{
			collector: collector.NewRoleCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedRoles,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			),
			name: collector.Role,
		},
		{
			collector: collector.NewRoleBindingCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedRoleBindings,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			),
			name: collector.RoleBinding,
		},
		{
			collector: collector.NewClusterRoleCollector(
				r.K8sClient,
				config.ExcludedClusterRoles,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			),
			name: collector.ClusterRole,
		},
		{
			collector: collector.NewClusterRoleBindingCollector(
				r.K8sClient,
				config.ExcludedClusterRoleBindings,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			),
			name: collector.ClusterRoleBinding,
		},
		{
			collector: collector.NewHorizontalPodAutoscalerCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedHPAs,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			),
			name: collector.HorizontalPodAutoscaler,
		},
		{
			collector: collector.NewVerticalPodAutoscalerCollector(
				r.DynamicClient,
				config.TargetNamespaces,
				config.ExcludedVPAs,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			),
			name: collector.VerticalPodAutoscaler,
		},
		{
			collector: collector.NewPodDisruptionBudgetCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedPDBs,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			),
			name: collector.PodDisruptionBudget,
		},
		{
			collector: collector.NewStorageClassCollector(
				r.K8sClient,
				config.ExcludedStorageClasses,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			),
			name: collector.StorageClass,
		},
		{
			collector: collector.NewReplicaSetCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedReplicaSet,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			),
			name: collector.ReplicaSet,
		},
		{
			collector: collector.NewCSINodeCollector(
				r.K8sClient,
				config.ExcludedCSINodes,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			),
			name: collector.CSINode,
		},
		{
			collector: collector.NewKarpenterCollector(
				r.DynamicClient,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			),
			name: collector.Karpenter,
		},
		{
			collector: collector.NewDatadogCollector(
				r.DynamicClient,
				config.TargetNamespaces,
				config.ExcludedDatadogReplicaSets,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			),
			name: collector.Datadog,
		},
		{
			collector: collector.NewArgoRolloutsCollector(
				r.DynamicClient,
				config.TargetNamespaces,
				config.ExcludedArgoRollouts,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			),
			name: collector.ArgoRollouts,
		},
		{
			collector: collector.NewCRDCollector(
				r.ApiExtensions,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			),
			name: collector.CustomResourceDefinition,
		},
		{
			collector: collector.NewScaledJobCollector(
				r.KEDAClient,
				config.TargetNamespaces,
				config.ExcludedKedaScaledJobs,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			),
			name: collector.KedaScaledJob,
		},
		{
			collector: collector.NewScaledObjectCollector(
				r.KEDAClient,
				config.TargetNamespaces,
				config.ExcludedKedaScaledObjects,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
			),
			name: collector.KedaScaledObject,
		},
		{
			collector: snap.NewClusterSnapshotter(
				r.K8sClient,
				15*time.Minute,
				r.Sender,
				config.TargetNamespaces,
				config.ExcludedPods,
				config.ExcludedNodes,
				logger,
			),
			name: collector.ClusterSnapshot,
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
				logger.Error(err, "Failed to register collector", "collector", c.name.String())
			} else {
				logger.Info("Registered collector", "collector", c.name.String())
			}
		}
	}

	return nil
}

// processCollectedResources reads from collection channel and forwards to sender
func (r *CollectionPolicyReconciler) processCollectedResources(ctx context.Context, logger logr.Logger) {
	logger.Info("Starting to process collected resources")

	resourceChan := r.CollectionManager.GetCombinedChannel()

	for {
		select {
		case <-ctx.Done():
			logger.Info("Context done, stopping processor")
			return
		case resources, ok := <-resourceChan:
			if !ok {
				logger.Info("Resource channel closed, stopping processor")
				return
			}

			if len(resources) == 0 {
				logger.Info("Empty list of resources")
				continue
			}

			// Send the raw resource directly to Dakr
			if _, err := r.Sender.SendBatch(ctx, resources, resources[0].ResourceType); err != nil {
				logger.Error(err, "Failed to send resource to Dakr",
					"resourcesCount", len(resources),
					"resourceType", resources[0].ResourceType)
			} else {
				// Update metrics for the number of resources processed
				r.TelemetryMetrics.MessagesSent.WithLabelValues(
					resources[0].ResourceType.String()).Add(float64(len(resources)))
				logger.Info("Sent resource to Dakr",
					"resourcesCount", len(resources),
					"resourceType", resources[0].ResourceType)
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
			//////////////////////////////////////////////////////////////////////////////////
			/// Namespaced resources
			//////////////////////////////////////////////////////////////////////////////////
			case "pod":
				replacedCollector = collector.NewPodCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedPods,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
				)
			case "deployment":
				replacedCollector = collector.NewDeploymentCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedDeployments,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
				)
			case "stateful_set":
				replacedCollector = collector.NewStatefulSetCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedStatefulSets,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
				)
			case "replica_set":
				replacedCollector = collector.NewReplicaSetCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedReplicaSet,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
				)
			case "daemon_set":
				replacedCollector = collector.NewDaemonSetCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedDaemonSets,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
				)
			case "service":
				replacedCollector = collector.NewServiceCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedServices,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
				)
			case "container_resource":
				// Use the reconciler's shared Prometheus metrics instance
				replacedCollector = collector.NewContainerResourceCollector(
					r.K8sClient,
					metricsClient,
					collector.ContainerResourceCollectorConfig{
						PrometheusURL:           newConfig.PrometheusURL,
						UpdateInterval:          newConfig.UpdateInterval,
						QueryTimeout:            10 * time.Second,
						DisableNetworkIOMetrics: newConfig.DisableNetworkIOMetrics,
						DisableGPUMetrics:       newConfig.DisableGPUMetrics,
					},
					newConfig.TargetNamespaces,
					newConfig.ExcludedPods,
					collector.DefaultMaxBatchSize,
					newConfig.UpdateInterval,
					logger,
					r.TelemetryMetrics,
				)
			case "persistent_volume_claim":
				replacedCollector = collector.NewPersistentVolumeClaimCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedPVCs,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
				)
			case "event":
				replacedCollector = collector.NewEventCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedEvents,
					10,
					10*time.Minute,
					400,
					collector.DefaultMaxBatchTime,
					logger,
				)
			case "job":
				replacedCollector = collector.NewJobCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedJobs,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
				)
			case "cron_job":
				replacedCollector = collector.NewCronJobCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedCronJobs,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
				)
			case "replication_controller":
				replacedCollector = collector.NewReplicationControllerCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedReplicationControllers,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
				)
			case "ingress":
				replacedCollector = collector.NewIngressCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedIngresses,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
				)
			case "network_policy":
				replacedCollector = collector.NewNetworkPolicyCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedNetworkPolicies,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
				)
			case "endpoints":
				replacedCollector = collector.NewEndpointCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedEndpoints,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
				)
			case "service_account":
				replacedCollector = collector.NewServiceAccountCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedServiceAccounts,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
				)
			case "limit_range":
				replacedCollector = collector.NewLimitRangeCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedLimitRanges,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
				)
			case "resource_quota":
				replacedCollector = collector.NewResourceQuotaCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedResourceQuotas,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
				)
			case "horizontal_pod_autoscaler":
				replacedCollector = collector.NewHorizontalPodAutoscalerCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedHPAs,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
				)
			case "vertical_pod_autoscaler":
				replacedCollector = collector.NewVerticalPodAutoscalerCollector(
					r.DynamicClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedVPAs,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
				)
			case "role":
				replacedCollector = collector.NewRoleCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedRoles,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
				)
			case "role_binding":
				replacedCollector = collector.NewRoleBindingCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedRoleBindings,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
				)
			case "pod_disruption_budget":
				replacedCollector = collector.NewPodDisruptionBudgetCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedPDBs,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
				)
			case "datadog":
				replacedCollector = collector.NewDatadogCollector(
					r.DynamicClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedDatadogReplicaSets,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
				)
			case "argo_rollouts":
				replacedCollector = collector.NewArgoRolloutsCollector(
					r.DynamicClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedArgoRollouts,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
				)
			//////////////////////////////////////////////////////////////////////////////////
			/// Cluster wide resources
			//////////////////////////////////////////////////////////////////////////////////
			case "node":
				// Use the reconciler's shared Prometheus metrics instance

				replacedCollector = collector.NewNodeCollector(
					r.K8sClient,
					metricsClient,
					collector.NodeCollectorConfig{
						PrometheusURL:           newConfig.PrometheusURL,
						UpdateInterval:          newConfig.UpdateInterval,
						QueryTimeout:            10 * time.Second,
						DisableNetworkIOMetrics: newConfig.DisableNetworkIOMetrics,
						DisableGPUMetrics:       newConfig.DisableGPUMetrics,
					},
					newConfig.ExcludedNodes,
					collector.DefaultMaxBatchSize,
					newConfig.UpdateInterval,
					logger,
					r.TelemetryMetrics,
				)
			case "persistent_volume":
				replacedCollector = collector.NewPersistentVolumeCollector(
					r.K8sClient,
					newConfig.ExcludedPVs,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
				)
			case "ingress_class":
				replacedCollector = collector.NewIngressClassCollector(
					r.K8sClient,
					newConfig.ExcludedIngressClasses,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
				)
			case "cluster_role":
				replacedCollector = collector.NewClusterRoleCollector(
					r.K8sClient,
					newConfig.ExcludedClusterRoles,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
				)
			case "cluster_role_binding":
				replacedCollector = collector.NewClusterRoleBindingCollector(
					r.K8sClient,
					newConfig.ExcludedClusterRoleBindings,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
				)
			case "storage_class":
				replacedCollector = collector.NewStorageClassCollector(
					r.K8sClient,
					newConfig.ExcludedStorageClasses,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
				)
			case "namespace":
				replacedCollector = collector.NewNamespaceCollector(
					r.K8sClient,
					newConfig.ExcludedNamespaces,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
				)
			case "csi_node":
				replacedCollector = collector.NewCSINodeCollector(
					r.K8sClient,
					newConfig.ExcludedCSINodes,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
				)
			case "karpenter":
				replacedCollector = collector.NewKarpenterCollector(
					r.DynamicClient,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
				)
			case "custom_resource_definition":
				replacedCollector = collector.NewCRDCollector(
					r.ApiExtensions,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
				)
			case "keda_scaled_job":
				replacedCollector = collector.NewScaledJobCollector(
					r.KEDAClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedKedaScaledJobs,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
				)
			case "keda_scaled_object":
				replacedCollector = collector.NewScaledObjectCollector(
					r.KEDAClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedKedaScaledObjects,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
				)
			case "cluster_snapshot":
				replacedCollector = snap.NewClusterSnapshotter(
					r.K8sClient,
					15*time.Minute,
					r.Sender,
					newConfig.TargetNamespaces,
					newConfig.ExcludedPods,
					newConfig.ExcludedNodes,
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

// waitForPrometheusAvailability checks if Prometheus is available with retry logic
// Returns true if Prometheus is available, false if not after maxRetries
func (r *CollectionPolicyReconciler) waitForPrometheusAvailability(ctx context.Context, prometheusURL string) bool {
	logger := r.Log.WithName("prometheus-check")

	if prometheusURL == "" {
		logger.Info("No Prometheus URL configured, skipping availability check")
		return true
	}

	logger.Info("Checking Prometheus availability", "url", prometheusURL)

	// Configuration for retry mechanism
	initialBackoff := 5 * time.Second
	maxBackoff := 2 * time.Minute
	backoff := initialBackoff
	maxRetries := 12 // About 25 minutes total with exponential backoff

	client := &http.Client{
		Timeout: 5 * time.Second,
	}

	// Endpoint to verify prometheus is ready
	healthEndpoint := fmt.Sprintf("%s/-/ready", prometheusURL)
	if !strings.HasPrefix(prometheusURL, "http") {
		healthEndpoint = fmt.Sprintf("http://%s/-/ready", prometheusURL)
	}

	for i := 0; i < maxRetries; i++ {
		select {
		case <-ctx.Done():
			logger.Info("Context cancelled while checking Prometheus availability")
			return false
		default:
			// Try to contact Prometheus
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthEndpoint, nil)
			if err != nil {
				logger.Error(err, "Failed to create request for Prometheus health check")
				time.Sleep(backoff)
				backoff = min(backoff*2, maxBackoff)
				continue
			}

			resp, err := client.Do(req)
			if err != nil {
				logger.Info("Prometheus not yet available",
					"attempt", i+1,
					"maxRetries", maxRetries,
					"backoff", backoff.String(),
					"error", err.Error())
				time.Sleep(backoff)
				backoff = min(backoff*2, maxBackoff)
				continue
			}

			// Check response status
			if resp.StatusCode == http.StatusOK {
				logger.Info("Prometheus is available", "statusCode", resp.StatusCode)
				resp.Body.Close()
				return true
			}

			logger.Info("Prometheus returned non-OK status",
				"statusCode", resp.StatusCode,
				"attempt", i+1,
				"maxRetries", maxRetries)
			resp.Body.Close()
			time.Sleep(backoff)
			backoff = min(backoff*2, maxBackoff)
		}
	}

	logger.Info("Prometheus availability check failed after maximum retries", "maxRetries", maxRetries)
	return false
}

// SetupWithManager sets up the controller with the Manager.
func (r *CollectionPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Set up basic components
	r.Log = util.NewLogger("controller")
	r.RestartInProgress = false

	// Create a Kubernetes clientset
	config := mgr.GetConfig()
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create kubernetes clientset: %w", err)
	}
	r.K8sClient = clientset

	// Create a periodic reconciliation instead of watching a CRD
	return ctrl.NewControllerManagedBy(mgr).
		// Instead of watching a CRD, we'll reconcile periodically
		// For(&corev1.ConfigMap{}). // Use a dummy object type like ConfigMap
		WithEventFilter(predicate.Funcs{
			// Return true only for the reconciler's initial sync
			CreateFunc:  func(e event.CreateEvent) bool { return false },
			UpdateFunc:  func(e event.UpdateEvent) bool { return false },
			DeleteFunc:  func(e event.DeleteEvent) bool { return false },
			GenericFunc: func(e event.GenericEvent) bool { return false },
		}).
		Complete(r)
}
