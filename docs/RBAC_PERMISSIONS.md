# RBAC Permissions

## ClusterRole Permissions

### Core Resources

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `""` (core) | `configmaps` | get, list, watch, update | Read cluster configuration; update ConfigMap to persist cluster authentication tokens |
| `""` (core) | `endpoints` | get, list, watch | Collect service endpoint information for cluster topology and service discovery tracking |
| `""` (core) | `events` | get, list, watch | Collect Kubernetes events for cluster activity monitoring and troubleshooting |
| `""` (core) | `limitranges` | get, list, watch | Collect LimitRange configurations to track resource constraints and governance policies |
| `""` (core) | `namespaces` | get, list, watch | Enumerate namespaces for multi-tenant cluster organization and resource scoping |
| `""` (core) | `nodes` | get, list, watch | Collect node information for infrastructure inventory, capacity planning, and cluster topology |
| `""` (core) | `nodes/metrics` | get | Retrieve node-level resource metrics (CPU, memory) from Metrics Server |
| `""` (core) | `nodes/status` | get | Access node status information for health monitoring and availability tracking |
| `""` (core) | `persistentvolumeclaims` | get, list, watch | Monitor PVC usage patterns and storage consumption for capacity management |
| `""` (core) | `persistentvolumes` | get, list, watch | Collect cluster-wide persistent volume information for storage resource tracking |
| `""` (core) | `pods` | get, list, watch | Monitor pod lifecycle events, workload deployment patterns, and OOMKilled events |
| `""` (core) | `pods/status` | get | Access pod status for workload health, phase tracking, and container state monitoring |
| `""` (core) | `replicationcontrollers` | get, list, watch | Monitor legacy ReplicationController resources for workload tracking |
| `""` (core) | `resourcequotas` | get, list, watch | Collect ResourceQuota configurations for capacity management and quota enforcement tracking |
| `""` (core) | `secrets` | create | Create Secret to store cluster authentication tokens when useSecretForToken is enabled |
| `""` (core) | `secrets` (specific) | get, patch, update | Read and update the operator's cluster authentication token Secret (devzero-zxporter-devzero-zxporter-token) |
| `""` (core) | `serviceaccounts` | create, get, list, patch, update, watch | Bootstrap metrics-server ServiceAccount; collect ServiceAccount information for RBAC tracking |
| `""` (core) | `services` | create, get, list, patch, update, watch | Bootstrap metrics-server Service; collect cluster service configurations for networking visibility |

### Apps Resources

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `apps` | `daemonsets` | get, list, watch | Collect DaemonSet configurations to track node-level workload deployments |
| `apps` | `deployments` | create, get, list, patch, update, watch | Bootstrap metrics-server Deployment if not available; collect application deployment configurations |
| `apps` | `replicasets` | get, list, watch | Collect ReplicaSet information for workload scaling patterns and pod ownership tracking |
| `apps` | `statefulsets` | get, list, watch | Collect StatefulSet configurations to track stateful application deployments |

### Batch Resources

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `batch` | `cronjobs` | get, list, watch | Collect CronJob configurations to track scheduled batch workload patterns |
| `batch` | `jobs` | get, list, watch | Collect Job configurations for batch processing workload tracking |

### Networking Resources

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `networking.k8s.io` | `ingressclasses` | get, list, watch | Collect IngressClass configurations for traffic routing and load balancer visibility |
| `networking.k8s.io` | `ingresses` | get, list, watch | Collect Ingress configurations to track external access patterns and routing rules |
| `networking.k8s.io` | `networkpolicies` | get, list, watch | Collect NetworkPolicy configurations for network security and traffic control analysis |

### RBAC Resources

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `rbac.authorization.k8s.io` | `clusterrolebindings` | create, get, list, patch, update, watch | Bootstrap metrics-server RBAC; collect cluster-wide role binding configurations for security tracking |
| `rbac.authorization.k8s.io` | `clusterroles` | create, get, list, patch, update, watch | Bootstrap metrics-server RBAC; collect cluster-wide role configurations for security policy analysis |
| `rbac.authorization.k8s.io` | `rolebindings` | create, get, list, patch, update, watch | Bootstrap metrics-server RBAC; collect namespace-scoped role bindings for security tracking |
| `rbac.authorization.k8s.io` | `roles` | get, list, watch | Collect namespace-scoped role configurations for RBAC security policy visibility |

