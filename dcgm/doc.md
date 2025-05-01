
# ZXporter GPU Metrics Collection

This document explains how ZXporter collects GPU metrics from Kubernetes clusters using NVIDIA's DCGM Exporter and Prometheus.

## Overview

ZXporter collects GPU metrics at both the node level and container level by leveraging the NVIDIA DCGM Exporter and Prometheus. The metrics collection flow is as follows:

1.  NVIDIA GPU Operator deploys DCGM Exporter as a DaemonSet on all GPU nodes
2.  DCGM Exporter exposes GPU metrics in Prometheus format
3.  ServiceMonitor configuration enables Prometheus to scrape these metrics
4.  ZXporter queries Prometheus to collect GPU metrics

## Prerequisites

-   Kubernetes cluster with GPU nodes
-   NVIDIA GPU Operator and DCGM exporter installed
-   Prometheus Operator installed
-   ZXporter deployed in the cluster

## ServiceMonitor Configuration

The ServiceMonitor resource is essential for Prometheus to discover and scrape metrics from the DCGM Exporter pods. It should be deployed in the same namespace as your Prometheus Operator.

```yaml
# ServiceMonitor for NVIDIA DCGM Exporter
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: nvidia-dcgm-exporter
  namespace: default
  labels: # Labels should be configured correctly
    app.kubernetes.io/component: metrics
    app.kubernetes.io/instance: prometheus
    app.kubernetes.io/managed-by: Helm
    app.kubernetes.io/name: nvidia-dcgm-exporter
    app.kubernetes.io/part-of: gpu-monitoring
    release: prometheus
  annotations:
    meta.helm.sh/release-name: prometheus
    meta.helm.sh/release-namespace: default
spec:
  selector:
    matchLabels:
      app: nvidia-dcgm-exporter
  endpoints:
  - port: gpu-metrics
    interval: 15s
    scheme: http
    relabelings:
    # Extract Kubernetes node name from pod label and set as 'nodeName' label
    - sourceLabels: [__meta_kubernetes_pod_node_name]
      targetLabel: nodeName
      action: replace
  jobLabel: app

```

This approach uses Kubernetes metadata (`__meta_kubernetes_pod_node_name`) to extract the node name, rather than relying on the `Hostname` label which can be inconsistent.

## Important Note on DCGM Exporter Installation and Label Inconsistencies

1.  **Installation Considerations:**
    -   GPU Operator Helm installation might not always include DCGM Exporter
    -   If GPU Operator is present but DCGM is missing, manual installation may be required
    -   Verify DCGM Exporter is running with `kubectl get pods -A -l app=nvidia-dcgm-exporter`
    -   Before configuring ServiceMonitor, confirm that DCGM labels follow the expected pattern where `container`, `pod`, and `namespace` labels contain the info of workloads actually using the GPU
    -   Once label consistency is confirmed, proceed to create the ServiceMonitor to expose DCGM metrics to Prometheus
2.  **Label Inconsistency Scenarios:**
    -   **When GPU operator, DCGM exporter, and workloads run in same namespace:**
        -   Container labels split between actual workload and exporter
        -   `container`, `pod`, `namespace` labels represent DCGM exporter itself
        -   `exported_container`, `exported_pod`, `exported_namespace` labels contain actual GPU workload info
    -   **In other configurations:**
        -   The `exported_*` labels may be absent
        -   `container`, `pod`, `namespace` labels directly represent the GPU workload
        -   No distinction between exporter and workload labels
3.  **Node Name Label Variations:**
    -   `Hostname` label sometimes contains node name, sometimes doesn't
    -   For reliability, always use `nodeName` label added by ServiceMonitor relabeling
    -   Use `__meta_kubernetes_pod_node_name` as source for consistent node identification

**Example of Mixed Labels in Metrics:**

```
DCGM_FI_DEV_GPU_UTIL{
  # DCGM exporter container information
  container="nvidia-dcgm-exporter",
  pod="nvidia-dcgm-exporter-7mqjg",
  namespace="default",
  
  # Actual GPU workload information
  exported_container="dcgmproftester2",
  exported_namespace="default",
  exported_pod="dcgmproftester2",
  
  # Node information
  Hostname="computeinstance-e00fpf26nw416hqf98",
  nodeName="computeinstance-e00fpf26nw416hqf98",
  
  # Other metadata
  DCGM_FI_DRIVER_VERSION="550.144.03",
  UUID="GPU-785d10fe-778c-7afa-ded5-3ad45532aa2d",
  ...
}
```