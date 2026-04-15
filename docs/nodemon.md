# Zxporter Nodemon

The Zxporter Nodemon is a DaemonSet that collects GPU metrics from NVIDIA DCGM exporters and exposes them via a unified JSON API. It supports multiple cloud providers (GCP, EKS, AKS) and can work with both embedded and external DCGM exporters.

## Overview

The Nodemon scrapes metrics from NVIDIA DCGM (Data Center GPU Manager) exporters and enriches them with Kubernetes workload information (Deployments, StatefulSets, DaemonSets, Jobs, CronJobs, Argo Rollouts).

### Key Features
- Per-container GPU metrics (utilization, memory, temperature, power)
- Automatic workload resolution (Pod → ReplicaSet → Deployment)
- Multi-cloud support (GCP, EKS, Azure)
- Embedded DCGM exporter or connect to external DCGM instances
- MIG (Multi-Instance GPU) support

## Quick Start

### Deploy with Embedded DCGM Exporter (Recommended)

```bash
helm install zxporter-nodemon helm-chart/zxporter-nodemon \
  --namespace devzero-zxporter --create-namespace \
  --set provider=gcp \
  --set dcgmExporter.enabled=true
```

### Deploy with External DCGM Exporter

```bash
helm install zxporter-nodemon helm-chart/zxporter-nodemon \
  --namespace devzero-zxporter --create-namespace \
  --set provider=gcp \
  --set dcgmExporter.enabled=false \
  --set gpuMetricsExporter.config.DCGM_LABELS="app.kubernetes.io/name=dcgm-exporter"
```

## Configuration Scenarios

### Scenario 1: All-in-One Deployment (Default)

Both nodemon and DCGM exporter run as sidecar containers in the same pod. Best for new deployments.

```yaml
# values.yaml
provider: gcp  # gcp | eks | azure
dcgmExporter:
  enabled: true
  useExternalHostEngine: false
```

### Scenario 2: Connect to Existing DCGM Exporter in Cluster

If you already have DCGM exporter deployed separately, disable the embedded one and configure discovery.

```yaml
# values.yaml
provider: gcp
dcgmExporter:
  enabled: false
gpuMetricsExporter:
  config:
    DCGM_LABELS: "app.kubernetes.io/name=dcgm-exporter"
```

### Scenario 3: Connect to Host-Level DCGM Engine GCP COS)

For environments where DCGM host engine runs at the node level (e.g., GCP with Container-Optimized OS).

```yaml
# values.yaml
provider: gcp
dcgmExporter:
  enabled: true
  useExternalHostEngine: true  # Connect to DCGM running on host
```

### Scenario 4: Single Host Mode (Development/Testing)

Point directly to a specific DCGM exporter instance.

```yaml
# values.yaml
provider: eks
dcgmExporter:
  enabled: false
gpuMetricsExporter:
  config:
    DCGM_HOST: "dcgm-exporter.monitoring.svc.cluster.local"
```

## Pre-Installation Checks

### Check if DCGM Exporter Already Exists

```bash
# Check for existing DCGM exporter pods
kubectl get pods -A -l app.kubernetes.io/name=dcgm-exporter

# Check for DCGM exporter daemonset
kubectl get ds -A | grep -i dcgm

# Check for NVIDIA GPU Operator (includes DCGM)
kubectl get pods -A | grep -i gpu-operator
```

If DCGM exporter already exists, use **Scenario 2** with appropriate labels.

### Check for Host-Level DCGM Engine

```bash
# On GCP, check if DCGM is running on the node
kubectl debug node/<node-name> -it --image=busybox -- \
  sh -c "ps aux | grep dcgm"

# Or check for DCGM socket
kubectl debug node/<node-name> -it --image=busybox -- \
  ls -la /var/run/nvidia/dcgm/
```

If host engine is present, consider using `useExternalHostEngine: true`.

### Verify GPU Nodes

```bash
# List nodes with GPU labels
kubectl get nodes -l cloud.google.com/gcp-accelerator  # GCP
kubectl get nodes -l nvidia.com/gpu.present=true       # Generic
kubectl get nodes -l kubernetes.azure.com/accelerator  # AKS

# Check GPU allocatable resources
kubectl describe nodes | grep -A5 "Allocatable:" | grep nvidia
```

## Helm Values Reference

