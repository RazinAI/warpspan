# Warpspan

GPU kernel profiling linked to OpenTelemetry traces. Warpspan records the CUDA
kernels a process runs, tags each one with the trace/span that was active on the
launching thread, and exports them as OTLP profiles (for example to Pyroscope).

It works like auto-instrumentation: no app code changes, only environment
variables set at process start.

```
 app process (e.g. PyTorch)                                   same node
┌───────────────────────────────────────────────┐
│ OTel SDK ─attach/detach─▶ span bridge (Python)│
│                              │ warpspan_set_span()
│ CUDA ──CUPTI──▶ libwarpspan.so ◀┘              │ unix socket  ┌────────────┐ OTLP/gRPC ┌───────────┐
│   launch:   correlation_id → current span      ├─────────────▶│ agent (Go) ├──────────▶│ Collector │─▶ Pyroscope
│   activity: kernel name, GPU start/end         │              └────────────┘           └───────────┘
└───────────────────────────────────────────────┘
```

| Path | What |
|---|---|
| `capture/` | `libwarpspan.so`, loaded by CUDA via `CUDA_INJECTION64_PATH` (CUPTI Callback + Activity API) |
| `bridge/python/` | publishes the active OTel span to the library (Python) |
| `bridge/cpp/` | the same for OpenTelemetry C++ (header-only) |
| `agent/`, `cmd/warpspan/` | node agent: aggregates per process/span/kernel, exports OTLP profiles |
| `tools/profiles-dump/` | tiny OTLP profiles receiver that prints what it gets |
| `deploy/` | docker-compose backend (Collector, Pyroscope, Tempo, Grafana), Kubernetes manifests |
| `docs/` | [ADRs](docs/decisions/), [wire protocol](docs/protocol.md) |

**Status:** prototype. Linux x86_64, NVIDIA GPUs. Tested on one machine (RTX
3070, CUDA 13.4, driver 615). The build has guards for CUDA 11.6+, but older
versions have not been tried.

Verified on that machine, with the docker-compose stack (Collector 0.165,
Pyroscope 2.4.4, Tempo 2.10, Grafana 13.3):

- PyTorch example (`examples/torch-spans`): all 130 kernels of 10 requests
  were linked to the right child span (preprocess / forward / postprocess).
- In Grafana, opening the trace in Tempo, clicking a span and then "Profiles
  for this span" shows a flame graph of exactly the kernels that span ran.
- The bridge also loads under `opentelemetry-instrument`.
- The inject image's CUDA 12.6 build (with its bundled libcupti) ran the same
  PyTorch example on the GPU, copied out to the host. It hasn't run inside a
  GPU container (no NVIDIA container toolkit on the test machine).

## Build

Requirements: CUDA toolkit (`cupti.h`, `libcupti.so`), `g++`, `make`, Go ≥ 1.26
(an older `go` downloads 1.26 automatically).

```sh
make                                           # capture/libwarpspan.so, bin/warpspan, bin/profiles-dump
make -C capture CUDA_HOME=/usr/local/cuda-12.4  # a specific toolkit
```

Build `libwarpspan.so` against a CUDA toolkit **no newer than what the driver
supports** (`nvidia-smi`, top right, "CUDA Version"). Otherwise CUPTI fails to
start: warpspan logs `CUPTI setup failed` and the app runs unprofiled.

## Try it on a GPU machine

```sh
make

# 1. A receiver. Either print what arrives:
./bin/profiles-dump -listen 127.0.0.1:4317 &
#    or run the full backend (Docker; Grafana on localhost:3000, GRAFANA_PORT=3001 to change):
#    docker compose -f deploy/docker-compose.yaml up -d

# 2. The agent
./bin/warpspan --socket /tmp/warpspan/warpspan.sock --otlp-endpoint http://127.0.0.1:4317 &

# 3. Any CUDA program, with warpspan injected
export CUDA_INJECTION64_PATH=$PWD/capture/libwarpspan.so
export WARPSPAN_SOCKET=/tmp/warpspan/warpspan.sock
export OTEL_SERVICE_NAME=torch-spans-example
export PYTHONPATH=$PWD/bridge/python/autoload:$PWD/bridge/python/src   # Python span bridge
python examples/torch-spans/app.py --requests 50                       # needs torch + opentelemetry-sdk
```

If the app runs under `opentelemetry-instrument`, install the bridge instead of
setting `PYTHONPATH` (`pip install ./bridge/python`). `opentelemetry-instrument`
puts its own `sitecustomize` first, and that one doesn't chain to ours, so the
bridge loads through its `opentelemetry_pre_instrument` entry point instead.