### Policy Resources

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `policy` | `poddisruptionbudgets` | get, list, watch | Collect PodDisruptionBudget configurations for availability requirements and disruption planning |

### Autoscaling Resources

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `autoscaling` | `horizontalpodautoscalers` | get, list, watch | Collect HorizontalPodAutoscaler configurations to track CPU/memory-based scaling patterns |
| `autoscaling.k8s.io` | `verticalpodautoscalers` | get, list, watch | Collect VerticalPodAutoscaler configurations for resource optimization recommendations tracking |

### Metrics Resources

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `metrics.k8s.io` | `nodes` | get, list, watch | Retrieve node-level resource usage metrics (CPU, memory) from Metrics Server for capacity monitoring |
| `metrics.k8s.io` | `pods` | get, list, watch | Retrieve pod-level resource usage metrics (CPU, memory) from Metrics Server for workload performance tracking |

### Storage Resources

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `storage.k8s.io` | `csidrivers` | get, list, watch | Collect CSI driver configurations for storage plugin and provider visibility |
| `storage.k8s.io` | `csinodes` | get, list, watch | Collect CSI node information for storage topology mapping and driver attachment tracking |
| `storage.k8s.io` | `csistoragecapacities` | get, list, watch | Track storage capacity information for volume provisioning and capacity planning |
| `storage.k8s.io` | `storageclasses` | get, list, watch | Collect StorageClass definitions for persistent volume provisioning options and storage tiers |
| `storage.k8s.io` | `volumeattachments` | get, list, watch | Monitor volume attachment state for storage connectivity and mount tracking |

### API Extensions

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `apiextensions.k8s.io` | `customresourcedefinitions` | get, list, watch | Discover installed CRDs for extended API resources and ecosystem visibility (detects availability of third-party operators) |
| `apiregistration.k8s.io` | `apiservices` | create, get, list, patch, update, watch | Bootstrap metrics-server API registration; discover API services for cluster API extension tracking |

### Custom Resources (CRDs)

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `devzero.io` | `collectionpolicies` | create, delete, get, list, patch, update, watch | Manage CollectionPolicy CRDs that control which namespaces, resources, and metrics to collect |
| `devzero.io` | `collectionpolicies/finalizers` | update | Manage finalizers for graceful CollectionPolicy deletion and cleanup |
| `devzero.io` | `collectionpolicies/status` | get, patch, update | Update CollectionPolicy status to reflect collection state and health |

### Argo Rollouts Resources (Optional)

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `argoproj.io` | `rollouts` | get, list, watch | Collect Argo Rollouts configurations for progressive delivery and canary deployment tracking (only if Argo Rollouts operator is installed) |

### Volcano Resources (Optional)

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `batch.volcano.sh` | `jobs` | get, list, watch | Collect Volcano Job configurations for HPC and AI/ML batch workload tracking (only if Volcano operator is installed) |

### Datadog Resources (Optional)

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `datadoghq.com` | `extendeddaemonsetreplicasets` | get, list, watch | Collect Datadog ExtendedDaemonSet ReplicaSet information for observability stack tracking (only if Datadog operator is installed) |

### Karpenter Resources (Optional)

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `karpenter.k8s.aws` | `awsnodetemplates` | get, list, watch | Collect AWS Karpenter NodeTemplate configurations for autoscaling tracking (only if Karpenter is installed on AWS) |
| `karpenter.k8s.aws` | `ec2nodeclasses` | get, list, watch | Collect EC2NodeClass configurations for AWS Karpenter autoscaling (only if Karpenter is installed on AWS) |
| `karpenter.azure.com` | `aksnodeclasses` | get, list, watch | Collect AKSNodeClass configurations for Azure Karpenter autoscaling (only if Karpenter is installed on Azure) |
| `karpenter.k8s.gcp` | `gcenodeclasses` | get, list, watch | Collect GCE NodeClass configurations for GCP Karpenter autoscaling (only if Karpenter is installed on GCP) |
| `karpenter.k8s.oracle` | `ocinodeclasses` | get, list, watch | Collect OCI NodeClass configurations for Oracle Cloud Karpenter autoscaling (only if Karpenter is installed on OCI) |
| `karpenter.sh` | `machines` | get, list, watch | Collect Karpenter Machine resources for node lifecycle tracking (only if Karpenter operator is installed) |
| `karpenter.sh` | `nodeclaims` | get, list, watch | Collect Karpenter NodeClaim resources for capacity requests tracking (only if Karpenter operator is installed) |
| `karpenter.sh` | `nodeoverlays` | get, list, watch | Collect Karpenter NodeOverlay configurations for node customization tracking (only if Karpenter operator is installed) |
| `karpenter.sh` | `nodepools` | get, list, watch | Collect Karpenter NodePool configurations for autoscaling pool management (only if Karpenter operator is installed) |
| `karpenter.sh` | `provisioners` | get, list, watch | Collect Karpenter Provisioner configurations for node provisioning policies (only if Karpenter operator is installed) |

