// internal/util/env.go
package util

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	v1 "github.com/devzero-inc/zxporter/api/v1"
)

// Configuration environment variables
const (
	// NUM_RESOURCE_SENDERS is the number of sender threads for sending resource data to control plane
	_ENV_NUM_RESOURCE_SENDERS = "NUM_RESOURCE_SENDERS"

	// KUBE_CONTEXT_NAME is the name of the current context being used to apply the installation yaml
	// Default value: ""
	_ENV_KUBE_CONTEXT_NAME = "KUBE_CONTEXT_NAME"

	// CLUSTER_TOKEN is the cluster token used to authenticate as a cluster
	// Default value: ""
	_ENV_CLUSTER_TOKEN = "CLUSTER_TOKEN"

	// PAT_TOKEN is the Personal Access Token used for automatic cluster token exchange
	// Default value: ""
	_ENV_PAT_TOKEN = "PAT_TOKEN"

	// DAKR_URL is the URL of the Dakr service.
	// Default value: ""
	_ENV_DAKR_URL = "DAKR_URL"

	// COLLECTION_FREQUENCY is how often to collect resource usage metrics.
	// Default value: 10s
	_ENV_COLLECTION_FREQUENCY = "COLLECTION_FREQUENCY"

	// BUFFER_SIZE is the size of the sender buffer.
	// Default value: 1000
	_ENV_BUFFER_SIZE = "BUFFER_SIZE"

	// EXCLUDED_NAMESPACES are namespaces to exclude from collection.
	// This is a comma-separated list.
	// Default value: []
	_ENV_EXCLUDED_NAMESPACES = "EXCLUDED_NAMESPACES"

	// EXCLUDED_NODES are nodes to exclude from collection.
	// This is a comma-separated list.
	// Default value: []
	_ENV_EXCLUDED_NODES = "EXCLUDED_NODES"

	// TARGET_NAMESPACES are namespaces to include in collection (empty means all).
	// This is a comma-separated list.
	// Default value: [] (all namespaces)
	_ENV_TARGET_NAMESPACES = "TARGET_NAMESPACES"

	// PROMETHEUS_URL is the URL of the Prometheus server.
	// Default value: ""
	_ENV_PROMETHEUS_URL = "PROMETHEUS_URL"

	// DISABLE_NETWORK_IO_METRICS determines whether to disable network IO metrics.
	// Default value: false
	_ENV_DISABLE_NETWORK_IO_METRICS = "DISABLE_NETWORK_IO_METRICS"

	// DISABLE_GPU_METRICS determines whether to disable GPU metrics.
	// Default value: false
	_ENV_DISABLE_GPU_METRICS = "DISABLE_GPU_METRICS"

	// MASK_SECRET_DATA determines whether to redact secret values.
	// Default value: false
	_ENV_MASK_SECRET_DATA = "MASK_SECRET_DATA"

	// NODE_METRICS_INTERVAL is how often to collect node metrics.
	// Default value: collection frequency * 6
	_ENV_NODE_METRICS_INTERVAL = "NODE_METRICS_INTERVAL"

	// CLUSTER_SNAPSHOT_INTERVAL is how often to take cluster snapshots.
	// Default value: 3h
	_ENV_CLUSTER_SNAPSHOT_INTERVAL = "CLUSTER_SNAPSHOT_INTERVAL"

	// WATCHED_CRDS is a list of custom resource definitions to explicitly watch.
	// This is a comma-separated list.
	// Default value: []
	_ENV_WATCHED_CRDS = "WATCHED_CRDS"

	// DISABLED_COLLECTORS is a list of collector types to completely disable.
	// This is a comma-separated list.
	// Default value: []
	_ENV_DISABLED_COLLECTORS = "DISABLED_COLLECTORS"

	// Exclusions for specific resources follow the format:
	// EXCLUDED_<RESOURCE>_<NAMESPACE>_<n>
	// Example: EXCLUDED_POD_default_my-pod=true

	// EXCLUDED_PODS is a JSON-encoded list of pods to exclude.
	// Format: [{"namespace":"default","name":"pod1"},{"namespace":"kube-system","name":"pod2"}]
	// Default value: []
	_ENV_EXCLUDED_PODS = "EXCLUDED_PODS"

	// EXCLUDED_DEPLOYMENTS is a JSON-encoded list of deployments to exclude.
	// Format: [{"namespace":"default","name":"deploy1"},{"namespace":"kube-system","name":"deploy2"}]
	// Default value: []
	_ENV_EXCLUDED_DEPLOYMENTS = "EXCLUDED_DEPLOYMENTS"

	// EXCLUDED_STATEFULSETS is a JSON-encoded list of statefulsets to exclude.
	// Format: [{"namespace":"default","name":"sts1"},{"namespace":"kube-system","name":"sts2"}]
	// Default value: []
	_ENV_EXCLUDED_STATEFULSETS = "EXCLUDED_STATEFULSETS"

	// EXCLUDED_DAEMONSETS is a JSON-encoded list of daemonsets to exclude.
	// Format: [{"namespace":"default","name":"ds1"},{"namespace":"kube-system","name":"ds2"}]
	// Default value: []
	_ENV_EXCLUDED_DAEMONSETS = "EXCLUDED_DAEMONSETS"

	// EXCLUDED_SERVICES is a JSON-encoded list of services to exclude.
	// Format: [{"namespace":"default","name":"svc1"},{"namespace":"kube-system","name":"svc2"}]
	// Default value: []
	_ENV_EXCLUDED_SERVICES = "EXCLUDED_SERVICES"

	// EXCLUDED_PVCS is a JSON-encoded list of persistent volume claims to exclude.
	// Format: [{"namespace":"default","name":"pvc1"},{"namespace":"kube-system","name":"pvc2"}]
	// Default value: []
	_ENV_EXCLUDED_PVCS = "EXCLUDED_PVCS"

	// EXCLUDED_EVENTS is a JSON-encoded list of events to exclude.
	// Format: [{"namespace":"default","reason":"Scheduled","source":"default-scheduler",
	//           "involvedObjectKind":"Pod","involvedObjectName":"pod1"}]
	// Default value: []
	_ENV_EXCLUDED_EVENTS = "EXCLUDED_EVENTS"

	// EXCLUDED_JOBS is a JSON-encoded list of jobs to exclude.
	// Format: [{"namespace":"default","name":"job1"},{"namespace":"kube-system","name":"job2"}]
	// Default value: []
	_ENV_EXCLUDED_JOBS = "EXCLUDED_JOBS"

	// EXCLUDED_CRONJOBS is a JSON-encoded list of cronjobs to exclude.
	// Format: [{"namespace":"default","name":"cron1"},{"namespace":"kube-system","name":"cron2"}]
	// Default value: []
	_ENV_EXCLUDED_CRONJOBS = "EXCLUDED_CRONJOBS"

	// EXCLUDED_REPLICATIONCONTROLLERS is a JSON-encoded list of replication controllers to exclude.
	// Format: [{"namespace":"default","name":"rc1"},{"namespace":"kube-system","name":"rc2"}]
	// Default value: []
	_ENV_EXCLUDED_REPLICATIONCONTROLLERS = "EXCLUDED_REPLICATIONCONTROLLERS"

	// EXCLUDED_INGRESSES is a JSON-encoded list of ingresses to exclude.
	// Format: [{"namespace":"default","name":"ing1"},{"namespace":"kube-system","name":"ing2"}]
	// Default value: []
	_ENV_EXCLUDED_INGRESSES = "EXCLUDED_INGRESSES"

	// EXCLUDED_INGRESSCLASSES is a comma-separated list of ingress classes to exclude.
	// Default value: []
	_ENV_EXCLUDED_INGRESSCLASSES = "EXCLUDED_INGRESSCLASSES"

	// EXCLUDED_NETWORKPOLICIES is a JSON-encoded list of network policies to exclude.
	// Format: [{"namespace":"default","name":"netpol1"},{"namespace":"kube-system","name":"netpol2"}]
	// Default value: []
	_ENV_EXCLUDED_NETWORKPOLICIES = "EXCLUDED_NETWORKPOLICIES"

	// EXCLUDED_ENDPOINTS is a JSON-encoded list of endpoints to exclude.
	// Format: [{"namespace":"default","name":"ep1"},{"namespace":"kube-system","name":"ep2"}]
	// Default value: []
	_ENV_EXCLUDED_ENDPOINTS = "EXCLUDED_ENDPOINTS"

	// EXCLUDED_SERVICEACCOUNTS is a JSON-encoded list of service accounts to exclude.
	// Format: [{"namespace":"default","name":"sa1"},{"namespace":"kube-system","name":"sa2"}]
	// Default value: []
	_ENV_EXCLUDED_SERVICEACCOUNTS = "EXCLUDED_SERVICEACCOUNTS"

	// EXCLUDED_LIMITRANGES is a JSON-encoded list of limit ranges to exclude.
	// Format: [{"namespace":"default","name":"lr1"},{"namespace":"kube-system","name":"lr2"}]
	// Default value: []
	_ENV_EXCLUDED_LIMITRANGES = "EXCLUDED_LIMITRANGES"

	// EXCLUDED_RESOURCEQUOTAS is a JSON-encoded list of resource quotas to exclude.
	// Format: [{"namespace":"default","name":"rq1"},{"namespace":"kube-system","name":"rq2"}]
	// Default value: []
	_ENV_EXCLUDED_RESOURCEQUOTAS = "EXCLUDED_RESOURCEQUOTAS"

	// EXCLUDED_HPAS is a JSON-encoded list of horizontal pod autoscalers to exclude.
	// Format: [{"namespace":"default","name":"hpa1"},{"namespace":"kube-system","name":"hpa2"}]
	// Default value: []
	_ENV_EXCLUDED_HPAS = "EXCLUDED_HPAS"

	// EXCLUDED_VPAS is a JSON-encoded list of vertical pod autoscalers to exclude.
	// Format: [{"namespace":"default","name":"vpa1"},{"namespace":"kube-system","name":"vpa2"}]
	// Default value: []
	_ENV_EXCLUDED_VPAS = "EXCLUDED_VPAS"

	// EXCLUDED_ROLES is a JSON-encoded list of roles to exclude.
	// Format: [{"namespace":"default","name":"role1"},{"namespace":"kube-system","name":"role2"}]
	// Default value: []
	_ENV_EXCLUDED_ROLES = "EXCLUDED_ROLES"

	// EXCLUDED_ROLEBINDINGS is a JSON-encoded list of role bindings to exclude.
	// Format: [{"namespace":"default","name":"rb1"},{"namespace":"kube-system","name":"rb2"}]
	// Default value: []
	_ENV_EXCLUDED_ROLEBINDINGS = "EXCLUDED_ROLEBINDINGS"

	// EXCLUDED_CLUSTERROLES is a comma-separated list of cluster roles to exclude.
	// Default value: []
	_ENV_EXCLUDED_CLUSTERROLES = "EXCLUDED_CLUSTERROLES"

	// EXCLUDED_CLUSTERROLEBINDINGS is a comma-separated list of cluster role bindings to exclude.
	// Default value: []
	_ENV_EXCLUDED_CLUSTERROLEBINDINGS = "EXCLUDED_CLUSTERROLEBINDINGS"

	// EXCLUDED_PDBS is a JSON-encoded list of pod disruption budgets to exclude.
	// Format: [{"namespace":"default","name":"pdb1"},{"namespace":"kube-system","name":"pdb2"}]
	// Default value: []
	_ENV_EXCLUDED_PDBS = "EXCLUDED_PDBS"

	// EXCLUDED_PSPS is a comma-separated list of pod security policies to exclude.
	// Default value: []
	_ENV_EXCLUDED_PSPS = "EXCLUDED_PSPS"

	// EXCLUDED_PVS is a comma-separated list of persistent volumes to exclude.
	// Default value: []
	_ENV_EXCLUDED_PVS = "EXCLUDED_PVS"

	// EXCLUDED_STORAGECLASSES is a comma-separated list of storage classes to exclude.
	// Default value: []
	_ENV_EXCLUDED_STORAGECLASSES = "EXCLUDED_STORAGECLASSES"

	// EXCLUDED_CRDS is a comma-separated list of custom resource definitions to exclude.
	// Default value: []
	_ENV_EXCLUDED_CRDS = "EXCLUDED_CRDS"

	// EXCLUDED_CRDGROUPS is a comma-separated list of custom resource definition groups to exclude.
	// Default value: []
	_ENV_EXCLUDED_CRDGROUPS = "EXCLUDED_CRDGROUPS"

	// USE_SECRET_FOR_TOKEN determines whether to store tokens in Secrets vs ConfigMap
	// Default value: false (ConfigMap storage for backward compatibility)
	_ENV_USE_SECRET_FOR_TOKEN = "USE_SECRET_FOR_TOKEN"

	// TOKEN_CREDENTIALS_SECRET_NAME specifies the name of the Secret for input credentials (PAT/CLUSTER tokens)
	// Default value: "devzero-zxporter-credentials"
	_ENV_TOKEN_CREDENTIALS_SECRET_NAME = "TOKEN_CREDENTIALS_SECRET_NAME"

	// TOKEN_RUNTIME_SECRET_NAME specifies the name of the Secret for runtime token storage (exchanged tokens)
	// Default value: "devzero-zxporter-token"
	_ENV_TOKEN_RUNTIME_SECRET_NAME = "TOKEN_RUNTIME_SECRET_NAME"

	// TOKEN_SECRET_NAME specifies the name of the Secret for token storage (deprecated, use TOKEN_RUNTIME_SECRET_NAME)
	// Default value: "devzero-zxporter-token"
	_ENV_TOKEN_SECRET_NAME = "TOKEN_SECRET_NAME"

	// TOKEN_CONFIGMAP_NAME specifies the name of the ConfigMap for token storage
	// Default value: "devzero-zxporter-env-config"
	_ENV_TOKEN_CONFIGMAP_NAME = "TOKEN_CONFIGMAP_NAME"
)

