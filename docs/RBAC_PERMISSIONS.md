# RBAC Permissions

## ClusterRole Permissions

### Core Resources

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `""` (core) | `configmaps` | get, list, update, watch | Read operator configuration and update cluster metadata snapshots |
| `""` (core) | `endpoints` | get, list, watch | Monitor service endpoints for cluster topology mapping |
| `""` (core) | `events` | get, list, watch | Collect Kubernetes events for cluster activity tracking |
| `""` (core) | `limitranges` | get, list, watch | Collect limit range configurations for resource governance visibility |
| `""` (core) | `namespaces` | get, list, watch | Enumerate and monitor namespaces for multi-tenant cluster organization |
| `""` (core) | `nodes` | get, list, watch | Collect node information for infrastructure inventory and capacity planning |
| `""` (core) | `nodes/metrics` | get | Retrieve node-level resource metrics from kubelet |
| `""` (core) | `nodes/status` | get | Access node status information for health monitoring |
| `""` (core) | `persistentvolumeclaims` | get, list, watch | Monitor PVC usage and storage consumption patterns |
| `""` (core) | `persistentvolumes` | get, list, watch | Collect cluster-wide storage resource information |
| `""` (core) | `pods` | get, list, watch | Monitor pod lifecycle and workload deployment patterns |
| `""` (core) | `pods/status` | get | Access pod status for workload health and state tracking |
| `""` (core) | `replicationcontrollers` | get, list, watch | Monitor legacy replication controllers for workload management |
| `""` (core) | `resourcequotas` | get, list, watch | Collect resource quota configurations for capacity management |
| `""` (core) | `secrets` | create | Create service account tokens for dynamic authentication |
| `""` (core) | `secrets` (specific) | get, patch, update | Manage operator's own authentication token (devzero-zxporter-devzero-zxporter-token) |
| `""` (core) | `serviceaccounts` | create, get, list, patch, update, watch | Manage service accounts for operator components and dynamic collectors |
| `""` (core) | `services` | create, get, list, patch, update, watch | Monitor cluster services and manage operator service endpoints |

### Apps Resources

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `apps` | `daemonsets` | get, list, watch | Monitor DaemonSet deployments for node-level workload tracking |
| `apps` | `deployments` | create, get, list, patch, update, watch | Monitor application deployments and manage operator components |
| `apps` | `replicasets` | get, list, watch | Track ReplicaSet information for workload ownership and scaling patterns |
| `apps` | `statefulsets` | get, list, watch | Monitor StatefulSet deployments for stateful application tracking |

### Batch Resources

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `batch` | `cronjobs` | get, list, watch | Monitor scheduled job configurations for batch workload tracking |
| `batch` | `jobs` | get, list, watch | Track job execution for batch processing visibility |

### Networking Resources

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `networking.k8s.io` | `ingressclasses` | get, list, watch | Collect ingress class configurations for traffic routing visibility |
| `networking.k8s.io` | `ingresses` | get, list, watch | Monitor ingress resources for external access patterns |
| `networking.k8s.io` | `networkpolicies` | get, list, watch | Collect network policies for security and traffic control analysis |

### RBAC Resources

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `rbac.authorization.k8s.io` | `clusterrolebindings` | create, get, list, patch, update, watch | Manage operator's cluster-wide RBAC bindings and collect security configurations |
| `rbac.authorization.k8s.io` | `clusterroles` | create, get, list, patch, update, watch | Manage operator's cluster roles and collect RBAC policies for security analysis |
| `rbac.authorization.k8s.io` | `rolebindings` | create, get, list, patch, update, watch | Manage namespace-scoped role bindings for operator components |
| `rbac.authorization.k8s.io` | `roles` | get, list, watch | Collect namespace-scoped roles for security policy visibility |

### Policy Resources

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `policy` | `poddisruptionbudgets` | get, list, watch | Collect PDB configurations for availability and disruption planning |

