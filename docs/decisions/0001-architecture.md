# 0001: Architecture: CUPTI injection library + span bridge + Go agent

- **Status:** accepted
- **Date:** 2026-11-23

## Context

Warpspan links GPU kernels to the OpenTelemetry trace/span (request) that launched them.

- Most GPU workloads are Python (PyTorch, vLLM), but C++, Rust and Go apps exist too. Warpspan should work like instrumentation, not be a library tied to one language.
- Every CUDA program, in any language, goes through NVIDIA's driver library (`libcuda.so`). CUDA can load a shared library into any unmodified CUDA process when `CUDA_INJECTION64_PATH` is set, and calls its `InitializeInjection()` function at CUDA initialisation. NVIDIA Nsight and Polar Signals' parcagpu use this mechanism.
- CUPTI (CUDA Profiling Tools Interface) provides:
  - the **Callback API**: runs our code synchronously on the app's thread at each CUDA call (e.g. a kernel launch), with a correlation ID;
  - the **Activity API**: asynchronous records of what actually ran on the GPU (kernel name, real start/end time, device, stream), with the same correlation ID.
- GPU execution is asynchronous, so the span must be captured **at launch time** on the CPU thread. Only the launch-side record has the thread ID.
- The current OTel span lives in the app's language runtime (Python `contextvars`, Go `context.Context`). A native library cannot read it directly.
- Warpspan is meant to be always-on, so anything on the launch path must be very cheap.

## Decision

Three components:

1. **Capture library** (`capture/`, C++): an injection `.so` loaded via `CUDA_INJECTION64_PATH`.
   - Callback API on kernel launch: record correlation ID, thread, and the currently published span.
   - Activity API: collect kernel GPU timings and join them to launches on correlation ID.
   - Written in a restricted C++ style: `extern "C"` entry points, no exceptions, no allocation or IO in callbacks, `-fvisibility=hidden`, statically linked libstdc++.
   - Does the minimum in-process; sends records to the agent from a background thread.
2. **Span bridge** (per language, Python first): a small OTel component (e.g. a SpanProcessor) that publishes the active span to the capture library on span start/end, so the library knows the span at launch time. Enabled like auto-instrumentation, with no app code changes.
3. **Agent** (Go): receives records from capture libraries on the node, builds OTLP profiles and exports them over **OTLP/gRPC** to an OTel Collector → Pyroscope → Grafana. May later become an OTel Collector receiver.

Transport from the capture library to the agent is local and simple (to start: a file or Unix domain socket; later possibly shared memory). **No gRPC inside the app process.**

## Alternatives considered

- **Language-specific libraries only (Python/Go):** covers fewer apps, and duplicates the hard CUPTI work per language.
- **Function interposition (`LD_PRELOAD`, as HAMi does):** sees CPU-side calls but not GPU execution time; fragile across CUDA versions.
- **eBPF only:** no app changes at all, but cannot see kernel execution time without CUPTI, and reading the OTel span from runtime memory is very hard (especially for Python).
- **Joining spans and kernels by time + thread in the Collector:** almost zero app changes, but approximate; breaks with async code (many requests on one thread), needs clock alignment, buffering and sampling-aware routing. Kept as a possible optional fallback.
- **Building on parcagpu** instead of writing our own capture library: worth revisiting; parcagpu links kernels to CPU stacks, not to OTel traces.

## Consequences

- Warpspan must be enabled at process start (env var), as with most auto-instrumentation. Injecting into an already running process does not deliver activity records.
- Child processes inherit the env var (e.g. `torchrun` / DataLoader workers), so the agent must handle many processes.
- A span bridge is needed per language; without one, kernels are captured but not linked to traces.
- Three languages in the repo: C++ (capture), Python (bridge), Go (agent).

## Open questions

- **Span bridge mechanism:** CUPTI external correlation (`cuptiActivityPushExternalCorrelationId_v2`; a 64-bit ID, which fits a span ID; needs a span→trace ID map) vs NVTX ranges vs a thread-local exported by the capture library. Needs a prototype.
- **Async runtimes:** Python asyncio runs many requests on one thread; thread-based mechanisms must push/pop correctly around context switches.
- **Batching attribution:** one kernel serving many requests (vLLM). Needs its own ADR.
- **Clock domains:** confirm CUPTI timestamps are comparable to OTel span timestamps (Unix ns).
