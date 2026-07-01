# Node.js runtime detection (zxporter-nodemon)

This adds an **optional** endpoint to zxporter-nodemon:

- `GET /container/nodejs-metrics`

It discovers Node.js processes on the node via `/proc`, the same way JVM detection works
(see `docs/jvm-metrics.md`), and resolves the Node.js version on a best-effort, **zero
opt-in** basis:

1. `NODE_VERSION` env var, set by official Docker Hub `node:*` images, read via
   `/proc/<hostpid>/environ`.
2. Failing that, a **read-only** scan (no execution) of the on-disk `node` binary via
   `/proc/<hostpid>/root/<exe>` for the release-URL string Node embeds in its own binary
   for `process.release` metadata (e.g. `nodejs.org/download/release/v20.11.1/`). This is
   deliberately anchored on Node's own release metadata rather than a generic semver regex,
   which would also match the V8/OpenSSL/ICU version strings baked into the same binary.
3. If neither resolves, the process is still reported as a detected Node.js workload with
   an empty version.

Scope note: unlike JVM detection, this does **not** capture live heap usage or
`--max-old-space-size`/`NODE_OPTIONS` flags — V8 has no hsperfdata-equivalent counters file
readable from outside the process without an opt-in flag, so that remains a separate,
not-yet-built follow-up.

This requires the same privileges as JVM detection:
- `hostPID: true`
- running zxporter-nodemon as **UID 0** (root) with `SYS_PTRACE`

Both JVM and Node.js detection share a single Pod informer (`PodContainerIndex`) rather than
each running their own Pod watch.

## Enable via Helm (chart: zxporter-nodemon)

Set:

```yaml
runtimeMetrics:
  enabled: true
```

(The older `jvmMetrics.enabled` key still works as a fallback if `runtimeMetrics.enabled` is
unset.)

## Local validation (kind)

Port-forward zxporter-nodemon and hit:

```bash
curl -sS localhost:6061/container/nodejs-metrics | jq '.[0]'
```

Expected fields:
- `node_version`, `node_version_source` (`"env"` or `"binary-scan"`, empty if unresolved)
- `raw_cmdline`