### Autoscaling Resources

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `autoscaling` | `horizontalpodautoscalers` | get, list, watch | Monitor HPA configurations for workload scaling patterns |
| `autoscaling.k8s.io` | `verticalpodautoscalers` | get, list, watch | Collect VPA configurations for resource optimization insights |

### Metrics Resources

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `metrics.k8s.io` | `nodes` | get, list, watch | Retrieve node-level resource metrics for capacity monitoring |
| `metrics.k8s.io` | `pods` | get, list, watch | Collect pod-level resource metrics for workload performance tracking |

### Storage Resources

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `storage.k8s.io` | `csidrivers` | get, list, watch | Collect CSI driver configurations for storage plugin visibility |
| `storage.k8s.io` | `csinodes` | get, list, watch | Monitor CSI node information for storage topology mapping |
| `storage.k8s.io` | `csistoragecapacities` | get, list, watch | Track storage capacity for volume provisioning insights |
| `storage.k8s.io` | `storageclasses` | get, list, watch | Collect storage class definitions for persistent volume provisioning |
| `storage.k8s.io` | `volumeattachments` | get, list, watch | Monitor volume attachment state for storage connectivity tracking |

### API Extensions

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `apiextensions.k8s.io` | `customresourcedefinitions` | get, list, watch | Discover CRDs for extended resource collection and ecosystem visibility |
| `apiregistration.k8s.io` | `apiservices` | create, get, list, patch, update, watch | Manage API service registrations for extension points |

### Custom Resources (CRDs)

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `devzero.io` | `collectionpolicies` | create, delete, get, list, patch, update, watch | Manage collection policies that control which resources are monitored |
| `devzero.io` | `collectionpolicies/finalizers` | update | Manage finalizers for graceful policy deletion |
| `devzero.io` | `collectionpolicies/status` | get, patch, update | Update policy status and health information |

### Argo Rollouts Resources

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `argoproj.io` | `rollouts` | get, list, watch | Monitor Argo Rollouts for progressive delivery tracking |

### Volcano Resources

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `batch.volcano.sh` | `jobs` | get, list, watch | Monitor Volcano batch jobs for specialized HPC and AI workload tracking |

### Datadog Resources

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `datadoghq.com` | `extendeddaemonsetreplicasets` | get, list, watch | Monitor Datadog ExtendedDaemonSet resources for observability stack tracking |

### Karpenter Resources

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `karpenter.k8s.aws` | `awsnodetemplates` | get, list, watch | Collect AWS Karpenter node templates for autoscaling configuration |
| `karpenter.k8s.aws` | `ec2nodeclasses` | get, list, watch | Monitor EC2 node class configurations for AWS Karpenter |
| `karpenter.azure.com` | `aksnodeclasses` | get, list, watch | Collect AKS node class configurations for Azure Karpenter |
| `karpenter.k8s.gcp` | `gcenodeclasses` | get, list, watch | Monitor GCE node class configurations for GCP Karpenter |
| `karpenter.k8s.oracle` | `ocinodeclasses` | get, list, watch | Collect OCI node class configurations for Oracle Cloud Karpenter |
| `karpenter.sh` | `machines` | get, list, watch | Monitor Karpenter machine resources for node lifecycle tracking |
| `karpenter.sh` | `nodeclaims` | get, list, watch | Track Karpenter node claims for capacity management |
| `karpenter.sh` | `nodeoverlays` | get, list, watch | Collect node overlay configurations for Karpenter customization |
| `karpenter.sh` | `nodepools` | get, list, watch | Monitor Karpenter node pools for autoscaling pool management |
| `karpenter.sh` | `provisioners` | get, list, watch | Track Karpenter provisioners for node provisioning policies |

