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

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	"github.com/devzero-inc/zxporter/internal/collector"
	"github.com/devzero-inc/zxporter/internal/collector/provider"
	"github.com/devzero-inc/zxporter/internal/collector/snap"
	telemetry_logger "github.com/devzero-inc/zxporter/internal/logger"
	"github.com/devzero-inc/zxporter/internal/server"
	"github.com/devzero-inc/zxporter/internal/transport"
	"github.com/devzero-inc/zxporter/internal/util"
	"github.com/devzero-inc/zxporter/internal/version"
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
	DakrClient        transport.DakrClient
	ApiExtensions     *apiextensionsclientset.Clientset
	CollectionManager *collector.CollectionManager
	FastReactionServer *server.FastReactionServer
	Sender            transport.DirectSender
	TelemetrySender   *transport.TelemetrySender
	TelemetryMetrics  *collector.TelemetryMetrics
	TelemetryLogger   telemetry_logger.Logger
	ZapLogger         *zap.Logger
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
	ExcludedCSIDrivers             []string
	ExcludedCSIStorageCapacities   []collector.ExcludedCSIStorageCapacity
	ExcludedVolumeAttachments      []string
	ExcludedKubeflowNotebooks      []collector.ExcludedKubeflowNotebook

	DisabledCollectors []string

	KubeContextName         string
	DakrURL                 string
	ClusterToken            string
	PrometheusURL           string
	DisableNetworkIOMetrics bool
	DisableGPUMetrics       bool
	UpdateInterval          time.Duration
	NodeMetricsInterval     time.Duration
	ClusterSnapshotInterval time.Duration
	BufferSize              int
	MaskSecretData          bool
	NumResourceProcessors   int
}

// ========================================
// COLLECTION POLICY CRD MANAGEMENT
// ========================================
//+kubebuilder:rbac:groups=devzero.io,resources=collectionpolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=devzero.io,resources=collectionpolicies/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=devzero.io,resources=collectionpolicies/finalizers,verbs=update

// ========================================
// BOOTSTRAP PERMISSIONS (entrypoint.sh)
// ========================================
// These permissions are required for automatic metrics-server installation
// via kubectl apply in entrypoint.sh when metrics-server is not detected

// ServiceAccount creation for metrics-server
//+kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch

// RBAC setup for metrics-server (ClusterRoles, ClusterRoleBindings, RoleBindings)
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=get;list;watch;create;update;patch
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;list;watch;create;update;patch
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch

// Service and Deployment creation for metrics-server
//+kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch

// APIService registration for metrics-server API
//+kubebuilder:rbac:groups=apiregistration.k8s.io,resources=apiservices,verbs=get;list;watch;create;update;patch

// ========================================
// RUNTIME PERMISSIONS
// ========================================
// ConfigMap access for cluster token persistence (ONLY write permission in runtime)
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;update

// Metrics access
//+kubebuilder:rbac:groups="",resources=nodes/metrics,verbs=get

// Core Kubernetes resources (READ-ONLY monitoring and collection)
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=pods/status,verbs=get
//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=nodes/status,verbs=get
//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=limitranges,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=resourcequotas,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=replicationcontrollers,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=endpoints,verbs=get;list;watch

// Apps API Group resources (READ-ONLY monitoring - note: deployments also needs write for metrics-server bootstrap)
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

// RBAC resources (READ-ONLY monitoring - note: write permissions declared above for bootstrap)
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles,verbs=get;list;watch

// Autoscaling API Group resources
//+kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch
//+kubebuilder:rbac:groups=autoscaling.k8s.io,resources=verticalpodautoscalers,verbs=get;list;watch

// Policy API Group resources
//+kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch

// Storage API Group resources
//+kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
//+kubebuilder:rbac:groups=storage.k8s.io,resources=csinodes,verbs=get;list;watch
//+kubebuilder:rbac:groups=storage.k8s.io,resources=csidrivers,verbs=get;list;watch
//+kubebuilder:rbac:groups=storage.k8s.io,resources=csistoragecapacities,verbs=get;list;watch
//+kubebuilder:rbac:groups=storage.k8s.io,resources=volumeattachments,verbs=get;list;watch

// ========================================
// OPTIONAL THIRD-PARTY RESOURCES
// ========================================
// These permissions are for optional third-party operators.
// All collectors gracefully handle missing CRDs and can be disabled via DisabledCollectors config.

// Karpenter node provisioning (optional - only if Karpenter operator installed)
//+kubebuilder:rbac:groups=karpenter.sh,resources=provisioners;machines;nodepools;nodeclaims;nodeoverlays,verbs=get;list;watch
//+kubebuilder:rbac:groups=karpenter.k8s.aws,resources=awsnodetemplates;ec2nodeclasses,verbs=get;list;watch
//+kubebuilder:rbac:groups=karpenter.azure.com,resources=aksnodeclasses,verbs=get;list;watch
//+kubebuilder:rbac:groups=karpenter.k8s.oracle,resources=ocinodeclasses,verbs=get;list;watch
//+kubebuilder:rbac:groups=karpenter.k8s.gcp,resources=gcenodeclasses,verbs=get;list;watch

// API Extensions (READ-ONLY for CRD discovery)
//+kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch

