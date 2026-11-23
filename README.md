# Warpspan

Continuous GPU profiling linked to OpenTelemetry traces. Warpspan records every
CUDA kernel your app runs, tags it with the trace/span (request) that launched
it, and exports it as OTLP profiles. In Grafana you can then go from a slow span
in Tempo to the GPU kernels it ran, as a flame graph in Pyroscope.

It works like auto-instrumentation: **no app code changes**, just environment
variables at process start.

```
 app process (any language, e.g. PyTorch)                   node
┌──────────────────────────────────────────────┐
│ OTel SDK ─span start/end─▶ span bridge (Py)  │
│                               │ warpspan_set_span()
│ CUDA ──CUPTI──▶ libwarpspan.so ◀┘            │  unix socket   ┌─────────────┐  OTLP/gRPC  ┌───────────┐
│   launch: correlation_id → span (thread-local)├──────────────▶│ warpspan    ├────────────▶│ Collector │─▶ Pyroscope ─▶ Grafana
│   activity: kernel name, GPU start/end        │               │ agent (Go)  │             └───────────┘
└──────────────────────────────────────────────┘               └─────────────┘
```

- **`capture/`**: `libwarpspan.so`, loaded by CUDA through `CUDA_INJECTION64_PATH`.
  It uses CUPTI's Callback API at launch and the Activity API for kernel timings,
  and joins them on correlation ID.
- **`bridge/python/`**: publishes the active OTel span to the library. It loads
  itself through `PYTHONPATH`.
- **`agent/`, `cmd/warpspan/`**: node agent. It aggregates per process, span
  and kernel, and exports OTLP profiles over gRPC.
- **`deploy/`**: docker-compose backend (Collector, Pyroscope, Tempo, Grafana)
  and Kubernetes manifests.
- Design: [`docs/decisions/`](docs/decisions/), wire format:
  [`docs/protocol.md`](docs/protocol.md).

Status: early prototype (v0.1.0). Linux x86_64, NVIDIA GPUs. It is written to
build against CUDA 11.6 and newer, but it has only been tested with CUDA 13.4 on
an RTX 3070.

## Build

Requirements: CUDA toolkit (for `cupti.h` / `libcupti.so`), `g++`, `make`,
Go ≥ 1.26 (older Go downloads it automatically).

```sh
make                      # capture/libwarpspan.so, bin/warpspan, bin/profiles-dump
make -C capture CUDA_HOME=/usr/local/cuda-12.4   # pick a specific toolkit
```

Build `libwarpspan.so` **with the same CUDA major version the GPU driver
supports** (`nvidia-smi`, top right "CUDA Version"). CUPTI from a newer toolkit
than the driver fails to initialise. Warpspan then logs `CUPTI setup failed` and
the app keeps running unprofiled.

## Try it on one GPU machine (no Kubernetes)

```sh
# 1. Build
make

# 2. Start a receiver. Either the debug printer:
./bin/profiles-dump -listen 127.0.0.1:4317 &
#    or the full backend (needs Docker); Grafana is then on http://localhost:3000:
#    docker compose -f deploy/docker-compose.yaml up -d

# 3. Start the agent
./bin/warpspan --socket /tmp/warpspan/warpspan.sock --otlp-endpoint 127.0.0.1:4317 &

# 4. Run any CUDA app with warpspan injected
export CUDA_INJECTION64_PATH=$PWD/capture/libwarpspan.so
export WARPSPAN_SOCKET=/tmp/warpspan/warpspan.sock
export PYTHONPATH=$PWD/bridge/python/autoload:$PWD/bridge/python/src   # Python span bridge
export OTEL_SERVICE_NAME=torch-spans-example
python examples/torch-spans/app.py --requests 50    # needs torch + opentelemetry-sdk
```

What you should see:

- The app prints `warpspan: loaded …`, `warpspan: connected to agent …`, and on
  exit `warpspan: exiting: N kernels captured, M linked to spans, 0 dropped`.
- The agent logs an `interval` line every 10 s with `kernels=` and
  `linked_to_spans=`, plus the top kernels.
- `profiles-dump` prints samples such as
  `[812345]  cuda:0 > at::native::… [trace=… span=…]`. The `trace=`/`span=`
  values match the IDs the example app prints.
- In Grafana: Explore → Pyroscope → profile type `gpu` / `gpu_time`, service
  `torch-spans-example`. The Tempo data source has "traces to profiles" set up.
  For that, also export the app's traces
  (`pip install opentelemetry-exporter-otlp-proto-grpc` and
  `OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4317`).

Debugging without the agent: `WARPSPAN_DEBUG_FILE=/tmp/kernels.log` (or `-` for
stderr) writes one line per kernel, including trace/span IDs.

## Kubernetes

See [`deploy/kubernetes/`](deploy/kubernetes/):

1. Build and push the two images:
   - `docker build -f agent/Dockerfile -t <registry>/warpspan-agent:0.1.0 .`
   - `docker build -f capture/Dockerfile --build-arg CUDA_IMAGE=nvidia/cuda:<your CUDA>-devel-ubuntu20.04 -t <registry>/warpspan-inject:0.1.0 .`
2. `kubectl apply -f deploy/kubernetes/agent-daemonset.yaml` (edit the image
   and `--otlp-endpoint` first).
3. Add the init container, env vars and volumes from
   `deploy/kubernetes/example-gpu-pod.yaml` to your GPU workload.

A mutating webhook and Helm chart that do step 3 automatically are planned.

## Configuration

Capture library (env vars of the app process):

| Variable | Default | |
|---|---|---|
| `CUDA_INJECTION64_PATH` | (none) | path to `libwarpspan.so`; enables warpspan |
| `WARPSPAN_SOCKET` | `/var/run/warpspan/warpspan.sock` | agent socket; empty = don't send |
| `WARPSPAN_FLUSH_INTERVAL_MS` | `1000` | how often CUPTI buffers are flushed |
| `WARPSPAN_DEBUG_FILE` | (none) | write kernel records as text (`-` = stderr) |
| `WARPSPAN_QUIET` | | `1` = no log lines on stderr |
| `WARPSPAN_DISABLE` | | `1` = load but do nothing |
| `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES` | | read for `service.name` and resource attributes |

Agent: `./bin/warpspan -h`. The main flags are `--socket`, `--otlp-endpoint`
(`host:port`, `http://`, `https://`, or `none`), `--otlp-header k=v`,
`--interval`, `--node-name`. It also reads `OTEL_EXPORTER_OTLP_ENDPOINT` and
`OTEL_EXPORTER_OTLP_HEADERS`.

The Collector needs `--feature-gates=service.profilesSupport`, because OTLP
profiles are still alpha. Tested versions: Collector 0.165.0, Pyroscope 2.4.4.

## Limitations (v0.1)

- Only kernels. Memcpy/memset aren't captured yet, and there are no CPU or
  Python stacks.
- Span linking needs a span bridge (Python only so far) and works per thread.
  asyncio and kernels launched from other threads (e.g. autograd backward) end
  up unlinked or misattributed. See ADR 0002.
- CUPTI allows one subscriber per process. While warpspan is active, Nsight
  Systems or `torch.profiler` may fail to attach (and the other way round). Set
  `WARPSPAN_DISABLE=1` for runs where you use those tools.
- Processes that `fork()` after CUDA initialisation stop being profiled in the
  child.

## Tests

```sh
make test     # Go unit tests + Python bridge tests (pip install opentelemetry-sdk pytest)
```
