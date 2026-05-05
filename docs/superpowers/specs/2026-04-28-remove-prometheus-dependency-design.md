# Remove Prometheus Dependency from ZXporter

## Goal

Eliminate zxporter's runtime dependency on Prometheus server, node-exporter, and kube-state-metrics. All metrics currently sourced from Prometheus will be collected directly from kubelet endpoints via the nodemon DaemonSet, following the pattern established by Cortex (`/api/v1/nodes/{node}/proxy/stats/summary`) and the existing GPU metrics exporter branch (`ph/gpu-metrics-exporter`).

## Current State

ZXporter deploys and queries a Prometheus stack (Prometheus server, node-exporter DaemonSet, kube-state-metrics) to collect container, node, PVC, and GPU metrics. Five collectors depend on Prometheus:

| Collector | What it queries | PromQL examples |
|---|---|---|
| ContainerResourceCollector | Container CPU, memory, network I/O, disk I/O, CPU throttle | `rate(container_cpu_usage_seconds_total[5m])`, `container_memory_working_set_bytes`, `rate(container_network_*[5m])`, `rate(container_fs_*[5m])`, `rate(container_cpu_cfs_throttled_periods_total[5m]) / rate(container_cpu_cfs_periods_total[5m])` |
| NodeCollector | Node network, disk, GPU metrics | `rate(node_network_*[5m])`, `rate(node_disk_*[5m])`, `DCGM_FI_DEV_*` |
| PVCMetricsCollector | PVC storage usage | `kubelet_volume_stats_used_bytes`, `kubelet_volume_stats_capacity_bytes`, `kubelet_volume_stats_available_bytes` |
| HistoricalMetricsCollector | 24h percentile aggregations (P50-P99, Pmax) for CPU and memory | `quantile_over_time(0.90, rate(container_cpu_usage_seconds_total[5m])[24h:1m])` |
| GPU metrics (NodeCollector + ContainerResourceCollector) | DCGM GPU utilization, memory, power, temp | `DCGM_FI_DEV_GPU_UTIL`, `DCGM_FI_DEV_FB_USED`, etc. |

## Data Source Replacement Map

### Container-level metrics (ContainerResourceCollector)

| Current PromQL | New Source | Endpoint |
|---|---|---|
| `rate(container_cpu_usage_seconds_total[5m])` | kubelet `stats/summary` -> `usageNanoCores` (instant rate, no computation needed) | `/api/v1/nodes/{node}/proxy/stats/summary` |
| `container_memory_working_set_bytes` | kubelet `stats/summary` -> `workingSetBytes` | same |
| `rate(container_network_receive_bytes_total[5m])` | kubelet `stats/summary` -> `rxBytes` | same |
| `rate(container_network_transmit_bytes_total[5m])` | kubelet `stats/summary` -> `txBytes` | same |
| `rate(container_network_receive_packets_total[5m])` | cAdvisor counter -> nodemon computes rate | `/metrics/cadvisor` |
| `rate(container_network_receive_errors_total[5m])` | cAdvisor counter -> nodemon computes rate | same |
| `rate(container_network_*_dropped_total[5m])` | cAdvisor counter -> nodemon computes rate | same |
| `rate(container_fs_reads_bytes_total[5m])` | cAdvisor counter -> nodemon computes rate | same |
| `rate(container_fs_writes_bytes_total[5m])` | cAdvisor counter -> nodemon computes rate | same |
| `rate(container_cpu_cfs_throttled_periods_total[5m]) / rate(container_cpu_cfs_periods_total[5m])` | cAdvisor counters -> nodemon computes ratio | same |

### Node-level metrics (NodeCollector)

| Current PromQL | New Source |
|---|---|
| `rate(node_network_*[5m])` | kubelet `stats/summary` for bytes; cAdvisor for errors/drops/packets |
| `rate(node_disk_*[5m])` | cAdvisor `/metrics/cadvisor` counters |

### PVC metrics (PVCMetricsCollector)

| Current PromQL | New Source |
|---|---|
| `kubelet_volume_stats_used_bytes` | kubelet `stats/summary` -> `volume[].usedBytes` |
| `kubelet_volume_stats_capacity_bytes` | kubelet `stats/summary` -> `volume[].capacityBytes` |
| `kubelet_volume_stats_available_bytes` | kubelet `stats/summary` -> `volume[].availableBytes` |

### GPU metrics

Already handled by nodemon on `ph/gpu-metrics-exporter` branch. No change needed.

### Historical 24h percentiles (HistoricalMetricsCollector)

| Current PromQL | New Source |
|---|---|
| `quantile_over_time(P, rate(cpu[5m])[24h:1m])` | DAKR ClickHouse `hourly_workload_summary_metrics_ch` via `quantilesGKMerge()` |
| `quantile_over_time(P, memory[24h])` | same |
| `max_over_time(rate(cpu[5m])[24h:1m])` | Derivable from P999 or `max()` on hourly table |
| `count_over_time(memory[24h])` | `count` column in hourly summary table |

## Architecture

### Nodemon DaemonSet (extended)