const configVolumeMountPath = "/etc/zxporter/config"

func getEnv(key string) string {
	if data := os.Getenv(key); data != "" {
		return data
	}
	// Fallback to reading from file mounted at /etc/zxporter/config/<key>
	filePath := fmt.Sprintf("%s/%s", configVolumeMountPath, key)
	if data, err := os.ReadFile(filePath); err == nil {
		return strings.TrimSpace(string(data))
	}

	return ""
}

// LoadCollectionPolicySpecFromEnv reads environment variables, applies them
// over the provided oldSpec, and returns the new spec along with a boolean
// indicating whether any field changed, or an error if parsing fails.
func LoadCollectionPolicySpecFromEnv() (v1.CollectionPolicySpec, error) {
	newSpec := v1.CollectionPolicySpec{}

	// Helper to split comma-separated lists
	splitCSV := func(envKey string) []string {
		if raw := getEnv(envKey); raw != "" {
			parts := strings.Split(raw, ",")
			for i := range parts {
				parts[i] = strings.TrimSpace(parts[i])
			}
			return parts
		}
		return nil
	}

	// Helper to unmarshal JSON-encoded env var into out
	unmarshalJSON := func(envKey string, out interface{}) error {
		if raw := getEnv(envKey); raw != "" {
			if err := json.Unmarshal([]byte(raw), out); err != nil {
				return fmt.Errorf("invalid JSON for %s: %w", envKey, err)
			}
		}
		return nil
	}

	// === TargetSelector ===
	if ns := splitCSV(_ENV_TARGET_NAMESPACES); ns != nil {
		newSpec.TargetSelector.Namespaces = ns
	}

	// === Exclusions (comma-separated) ===
	if excl := splitCSV(_ENV_EXCLUDED_NAMESPACES); excl != nil {
		newSpec.Exclusions.ExcludedNamespaces = excl
	}
	if excl := splitCSV(_ENV_EXCLUDED_NODES); excl != nil {
		newSpec.Exclusions.ExcludedNodes = excl
	}
	if excl := splitCSV(_ENV_EXCLUDED_INGRESSCLASSES); excl != nil {
		newSpec.Exclusions.ExcludedIngressClasses = excl
	}
	if excl := splitCSV(_ENV_EXCLUDED_PSPS); excl != nil {
		newSpec.Exclusions.ExcludedPSPs = excl
	}
	if excl := splitCSV(_ENV_EXCLUDED_PVS); excl != nil {
		newSpec.Exclusions.ExcludedPVs = excl
	}
	if excl := splitCSV(_ENV_EXCLUDED_STORAGECLASSES); excl != nil {
		newSpec.Exclusions.ExcludedStorageClasses = excl
	}
	if excl := splitCSV(_ENV_EXCLUDED_CRDS); excl != nil {
		newSpec.Exclusions.ExcludedCRDs = excl
	}
	if excl := splitCSV(_ENV_EXCLUDED_CRDGROUPS); excl != nil {
		newSpec.Exclusions.ExcludedCRDGroups = excl
	}
	if excl := splitCSV(_ENV_EXCLUDED_CLUSTERROLES); excl != nil {
		newSpec.Exclusions.ExcludedClusterRoles = excl
	}
	if excl := splitCSV(_ENV_EXCLUDED_CLUSTERROLEBINDINGS); excl != nil {
		newSpec.Exclusions.ExcludedClusterRoleBindings = excl
	}

	// === Exclusions (JSON-encoded structs) ===
	var tmpPods []v1.ExcludedPod
	if err := unmarshalJSON(_ENV_EXCLUDED_PODS, &tmpPods); err != nil {
		return newSpec, err
	}
	if tmpPods != nil {
		newSpec.Exclusions.ExcludedPods = tmpPods
	}

	var tmpDeploys []v1.ExcludedDeployment
	if err := unmarshalJSON(_ENV_EXCLUDED_DEPLOYMENTS, &tmpDeploys); err != nil {
		return newSpec, err
	}
	if tmpDeploys != nil {
		newSpec.Exclusions.ExcludedDeployments = tmpDeploys
	}

	var tmpSts []v1.ExcludedStatefulSet
	if err := unmarshalJSON(_ENV_EXCLUDED_STATEFULSETS, &tmpSts); err != nil {
		return newSpec, err
	}
	if tmpSts != nil {
		newSpec.Exclusions.ExcludedStatefulSets = tmpSts
	}

	var tmpDs []v1.ExcludedDaemonSet
	if err := unmarshalJSON(_ENV_EXCLUDED_DAEMONSETS, &tmpDs); err != nil {
		return newSpec, err
	}
	if tmpDs != nil {
		newSpec.Exclusions.ExcludedDaemonSets = tmpDs
	}

	var tmpSvcs []v1.ExcludedService
	if err := unmarshalJSON(_ENV_EXCLUDED_SERVICES, &tmpSvcs); err != nil {
		return newSpec, err
	}
	if tmpSvcs != nil {
		newSpec.Exclusions.ExcludedServices = tmpSvcs
	}

	var tmpPVCs []v1.ExcludedPVC
	if err := unmarshalJSON(_ENV_EXCLUDED_PVCS, &tmpPVCs); err != nil {
		return newSpec, err
	}
	if tmpPVCs != nil {
		newSpec.Exclusions.ExcludedPVCs = tmpPVCs
	}

	var tmpEvents []v1.ExcludedEvent
	if err := unmarshalJSON(_ENV_EXCLUDED_EVENTS, &tmpEvents); err != nil {
		return newSpec, err
	}
	if tmpEvents != nil {
		newSpec.Exclusions.ExcludedEvents = tmpEvents
	}

	var tmpJobs []v1.ExcludedJob
	if err := unmarshalJSON(_ENV_EXCLUDED_JOBS, &tmpJobs); err != nil {
		return newSpec, err
	}
	if tmpJobs != nil {
		newSpec.Exclusions.ExcludedJobs = tmpJobs
	}

	var tmpCron []v1.ExcludedCronJob
	if err := unmarshalJSON(_ENV_EXCLUDED_CRONJOBS, &tmpCron); err != nil {
		return newSpec, err
	}
	if tmpCron != nil {
		newSpec.Exclusions.ExcludedCronJobs = tmpCron
	}

	var tmpRCs []v1.ExcludedReplicationController
	if err := unmarshalJSON(_ENV_EXCLUDED_REPLICATIONCONTROLLERS, &tmpRCs); err != nil {
		return newSpec, err
	}
	if tmpRCs != nil {
		newSpec.Exclusions.ExcludedReplicationControllers = tmpRCs
	}

	var tmpIngs []v1.ExcludedIngress
	if err := unmarshalJSON(_ENV_EXCLUDED_INGRESSES, &tmpIngs); err != nil {
		return newSpec, err
	}
	if tmpIngs != nil {
		newSpec.Exclusions.ExcludedIngresses = tmpIngs
	}

	var tmpNetPol []v1.ExcludedNetworkPolicy
	if err := unmarshalJSON(_ENV_EXCLUDED_NETWORKPOLICIES, &tmpNetPol); err != nil {
		return newSpec, err
	}
	if tmpNetPol != nil {
		newSpec.Exclusions.ExcludedNetworkPolicies = tmpNetPol
	}

	var tmpEndpoints []v1.ExcludedEndpoint
	if err := unmarshalJSON(_ENV_EXCLUDED_ENDPOINTS, &tmpEndpoints); err != nil {
		return newSpec, err
	}
	if tmpEndpoints != nil {
		newSpec.Exclusions.ExcludedEndpoints = tmpEndpoints
	}

	var tmpSAs []v1.ExcludedServiceAccount
	if err := unmarshalJSON(_ENV_EXCLUDED_SERVICEACCOUNTS, &tmpSAs); err != nil {
		return newSpec, err
	}
	if tmpSAs != nil {
		newSpec.Exclusions.ExcludedServiceAccounts = tmpSAs
	}

	var tmpLR []v1.ExcludedLimitRange
	if err := unmarshalJSON(_ENV_EXCLUDED_LIMITRANGES, &tmpLR); err != nil {
		return newSpec, err
	}
	if tmpLR != nil {
		newSpec.Exclusions.ExcludedLimitRanges = tmpLR
	}

	var tmpRQs []v1.ExcludedResourceQuota
	if err := unmarshalJSON(_ENV_EXCLUDED_RESOURCEQUOTAS, &tmpRQs); err != nil {
		return newSpec, err
	}
	if tmpRQs != nil {
		newSpec.Exclusions.ExcludedResourceQuotas = tmpRQs
	}

	var tmpHPAs []v1.ExcludedHPA
	if err := unmarshalJSON(_ENV_EXCLUDED_HPAS, &tmpHPAs); err != nil {
		return newSpec, err
	}
	if tmpHPAs != nil {
		newSpec.Exclusions.ExcludedHPAs = tmpHPAs
	}

	var tmpVPAs []v1.ExcludedVPA
	if err := unmarshalJSON(_ENV_EXCLUDED_VPAS, &tmpVPAs); err != nil {
		return newSpec, err
	}
	if tmpVPAs != nil {
		newSpec.Exclusions.ExcludedVPAs = tmpVPAs
	}

	var tmpRoles []v1.ExcludedRole
	if err := unmarshalJSON(_ENV_EXCLUDED_ROLES, &tmpRoles); err != nil {
		return newSpec, err
	}
	if tmpRoles != nil {
		newSpec.Exclusions.ExcludedRoles = tmpRoles
	}

	var tmpRBs []v1.ExcludedRoleBinding
	if err := unmarshalJSON(_ENV_EXCLUDED_ROLEBINDINGS, &tmpRBs); err != nil {
		return newSpec, err
	}
	if tmpRBs != nil {
		newSpec.Exclusions.ExcludedRoleBindings = tmpRBs
	}

	var tmpPDBs []v1.ExcludedPDB
	if err := unmarshalJSON(_ENV_EXCLUDED_PDBS, &tmpPDBs); err != nil {
		return newSpec, err
	}
	if tmpPDBs != nil {
		newSpec.Exclusions.ExcludedPDBs = tmpPDBs
	}

	// === Policies ===
	if v := getEnv(_ENV_NUM_RESOURCE_SENDERS); v != "" {
		num, err := strconv.Atoi(v)
		if err != nil {
			num = 16
		}
		newSpec.Policies.NumResourceProcessors = &num
	}
	if v := getEnv(_ENV_KUBE_CONTEXT_NAME); v != "" {
		newSpec.Policies.KubeContextName = v
	}
	if v := getEnv(_ENV_CLUSTER_TOKEN); v != "" {
		newSpec.Policies.ClusterToken = v
	}
	if v := getEnv(_ENV_PAT_TOKEN); v != "" {
		newSpec.Policies.PATToken = v
	}
	if v := getEnv(_ENV_DAKR_URL); v != "" {
		newSpec.Policies.DakrURL = v
	}
	if v := getEnv(_ENV_PROMETHEUS_URL); v != "" {
		newSpec.Policies.PrometheusURL = v
	}
	if v := getEnv(_ENV_COLLECTION_FREQUENCY); v != "" {
		newSpec.Policies.Frequency = v
	}
	if v := getEnv(_ENV_BUFFER_SIZE); v != "" {
		if i, err := strconv.Atoi(v); err != nil {
			return newSpec, fmt.Errorf("invalid %s: %w", _ENV_BUFFER_SIZE, err)
		} else {
			newSpec.Policies.BufferSize = i
		}
	}
	if v := getEnv(_ENV_DISABLE_NETWORK_IO_METRICS); v != "" {
		if b, err := strconv.ParseBool(v); err != nil {
			return newSpec, fmt.Errorf("invalid %s: %w", _ENV_DISABLE_NETWORK_IO_METRICS, err)
		} else {
			newSpec.Policies.DisableNetworkIOMetrics = b
		}
	}
	if v := getEnv(_ENV_DISABLE_GPU_METRICS); v != "" {
		if b, err := strconv.ParseBool(v); err != nil {
			return newSpec, fmt.Errorf("invalid %s: %w", _ENV_DISABLE_GPU_METRICS, err)
		} else {
			newSpec.Policies.DisableGPUMetrics = b
		}
	}
	if v := getEnv(_ENV_MASK_SECRET_DATA); v != "" {
		if b, err := strconv.ParseBool(v); err != nil {
			return newSpec, fmt.Errorf("invalid %s: %w", _ENV_MASK_SECRET_DATA, err)
		} else {
			newSpec.Policies.MaskSecretData = b
		}
	}
	if v := getEnv(_ENV_NODE_METRICS_INTERVAL); v != "" {
		newSpec.Policies.NodeMetricsInterval = v
	}
	if v := getEnv(_ENV_CLUSTER_SNAPSHOT_INTERVAL); v != "" {
		newSpec.Policies.ClusterSnapshotInterval = v
	}
	if list := splitCSV(_ENV_WATCHED_CRDS); list != nil {
		newSpec.Policies.WatchedCRDs = list
	}
	if list := splitCSV(_ENV_DISABLED_COLLECTORS); list != nil {
		newSpec.Policies.DisabledCollectors = list
	}

	// Detect any change
	return newSpec, nil
}