// Optional third-party monitoring integrations
//+kubebuilder:rbac:groups=datadoghq.com,resources=extendeddaemonsetreplicasets,verbs=get;list;watch
//+kubebuilder:rbac:groups=argoproj.io,resources=rollouts,verbs=get;list;watch
//+kubebuilder:rbac:groups=keda.sh,resources=scaledobjects;scaledjobs;triggerauthentications;clustertriggerauthentications,verbs=get;list;watch
//+kubebuilder:rbac:groups=kubeflow.org,resources=notebooks,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *CollectionPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Check if TelemetryLogger is nil
	if r.TelemetryLogger == nil {
		return ctrl.Result{}, fmt.Errorf("TelemetryLogger is nil")
	}

	// Create a new config object from the policy and environment
	newEnvSpec, err := util.LoadCollectionPolicySpecFromEnv()
	if err != nil {
		r.TelemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_ERROR,
			"CollectionPolicyReconciler_Reconcile",
			"Failed to load collection policy spec from environment variables",
			err,
			map[string]string{
				"error_type":       "env_config_load_failed",
				"zxporter_version": version.Get().String(),
			},
		)
		logger.Error(err, "Error loading ENV varaibles")
	}

	// Create a new config object from the policy and environment
	newConfig, configChanged := r.createNewConfig(&newEnvSpec, logger)

	// Check if we need to restart collectors due to config change
	if configChanged && r.IsRunning {
		r.TelemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_INFO,
			"CollectionPolicyReconciler_Reconcile",
			"Configuration changed, restarting collectors",
			nil,
			map[string]string{
				"config_changed":   fmt.Sprintf("%t", configChanged),
				"zxporter_version": version.Get().String(),
			},
		)
		logger.Info("Configuration changed, restarting collectors")
		return r.restartCollectors(ctx, newConfig)
	}

	// Initialize collection system if not already running
	if !r.IsRunning {
		logger.Info("Collection system not running, initializing")
		return r.initializeCollectors(ctx, newConfig)
	}

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

	if len(newConfig.DisabledCollectors) > 0 {
		logger.Info("Disabled collectors", "name", newConfig.DisabledCollectors)
	}

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

	// Set cluster snapshot interval (defaults to 3h for reduced network usage)
	clusterSnapshotIntervalStr := envSpec.Policies.ClusterSnapshotInterval
	if clusterSnapshotIntervalStr != "" {
		if interval, err := time.ParseDuration(clusterSnapshotIntervalStr); err == nil {
			newConfig.ClusterSnapshotInterval = interval
		} else {
			logger.Error(err, "Error parsing cluster snapshot interval", "interval", clusterSnapshotIntervalStr)
			newConfig.ClusterSnapshotInterval = 3 * time.Hour // Default to 3 hours
		}
	} else {
		newConfig.ClusterSnapshotInterval = 3 * time.Hour // Default to 3 hours
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

	// CSI
	newConfig.ExcludedCSIDrivers = envSpec.Exclusions.ExcludedCSIDrivers
	for _, csc := range envSpec.Exclusions.ExcludedCSIStorageCapacities {
		newConfig.ExcludedCSIStorageCapacities = append(newConfig.ExcludedCSIStorageCapacities, collector.ExcludedCSIStorageCapacity{
			Namespace: csc.Namespace,
			Name:      csc.Name,
		})
	}
	newConfig.ExcludedVolumeAttachments = envSpec.Exclusions.ExcludedVolumeAttachments

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

	if oldConfig.ClusterSnapshotInterval != newConfig.ClusterSnapshotInterval {
		affectedCollectors["cluster_snapshot"] = true
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

	if !reflect.DeepEqual(oldConfig.ExcludedCSIDrivers, newConfig.ExcludedCSIDrivers) {
		affectedCollectors["csi_driver"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedCSIStorageCapacities, newConfig.ExcludedCSIStorageCapacities) {
		affectedCollectors["csi_storage_capacity"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedVolumeAttachments, newConfig.ExcludedVolumeAttachments) {
		affectedCollectors["volume_attachment"] = true
	}

	if !reflect.DeepEqual(oldConfig.ExcludedKubeflowNotebooks, newConfig.ExcludedKubeflowNotebooks) {
		affectedCollectors["kubeflow_notebook"] = true
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

		r.TelemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_DEBUG,
			"CollectionPolicyReconciler_restartCollectors",
			"Prometheus or metrics configuration changed",
			nil,
			map[string]string{
				"prometheus_url":   fmt.Sprintf("%v", newConfig.PrometheusURL),
				"zxporter_version": version.Get().String(),
			},
		)

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
			r.TelemetryLogger.Report(
				gen.LogLevel_LOG_LEVEL_ERROR,
				"CollectionPolicyReconciler_restartCollectors",
				"Error handling disabled collectors change",
				err,
				map[string]string{
					"current_config":   fmt.Sprintf("%v", r.CurrentConfig),
					"new_config":       fmt.Sprintf("%v", newConfig),
					"zxporter_version": version.Get().String(),
				},
			)
			// Continue with other updates despite error
		}
	}

	// Identify which collectors need to be restarted
	affectedCollectors := r.identifyAffectedCollectors(r.CurrentConfig, newConfig)

	// If affectedCollectors is nil, it signals a full restart is needed
	if affectedCollectors == nil {
		logger.Info("Major configuration change detected, performing full restart")
		r.TelemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_WARN,
			"CollectionPolicyReconciler_restartCollectors",
			"Major configuration change detected, performing full restart",
			nil,
			map[string]string{
				"current_config":   fmt.Sprintf("%v", r.CurrentConfig),
				"new_config":       fmt.Sprintf("%v", newConfig),
				"zxporter_version": version.Get().String(),
			},
		)

		// Stop all existing collectors
		if r.CollectionManager != nil {
			logger.Info("Stopping all collectors")
			if err := r.CollectionManager.StopAll(); err != nil {
				logger.Error(err, "Error stopping collection manager")
				r.TelemetryLogger.Report(
					gen.LogLevel_LOG_LEVEL_ERROR,
					"CollectionPolicyReconciler_restartCollectors",
					"Error stopping collection manager",
					fmt.Errorf("error stopping collection manager"),
					map[string]string{
						"current_config":   fmt.Sprintf("%v", r.CurrentConfig),
						"new_config":       fmt.Sprintf("%v", newConfig),
						"zxporter_version": version.Get().String(),
					},
				)
			}
		}

		// Reset state
		r.IsRunning = false
		r.CollectionManager = nil
		r.CurrentConfig = nil

		// Initialize with new config
		r.TelemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_WARN,
			"CollectionPolicyReconciler_restartCollectors",
			"Initialize collectors with new config due to major config changes",
			nil,
			map[string]string{
				"current_config":   fmt.Sprintf("%v", r.CurrentConfig),
				"new_config":       fmt.Sprintf("%v", newConfig),
				"zxporter_version": version.Get().String(),
			},
		)
		return r.initializeCollectors(ctx, newConfig)
	}

	// If no collectors need to be restarted, just update the config
	if len(affectedCollectors) == 0 {
		logger.Info("No collectors affected by this configuration change")
		r.TelemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_INFO,
			"CollectionPolicyReconciler_restartCollectors",
			"No collectors affected by this configuration change",
			nil,
			map[string]string{
				"current_config":   fmt.Sprintf("%v", r.CurrentConfig),
				"new_config":       fmt.Sprintf("%v", newConfig),
				"zxporter_version": version.Get().String(),
			},
		)
		r.CurrentConfig = newConfig
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}

	logger.Info("Performing selective restart of affected collectors", "affectedCount", len(affectedCollectors))
	r.TelemetryLogger.Report(
		gen.LogLevel_LOG_LEVEL_WARN,
		"CollectionPolicyReconciler_restartCollectors",
		"Performing selective restart of affected collectors",
		nil,
		map[string]string{
			"affected_count":      fmt.Sprintf("%v", len(affectedCollectors)),
			"affected_collectors": fmt.Sprintf("%v", affectedCollectors),
			"zxporter_version":    version.Get().String(),
		},
	)

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
			r.TelemetryLogger.Report(
				gen.LogLevel_LOG_LEVEL_ERROR,
				"CollectionPolicyReconciler_restartCollectors",
				"Error stopping telemetry sender",
				err,
				map[string]string{
					"error_type":       "error_telemetry_sender",
					"zxporter_version": version.Get().String(),
				},
			)
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
			r.TelemetryLogger.Report(
				gen.LogLevel_LOG_LEVEL_ERROR,
				"CollectionPolicyReconciler_restartCollectors",
				"Failed to restart telemetry sender",
				err,
				map[string]string{
					"error_type":       "error_telemetry_sender",
					"zxporter_version": version.Get().String(),
				},
			)
		} else {
			logger.Info("Successfully restarted telemetry sender")
		}
	}

	// Now recreate and restart the affected collectors
	clientConfig := ctrl.GetConfigOrDie()
	metricsClient, err := metricsv1.NewForConfig(clientConfig)
	if err != nil {
		r.TelemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_ERROR,
			"CollectionPolicyReconciler_restartCollectors",
			"Failed to create metrics client, some collectors may be limited",
			err,
			map[string]string{
				"client_config":    fmt.Sprintf("%v", clientConfig),
				"zxporter_version": version.Get().String(),
			},
		)
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
				r.TelemetryLogger,
			)
		case "deployment":
			replacedCollector = collector.NewDeploymentCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedDeployments,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
			)
		case "stateful_set":
			replacedCollector = collector.NewStatefulSetCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedStatefulSets,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
			)
		case "replica_set":
			replacedCollector = collector.NewReplicaSetCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedReplicaSet,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
			)
		case "daemon_set":
			replacedCollector = collector.NewDaemonSetCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedDaemonSets,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
			)
		case "service":
			replacedCollector = collector.NewServiceCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedServices,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
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
				r.TelemetryLogger,
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
				r.TelemetryLogger,
			)
		case "persistent_volume_claim":
			replacedCollector = collector.NewPersistentVolumeClaimCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedPVCs,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
			)
		case "persistent_volume":
			replacedCollector = collector.NewPersistentVolumeCollector(
				r.K8sClient,
				newConfig.ExcludedPVs,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
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
				r.TelemetryLogger,
			)
		case "job":
			replacedCollector = collector.NewJobCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedJobs,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
			)
		case "cron_job":
			replacedCollector = collector.NewCronJobCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedCronJobs,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
			)
		case "replication_controller":
			replacedCollector = collector.NewReplicationControllerCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedReplicationControllers,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
			)
		case "ingress":
			replacedCollector = collector.NewIngressCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedIngresses,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
			)
		case "ingress_class":
			replacedCollector = collector.NewIngressClassCollector(
				r.K8sClient,
				newConfig.ExcludedIngressClasses,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
			)
		case "network_policy":
			replacedCollector = collector.NewNetworkPolicyCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedNetworkPolicies,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
			)
		case "endpoints":
			replacedCollector = collector.NewEndpointCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedEndpoints,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
			)
		case "service_account":
			replacedCollector = collector.NewServiceAccountCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedServiceAccounts,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
			)
		case "limit_range":
			replacedCollector = collector.NewLimitRangeCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedLimitRanges,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
			)
		case "resource_quota":
			replacedCollector = collector.NewResourceQuotaCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedResourceQuotas,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
			)
		case "horizontal_pod_autoscaler":
			replacedCollector = collector.NewHorizontalPodAutoscalerCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedHPAs,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
			)
		case "vertical_pod_autoscaler":
			replacedCollector = collector.NewVerticalPodAutoscalerCollector(
				r.DynamicClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedVPAs,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
			)
		case "role":
			replacedCollector = collector.NewRoleCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedRoles,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
			)
		case "role_binding":
			replacedCollector = collector.NewRoleBindingCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedRoleBindings,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
			)
		case "cluster_role":
			replacedCollector = collector.NewClusterRoleCollector(
				r.K8sClient,
				newConfig.ExcludedClusterRoles,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
			)
		case "cluster_role_binding":
			replacedCollector = collector.NewClusterRoleBindingCollector(
				r.K8sClient,
				newConfig.ExcludedClusterRoleBindings,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
			)
		case "pod_disruption_budget":
			replacedCollector = collector.NewPodDisruptionBudgetCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedPDBs,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
			)
		case "storage_class":
			replacedCollector = collector.NewStorageClassCollector(
				r.K8sClient,
				newConfig.ExcludedStorageClasses,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
			)
		case "csi_node":
			replacedCollector = collector.NewCSINodeCollector(
				r.K8sClient,
				newConfig.ExcludedNodes,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
			)
		case "karpenter":
			replacedCollector = collector.NewKarpenterCollector(
				r.DynamicClient,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
			)
		case "datadog":
			replacedCollector = collector.NewDatadogCollector(
				r.DynamicClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedDatadogReplicaSets,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
			)
		case "argo_rollouts":
			replacedCollector = collector.NewArgoRolloutsCollector(
				r.DynamicClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedArgoRollouts,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
			)
		case "custom_resource_definition":
			replacedCollector = collector.NewCRDCollector(
				r.ApiExtensions,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
			)
		case "keda_scaled_job":
			replacedCollector = collector.NewScaledJobCollector(
				r.KEDAClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedKedaScaledJobs,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
			)
		case "keda_scaled_object":
			replacedCollector = collector.NewScaledObjectCollector(
				r.KEDAClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedKedaScaledObjects,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
			)
		case "cluster_snapshot":
			replacedCollector = snap.NewClusterSnapshotter(
				r.K8sClient,
				r.KEDAClient,
				newConfig.ClusterSnapshotInterval,
				r.Sender,
				r.CollectionManager,
				newConfig.TargetNamespaces,
				newConfig.ExcludedPods,
				newConfig.ExcludedNodes,
				logger,
			)
		case "csi_driver":
			replacedCollector = collector.NewCSIDriverCollector(
				r.K8sClient,
				newConfig.ExcludedCSIDrivers,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
			)
		case "csi_storage_capacity":
			replacedCollector = collector.NewCSIStorageCapacityCollector(
				r.K8sClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedCSIStorageCapacities,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
			)
		case "volume_attachment":
			replacedCollector = collector.NewVolumeAttachmentCollector(
				r.K8sClient,
				newConfig.ExcludedVolumeAttachments,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
			)
		case "kubeflow_notebook":
			replacedCollector = collector.NewKubeflowNotebookCollector(
				r.DynamicClient,
				newConfig.TargetNamespaces,
				newConfig.ExcludedKubeflowNotebooks,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
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
			r.TelemetryLogger.Report(
				gen.LogLevel_LOG_LEVEL_ERROR,
				"CollectionPolicyReconciler_restartCollectors",
				"Failed to start collector",
				err,
				map[string]string{
					"event_type":       "resource_collectors_restart_failed",
					"collector_type":   collectorType,
					"zxporter_version": version.Get().String(),
				},
			)
		} else {
			r.TelemetryLogger.Report(
				gen.LogLevel_LOG_LEVEL_INFO,
				"CollectionPolicyReconciler_restartCollectors",
				"Successfully restarted collector",
				nil,
				map[string]string{
					"event_type":       "resource_collectors_restart_succeed",
					"collector_type":   collectorType,
					"zxporter_version": version.Get().String(),
				},
			)
			logger.Info("Successfully restarted collector", "type", collectorType)
		}
	}

	logger.Info("Completed selective restart of collectors")
	r.TelemetryLogger.Report(
		gen.LogLevel_LOG_LEVEL_INFO,
		"CollectionPolicyReconciler_restartCollectors",
		"Completed selective restart of collectors",
		nil,
		map[string]string{
			"affected_count":   fmt.Sprintf("%v", len(affectedCollectors)),
			"zxporter_version": version.Get().String(),
		},
	)
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
			r.TelemetryLogger.Report(
				gen.LogLevel_LOG_LEVEL_DEBUG,
				"CollectionPolicyReconciler_initializeCollectors",
				"Prometheus is not available after waiting, will continue initialization but metrics may be limited",
				nil,
				map[string]string{
					"prometheus_url":   fmt.Sprintf("%v", config.PrometheusURL),
					"zxporter_version": version.Get().String(),
				},
			)
			// We continue initialization, but log the warning
		} else {
			r.TelemetryLogger.Report(
				gen.LogLevel_LOG_LEVEL_INFO,
				"CollectionPolicyReconciler_initializeCollectors",
				"Prometheus is available, continuing with full metrics collection",
				nil,
				map[string]string{
					"prometheus_url":   fmt.Sprintf("%v", config.PrometheusURL),
					"zxporter_version": version.Get().String(),
				},
			)
			logger.Info("Prometheus is available, continuing with full metrics collection")
		}
	} else {
		r.TelemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_WARN,
			"CollectionPolicyReconciler_initializeCollectors",
			"Prometheus URL is empty in config, metrics will be limited",
			nil,
			map[string]string{
				"prometheus_url":   fmt.Sprintf("%v", config.PrometheusURL),
				"zxporter_version": version.Get().String(),
			},
		)
	}

	// Setup collection manager and basic services
	if err := r.setupCollectionManager(ctx, config, logger); err != nil {
		if r.TelemetryLogger != nil {
			r.TelemetryLogger.Report(
				gen.LogLevel_LOG_LEVEL_ERROR,
				"CollectionPolicyReconciler_initializeCollectors",
				"Failed to setup collection manager and basic services",
				err,
				map[string]string{
					"error_type":       "collection_manager_setup_failed",
					"dakr_url":         config.DakrURL,
					"zxporter_version": version.Get().String(),
				},
			)
		}
		return ctrl.Result{}, err
	}

	// Setup and start Fast Reaction Server
	if err := r.setupFastReactionServer(logger); err != nil {
		logger.Error(err, "Failed to setup fast reaction server")
		// We can continue even if this fails
	}

	r.TelemetryLogger.Report(
		gen.LogLevel_LOG_LEVEL_INFO,
		"CollectionPolicyReconciler_initializeCollectors",
		"Successfully setup collection manager and basic services",
		nil,
		map[string]string{
			"event_type":       "collection_manager_setup_success",
			"dakr_url":         config.DakrURL,
			"zxporter_version": version.Get().String(),
		},
	)

	// First register and start only the cluster collector
	clusterCollector, err := r.setupClusterCollector(ctx, logger, config)
	if err != nil {
		if r.TelemetryLogger != nil {
			r.TelemetryLogger.Report(
				gen.LogLevel_LOG_LEVEL_ERROR,
				"CollectionPolicyReconciler_initializeCollectors",
				"Failed to setup cluster collector",
				err,
				map[string]string{
					"error_type":       "cluster_collector_setup_failed",
					"zxporter_version": version.Get().String(),
				},
			)
		}
		return ctrl.Result{}, err
	}

	if r.TelemetryLogger != nil {
		r.TelemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_INFO,
			"CollectionPolicyReconciler_initializeCollectors",
			"Successfully setup cluster collector",
			nil,
			map[string]string{
				"event_type":       "cluster_collector_setup_success",
				"zxporter_version": version.Get().String(),
			},
		)
	}

	// Wait for cluster data to be sent to Dakr
	registrationResult := r.waitForClusterRegistration(ctx, logger)
	if registrationResult != nil {
		r.TelemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_ERROR,
			"CollectionPolicyReconciler_initializeCollectors",
			"Failed to register cluster with Dakr",
			fmt.Errorf("cluster registration failed or timed out"),
			map[string]string{
				"error_type":       "cluster_registration_failed",
				"dakr_url":         config.DakrURL,
				"zxporter_version": version.Get().String(),
			},
		)
		// Clean up and return the error result
		r.cleanupOnFailure(logger)
		return *registrationResult, fmt.Errorf("failed to register cluster with Dakr")
	}

	r.TelemetryLogger.Report(
		gen.LogLevel_LOG_LEVEL_INFO,
		"CollectionPolicyReconciler_initializeCollectors",
		"Successfully registered cluster with Dakr",
		nil,
		map[string]string{
			"event_type":       "cluster_registration_success",
			"dakr_url":         config.DakrURL,
			"zxporter_version": version.Get().String(),
		},
	)

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
		r.TelemetryLogger,
	)

	// Create and start the telemetry sender
	r.TelemetrySender = transport.NewTelemetrySender(
		logger.WithName("telemetry"),
		r.DakrClient, // Use the dakrClient directly
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

// setupFastReactionServer initializes and starts the gRPC server
func (r *CollectionPolicyReconciler) setupFastReactionServer(logger logr.Logger) error {
	r.FastReactionServer = server.NewFastReactionServer(logger)
	// Hardcoded port 50051 for now as per plan
	if err := r.FastReactionServer.Start(50051); err != nil {
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
		r.TelemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_ERROR,
			"CollectionPolicyReconciler_setupClusterCollector",
			"Failed to create metrics client",
			err,
			map[string]string{
				"client_config":    fmt.Sprintf("%v", clientConfig),
				"zxporter_version": version.Get().String(),
			},
		)
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
		r.TelemetryLogger,
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
		r.TelemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_INFO,
			"CollectionPolicyReconciler_waitForClusterRegistration",
			"Cluster registration completed, proceeding with other collectors",
			nil,
			map[string]string{
				"error_type":       "cluster_registration_succeed",
				"zxporter_version": version.Get().String(),
			},
		)
		return nil
	case <-clusterFailedCh:
		logger.Error(nil, "Cluster data registration failed after maximum attempts, operator entering error state")
		return &ctrl.Result{Requeue: true, RequeueAfter: 1 * time.Minute}
	case <-clusterTimeoutCh:
		logger.Info("Timeout waiting for cluster data")
		r.TelemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_ERROR,
			"CollectionPolicyReconciler_waitForClusterRegistration",
			"Timeout waiting for cluster data",
			fmt.Errorf("timeout waiting for cluster data"),
			map[string]string{
				"error_type":       "cluster_registration_failed",
				"zxporter_version": version.Get().String(),
			},
		)
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
			r.TelemetryLogger.Report(
				gen.LogLevel_LOG_LEVEL_ERROR,
				"CollectionPolicyReconciler_monitorClusterRegistration",
				"Resource channel closed while waiting for cluster data",
				fmt.Errorf("resourceChan closed unexpectedly"),
				map[string]string{
					"error_type":       "cluster_registration_failed",
					"zxporter_version": version.Get().String(),
				},
			)
			close(clusterFailedCh)
			return
		}

		// Only process cluster resources at this stage
		if len(resources) == 1 && resources[0].ResourceType == collector.Cluster {
			// Send the cluster data to Dakr
			_, err := r.Sender.Send(ctx, resources[0])
			if err != nil {
				logger.Error(err, "Failed to send cluster data")
				r.TelemetryLogger.Report(
					gen.LogLevel_LOG_LEVEL_ERROR,
					"CollectionPolicyReconciler_monitorClusterRegistration",
					"Failed to send cluster data for cluster registration",
					err,
					map[string]string{
						"error_type":       "cluster_registration_failed",
						"zxporter_version": version.Get().String(),
					},
				)
				close(clusterFailedCh)
				return
				// Continue waiting for next attempt
			} else {
				logger.Info("Successfully sent cluster data to Dakr")
				r.TelemetryLogger.Report(
					gen.LogLevel_LOG_LEVEL_INFO,
					"CollectionPolicyReconciler_monitorClusterRegistration",
					"Successfully sent cluster data to Dakr",
					nil,
					map[string]string{
						"event_type":       "cluster_data_sent",
						"zxporter_version": version.Get().String(),
					},
				)
				close(clusterRegisteredCh)
				return
			}
		} else {
			rLength := len(resources)
			rType := collector.Unknown
			if rLength == 1 {
				rType = resources[0].ResourceType
			}
			r.TelemetryLogger.Report(
				gen.LogLevel_LOG_LEVEL_ERROR,
				"CollectionPolicyReconciler_monitorClusterRegistration",
				"non cluster resource received",
				fmt.Errorf("expected cluster resource, got non cluster resource"),
				map[string]string{
					"error_type":       "cluster_registration_failed",
					"zxporter_version": version.Get().String(),
				},
			)
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

	if r.TelemetryLogger != nil {
		r.TelemetryLogger.Stop()
		logger.Info("Stopped telemetry logger during failure cleanup.")
	}

	// Stop telemetry sender if it's running
	if r.TelemetrySender != nil {
		if err := r.TelemetrySender.Stop(); err != nil {
			logger.Error(err, "Error stopping telemetry sender during failure")
		}
		r.TelemetrySender = nil
	}

	if r.FastReactionServer != nil {
		r.FastReactionServer.Stop()
		r.FastReactionServer = nil
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
		r.TelemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_ERROR,
			"CollectionPolicyReconciler_setupAllCollectors",
			"Error stopping collection manager",
			err,
			map[string]string{
				"error_type":       "resource_collectors_stopping_failed",
				"zxporter_version": version.Get().String(),
			},
		)
		logger.Error(err, "Error stopping collection manager")
	}

	// Create metrics client if needed for resource collectors
	clientConfig := ctrl.GetConfigOrDie()
	metricsClient, err := metricsv1.NewForConfig(clientConfig)
	if err != nil {
		r.TelemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_ERROR,
			"CollectionPolicyReconciler_setupAllCollectors",
			"Failed to create metrics client, some collectors may be limited",
			err,
			map[string]string{
				"client_config":    fmt.Sprintf("%v", clientConfig),
				"zxporter_version": version.Get().String(),
			},
		)
		logger.Error(err, "Failed to create metrics client, some collectors may be limited")
	}

	// Register all other collectors
	if err := r.registerResourceCollectors(logger, config, metricsClient); err != nil {
		r.TelemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_ERROR,
			"CollectionPolicyReconciler_setupAllCollectors",
			"Failed to register resource collectors",
			err,
			map[string]string{
				"error_type":       "resource_collectors_registration_failed",
				"zxporter_version": version.Get().String(),
			},
		)
		return err
	}

	r.TelemetryLogger.Report(
		gen.LogLevel_LOG_LEVEL_INFO,
		"CollectionPolicyReconciler",
		"Successfully registered all resource collectors",
		nil,
		map[string]string{
			"event_type":       "resource_collectors_registration_success",
			"zxporter_version": version.Get().String(),
		},
	)

	// Start the collection manager with all collectors
	if err := r.CollectionManager.StartAll(ctx); err != nil {
		r.TelemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_ERROR,
			"CollectionPolicyReconciler_setupAllCollectors",
			"Failed to start collection manager with all collectors",
			err,
			map[string]string{
				"error_type":       "resource_collectors_start_all_failed",
				"zxporter_version": version.Get().String(),
			},
		)
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
				r.TelemetryLogger,
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
				r.TelemetryLogger,
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
				r.TelemetryLogger,
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
				r.TelemetryLogger,
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
				r.TelemetryLogger,
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
				r.TelemetryLogger,
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
				r.TelemetryLogger,
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
				r.TelemetryLogger,
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
				r.TelemetryLogger,
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
				r.TelemetryLogger,
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
				r.TelemetryLogger,
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
				r.TelemetryLogger,
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
				r.TelemetryLogger,
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
				r.TelemetryLogger,
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
				r.TelemetryLogger,
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
				r.TelemetryLogger,
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
				r.TelemetryLogger,
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
				r.TelemetryLogger,
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
				r.TelemetryLogger,
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
				r.TelemetryLogger,
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
				r.TelemetryLogger,
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
				r.TelemetryLogger,
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
				r.TelemetryLogger,
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
				r.TelemetryLogger,
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
				r.TelemetryLogger,
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
				r.TelemetryLogger,
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
				r.TelemetryLogger,
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
				r.TelemetryLogger,
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
				r.TelemetryLogger,
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
				r.TelemetryLogger,
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
				r.TelemetryLogger,
			),
			name: collector.CSINode,
		},
		{
			collector: collector.NewKarpenterCollector(
				r.DynamicClient,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
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
				r.TelemetryLogger,
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
				r.TelemetryLogger,
			),
			name: collector.ArgoRollouts,
		},
		{
			collector: collector.NewCRDCollector(
				r.ApiExtensions,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
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
				r.TelemetryLogger,
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
				r.TelemetryLogger,
			),
			name: collector.KedaScaledObject,
		},
		{
			collector: snap.NewClusterSnapshotter(
				r.K8sClient,
				r.KEDAClient,
				config.ClusterSnapshotInterval,
				r.Sender,
				r.CollectionManager,
				config.TargetNamespaces,
				config.ExcludedPods,
				config.ExcludedNodes,
				logger,
			),
			name: collector.ClusterSnapshot,
		},
		{
			collector: collector.NewCSIDriverCollector(
				r.K8sClient,
				config.ExcludedCSIDrivers,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
			),
			name: collector.CSIDriver,
		},
		{
			collector: collector.NewCSIStorageCapacityCollector(
				r.K8sClient,
				config.TargetNamespaces,
				config.ExcludedCSIStorageCapacities,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
			),
			name: collector.CSIStorageCapacity,
		},
		{
			collector: collector.NewVolumeAttachmentCollector(
				r.K8sClient,
				config.ExcludedVolumeAttachments,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
			),
			name: collector.VolumeAttachment,
		},
		{
			collector: collector.NewKubeflowNotebookCollector(
				r.DynamicClient,
				config.TargetNamespaces,
				config.ExcludedKubeflowNotebooks,
				collector.DefaultMaxBatchSize,
				collector.DefaultMaxBatchTime,
				logger,
				r.TelemetryLogger,
			),
			name: collector.KubeflowNotebook,
		},
	}

	// Register all collectors
	for _, c := range collectors {
		if disabledCollectorsMap[c.name.String()] {
			r.TelemetryLogger.Report(
				gen.LogLevel_LOG_LEVEL_WARN,
				"CollectionPolicyReconciler_registerResourceCollectors",
				"Collector is disabled, skipping registration",
				nil,
				map[string]string{
					"collector_type":   c.name.String(),
					"event_type":       "collector_disabled",
					"zxporter_version": version.Get().String(),
				},
			)
			logger.Info("Skipping disabled collector", "type", c.name.String())
			continue
		}
		if c.collector.IsAvailable(context.Background()) {
			logger.Info("Registering collector", "name", c.name.String())
			if err := r.CollectionManager.RegisterCollector(c.collector); err != nil {
				r.TelemetryLogger.Report(
					gen.LogLevel_LOG_LEVEL_ERROR,
					"CollectionPolicyReconciler_registerResourceCollectors",
					"Failed to register collector",
					err,
					map[string]string{
						"collector_type":   c.name.String(),
						"error_type":       "collector_registration_failed",
						"zxporter_version": version.Get().String(),
					},
				)
				logger.Error(err, "Failed to register collector", "collector", c.name.String())
			} else {
				r.TelemetryLogger.Report(
					gen.LogLevel_LOG_LEVEL_INFO,
					"CollectionPolicyReconciler_registerResourceCollectors",
					"Successfully registered collector",
					nil,
					map[string]string{
						"collector_type":   c.name.String(),
						"event_type":       "collector_registration_success",
						"zxporter_version": version.Get().String(),
					},
				)
				logger.Info("Registered collector", "collector", c.name.String())
			}
		} else {
			r.TelemetryLogger.Report(
				gen.LogLevel_LOG_LEVEL_WARN,
				"CollectionPolicyReconciler_registerResourceCollectors",
				"Collector is not available, skipping registration",
				nil,
				map[string]string{ // Note: we should get the reason why it's not available
					"collector_type":   c.name.String(),
					"event_type":       "collector_not_available",
					"zxporter_version": version.Get().String(),
				},
			)
			logger.Info("Collector not available, skipping registration", "type", c.name.String())
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
				r.TelemetryLogger.Report(
					gen.LogLevel_LOG_LEVEL_ERROR,
					"CollectionPolicyReconciler_processCollectedResources",
					"Failed to send resource to Dakr",
					err,
					map[string]string{
						"resources_count":  fmt.Sprintf("%v", len(resources)),
						"zxporter_version": version.Get().String(),
					},
				)
				logger.Error(err, "Failed to send resource to Dakr",
					"resourcesCount", len(resources),
					"resourceType", resources[0].ResourceType)
			} else {
				// Update metrics for the number of resources processed
				r.TelemetryMetrics.MessagesSent.WithLabelValues(
					resources[0].ResourceType.String()).Add(float64(len(resources)))
			}

			// Broadcast to Fast Reaction subscribers if it's container metrics
			if r.FastReactionServer != nil && resources[0].ResourceType == collector.ContainerResource {
				for _, res := range resources {
					if data, ok := res.Object.(map[string]interface{}); ok {
						r.FastReactionServer.PublishMetrics(data, res.Timestamp)
					}
				}
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
				r.TelemetryLogger.Report(
					gen.LogLevel_LOG_LEVEL_ERROR,
					"CollectionPolicyReconciler_handleDisabledCollectorsChange",
					"Failed to deregister collector",
					err,
					map[string]string{
						"collector_type":   fmt.Sprintf("%v", collectorType),
						"zxporter_version": version.Get().String(),
					},
				)
				// Continue with other collectors even if one fails
			}
		}
	}

	// Find collectors that need to be registered (they were disabled before but now are enabled)
	clientConfig := ctrl.GetConfigOrDie()
	metricsClient, err := metricsv1.NewForConfig(clientConfig)
	if err != nil {
		r.TelemetryLogger.Report(
			gen.LogLevel_LOG_LEVEL_ERROR,
			"CollectionPolicyReconciler_handleDisabledCollectorsChange",
			"Failed to create metrics client for newly enabled collectors",
			err,
			map[string]string{
				"client_config":    fmt.Sprintf("%v", clientConfig),
				"zxporter_version": version.Get().String(),
			},
		)
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
					r.TelemetryLogger,
				)
			case "deployment":
				replacedCollector = collector.NewDeploymentCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedDeployments,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
					r.TelemetryLogger,
				)
			case "stateful_set":
				replacedCollector = collector.NewStatefulSetCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedStatefulSets,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
					r.TelemetryLogger,
				)
			case "replica_set":
				replacedCollector = collector.NewReplicaSetCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedReplicaSet,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
					r.TelemetryLogger,
				)
			case "daemon_set":
				replacedCollector = collector.NewDaemonSetCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedDaemonSets,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
					r.TelemetryLogger,
				)
			case "service":
				replacedCollector = collector.NewServiceCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedServices,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
					r.TelemetryLogger,
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
					r.TelemetryLogger,
				)
			case "persistent_volume_claim":
				replacedCollector = collector.NewPersistentVolumeClaimCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedPVCs,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
					r.TelemetryLogger,
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
					r.TelemetryLogger,
				)
			case "job":
				replacedCollector = collector.NewJobCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedJobs,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
					r.TelemetryLogger,
				)
			case "cron_job":
				replacedCollector = collector.NewCronJobCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedCronJobs,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
					r.TelemetryLogger,
				)
			case "replication_controller":
				replacedCollector = collector.NewReplicationControllerCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedReplicationControllers,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
					r.TelemetryLogger,
				)
			case "ingress":
				replacedCollector = collector.NewIngressCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedIngresses,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
					r.TelemetryLogger,
				)
			case "network_policy":
				replacedCollector = collector.NewNetworkPolicyCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedNetworkPolicies,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
					r.TelemetryLogger,
				)
			case "endpoints":
				replacedCollector = collector.NewEndpointCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedEndpoints,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
					r.TelemetryLogger,
				)
			case "service_account":
				replacedCollector = collector.NewServiceAccountCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedServiceAccounts,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
					r.TelemetryLogger,
				)
			case "limit_range":
				replacedCollector = collector.NewLimitRangeCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedLimitRanges,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
					r.TelemetryLogger,
				)
			case "resource_quota":
				replacedCollector = collector.NewResourceQuotaCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedResourceQuotas,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
					r.TelemetryLogger,
				)
			case "horizontal_pod_autoscaler":
				replacedCollector = collector.NewHorizontalPodAutoscalerCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedHPAs,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
					r.TelemetryLogger,
				)
			case "vertical_pod_autoscaler":
				replacedCollector = collector.NewVerticalPodAutoscalerCollector(
					r.DynamicClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedVPAs,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
					r.TelemetryLogger,
				)
			case "role":
				replacedCollector = collector.NewRoleCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedRoles,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
					r.TelemetryLogger,
				)
			case "role_binding":
				replacedCollector = collector.NewRoleBindingCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedRoleBindings,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
					r.TelemetryLogger,
				)
			case "pod_disruption_budget":
				replacedCollector = collector.NewPodDisruptionBudgetCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedPDBs,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
					r.TelemetryLogger,
				)
			case "datadog":
				replacedCollector = collector.NewDatadogCollector(
					r.DynamicClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedDatadogReplicaSets,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
					r.TelemetryLogger,
				)
			case "argo_rollouts":
				replacedCollector = collector.NewArgoRolloutsCollector(
					r.DynamicClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedArgoRollouts,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
					r.TelemetryLogger,
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
					r.TelemetryLogger,
				)
			case "persistent_volume":
				replacedCollector = collector.NewPersistentVolumeCollector(
					r.K8sClient,
					newConfig.ExcludedPVs,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
					r.TelemetryLogger,
				)
			case "ingress_class":
				replacedCollector = collector.NewIngressClassCollector(
					r.K8sClient,
					newConfig.ExcludedIngressClasses,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
					r.TelemetryLogger,
				)
			case "cluster_role":
				replacedCollector = collector.NewClusterRoleCollector(
					r.K8sClient,
					newConfig.ExcludedClusterRoles,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
					r.TelemetryLogger,
				)
			case "cluster_role_binding":
				replacedCollector = collector.NewClusterRoleBindingCollector(
					r.K8sClient,
					newConfig.ExcludedClusterRoleBindings,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
					r.TelemetryLogger,
				)
			case "storage_class":
				replacedCollector = collector.NewStorageClassCollector(
					r.K8sClient,
					newConfig.ExcludedStorageClasses,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
					r.TelemetryLogger,
				)
			case "namespace":
				replacedCollector = collector.NewNamespaceCollector(
					r.K8sClient,
					newConfig.ExcludedNamespaces,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
					r.TelemetryLogger,
				)
			case "csi_node":
				replacedCollector = collector.NewCSINodeCollector(
					r.K8sClient,
					newConfig.ExcludedCSINodes,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
					r.TelemetryLogger,
				)
			case "karpenter":
				replacedCollector = collector.NewKarpenterCollector(
					r.DynamicClient,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
					r.TelemetryLogger,
				)
			case "custom_resource_definition":
				replacedCollector = collector.NewCRDCollector(
					r.ApiExtensions,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
					r.TelemetryLogger,
				)
			case "keda_scaled_job":
				replacedCollector = collector.NewScaledJobCollector(
					r.KEDAClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedKedaScaledJobs,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
					r.TelemetryLogger,
				)
			case "keda_scaled_object":
				replacedCollector = collector.NewScaledObjectCollector(
					r.KEDAClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedKedaScaledObjects,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
					r.TelemetryLogger,
				)
			case "cluster_snapshot":
				replacedCollector = snap.NewClusterSnapshotter(
					r.K8sClient,
					r.KEDAClient,
					newConfig.ClusterSnapshotInterval,
					r.Sender,
					r.CollectionManager,
					newConfig.TargetNamespaces,
					newConfig.ExcludedPods,
					newConfig.ExcludedNodes,
					logger,
				)
			case "csi_driver":
				replacedCollector = collector.NewCSIDriverCollector(
					r.K8sClient,
					newConfig.ExcludedCSIDrivers,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
					r.TelemetryLogger,
				)
			case "csi_storage_capacity":
				replacedCollector = collector.NewCSIStorageCapacityCollector(
					r.K8sClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedCSIStorageCapacities,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
					r.TelemetryLogger,
				)
			case "volume_attachment":
				replacedCollector = collector.NewVolumeAttachmentCollector(
					r.K8sClient,
					newConfig.ExcludedVolumeAttachments,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
					r.TelemetryLogger,
				)
			case "kubeflow_notebook":
				replacedCollector = collector.NewKubeflowNotebookCollector(
					r.DynamicClient,
					newConfig.TargetNamespaces,
					newConfig.ExcludedKubeflowNotebooks,
					collector.DefaultMaxBatchSize,
					collector.DefaultMaxBatchTime,
					logger,
					r.TelemetryLogger,
				)
			default:
				logger.Info("Unknown collector type, skipping", "type", collectorType)
				continue
			}

			// Register and start the collector
			if err := r.CollectionManager.RegisterCollector(replacedCollector); err != nil {
				logger.Error(err, "Failed to register collector", "type", collectorType)
				r.TelemetryLogger.Report(
					gen.LogLevel_LOG_LEVEL_ERROR,
					"CollectionPolicyReconciler_handleDisabledCollectorsChange",
					"Failed to register collector",
					err,
					map[string]string{
						"collector_type":   fmt.Sprintf("%v", collectorType),
						"zxporter_version": version.Get().String(),
					},
				)
				continue
			}

			if err := r.CollectionManager.StartCollector(ctx, collectorType); err != nil {
				logger.Error(err, "Failed to start collector", "type", collectorType)
				r.TelemetryLogger.Report(
					gen.LogLevel_LOG_LEVEL_ERROR,
					"CollectionPolicyReconciler_handleDisabledCollectorsChange",
					"Failed to start collector",
					err,
					map[string]string{
						"collector_type":   fmt.Sprintf("%v", collectorType),
						"zxporter_version": version.Get().String(),
					},
				)
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

	// Clone DefaultTransport to preserve proxy settings, TLS config, and connection pooling
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DisableCompression = false // Ensure gzip compression is enabled for responses
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: tr,
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
				r.TelemetryLogger.Report(
					gen.LogLevel_LOG_LEVEL_ERROR,
					"CollectionPolicyReconciler_waitForPrometheusAvailability",
					"Failed to start collector",
					err,
					map[string]string{
						"prometheus_url":   prometheusURL,
						"zxporter_version": version.Get().String(),
					},
				)
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
			r.TelemetryLogger.Report(
				gen.LogLevel_LOG_LEVEL_ERROR,
				"CollectionPolicyReconciler_waitForPrometheusAvailability",
				"Prometheus returned non-OK status",
				err,
				map[string]string{
					"prometheus_url":   prometheusURL,
					"zxporter_version": version.Get().String(),
				},
			)
			resp.Body.Close()
			time.Sleep(backoff)
			backoff = min(backoff*2, maxBackoff)
		}
	}

	logger.Info("Prometheus availability check failed after maximum retries", "maxRetries", maxRetries)
	r.TelemetryLogger.Report(
		gen.LogLevel_LOG_LEVEL_ERROR,
		"CollectionPolicyReconciler_waitForPrometheusAvailability",
		"Prometheus availability check failed after maximum retries",
		fmt.Errorf("prometheus not available"),
		map[string]string{
			"prometheus_url":   prometheusURL,
			"max_retries":      fmt.Sprintf("%v", maxRetries),
			"zxporter_version": version.Get().String(),
		},
	)
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