The existing nodemon DaemonSet (`ph/gpu-metrics-exporter` branch) is extended from GPU-only to a unified node-local metric collector:

```
nodemon pod (DaemonSet, one per node)
|-- stats/summary poller        <-- kubelet JSON API (Cortex pattern)
|   CPU, memory, network bytes, PVC storage
|
|-- cAdvisor scraper            <-- /metrics/cadvisor (Prometheus text format)
|   CPU throttle, disk I/O, network errors/drops/packets
|
|-- DCGM scraper                <-- already exists
|   GPU metrics
|
|-- HTTP API server (:6061)     <-- already exists
    GET /container/metrics      <-- extended: all container metrics
    GET /node/metrics           <-- new: node-level aggregates
    GET /pvc/metrics            <-- new: PVC storage stats
    GET /healthz                <-- already exists
```

### Rate computation for counters

Nodemon computes rates locally for all counter-based metrics. This covers:
- cAdvisor counters (CPU throttle periods, disk I/O ops, network packets/errors/drops) scraped from the local kubelet's `/metrics/cadvisor` endpoint (not via API proxy — nodemon runs on the same node)
- `stats/summary` cumulative values (network `rxBytes`/`txBytes` are cumulative totals, not instant rates)

Rate computation approach:
- Scrapes every 30 seconds (configurable)
- Stores previous counter values in memory per container/node
- Computes `(current - previous) / elapsed_seconds`
- First sample after startup returns 0 (no previous value)
- Handles counter resets (if current < previous, treat as reset, skip one sample)

This is the same approach Prometheus uses internally.

### Historical percentile cache (replaces HistoricalMetricsCollector)

New component `HistoricalPercentileCache` in zxporter replaces `HistoricalMetricsCollector`:

```
DAKR ClickHouse (source of truth, minute-bucketed data + hourly GK sketches)
    | (every 15 min, background fetch via existing DakrClient)
zxporter HistoricalPercentileCache (in-memory map)
    | (gRPC stream, sub-ms, in-cluster)
DAKR operator (HPA engine, 30s evaluation)
```

Design rationale for keeping historical data in-cluster (zxporter -> operator):
- Operator makes autoscaling decisions every 30 seconds; this is a hot path
- In-cluster latency is sub-millisecond vs 10-200ms+ to control plane
- Autoscaling continues working during control plane outages
- zxporter caches pre-computed percentiles, serves them to operator via existing fast gRPC stream

The `HistoricalPercentileCache` implements the same interface as `HistoricalMetricsCollector`:

```go
FetchPercentilesForAll(ctx, queries []WorkloadQuery) []PercentileResult
```

MPA Server and DAKR operator see zero change.

### Failure modes

| Scenario | Behavior |
|---|---|
| Control plane unreachable on startup | MPA Server starts without historical data (same as today when Prometheus is starting up) |
| Control plane goes down after running | Stale cache continues serving last-fetched percentiles. Log warning. |
| Control plane slow (>5s) | Background fetch with timeout. Never blocks the MPA stream. |
| zxporter pod restarts | First fetch on startup (~1 API call). No 24h blind spot (unlike Prometheus which needs to re-scrape). |
| Nodemon pod restarts | First cAdvisor scrape returns 0 for rate metrics (no previous sample). Normal after one interval. |

## Components Removed

| Component | Action |
|---|---|
| Prometheus server Deployment | Remove from Helm chart |
| Node-exporter DaemonSet | Remove (nodemon replaces it) |
| Kube-state-metrics Deployment | Remove (zxporter already watches nodes via K8s API) |
| Prometheus ConfigMap (scrape config) | Remove |
| Prometheus ServiceAccount + ClusterRole + ClusterRoleBinding | Remove |
| `prometheus-dz-prometheus-server` Service | Remove |

## Components Unchanged

| Component | Why unchanged |
|---|---|
| MPA Server gRPC stream | Same interface, same proto messages |
| DAKR operator | Receives identical data, unaware of source change |
| OOM detection (pod_collector.go) | Already from pod status, not Prometheus |
| TelemetrySender | Collects from local Prometheus *registry* (self-instrumentation), not the Prometheus *server* |
| Collection pipeline (batcher, DirectSender, DakrClient) | Data source is upstream of these |
| Nodemon GPU scraping | Already exists, no change |

## RBAC Changes

| Permission | ServiceAccount | Change |
|---|---|---|
| `nodes/proxy` | nodemon | Add (needed for `stats/summary` and cAdvisor) |
| `nodes/proxy` | Prometheus (`prometheus-dz-prometheus-server`) | Remove (Prometheus gone) |
| `nodes/metrics` | zxporter (`devzero-zxporter-manager-role`) | Keep (already has it) |
| All Prometheus RBAC | `prometheus-dz-prometheus-server`, `prometheus-kube-state-metrics` | Remove |

## Migration Strategy

### Phase 1: Test current behavior

Add tests that pin down the transformation logic of each Prometheus-dependent collector. These tests mock the Prometheus API client, feed known PromQL responses, and assert exact output.

