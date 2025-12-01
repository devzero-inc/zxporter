# ZXporter Operator RBAC Documentation

## Overview

The ZXporter Operator is a comprehensive Kubernetes monitoring and data collection system that automatically gathers resource metrics, events, and configuration data from clusters. It features automatic metrics-server installation, extensive third-party integration support, and exports collected data to external analytics platforms.

## Complete Permissions Matrix

### Main Operator ClusterRole
| API Group | Resource | get | list | watch | create | update | patch | delete | Purpose |
|-----------|----------|-----|------|-------|--------|--------|-------|--------|---------|
| `devzero.io` | `collectionpolicies` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | CRD management |
| `devzero.io` | `collectionpolicies/status` | ✓ | | | | ✓ | ✓ | | Status updates |
| `devzero.io` | `collectionpolicies/finalizers` | | | | | ✓ | | | Cleanup coordination |
| `""` (core) | `serviceaccounts` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | | Bootstrap metrics-server |
| `""` (core) | `services` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | | Bootstrap metrics-server |
| `""` (core) | `configmaps` | ✓ | ✓ | ✓ | | ✓ | | | Token persistence |
| `""` (core) | `pods` | ✓ | ✓ | ✓ | | | | | Resource monitoring |
| `""` (core) | `pods/status` | ✓ | | | | | | | Pod metrics |
| `""` (core) | `nodes` | ✓ | ✓ | ✓ | | | | | Node monitoring |
| `""` (core) | `nodes/status` | ✓ | | | | | | | Node metrics |
| `""` (core) | `nodes/metrics` | ✓ | | | | | | | Metrics collection |
| `""` (core) | `namespaces` | ✓ | ✓ | ✓ | | | | | Namespace discovery |
| `""` (core) | `persistentvolumeclaims` | ✓ | ✓ | ✓ | | | | | Storage monitoring |
| `""` (core) | `persistentvolumes` | ✓ | ✓ | ✓ | | | | | Storage monitoring |
| `""` (core) | `events` | ✓ | ✓ | ✓ | | | | | Event collection |
| `""` (core) | `limitranges` | ✓ | ✓ | ✓ | | | | | Resource limits |
| `""` (core) | `resourcequotas` | ✓ | ✓ | ✓ | | | | | Quota monitoring |
| `""` (core) | `replicationcontrollers` | ✓ | ✓ | ✓ | | | | | Legacy workloads |
| `""` (core) | `endpoints` | ✓ | ✓ | ✓ | | | | | Service endpoints |
| `apps` | `deployments` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | | Bootstrap + monitoring |
| `apps` | `statefulsets` | ✓ | ✓ | ✓ | | | | | Workload monitoring |
| `apps` | `daemonsets` | ✓ | ✓ | ✓ | | | | | Workload monitoring |
| `apps` | `replicasets` | ✓ | ✓ | ✓ | | | | | Workload monitoring |
| `batch` | `jobs` | ✓ | ✓ | ✓ | | | | | Job monitoring |
| `batch` | `cronjobs` | ✓ | ✓ | ✓ | | | | | Scheduled job monitoring |
| `metrics.k8s.io` | `nodes` | ✓ | ✓ | ✓ | | | | | Metrics collection |
| `metrics.k8s.io` | `pods` | ✓ | ✓ | ✓ | | | | | Metrics collection |
| `networking.k8s.io` | `ingresses` | ✓ | ✓ | ✓ | | | | | Network monitoring |
| `networking.k8s.io` | `networkpolicies` | ✓ | ✓ | ✓ | | | | | Security monitoring |
| `networking.k8s.io` | `ingressclasses` | ✓ | ✓ | ✓ | | | | | Ingress discovery |
| `autoscaling` | `horizontalpodautoscalers` | ✓ | ✓ | ✓ | | | | | HPA monitoring |
| `autoscaling.k8s.io` | `verticalpodautoscalers` | ✓ | ✓ | ✓ | | | | | VPA monitoring |
| `policy` | `poddisruptionbudgets` | ✓ | ✓ | ✓ | | | | | Policy monitoring |
| `storage.k8s.io` | `storageclasses` | ✓ | ✓ | ✓ | | | | | Storage monitoring |
| `storage.k8s.io` | `csinodes` | ✓ | ✓ | ✓ | | | | | CSI monitoring |
| `storage.k8s.io` | `csidrivers` | ✓ | ✓ | ✓ | | | | | CSI monitoring |
| `storage.k8s.io` | `csistoragecapacities` | ✓ | ✓ | ✓ | | | | | Storage capacity |
| `storage.k8s.io` | `volumeattachments` | ✓ | ✓ | ✓ | | | | | Volume monitoring |
| `rbac.authorization.k8s.io` | `clusterroles` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | | Bootstrap + monitoring |
| `rbac.authorization.k8s.io` | `clusterrolebindings` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | | Bootstrap + monitoring |
| `rbac.authorization.k8s.io` | `rolebindings` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | | Bootstrap + monitoring |
| `rbac.authorization.k8s.io` | `roles` | ✓ | ✓ | ✓ | | | | | RBAC monitoring |
| `apiregistration.k8s.io` | `apiservices` | ✓ | ✓ | | ✓ | ✓ | ✓ | | Bootstrap metrics API |
| `apiextensions.k8s.io` | `customresourcedefinitions` | ✓ | ✓ | ✓ | | | | | CRD discovery |

