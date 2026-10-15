// Package redmetrics turns socket observations into per-container, per-minute RED
// measurements for the containers running on this node.
//
// The shape of the thing is fixed by where it ends up. Rows land in a ClickHouse
// SummingMergeTree keyed by (cluster_id, timestamp, container_id), which means
// merges ADD rows that share a key. Three consequences run through this package:
//
//  1. Every value emitted is a DELTA for one minute, never a running total.
//  2. A minute bucket closes when the wall clock passes it and is never reopened,
//     so a bucket is emitted exactly once.
//  3. Nothing may retry. A re-sent item is indistinguishable from real extra
//     traffic, and no amount of care downstream can undo it.
//
// See docs/features/red-metrics/INVARIANTS.md.
//
// The aggregation is keyed by {minute, container} and deliberately NOT by peer.
// That is what keeps the in-memory footprint bounded by the number of containers
// the kubelet is running rather than by how many distinct callers reach them.
package redmetrics
