# Generic runtime detection (.NET, Go, GraalVM, Python, Ruby, Deno, Bun)

`zxporter-nodemon` automatically detects the language runtime running in every
container on the node — no customer opt-in, no workload changes — and reports
existence + best-effort version to the control plane, the same way JVM and
Node.js detection work. This seeds runtime-aware heuristics for memory
request/limit recommendations (each runtime has different heap-sizing and
OOM-failure behavior).

Detection is entirely passive: nothing is ever executed, and only /proc
pseudo-files and bounded, streamed reads of on-disk binaries are used. It rides
on the same privilege grant as JVM detection (`hostPID` + root + `SYS_PTRACE`),
gated by `runtimeMetrics.enabled` (default `true`).

## Detection & version resolution per runtime

| Runtime | Detected via | Version sources (in order) |
|---|---|---|
| .NET (`dotnet`) | comm/argv0 `dotnet`, or mapped `libcoreclr.so` (apphost-deployed apps) | `DOTNET_VERSION` env; `Microsoft.NETCore.App/<ver>/` in `/proc/<pid>/maps` |
| Go (`go`) | embedded Go build info in the executable (probe) | build info (`go1.x.y`, prefix stripped) |
| GraalVM native-image (`graalvm-native-image`) | SubstrateVM ELF section in the executable (probe) | embedded `GraalVM ...` release string (best-effort) |
| Python (`python`) | comm/argv0 `python[x[.y]]`, or interpreter exe basename (gunicorn/celery-style script comms) | `PYTHON_VERSION` env; exe basename; comm |
| Ruby (`ruby`) | comm/argv0 `ruby`, or interpreter exe basename (puma/rails-style) | `RUBY_VERSION` env; `libruby.so.<ver>` in maps |
| Deno (`deno`) | comm/argv0 `deno` | `DENO_VERSION` env; embedded `Deno/x.y.z` user-agent scan |
| Bun (`bun`) | comm/argv0 `bun` | `BUN_VERSION` env; embedded `Bun/x.y.z` user-agent scan |

Version may legitimately be empty (stripped/custom builds) — the workload is
still reported as detected.

GraalVM native-image detection exists specifically because those are Java
workloads the hsperfdata-based JVM detector cannot see (no HotSpot, no perf
counters file).

## Endpoint

These detections are served in the `runtimes` bucket of the combined endpoint
(alongside the `jvm` and `nodejs` buckets — see `docs/jvm-metrics.md` and
`docs/nodejs-metrics.md`):

```
GET http://<nodemon>:6061/container/runtime-metrics
```

Supports the same `?container=`, `?pod=`, `?namespace=`, `?node=` filters.
Example entry:

```json
{
  "runtime": "go",
  "node_name": "worker-1",
  "pod": "go-app-5b5f7dd8b9-x2j4k",
  "namespace": "default",
  "container": "app",
  "container_id": "42726430c4ac…",
  "pid_host": 6339,
  "pid_ns": 1,
  "version": "1.26.3",
  "version_source": "buildinfo",
  "raw_cmdline": "caddy run --config /etc/caddy/Caddyfile",
  "timestamp": "2026-07-01T20:41:09Z"
}
```

The zxporter collector attaches these to `ContainerMetricsSnapshot.runtimeProcesses`
(one entry per detected runtime per container) flowing through the generic
`ContainerResource` pipeline to the control plane.

## Cost bounds

- The /proc walk is shared with JVM/Node.js discovery — one walk per query.
- Executable probes (Go build info, GraalVM ELF section) read only file
  headers/sections and run only for containerized processes not classifiable
  from comm/cmdline.
- Binary version scans stream in 4MiB chunks with a hard 64MiB cap — peak
  memory is the chunk buffer, not the scan window — and results are cached per
  container with bounded retries (5 attempts) for unresolvable binaries.