| Parameter | Description | Default |
|-----------|-------------|---------|
| `provider` | Cloud provider: `gcp`, `eks`, `azure` | **Required** |
| `dcgmExporter.enabled` | Deploy DCGM exporter sidecar | `true` |
| `dcgmExporter.useExternalHostEngine` | Connect to host-level DCGM engine | `false` |
| `dcgmExporter.image.tag` | DCGM exporter image tag | `3.3.7-3.5.0-ubuntu22.04` |
| `gpuMetricsExporter.config.DCGM_HOST` | Direct DCGM host (single host mode) | `""` |
| `gpuMetricsExporter.config.DCGM_LABELS` | Labels to discover DCGM pods | `""` |
| `gpuMetricsExporter.port` | HTTP API port | `6061` |
| `gpuMetricsExporter.affinity` | Custom node affinity | Provider-specific |
| `gpuMetricsExporter.rbac.clusterWide` | Use ClusterRole for cross-namespace discovery | `true` |

## API Reference

### GET /container/metrics

Returns GPU metrics for containers.

```bash
curl http://<pod-ip>:6061/container/metrics
```

**Query Parameters:**
- `pod` - Filter by pod name
- `namespace` - Filter by namespace
- `container` - Filter by container name
- `node` - Filter by node name

**Example Response:**
```json
[
  {
    "node_name": "gke-gpu-pool-abc123",
    "model_name": "Tesla T4",
    "device": "nvidia0",
    "device_uuid": "GPU-93461651-6be6-8fb7-a69a-c9eedc6984db",
    "pod": "training-job-xyz",
    "container": "trainer",
    "namespace": "ml-workloads",
    "workload_name": "training-job",
    "workload_kind": "Job",
    "gpu_utilization": 85.5,
    "temperature": 72.0,
    "power_usage": 125.0,
    "framebuffer_used": 8192.0,
    "framebuffer_total": 16384.0,
    "timestamp": "2026-03-10T12:30:00Z"
  }
]
```

### GET /healthz

Health check endpoint.

```bash
curl http://<pod-ip>:6061/healthz
```

## Cloud Provider Notes

### GCP
- Uses `cloud.google.com/gke-accelerator` node selector
- Requires privileged container for DCGM on COS nodes
- NVIDIA drivers mounted from `/home/kubernetes/bin/nvidia`

### EKS
- Uses `system-node-critical` priority class
- NVIDIA drivers at `/usr/local/nvidia`
- Works with GPU AMIs (Amazon Linux 2, Bottlerocket)

### AKS
- NVIDIA drivers at `/usr/local/nvidia`
- Requires `nvidia.com/gpu` toleration

## Troubleshooting

### DCGM Exporter Not Starting

```bash
# Check DCGM exporter logs
kubectl logs -n devzero-zxporter <pod-name> -c dcgm-exporter

# Common issues:
# - "Could not load NVML library" → NVIDIA drivers not installed
# - "Failed to connect to nv-hostengine" → Use useExternalHostEngine: false
```

### No Metrics Returned

```bash
# Verify DCGM exporter is accessible
kubectl exec -n devzero-zxporter <nodemon-pod> -- \
  curl http://localhost:9400/metrics

# Check nodemon logs
kubectl logs -n devzero-zxporter <pod-name> -c zxporter-nodemon
```

### Pod Discovery Issues

```bash
# Verify RBAC permissions
kubectl auth can-i list pods --as=system:serviceaccount:devzero-zxporter:zxporter-nodemon

# Check configured labels match DCGM exporter
kubectl get pods -A -l <your-dcgm-labels>
```

## Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `HTTP_LISTEN_PORT` | API server port | `6061` |
| `DCGM_HOST` | Direct DCGM exporter host | `""` |
| `DCGM_PORT` | DCGM exporter port | `9400` |
| `DCGM_METRICS_ENDPOINT` | DCGM metrics path | `/metrics` |
| `DCGM_LABELS` | Label selector for DCGM pod discovery | `app.kubernetes.io/name=dcgm-exporter` |
| `NODE_NAME` | Current node name (auto-injected) | - |

## Metrics Collected

| Metric | Description |
|--------|-------------|
| `gpu_utilization` | GPU compute utilization (%) |
| `temperature` | GPU temperature (°C) |
| `memory_temperature` | Memory temperature (°C) |
| `power_usage` | Power draw (W) |
| `framebuffer_used` | GPU memory used (MiB) |
| `framebuffer_free` | GPU memory free (MiB) |
| `framebuffer_total` | Total GPU memory (MiB) |
| `sm_active` | Streaming multiprocessor activity |
| `tensor_active` | Tensor core activity |
| `pcie_tx_bytes` | PCIe transmit bytes |
| `pcie_rx_bytes` | PCIe receive bytes |
| `sm_clock` | SM clock frequency (MHz) |
| `mem_clock` | Memory clock frequency (MHz) |
| `xid_errors` | XID error count |