For a C++ app using OpenTelemetry C++, include
`bridge/cpp/include/warpspan/otel_bridge.h` and call
`warpspan::otel::Install()` at startup, before other threads use OpenTelemetry.
It loads the library from `CUDA_INJECTION64_PATH` and does nothing (returns
false) when that's unset. Link with `-ldl` on glibc older than 2.34.

What to look for:

- On stderr the app prints `warpspan: loaded …`, `warpspan: connected to agent …`,
  and at exit `warpspan: exiting: N kernels, M linked to spans, …`. If M is 0
  while the app creates spans, the bridge isn't loaded.
- The agent logs one `interval` line per export with `kernels=` and `linked_to_spans=`.
- `profiles-dump` prints samples such as `[812345]  cuda:0 > at::native::… [trace=… span=…]`.
  The trace/span IDs match the ones the example app prints.
- In Pyroscope or Grafana: profile type `gpu` → `gpu_time`, service
  `torch-spans-example`. A freshly started Pyroscope can take about a minute
  before data becomes queryable.
- In Grafana's trace view, spans have a "Profiles for this span" button. Grafana
  shows it only for spans with the `pyroscope.profile.id` attribute, which the
  bridge sets (to the span ID) on every span that becomes current.

Without any agent, `WARPSPAN_DEBUG_FILE=/tmp/kernels.log` (`-` for stderr)
writes one line per kernel, including trace/span IDs.

## Kubernetes

[`deploy/kubernetes/`](deploy/kubernetes/) has the manual setup (the webhook and
Helm chart that automate it don't exist yet):

1. Build and push the images:
   - `docker build -f agent/Dockerfile -t <registry>/warpspan-agent:0.1.0 .`
   - `docker build -f capture/Dockerfile --build-arg CUDA_IMAGE=nvidia/cuda:<your CUDA>-devel-ubuntu20.04 -t <registry>/warpspan-inject:0.1.0 .`
2. Edit the image and `--otlp-endpoint` in `agent-daemonset.yaml`, then `kubectl apply` it.
3. Add the init container, env vars and volumes from `example-gpu-pod.yaml` to the GPU workload.
   `hostPath` volumes are rejected by the `baseline`/`restricted` Pod Security levels.

## Configuration

Capture library (environment of the app process, read once at CUDA initialisation):

| Variable | Default | |
|---|---|---|
| `CUDA_INJECTION64_PATH` | (none) | path to `libwarpspan.so`; enables warpspan |
| `WARPSPAN_SOCKET` | `/var/run/warpspan/warpspan.sock` | agent socket; empty = don't send |
| `WARPSPAN_DEBUG_FILE` | (none) | write kernel records as text (`-` = stderr) |
| `WARPSPAN_QUIET` | | `1` = no log lines on stderr |
| `WARPSPAN_DISABLE` | | `1` = load but do nothing |
| `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES` | | used for `service.name` and resource attributes |

Agent: `./bin/warpspan -h`. The flags are `--socket`, `--otlp-endpoint`
(`http://host:port` is plaintext, `https://host:port` or `host:port` uses TLS,
`none` disables export), `--otlp-header k=v`, `--interval`, `--node-name`.
It also reads `OTEL_EXPORTER_OTLP_ENDPOINT` and `OTEL_EXPORTER_OTLP_HEADERS`.

OTLP profiles are alpha: the Collector needs `--feature-gates=service.profilesSupport`.
Tested with Collector 0.165.0 and Pyroscope 2.4.4.

## Limitations

- Only kernels: no memcpy/memset, no CPU or Python stacks.
- Span linking works per thread and needs a span bridge (Python and C++ so far).
  asyncio, and kernels launched from other threads (e.g. autograd backward), end
  up unlinked or attributed to the last span on that thread. See ADR 0002.
- Records are dropped rather than slowing the app: on CUPTI buffer overflow and
  when the agent is busy. If more than 131k launches happen between flushes, the
  oldest are still counted but lose their span.
- CUPTI allows one subscriber per process. Nsight Systems or `torch.profiler`
  may fail to attach while warpspan is active; use `WARPSPAN_DISABLE=1` for those runs.
- A process that `fork()`s after CUDA initialisation isn't profiled in the child.
- The agent socket is world-writable; see the trust model in `docs/protocol.md`.

## Tests

```sh
make test   # Go tests + Python bridge tests (pip install opentelemetry-sdk pytest)
            # + C++ bridge tests if OTEL_CPP points at opentelemetry-cpp sources
```
