# zxporter

## development

1. install `kind` cluster or something like that
2. `kubectl` -- set the context to the kind cluster
3. build the thing: `make docker-buildx IMG=ttl.sh/zxporter:dev-build`
4. deploy the thing: `make deploy IMG=ttl.sh/zxporter:dev-build`

## Implementation 

### Core Resources

[x] PodCollector
[x] NodeCollector
[x] NamespaceCollector
[x] EventCollector
[ ] EndpointsCollector
[ ] ServiceAccountCollector
[ ] LimitRangeCollector
[ ] ResourceQuotaCollector

### Workload Resources

[x] DeploymentCollector
[x] StatefulSetCollector
[x] DaemonSetCollector
[x] ReplicaSetCollector
[ ] ReplicationControllerCollector
[x] JobCollector
[x] CronJobCollector

### Storage Resources

[x] PersistentVolumeClaimCollector
[ ] PersistentVolumeCollector
[ ] StorageClassCollector

### Networking Resources

[x] ServiceCollector
[ ] IngressCollector
[ ] IngressClassCollector
[ ] NetworkPolicyCollector

### RBAC Resources

[ ] RoleCollector
[ ] RoleBindingCollector
[ ] ClusterRoleCollector
[ ] ClusterRoleBindingCollector

### Autoscaling Resources

[ ] HorizontalPodAutoscalerCollector
[ ] VerticalPodAutoscalerCollector

### Policy Resources

[ ] PodDisruptionBudgetCollector
[ ] PodSecurityPolicyCollector

### Custom Resources

[ ] CRDCollector (Custom Resource Definitions)
[ ] CustomResourceCollector (Instances of custom resources)

### Configuration Resources

[x] ConfigMapCollector
[ ] SecretCollector

## Local test flow:

* build kind cluster
  `kind create cluster`

* update kubeconfig of kind cluster
  `kubectl cluster-info --context kind-kind`

* install metrics service in kind cluster
  ```
    helm repo add metrics-server https://kubernetes-sigs.github.io/metrics-server/
    helm repo update
    helm upgrade --install --set args={--kubelet-insecure-tls} metrics-server metrics-server/metrics-server --namespace kube-system
  ``` 

* install node exporter
  ```
    helm repo add prometheus-community https://prometheus-community.github.io/helm-charts 
    helm repo update
    helm install node-exporter prometheus-community/prometheus-node-exporter
  ```

* build docker image to push into cluster
  ```
    make docker-build docker-push IMG=<some-registry>/zxporter:tag or docker build --build-arg GITHUB_TOKEN=<token> -t <some-registry>/zxporter:tag --push .
    make deploy IMG=<some-registry>/zxporter:tag
  ```

* uninstall things from cluster
  ```
    make undeploy
  ```

* cluster needs collection policy to understand what to collect, so install this there
  ```
    apiVersion: monitoring.devzero.io/v1
    kind: CollectionPolicy
    metadata:
      name: default-policy
      namespace: zxporter-system
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

## Description
// TODO(user): An in-depth paragraph about your project and overview of use

## Getting Started

### Prerequisites
- go version v1.22.0+
- docker version 17.03+.
- kubectl version v1.11.3+.
- Access to a Kubernetes v1.11.3+ cluster.

### To Deploy on the cluster
**Build and push your image to the location specified by `IMG`:**

```sh
make docker-build docker-push IMG=<some-registry>/zxporter:tag
```

**NOTE:** This image ought to be published in the personal registry you specified.
And it is required to have access to pull the image from the working environment.
Make sure you have the proper permission to the registry if the above commands don’t work.

**Install the CRDs into the cluster:**

```sh
make install
```

**Deploy the Manager to the cluster with the image specified by `IMG`:**

```sh
make deploy IMG=<some-registry>/zxporter:tag
```

> **NOTE**: If you encounter RBAC errors, you may need to grant yourself cluster-admin
privileges or be logged in as admin.

**Create instances of your solution**
You can apply the samples (examples) from the config/sample:

```sh
kubectl apply -k config/samples/
```

>**NOTE**: Ensure that the samples has default values to test it out.

### To Uninstall
**Delete the instances (CRs) from the cluster:**

```sh
kubectl delete -k config/samples/
```

**Delete the APIs(CRDs) from the cluster:**

```sh
make uninstall
```

**UnDeploy the controller from the cluster:**

```sh
make undeploy
```

## Project Distribution

Following are the steps to build the installer and distribute this project to users.

1. Build the installer for the image built and published in the registry:

```sh
make build-installer IMG=<some-registry>/zxporter:tag
```

NOTE: The makefile target mentioned above generates an 'install.yaml'
file in the dist directory. This file contains all the resources built
with Kustomize, which are necessary to install this project without
its dependencies.

2. Using the installer

Users can just run kubectl apply -f <URL for YAML BUNDLE> to install the project, i.e.:

```sh
kubectl apply -f https://raw.githubusercontent.com/<org>/zxporter/<tag or branch>/dist/install.yaml
```

## Contributing
// TODO(user): Add detailed information on how you would like others to contribute to this project

**NOTE:** Run `make help` for more information on all potential `make` targets

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

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