### KEDA Resources

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `keda.sh` | `clustertriggerauthentications` | get, list, watch | Collect KEDA cluster-wide trigger authentication configurations |
| `keda.sh` | `scaledjobs` | get, list, watch | Monitor KEDA ScaledJob resources for event-driven job scaling |
| `keda.sh` | `scaledobjects` | get, list, watch | Track KEDA ScaledObject resources for event-driven autoscaling |
| `keda.sh` | `triggerauthentications` | get, list, watch | Collect KEDA trigger authentication configurations |

### Kubeflow Resources

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `kubeflow.org` | `notebooks` | get, list, watch | Monitor Kubeflow Notebook resources for ML workload tracking |

### Spark Operator Resources

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `sparkoperator.k8s.io` | `scheduledsparkapplications` | get, list, watch | Monitor scheduled Spark application resources for data processing workload tracking |
| `sparkoperator.k8s.io` | `sparkapplications` | get, list, watch | Track Spark application resources for big data processing visibility |

### Authentication & Authorization

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `authentication.k8s.io` | `tokenreviews` | create | Validate service account tokens for metrics endpoint authentication |
| `authorization.k8s.io` | `subjectaccessreviews` | create | Perform authorization checks for metrics endpoint access control |

### Non-Resource URLs

| URL Path | Verbs | Purpose |
|----------|-------|---------|
| `/metrics` | get | Allow Prometheus to scrape operator metrics for monitoring |

---

## Role (Namespace-Scoped) Permissions

These permissions are scoped to the `devzero-zxporter` namespace only:

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `""` (core) | `configmaps` | get, list, watch, create, update, patch, delete | Manage operator configuration and leader election state |
| `""` (core) | `deployments` | get, list, watch, create, update, patch, delete | Manage operator deployment components (legacy - appears redundant with apps group) |
| `""` (core) | `serviceaccounts` | get, list, watch, create, update, patch, delete | Manage service accounts for operator components |
| `""` (core) | `services` | get, list, watch, create, update, patch, delete | Manage operator service endpoints |
| `""` (core) | `events` | create, patch | Emit events for operator lifecycle and status updates |
| `apps` | `deployments` | get, list, watch, create, update, patch, delete | Manage operator and collector deployments |
| `coordination.k8s.io` | `leases` | get, list, watch, create, update, patch, delete | Coordinate leader election for high availability |
| `rbac.authorization.k8s.io` | `rolebindings` | create, delete, get, list, patch, update, watch | Manage role bindings for operator components |
| `rbac.authorization.k8s.io` | `roles` | create, delete, get, list, patch, update, watch | Manage roles for operator components |
| `rbac.authorization.k8s.io` | `clusterrolebindings` | create, delete, get, list, patch, update, watch | Manage cluster role bindings for operator components |
| `rbac.authorization.k8s.io` | `clusterroles` | create, delete, get, list, patch, update, watch | Manage cluster roles for operator components |

---

## Additional ClusterRoles

### CollectionPolicy Editor Role

Allows users to create and manage CollectionPolicy custom resources:

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `devzero.io` | `collectionpolicies` | create, delete, get, list, patch, update, watch | Full management of collection policies |
| `devzero.io` | `collectionpolicies/status` | get | Read collection policy status information |

### CollectionPolicy Viewer Role

Allows users to view CollectionPolicy custom resources (read-only):

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `devzero.io` | `collectionpolicies` | get, list, watch | Read-only access to collection policies |
| `devzero.io` | `collectionpolicies/status` | get | Read collection policy status information |

### Metrics Authentication Role

Used for authenticating requests to the metrics endpoint:

| API Group | Resource | Verbs | Purpose |
|-----------|----------|-------|---------|
| `authentication.k8s.io` | `tokenreviews` | create | Validate tokens for metrics access |
| `authorization.k8s.io` | `subjectaccessreviews` | create | Authorize metrics endpoint access |

### Metrics Reader Role

Allows scraping of operator metrics:

| URL Path | Verbs | Purpose |
|----------|-------|---------|
| `/metrics` | get | Read operator metrics for monitoring and observability |

---