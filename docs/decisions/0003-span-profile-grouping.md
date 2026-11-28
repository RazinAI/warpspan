# 0003: Group similar spans instead of keeping every span's kernels

- **Status:** accepted
- **Date:** 2026-11-28

## Context

ADR 0002 exports one sample per (process, kernel, device, span) per interval,
and NVTX labels added the label path to that key. With spans, almost nothing
aggregates: each span runs each of its kernels about once. On a TensorRT video
pipeline (8 streams, NVTX labels on every layer) one interval held 50,682
kernels and 48,608 span-linked samples, against 2,532 for the whole service
without spans. Pyroscope kept about 70 bytes per sample before compaction:
about 30 GB a day for one such process running live. The cost grows with
traffic, like a metric with a request ID label.

The same run showed that spans of one kind repeat the same structure (all 245
full batches ran the identical 192 kernel/layer rows) while their GPU time
varies (10 to 23 ms).

## Decision

The agent groups spans before export (`agent/group`), with
`--span-profiles=grouped` as the default:

- A span's **shape** is the set of its (NVTX path, kernel, device) rows. Spans
  with the same shape form a group; within a group of at least 10 spans, those
  above the 90th percentile of GPU time are `slow`, the rest `typical`. The
  group frame `[spans <hash> · typical|slow]` names it, stable across intervals.
- Two profile views, so no kernel is counted twice in either:
  - `gpu_time` / `kernel_launches`: the service view, no span links, kernels
    from spans under their group frame;
  - `span_gpu_time`: the per-span view. Every span gets its own real GPU time
    under its group frame and the NVTX ranges all its kernels share; the
    slowest spans (`--exemplars`, default 1 per interval) keep all their rows.
- Grafana's trace-to-profiles uses `span_gpu_time`.
- Spans whose last kernel ended within 2 s of the newest kernel of the process
  wait for the next interval, at most once: a normal span is grouped whole, a
  span that keeps running (training loop, long stream) is exported in parts.
- At most 64 groups per process per interval keep their own frame; smaller
  ones share `[spans · other shapes]`. Slow also requires 1.2x the median.
- `full` keeps the ADR 0002 behaviour (every kernel of every span); `off`
  drops the per-span view.

## Consequences

- Per-span rows go from about one per kernel launch to about one per span
  (439 instead of 48,608 in the run above). Every span still has its own
  recorded GPU time, and the slowest have per-kernel detail.
- A typical span's per-kernel breakdown is the group's, in `gpu_time`, not its
  own. The span's profile says so in its leaf frame.
- Exact-shape grouping saves little when spans never repeat their kernels
  (dynamic shapes, autotuning); a looser grouping (span name, top NVTX levels)
  is future work, as is passing span names from the bridges.
- `span_gpu_time` is a new profile type: existing Grafana setups must change
  the trace-to-profiles profile type.
