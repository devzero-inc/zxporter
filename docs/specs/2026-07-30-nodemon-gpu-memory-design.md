# Nodemon GPU Memory — OOM Fix

**Date:** 2026-07-30
**Component:** `operators/zxporter` — `zxporter-nodemon` DaemonSet (GPU nodes)
**Status:** Approved — A+C this pass, B as follow-up. Keep the 256Mi limit.

## Problem

`zxporter-nodemon-gpu-*` pods OOM at their **256Mi** cgroup cap (observed
12:05 and 12:31, OOM → restart → OOM). Non-GPU nodemon peaks at ~107MB; GPU
nodemon reaches 256MB+. The entire delta is the **DCGM scrape+parse path**,
which only runs on GPU nodes.

### Root cause (three compounding factors)

1. **Full-export parse.** `scraper.go` calls
   `expfmt.TextToMetricFamilies(resp.Body)`, materializing the *entire* DCGM
   export as a pointer-dense object graph (`*MetricFamily`→`[]*Metric`→
   `[]*LabelPair`, values `*float64`). The `EnabledMetrics` filter (~30 metrics)
   runs only *after* the full parse (`mapper.go`), so every series is
   materialized then mostly discarded. This is the peak.
2. **Redundant/overlapping parses.** GPU DCGM is parsed on two independent
   paths: the 30s unified loop *and* every `/container/metrics` request
   (`handler.go` → `QueryMetrics` re-scrapes per request; the collector hits it
   once per node per cycle). Overlap → concurrent parses → additive peak.
3. **No runtime containment.** The nodemon binary
   (`cmd/zxporter-nodemon/main.go`) does not import `automemlimit` (only the
   controller-manager does), and has **no CPU limit** — so on a 100+‑core GPU
   node `GOMAXPROCS` = core count, inflating per-P mcaches and GC worker count.
   With `GOGC=100` and no soft limit, a transient parse burst sails past the
   256Mi cap before GC runs.

### Consumer cadence (data-quality anchor)

- DCGM exporter refreshes every **5s** (`DCGM_EXPORTER_INTERVAL=5000`).
- The container collector polls `/container/metrics` every **10s**
  (`container_resource_collector.go`, default `UpdateInterval`).
- So today's per-request scrape already yields ~10s-granular data downstream.

## Data-quality invariants (must not break)

1. **Value fidelity** — enabled metric values byte-identical to today.
2. **Full cardinality** — every GPU×pod×container×MIG series still emitted.
3. **Freshness** — no consumer sees staler data than today. Cache refresh
   interval ∈ [5s (DCGM), 10s (collector poll)]. 10s is data-quality-neutral.

## Fixes

### This pass

**Fix A — Single shared, periodically-refreshed GPU snapshot.**
Wrap the GPU `*Exporter` with a cache: `[]GPUMetric` + `lastRefresh` +
`lastErr`, guarded by `RWMutex`. A single background goroutine
(`Start(ctx)`) scrapes+parses+maps on a ticker (default **10s**, env
`GPU_REFRESH_INTERVAL`, clamped ≥5s). `QueryMetrics` becomes a non-blocking
cache read. Both the legacy `/container/metrics` handler and the unified
exporter's `fetchGPU` read the snapshot → **exactly one parse per interval**,
independent of request rate; no overlapping parses.
- *Staleness guard:* if `now - lastRefresh > k×interval`, still serve the
  snapshot but log + flag stale (protects quality; never silently serves very
  old data).
- *Decoupled from the 30s unified loop* — GPU refresh runs at 10s so GPU
  granularity is preserved.

**Fix C — Runtime containment.**
- Import `automemlimit` in `cmd/zxporter-nodemon/main.go`; log effective
  `GOMEMLIMIT` (mirror the controller-manager).
- Pin `GOMAXPROCS` low via env (e.g. `2`–`4`) in the DaemonSet — nodemon's work
  is light; avoids the high-core baseline. (Preferred over adding a CPU limit,
  which causes CFS throttling.)
- **Keep the 256Mi limit** (per decision). `GOMEMLIMIT` derives from it.

### Follow-up (next PR)

**Fix B — Streaming, filter-during-parse.**
Replace `TextToMetricFamilies` with a dependency-free streaming parser
(`bufio.Reader` over `io.LimitReader(body)`) that keeps only
`EnabledMetrics` lines and discards the rest immediately. Avoid
`prometheus/prometheus/model/textparse` (heavy dep tree). Peak drops from
"whole export" to "≈30 kept series."
- **Gate:** golden-sample parity test — a captured DCGM `/metrics` body run
  through both the current `TextToMetricFamilies`+mapper and the new parser must
  yield identical `[]GPUMetric`. This guarantees B changes memory, not data.

## Risk called out

Keeping 256Mi while deferring B means A+C must fit **one** DCGM parse + reduced
baseline under 256Mi. A removes the overlap peak; the `GOMAXPROCS` cut lowers
the ~107MB floor; but a single large parse is only fully addressed by B. Read
the pprof heap after deploy — if the single-parse peak still crowds 256Mi,
promote B.

## Verification

- Unit test: legacy handler serves the cached snapshot without scraping
  per-request; staleness guard behaves.
- Golden-sample parity test (with Fix B).
- Existing nodemon suite green; `just fmt` / `just lint` / build.
- Live: pprof heap (`:6061/debug/pprof/heap`, `allocs`) before/after on a GPU
  nodemon pod; `kubectl top` RSS.