| Collector | What to test |
|---|---|
| ContainerResourceCollector | Given known Prometheus responses -> assert exact `ContainerMetricsSnapshot` (CPU millis, memory bytes, throttle fraction, network, disk) |
| NodeCollector | Given known Prometheus responses -> assert node metric output |
| PVCMetricsCollector | Given known Prometheus responses -> assert PVC used/capacity/available bytes |
| HistoricalMetricsCollector | Given known Prometheus responses -> assert percentile values (P50-P99, Pmax, sample count) |
| MPA Server Broadcast | Given a `ContainerMetricsSnapshot` -> assert exact `ContainerMetricItem` proto message |

These tests validate the transformation, not the data source. When we swap the source, these tests must still pass.

### Phase 2: Extend nodemon (behind feature flag)

Add `stats/summary` + cAdvisor scraping to nodemon. Deploy alongside existing Prometheus.

Feature flag: `ENABLE_NODEMON_METRICS=true` (default: false)

When enabled, ContainerResourceCollector, NodeCollector, and PVCMetricsCollector query nodemon instead of Prometheus. Existing Prometheus path stays as fallback.

Use the `CompareGPUMetrics()` validation pattern (already exists on GPU branch) to compare nodemon output vs Prometheus output in production.

### Phase 3: Replace HistoricalMetricsCollector

Swap to `HistoricalPercentileCache` (DAKR control plane fetch). Also behind the same feature flag.

### Phase 4: Remove Prometheus

Once validated in production:
- Remove Prometheus, node-exporter, kube-state-metrics from Helm chart
- Deprecate `PROMETHEUS_URL` config with warning log
- Remove Prometheus client imports from collectors
- Clean up RBAC
- Feature flag becomes default-on, eventually removed

## Nodemon HTTP API (extended)

### GET /container/metrics

Query params: `?node=<name>` (required)

Optional filters: `?namespace=<ns>&pod=<name>&container=<name>`

Response:
```json
[
  {
    "node_name": "node-1",
    "namespace": "default",
    "pod": "web-abc123",
    "container": "nginx",
    "timestamp": "2026-04-28T12:00:00Z",

    "cpu_usage_nanocores": 50000000,
    "memory_working_set_bytes": 104857600,
    "memory_usage_bytes": 134217728,
    "memory_rss_bytes": 94371840,

    "network_rx_bytes": 1024000,
    "network_tx_bytes": 512000,
    "network_rx_packets_per_sec": 150.5,
    "network_tx_packets_per_sec": 120.3,
    "network_rx_errors_per_sec": 0.0,
    "network_tx_errors_per_sec": 0.0,
    "network_rx_drops_per_sec": 0.0,
    "network_tx_drops_per_sec": 0.0,

    "disk_read_bytes_per_sec": 4096.0,
    "disk_write_bytes_per_sec": 8192.0,
    "disk_read_ops_per_sec": 10.0,
    "disk_write_ops_per_sec": 20.0,

    "cpu_throttle_fraction": 0.05,

    "gpu_utilization": 85.5,
    "gpu_memory_used_mib": 8192.0,
    "gpu_memory_free_mib": 8192.0,
    "gpu_power_watts": 125.0,
    "gpu_temperature_celsius": 72.0
  }
]
```

### GET /node/metrics

Query params: `?node=<name>` (required)

Response:
```json
{
  "node_name": "node-1",
  "timestamp": "2026-04-28T12:00:00Z",

  "network_rx_bytes_per_sec": 52428800.0,
  "network_tx_bytes_per_sec": 26214400.0,
  "network_rx_packets_per_sec": 50000.0,
  "network_tx_packets_per_sec": 40000.0,
  "network_rx_errors_per_sec": 0.0,
  "network_tx_errors_per_sec": 0.0,
  "network_rx_drops_per_sec": 0.0,
  "network_tx_drops_per_sec": 0.0,

  "disk_read_bytes_per_sec": 10485760.0,
  "disk_write_bytes_per_sec": 20971520.0,
  "disk_read_ops_per_sec": 500.0,
  "disk_write_ops_per_sec": 1000.0,

  "gpu_count": 4,
  "gpu_utilization_avg": 72.5,
  "gpu_utilization_max": 95.0,
  "gpu_memory_used_total_mib": 32768.0,
  "gpu_memory_free_total_mib": 32768.0,
  "gpu_power_usage_total_watts": 500.0,
  "gpu_temperature_avg_celsius": 68.0,
  "gpu_temperature_max_celsius": 78.0
}
```

### GET /pvc/metrics

Query params: `?node=<name>` (required)

Response:
```json
[
  {
    "namespace": "default",
    "pod": "postgres-0",
    "pvc_name": "data-postgres-0",
    "used_bytes": 5368709120,
    "capacity_bytes": 10737418240,
    "available_bytes": 5368709120
  }
]
```

## Dependencies

- `ph/gpu-metrics-exporter` branch must be merged to `main` first (nodemon base)
- DAKR control plane must expose a percentile fetch API accessible to zxporter (may already exist via existing DakrClient connection; needs verification)
- Nodemon Helm chart needs `nodes/proxy` RBAC permission added
