# ZXporter - Kubernetes Resource Exporter

ZXporter is a Kubernetes operator that collects and exports various Kubernetes resources for monitoring and observability purposes. It provides a comprehensive solution for gathering metrics and data from different types of Kubernetes resources across your cluster.

## Overview

ZXporter is designed to help you monitor and observe your Kubernetes cluster by collecting data from various resources and making it available for analysis. It's particularly useful for:
- Monitoring cluster health and performance
- Collecting resource utilization metrics
- Tracking configuration changes
- Gathering security-related information
- Supporting compliance and auditing requirements

## Architecture

ZXporter operates as a Kubernetes operator with the following components:

- **Controller**: Manages the lifecycle of collection policies and coordinates data collection
- **Collectors**: Specialized components for different resource types
- **Storage**: Temporary buffer for collected data
- **Exporters**: Components that make the collected data available to external systems

The operator uses a modular design that allows for easy extension and customization.

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

### Advanced Configuration

```yaml
apiVersion: devzero.io/v1
kind: CollectionPolicy
metadata:
  name: advanced-policy
  namespace: devzero-zxporter
spec:
  targetSelector:
    namespaces: ["app1", "app2"]
    labelSelector:
      matchLabels:
        environment: production
  exclusions:
    excludedNamespaces:
      - kube-system
      - kube-public
    excludedResources:
      - secrets
      - configmaps
  policies:
    frequency: "30s"
    bufferSize: 1000
    retentionPeriod: "24h"
    exportFormat: "prometheus"
```

## Troubleshooting

### Common Issues

1. **RBAC Permission Errors**
   ```sh
   Error: failed to create resource: the server does not allow access to the requested resource
   ```
   Solution: Ensure the operator has the necessary RBAC permissions:
   ```sh
   kubectl create clusterrolebinding zxporter-admin --clusterrole=cluster-admin --serviceaccount=devzero-zxporter:zxporter-controller-manager
   ```

2. **Collection Policy Not Applied**
   ```sh
   Error: no collection policy found in namespace
   ```
   Solution: Verify the CollectionPolicy CR is properly created:
   ```sh
   kubectl get collectionpolicy -n devzero-zxporter
   ```

3. **High Resource Usage**
   If the operator is consuming too many resources, adjust the collection frequency and buffer size in the policy.

### Logs and Debugging

View operator logs:
```sh
kubectl logs -n devzero-zxporter deployment/zxporter-controller-manager
```

Enable debug logging:
```yaml
apiVersion: devzero.io/v1
kind: CollectionPolicy
metadata:
  name: debug-policy
spec:
  policies:
    logLevel: "debug"
```

## Security Considerations

- The operator requires cluster-wide permissions to collect data
- Sensitive data (like Secrets) are excluded from collection
- Network communication is secured using TLS
- RBAC policies should be carefully configured

## Performance Tuning

For optimal performance:

1. Adjust collection frequency based on your needs
2. Configure appropriate buffer sizes
3. Use namespace selectors to limit collection scope
4. Monitor resource usage and adjust accordingly

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

### Development Setup

1. Fork the repository
2. Create a feature branch
3. Make your changes
4. Run tests: `make test`
5. Submit a pull request

### Testing

Run the test suite:
```sh
make test
```

Run specific tests:
```sh
go test ./... -run TestName
```

## Support

- GitHub Issues: [Report bugs or request features](https://github.com/devzero-inc/zxporter/issues)
- Documentation: [Detailed documentation](https://github.com/devzero-inc/zxporter/docs)
- Community: [Join our community](https://github.com/devzero-inc/zxporter/discussions)

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

