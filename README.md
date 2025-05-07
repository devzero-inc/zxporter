# ZXporter - Kubernetes Resource Exporter

ZXporter is a Kubernetes operator that collects and exports various Kubernetes resources for monitoring and observability purposes. It provides a comprehensive solution for gathering metrics and data from different types of Kubernetes resources across your cluster.

## Features

### Core Resources
- Pod, Node, Namespace, Event, Endpoints
- ServiceAccount, LimitRange, ResourceQuota

### Workload Resources
- Deployments, StatefulSets, DaemonSets
- ReplicaSets, ReplicationControllers
- Jobs, CronJobs

### Storage Resources
- PersistentVolumeClaims
- PersistentVolumes
- StorageClasses

### Networking Resources
- Services
- Ingress, IngressClasses
- NetworkPolicies

### RBAC Resources
- Roles, RoleBindings
- ClusterRoles, ClusterRoleBindings

### Autoscaling Resources
- HorizontalPodAutoscalers
- VerticalPodAutoscalers

### Policy Resources
- PodDisruptionBudgets

### Custom Resources
- Custom Resource Definitions (CRDs)
- Custom Resource Instances

### Configuration Resources
- ConfigMaps
- Secrets

## Prerequisites

- Go version v1.22.0+
- Docker version 17.03+
- kubectl version v1.11.3+
- Access to a Kubernetes v1.11.3+ cluster
- kind (for local development)

## Quick Start

### Local Development Setup

1. Create a kind cluster:
```sh
kind create cluster
```

2. Update kubeconfig:
```sh
kubectl cluster-info --context kind-kind
```

3. Install required services:
```sh
# Install metrics-server
helm repo add metrics-server https://kubernetes-sigs.github.io/metrics-server/
helm repo update
helm upgrade --install --set args={--kubelet-insecure-tls} metrics-server metrics-server/metrics-server --namespace kube-system

# Install node exporter
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts 
helm repo update
helm install node-exporter prometheus-community/prometheus-node-exporter
```

4. Build and deploy:
```sh
make docker-build IMG=zxporter:tag
kind load docker-image zxporter:tag
make deploy IMG=zxporter:tag
```

### Production Deployment

1. Build and push the image:
```sh
make docker-build docker-push IMG=<your-registry>/zxporter:tag
```

2. Install CRDs:
```sh
make install
```

3. Deploy the operator:
```sh
make deploy IMG=<your-registry>/zxporter:tag
```

4. Apply the collection policy:
```yaml
apiVersion: devzero.io/v1
kind: CollectionPolicy
metadata:
  name: default-policy
  namespace: devzero-zxporter
spec:
  targetSelector:
    namespaces: [] # Empty means all namespaces
  exclusions:
    excludedNamespaces:
      - kube-system
      - kube-public
  policies:
    frequency: "30s"
    bufferSize: 1000
```

## Configuration

The operator is configured through the `CollectionPolicy` Custom Resource. Key configuration options include:

- `targetSelector`: Define which namespaces to collect from
- `exclusions`: Specify namespaces to exclude
- `policies`: Configure collection frequency and buffer size

## Uninstallation

1. Remove CR instances:
```sh
kubectl delete -k config/samples/
```

2. Remove CRDs:
```sh
make uninstall
```

3. Remove the operator:
```sh
make undeploy
```

## Project Distribution

To build the installer:

```sh
make build-installer IMG=<your-registry>/zxporter:tag
```

This generates an `install.yaml` in the `dist` directory containing all necessary resources.

Users can install using:
```sh
kubectl apply -f https://raw.githubusercontent.com/<org>/zxporter/<tag>/dist/install.yaml
```

## Contributing

We welcome contributions! Please see our contributing guidelines for more information.

## License

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