### Conditional Third-Party Permissions
| API Group | Resource | get | list | watch | create | update | patch | delete | Condition |
|-----------|----------|-----|------|-------|--------|--------|-------|--------|-----------|
| `karpenter.sh` | `provisioners` | ✓ | ✓ | ✓ | | | | | Karpenter installed |
| `karpenter.sh` | `machines` | ✓ | ✓ | ✓ | | | | | Karpenter installed |
| `karpenter.sh` | `nodepools` | ✓ | ✓ | ✓ | | | | | Karpenter installed |
| `karpenter.sh` | `nodeclaims` | ✓ | ✓ | ✓ | | | | | Karpenter installed |
| `karpenter.sh` | `nodeoverlays` | ✓ | ✓ | ✓ | | | | | Karpenter installed |
| `karpenter.k8s.aws` | `awsnodetemplates` | ✓ | ✓ | ✓ | | | | | Karpenter AWS |
| `karpenter.k8s.aws` | `ec2nodeclasses` | ✓ | ✓ | ✓ | | | | | Karpenter AWS |
| `karpenter.azure.com` | `aksnodeclasses` | ✓ | ✓ | ✓ | | | | | Karpenter Azure |
| `karpenter.k8s.oracle` | `ocinodeclasses` | ✓ | ✓ | ✓ | | | | | Karpenter Oracle |
| `karpenter.k8s.gcp` | `gcenodeclasses` | ✓ | ✓ | ✓ | | | | | Karpenter GCP |
| `keda.sh` | `scaledobjects` | ✓ | ✓ | ✓ | | | | | KEDA installed |
| `keda.sh` | `scaledjobs` | ✓ | ✓ | ✓ | | | | | KEDA installed |
| `keda.sh` | `triggerauthentications` | ✓ | ✓ | ✓ | | | | | KEDA installed |
| `keda.sh` | `clustertriggerauthentications` | ✓ | ✓ | ✓ | | | | | KEDA installed |
| `argoproj.io` | `rollouts` | ✓ | ✓ | ✓ | | | | | Argo Rollouts installed |
| `datadoghq.com` | `extendeddaemonsetreplicasets` | ✓ | ✓ | ✓ | | | | | Datadog operator installed |
| `kubeflow.org` | `notebooks` | ✓ | ✓ | ✓ | | | | | Kubeflow installed |

### Leader Election Role
| API Group | Resource | get | list | watch | create | update | patch | delete |
|-----------|----------|-----|------|-------|--------|--------|-------|--------|
| `coordination.k8s.io` | `leases` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| `""` (core) | `configmaps` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| `""` (core) | `events` | | | | ✓ | | ✓ | |

**Legend**: ✓ = Permission granted | Conditional = Only when specific CRDs detected

## ServiceAccount

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: devzero-zxporter-controller-manager
  namespace: {{ .Values.namespace }}