### KEDA Resources (Optional)

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `keda.sh` | `clustertriggerauthentications` | get, list, watch | Collect KEDA cluster-wide TriggerAuthentication configurations for event-driven autoscaling credentials (only if KEDA is installed) |
| `keda.sh` | `scaledjobs` | get, list, watch | Collect KEDA ScaledJob configurations for event-driven job scaling patterns (only if KEDA is installed) |
| `keda.sh` | `scaledobjects` | get, list, watch | Collect KEDA ScaledObject configurations for event-driven HPA tracking (only if KEDA is installed) |
| `keda.sh` | `triggerauthentications` | get, list, watch | Collect KEDA TriggerAuthentication configurations for event source credentials (only if KEDA is installed) |

### Kubeflow Resources (Optional)

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `kubeflow.org` | `notebooks` | get, list, watch | Collect Kubeflow Notebook configurations for ML/AI workload tracking (only if Kubeflow operator is installed) |

### Spark Operator Resources (Optional)

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `sparkoperator.k8s.io` | `scheduledsparkapplications` | get, list, watch | Collect ScheduledSparkApplication configurations for scheduled big data processing workloads (only if Spark Operator is installed) |
| `sparkoperator.k8s.io` | `sparkapplications` | get, list, watch | Collect SparkApplication configurations for big data processing workload tracking (only if Spark Operator is installed) |

### Authentication & Authorization

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `authentication.k8s.io` | `tokenreviews` | create | Validate service account tokens for metrics endpoint authentication (used by kube-rbac-proxy sidecar) |
| `authorization.k8s.io` | `subjectaccessreviews` | create | Perform authorization checks for metrics endpoint access control (used by kube-rbac-proxy sidecar) |

### Non-Resource URLs

| URL Path | Verbs | Purpose |
|----------|-------|---------|
| `/metrics` | get | Allow Prometheus to scrape operator's internal metrics for monitoring operator health and performance |

---

## Role (Namespace-Scoped) Permissions

These permissions are scoped to the `devzero-zxporter` namespace only and are used during leader election and operator lifecycle management:

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `""` (core) | `configmaps` | get, list, watch, create, update, patch, delete | Manage operator configuration, leader election state, and token persistence |
| `""` (core) | `deployments` | get, list, watch, create, update, patch, delete | Manage operator deployment (legacy - may be redundant with apps group) |
| `""` (core) | `serviceaccounts` | get, list, watch, create, update, patch, delete | Manage service accounts for operator components (for metrics-server bootstrap) |
| `""` (core) | `services` | get, list, watch, create, update, patch, delete | Manage operator service endpoints (for metrics-server bootstrap) |
| `""` (core) | `events` | create, patch | Emit Kubernetes events for operator lifecycle notifications and status updates |
| `apps` | `deployments` | get, list, watch, create, update, patch, delete | Manage operator and metrics-server deployments in the operator namespace |
| `coordination.k8s.io` | `leases` | get, list, watch, create, update, patch, delete | Coordinate leader election for high availability (prevents multiple operator instances from running simultaneously) |
| `rbac.authorization.k8s.io` | `rolebindings` | create, delete, get, list, patch, update, watch | Bootstrap metrics-server RBAC within the operator namespace |
| `rbac.authorization.k8s.io` | `roles` | create, delete, get, list, patch, update, watch | Bootstrap metrics-server RBAC within the operator namespace |
| `rbac.authorization.k8s.io` | `clusterrolebindings` | create, delete, get, list, patch, update, watch | Bootstrap metrics-server cluster-wide RBAC bindings |
| `rbac.authorization.k8s.io` | `clusterroles` | create, delete, get, list, patch, update, watch | Bootstrap metrics-server cluster-wide RBAC roles |

