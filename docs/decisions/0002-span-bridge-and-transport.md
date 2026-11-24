# 0002: Span bridge mechanism, local transport and profile shape

- **Status:** accepted
- **Date:** 2026-11-24

Resolves the "span bridge mechanism" open question of ADR 0001 and fixes the
first versions of the local transport and the exported profile format.

## Decisions

### 1. Span bridge: thread-local set through an exported C function

`libwarpspan.so` exports `warpspan_set_span(trace_id[16], span_id[8])`, which
stores the span in a `thread_local`. The CUPTI launch callback runs on the app
thread, so it reads the same thread-local with no locks.

The Python bridge wraps `opentelemetry.context._RUNTIME_CONTEXT` and calls
`warpspan_set_span` after every `attach` / `detach`, so it tracks what
`start_as_current_span` makes current (not merely started spans). It loads
without app changes in one of two ways. With `opentelemetry-instrument`, the
installed package registers an `opentelemetry_pre_instrument` entry point.
Without it, `sitecustomize` goes on `PYTHONPATH`; it chains to the next
`sitecustomize`, but `opentelemetry-instrument`'s own does not chain, which is
why the entry point exists. The bridge loads the library with `ctypes.PyDLL`
from `CUDA_INJECTION64_PATH`; `dlopen` returns the copy CUDA loaded, so both
share one thread-local.

The C++ bridge (`bridge/cpp`, header-only) does the same through the
OpenTelemetry C++ API: `Install()` sets a `RuntimeContextStorage` that wraps the
default thread-local storage and publishes after each `Attach` / `Detach`. The
API only allows replacing the storage at startup, before other threads use
it, so the app has to call `Install()` there; C++ has no equivalent of
`sitecustomize`. All bridges set `pyroscope.profile.id` once per span and
thread, since `SetAttribute` takes a lock in the SDKs.

Go keeps the span in `context.Context`, not per thread, and goroutines move
between OS threads, so there is nothing to hook. The Go bridge (`bridge/go`)
instead wraps the cgo calls that launch GPU work: `Do(ctx, fn)` locks the OS
thread, publishes the span from `ctx`, runs `fn` and restores the previous span.
Only launches on that OS thread are linked: not those from goroutines `fn`
starts, nor from threads a C library or Go CUDA binding launches on. Go also
exits without running C `atexit` handlers, so the library exports
`warpspan_flush()` (optional symbol, no ABI bump) and the bridge exposes it as
`Flush()`.

At runtime each bridge only uses its language's OTel API, never the SDK (the
SDK is a test dependency only). The app
keeps whatever SDK it configures.

**Alternatives:** CUPTI external correlation (push/pop per span, but it needs
CUPTI loaded before the bridge runs, and still needs a span→trace map); NVTX
ranges (string-based and slower, and NVTX can be disabled by the app). Either
can be added later behind the same bridge API.

**Known gaps:**
- *asyncio:* tasks switch contextvars without attach/detach, so the last
  attached span on the event-loop thread wins.
- *Kernels launched from other threads:* the span doesn't propagate. One example
  is PyTorch autograd's backward pass, which runs on engine threads. Those kernels
  are captured but unlinked.
- *CUDA graphs:* kernels in a graph launch share the graph launch's correlation
  ID and are linked to the span active at `cudaGraphLaunch`.

### 2. Join table

`correlation_id → span` lives in a direct-mapped table of 2^17 slots, with a
per-slot seqlock. Writing at launch is a few relaxed stores, with no allocation
or locks. The table is allocated with `calloc`, so its 4 MiB is only touched as
it's used. If more than 131k launches happen between two activity
flushes (1 s by default), older entries are overwritten and those kernels
become unlinked.

### 3. Transport: unix SOCK_SEQPACKET, non-blocking, drop on pressure

There's a single socket on the node, shared via `hostPath` in Kubernetes. See
`docs/protocol.md`, including its trust model. Sends happen on our flush thread
or CUPTI's worker thread. The one exception is the final flush in the `atexit`
handler, which runs on the exiting app thread. On `EAGAIN` the batch is dropped
and counted. With no agent connected, records aren't encoded at all. The library
has no gRPC or protobuf (see ADR 0001). The process metadata message is built
once at startup, so `getenv()` is never called later from our threads, where it
would race with the app's `setenv()`.

### 4. Profile shape (OTLP profiles, Pyroscope)

Per process, in one `ResourceProfiles`, there are two profiles:
`gpu_time` (nanoseconds) and `kernel_launches` (count). Both use period type
`gpu/nanoseconds`, so Pyroscope names them `gpu`. The type IDs are
`gpu:gpu_time:nanoseconds:gpu:nanoseconds` and
`gpu:kernel_launches:count:gpu:nanoseconds`. The stack is
`cuda:<device> > <kernel>`. The kernel name is demangled and shortened:
inside template arguments namespaces are dropped, lambdas print as `{lambda}`,
and if the name is still over 160 characters, arguments are elided as `<…>`
from the deepest level up. Demangled PyTorch kernels reach thousands of
characters, but the op is usually a type in the arguments (`GeluCUDAKernelImpl`,
`DivFunctor`), so dropping all template arguments would merge every elementwise
op into one frame. The launching span is the sample's
`Link`, which Pyroscope turns into `span_id`/`trace_id` labels for
traces-to-profiles. Grafana offers "Profiles for this span" only on spans with
the attribute `pyroscope.profile.id`, so the Python bridge sets it to the span
ID (hex) when a span becomes current. Spans that launched no kernels still get
it and show an empty profile.

`service.name` follows the OTel SDK rules from the process's own
`OTEL_SERVICE_NAME`/`OTEL_RESOURCE_ATTRIBUTES`, so profiles and traces land in
the same service. The agent keeps at most 20,000 distinct (kernel, device,
span) samples per process per interval. Beyond that, kernels are counted
without their span, and past 22,000 keys under `[other kernels]`. Together with
the 2 KiB name limit, the 1024-connection limit and the bounded name shortening,
this caps what one process (or a hostile client) can make the agent hold. Each
process is exported in its own request.

The Python bridge publishes only sampled, local spans. Kernels of unsampled
requests are counted without a span, because their span IDs never reach the
trace backend.

## Consequences

- No CPU (Python/native) stacks yet. Frames are only device + kernel. Adding
  CPU stacks at launch is future work and has to stay cheap.
- Pinned, tested combination: OTLP profiles proto v0.4.1 (wire-identical to
  v0.3.0), Collector v0.165 with `--feature-gates=service.profilesSupport`,
  Pyroscope v2.4.4, Grafana 13.3. Verified locally with PyTorch: Grafana's
  "Profiles for this span" on a Tempo span shows that span's kernels.
- Pyroscope 2.4.4 keeps `service_name` as a series label but did not keep the
  other resource attributes (`host.name`, `process.pid`) in that test.
