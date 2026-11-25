## Install

### On a machine

Pick the build whose CUDA version is no newer than what your driver supports (`nvidia-smi` shows it as "CUDA Version"). The CUDA 13.0 build needs glibc 2.35 or newer; the others need 2.31.

| CUDA | Library and Python bridge |
|---|---|
| 11.8 | `warpspan-${VERSION}-cuda11.8-linux-x86_64.tar.gz` |
| 12.4 | `warpspan-${VERSION}-cuda12.4-linux-x86_64.tar.gz` |
| 12.6 | `warpspan-${VERSION}-cuda12.6-linux-x86_64.tar.gz` |
| 12.8 | `warpspan-${VERSION}-cuda12.8-linux-x86_64.tar.gz` |
| 13.0 | `warpspan-${VERSION}-cuda13.0-linux-x86_64.tar.gz` |

Each archive has `libwarpspan.so`, the `libcupti` it was built against, and the Python span bridge. The agent is `warpspan-agent-${VERSION}-linux-amd64`.

```sh
tar -xzf warpspan-${VERSION}-cuda12.6-linux-x86_64.tar.gz
chmod +x warpspan-agent-${VERSION}-linux-amd64
export WARPSPAN_SOCKET=/tmp/warpspan/warpspan.sock

./warpspan-agent-${VERSION}-linux-amd64 --otlp-endpoint http://127.0.0.1:4317 &

export CUDA_INJECTION64_PATH=$PWD/warpspan-${VERSION}-cuda12.6/libwarpspan.so
export PYTHONPATH=$PWD/warpspan-${VERSION}-cuda12.6/python   # Python span bridge
python app.py
```

Under `opentelemetry-instrument`, install the bridge instead of setting `PYTHONPATH`: `pip install warpspan-${VERSION}-py3-none-any.whl`.

To check the downloads: `sha256sum -c SHA256SUMS --ignore-missing`.

### On Kubernetes

```sh
helm install warpspan oci://ghcr.io/razinai/charts/warpspan --version ${CHART_VERSION} \
  --namespace warpspan --create-namespace \
  --set otlp.endpoint=http://otel-collector.observability.svc:4317
```

Images: `ghcr.io/razinai/warpspan-agent:${VERSION}` and `ghcr.io/razinai/warpspan-inject:${VERSION}-cuda<X.Y>` (plain `${VERSION}` is CUDA 12.6).

### Other bridges

- Go: `go get github.com/RazinAI/warpspan/bridge/go@v${VERSION}`
- C++, Java and Rust: from the source archive below (`bridge/cpp`, `bridge/java`, `bridge/rust`).

Docs: https://warpspan.docs.razin.ai

## Changes