---

## Additional ClusterRoles

### CollectionPolicy Editor Role

Allows users to create and manage CollectionPolicy custom resources:

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `devzero.io` | `collectionpolicies` | create, delete, get, list, patch, update, watch | Full management of collection policies to control resource collection behavior |
| `devzero.io` | `collectionpolicies/status` | get | Read collection policy status information |

### CollectionPolicy Viewer Role

Allows users to view CollectionPolicy custom resources (read-only):

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `devzero.io` | `collectionpolicies` | get, list, watch | Read-only access to view collection policies |
| `devzero.io` | `collectionpolicies/status` | get | Read collection policy status information |

### Metrics Authentication Role

Used for authenticating requests to the metrics endpoint (typically used by kube-rbac-proxy):

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `authentication.k8s.io` | `tokenreviews` | create | Validate tokens for metrics endpoint access |
| `authorization.k8s.io` | `subjectaccessreviews` | create | Authorize metrics endpoint access |

### Metrics Reader Role

Allows scraping of operator metrics:

| URL Path | Verbs | Purpose |
|----------|-------|---------|
| `/metrics` | get | Read operator metrics for monitoring and observability platforms (Prometheus, Grafana, etc.) |

---

## Permission Rationale

### What is ZXporter?

ZXporter is a **Kubernetes resource collection and export operator** that:

1. **Collects Cluster Resources**: Watches and collects all Kubernetes resources (pods, deployments, services, etc.) and sends them to a DAKR backend service for analysis, monitoring, and cost optimization
2. **Gathers Performance Metrics**: Integrates with Prometheus and Metrics Server to collect time-series metrics including:
   - Container CPU, memory usage (from Metrics Server)
   - Network I/O metrics (from Prometheus node-exporter)
   - GPU utilization (from Prometheus DCGM exporter for NVIDIA GPUs)
3. **Generates Cluster Snapshots**: Periodically creates comprehensive cluster snapshots for backup, compliance, and auditing
4. **Supports Multi-Cloud**: Detects and tracks cloud provider-specific resources (AWS, Azure, GCP, Oracle Cloud)
5. **Extends to Third-Party Operators**: Automatically detects and collects resources from third-party operators like Argo Rollouts, KEDA, Karpenter, Spark Operator, Kubeflow, and Volcano

### Why These Permissions?

**Read-Only Access (Most Resources)**:
- The operator primarily performs **read-only collection** of Kubernetes resources
- Uses Kubernetes informers to watch for resource changes and send updates to DAKR backend
- All workload, networking, RBAC, and storage resources are read-only

**Write Access (Limited Cases)**:
1. **Token Persistence** (`configmaps`, `secrets`):
   - Exchanges PAT tokens for cluster authentication tokens via DAKR API
   - Persists cluster tokens in ConfigMap or Secret to survive pod restarts
   - Only writes to specific resources: `devzero-zxporter-env-config` ConfigMap or `devzero-zxporter-devzero-zxporter-token` Secret

2. **Metrics Server Bootstrap** (`serviceaccounts`, `services`, `deployments`, RBAC, `apiservices`):
   - Automatically deploys Metrics Server via `entrypoint.sh` kubectl apply if not detected
   - Requires create/update permissions to deploy all Metrics Server components
   - This is a one-time bootstrap operation executed during operator startup

3. **Operator Lifecycle** (namespace-scoped):
   - Leader election via `coordination.k8s.io/leases`
   - Event emission for status updates
   - Configuration management via ConfigMaps

**Optional Permissions**:
- All third-party operator resources (Karpenter, KEDA, Argo, etc.) are **optional**
- Collectors gracefully handle missing CRDs using `IsAvailable()` checks
- Can be disabled via `DisabledCollectors` configuration

### Security Considerations

- The operator requires **cluster-admin-like permissions** for comprehensive monitoring
- Sensitive data like Secrets can be excluded from collection via configuration
- Token persistence uses namespace-scoped Secrets with RBAC protection
- All data transmission to DAKR uses gRPC with TLS and bearer token authentication
- Each collector can be individually disabled for fine-grained control