```
**Scope**: Service account for the zxporter controller deployment

## ClusterRole Breakdown

### Bootstrap Phase (entrypoint.sh)

#### Metrics Server Installation
**Purpose**: Automatic metrics-server deployment when missing from cluster

- **`serviceaccounts`** (`create`, `get`, `list`, `update`, `patch`, `watch`)
  - Creates `dz-metrics-server` ServiceAccount
  - **Code**: `entrypoint.sh:29`, `dist/metrics-server.yaml`

- **`services`** (`create`, `get`, `list`, `update`, `patch`, `watch`)
  - Creates metrics-server service endpoint
  - **Code**: `entrypoint.sh:29`, `dist/metrics-server.yaml`

- **`deployments`** (`create`, `get`, `list`, `update`, `patch`, `watch`)
  - Creates metrics-server deployment
  - **Code**: `entrypoint.sh:29`, `dist/metrics-server.yaml`

#### RBAC Setup for Metrics Server
**Purpose**: Creates necessary RBAC for metrics-server operation

- **`clusterroles`** (`create`, `get`, `list`, `update`, `patch`, `watch`)
  - Creates `system:dz-metrics-server` ClusterRole
  - **Code**: `entrypoint.sh:29`, metrics-server RBAC

- **`clusterrolebindings`** (`create`, `get`, `list`, `update`, `patch`, `watch`)
  - Binds metrics-server ServiceAccount to ClusterRoles
  - **Code**: `entrypoint.sh:29`, metrics-server RBAC

- **`rolebindings`** (`create`, `get`, `list`, `update`, `patch`, `watch`)
  - Creates auth-reader RoleBinding for extension API server
  - **Code**: `entrypoint.sh:29`, metrics-server RBAC

#### API Registration
**Purpose**: Registers metrics API with Kubernetes aggregation layer

- **`apiservices`** (`create`, `get`, `list`, `update`, `patch`)
  - Registers `v1beta1.metrics.k8s.io` APIService
  - **Code**: `entrypoint.sh:29`, metrics-server installation

### Runtime Phase (Go Controller)

#### Collection Policy Management
**Purpose**: Core CRD lifecycle management

- **`collectionpolicies`** (Full CRUD)
  - Manages CollectionPolicy custom resources
  - **Code**: `internal/controller/collectionpolicy_controller.go:239`

- **Status subresources** (`get`, `update`, `patch`)
  - Reports collection status and health
  - **Code**: Controller status updates

- **Finalizer management** (`update`)
  - Ensures proper cleanup sequencing
  - **Code**: Controller finalizer management

#### Token Persistence
**Purpose**: Cluster token management for external API authentication

- **`configmaps`** (`get`, `list`, `watch`, `update`)
  - Updates `devzero-zxporter-env-config` with cluster tokens
  - **Code**: `internal/controller/custom.go:335`

#### Core Resource Monitoring
**Purpose**: Comprehensive Kubernetes resource collection

- **Pod monitoring** (`get`, `list`, `watch`)
  - Collects pod lifecycle events, container metrics, resource usage
  - **Code**: `internal/collector/pod_collector.go`

- **Node monitoring** (`get`, `list`, `watch`)
  - Gathers node metrics, capacity, conditions, and allocatable resources
  - **Code**: `internal/collector/node_collector.go`

- **Storage monitoring** (`get`, `list`, `watch`)
  - Tracks PVs, PVCs, StorageClasses, and CSI resources
  - **Code**: `internal/collector/pv_collector.go`, `internal/collector/pvc_collector.go`

- **Workload monitoring** (`get`, `list`, `watch`)
  - Monitors Deployments, StatefulSets, DaemonSets, Jobs, CronJobs
  - **Code**: `internal/collector/deployment_collector.go`, etc.

#### Metrics Collection
**Purpose**: Resource utilization data gathering

- **`nodes/metrics`** (`get`)
  - Retrieves node-level resource metrics
  - **Code**: `internal/collector/node_collector.go`

- **`metrics.k8s.io`** resources (`get`, `list`, `watch`)
  - Collects pod and node metrics from metrics-server
  - **Code**: Metrics collection components

#### Network and Security Monitoring
**Purpose**: Network topology and security policy tracking

- **Networking resources** (`get`, `list`, `watch`)
  - Monitors Ingresses, NetworkPolicies, IngressClasses
  - **Code**: `internal/collector/ingress_collector.go`, etc.

- **RBAC monitoring** (`get`, `list`, `watch`)
  - Tracks Roles, RoleBindings, ClusterRoles, ClusterRoleBindings
  - **Code**: `internal/collector/role_collector.go`, etc.

#### Autoscaling Monitoring
**Purpose**: Scaling behavior and policy tracking

- **HPA/VPA monitoring** (`get`, `list`, `watch`)
  - Collects autoscaling configurations and status
  - **Code**: `internal/collector/hpa_collector.go`

- **PDB monitoring** (`get`, `list`, `watch`)
  - Tracks pod disruption budgets
  - **Code**: `internal/collector/pdb_collector.go`

### Third-Party Integrations

#### Karpenter Integration
**Purpose**: Node provisioning and scaling monitoring

- **All Karpenter resources** (`get`, `list`, `watch`)
  - Monitors node provisioning across cloud providers
  - **Code**: `internal/collector/karpenter_collector.go`
  - **Condition**: Gracefully handles missing Karpenter CRDs

#### KEDA Integration
**Purpose**: Event-driven autoscaling monitoring

- **KEDA resources** (`get`, `list`, `watch`)
  - Tracks ScaledObjects, ScaledJobs, TriggerAuthentications
  - **Code**: `internal/collector/keda_*_collector.go`
  - **Condition**: Only when KEDA CRDs detected

#### Argo Rollouts Integration
**Purpose**: Progressive delivery monitoring

- **`rollouts`** (`get`, `list`, `watch`)
  - Monitors advanced deployment strategies
  - **Code**: `internal/collector/argo_rollouts_collector.go`
  - **Condition**: Only when Argo Rollouts CRDs detected

#### Datadog Integration
**Purpose**: Datadog operator monitoring

- **`extendeddaemonsetreplicasets`** (`get`, `list`, `watch`)
  - Tracks Datadog-specific resources
  - **Code**: `internal/collector/datadog_collector.go`
  - **Condition**: Only when Datadog operator CRDs detected

#### Kubeflow Integration
**Purpose**: ML workload monitoring

- **`notebooks`** (`get`, `list`, `watch`)
  - Monitors Jupyter notebook resources
  - **Code**: `internal/collector/kubeflow_collector.go`
  - **Condition**: Only when Kubeflow CRDs detected

### Leader Election
**Purpose**: High availability support for controller deployments

- **`leases`** (Full CRUD)
  - Primary leader election mechanism
  - **Code**: Controller runtime leader election

- **`configmaps`** (Full CRUD within namespace)
  - Backup coordination mechanism
  - **Code**: Leader election framework

- **`events`** (`create`, `patch`)
  - Leader election event logging
  - **Code**: Leader election framework

## Security Considerations

### Principle of Least Privilege

The zxporter operator follows a two-phase permission model:

1. **Bootstrap Phase**: Requires elevated permissions for one-time metrics-server installation
2. **Runtime Phase**: Operates with minimal write permissions (only ConfigMap updates)

### Permission Justification

- **Write permissions** are limited to essential operations:
  - Metrics server bootstrap (one-time setup)
  - Token persistence (security requirement)
  - Leader election (HA requirement)

- **Read permissions** enable comprehensive monitoring without cluster modification

### Third-Party Graceful Degradation

All third-party integrations:
- Test for CRD availability before attempting access
- Gracefully handle missing operators
- Can be disabled via `DisabledCollectors` configuration
- Do not block core functionality if unavailable

### Metrics Server Bootstrap

The automatic metrics-server installation feature:
- Only activates when metrics-server is missing
- Uses standard Helm-generated manifests
- Follows Kubernetes security best practices
- Can be disabled if metrics-server is pre-installed

## Configuration Options

### Disabling Collectors

```yaml
apiVersion: devzero.io/v1
kind: CollectionPolicy
spec:
  policies:
    disabledCollectors:
      - "karpenter"
      - "keda_scaled_job"
      - "argo_rollouts"
      - "datadog"
      - "kubeflow_notebook"
```

### Namespace Targeting

```yaml
apiVersion: devzero.io/v1
kind: CollectionPolicy
spec:
  targetSelector:
    namespaces:
      - "production"
      - "staging"
  exclusions:
    excludedNamespaces:
      - "kube-system"
      - "kube-public"
```