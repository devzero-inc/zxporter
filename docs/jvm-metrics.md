# JVM metrics (zxporter-nodemon) — hsperfdata (no hooking)

This spike adds an **optional** endpoint to zxporter-nodemon:

- `GET /container/jvm-metrics`

It discovers Java processes on the node via `/proc` and reads HotSpot's `hsperfdata` file via:

- `/proc/<hostpid>/root/tmp/hsperfdata_*/<nsPid>`

This requires:
- `hostPID: true`
- running zxporter-nodemon as **UID 0** (root) to read those paths

No attach/JMX/javaagent/async-profiler is used.

## Enable via Helm (chart: zxporter-nodemon)

Set:

```yaml
runtimeMetrics:
  enabled: true
```

(The older `jvmMetrics.enabled` key still works as a fallback if `runtimeMetrics.enabled` is
unset — it was renamed because this same toggle now also gates Node.js runtime detection.)

When enabled, the DaemonSet will:
- set `spec.hostPID: true`
- set `runAsUser: 0` and `runAsNonRoot: false` for the `zxporter-nodemon` container

## Local validation (kind)

Assumes you have a kind cluster and a Java workload running.

If using the `kind-jvm-spike` cluster from this spike:

```bash
kubectl config use-context kind-jvm-spike
kubectl -n jvm-spike get pods
```

Then port-forward zxporter-nodemon (namespace depends on your install) and hit:

```bash
curl -sS localhost:6061/container/jvm-metrics | jq '.[0]'
```

Expected fields include:
- `heap_used_bytes`, `heap_size_bytes`, `heap_max_size_bytes`
- `gc_time_seconds_total` (map)
- `safepoint_time_seconds_total`, `safepoint_sync_time_seconds_total`
- `flags_extracted` (best-effort from `/proc/<pid>/cmdline`)
