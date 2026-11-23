"""Example workload: OpenTelemetry spans around PyTorch GPU work.

Every "request" is a span with child spans for preprocessing and the model
forward pass. Run it with warpspan enabled and each kernel gets linked to the
span that launched it. Nothing in this file knows about warpspan.

    python examples/torch-spans/app.py --requests 20

Traces are exported over OTLP if OTEL_EXPORTER_OTLP_ENDPOINT is set and
opentelemetry-exporter-otlp-proto-grpc is installed; otherwise span IDs are
only printed.
"""

from __future__ import annotations

import argparse
import os
import time

import torch
from opentelemetry import trace
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor


def setup_tracing() -> TracerProvider:
    # Resource (service.name) comes from OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES,
    # the same variables warpspan reads, so traces and profiles share a service.
    provider = TracerProvider()
    if os.environ.get("OTEL_EXPORTER_OTLP_ENDPOINT") or os.environ.get("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"):
        try:
            from opentelemetry.exporter.otlp.proto.grpc.trace_exporter import OTLPSpanExporter

            provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporter()))
        except ImportError:
            print("opentelemetry-exporter-otlp-proto-grpc not installed; traces are not exported")
    trace.set_tracer_provider(provider)
    return provider


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--requests", type=int, default=20)
    parser.add_argument("--batch", type=int, default=256)
    parser.add_argument("--sleep", type=float, default=0.05, help="seconds between requests")
    args = parser.parse_args()

    if not torch.cuda.is_available():
        raise SystemExit("CUDA is not available to PyTorch")

    provider = setup_tracing()
    tracer = trace.get_tracer("warpspan.example")
    device = torch.device("cuda")
    model = torch.nn.Sequential(
        torch.nn.Linear(1024, 4096),
        torch.nn.GELU(),
        torch.nn.Linear(4096, 4096),
        torch.nn.GELU(),
        torch.nn.Linear(4096, 1024),
    ).to(device)
    print(f"device: {torch.cuda.get_device_name(0)}")

    with torch.inference_mode():
        for i in range(args.requests):
            with tracer.start_as_current_span("request") as span:
                sc = span.get_span_context()
                with tracer.start_as_current_span("preprocess"):
                    x = torch.randn(args.batch, 1024, device=device)
                    x = (x - x.mean(dim=1, keepdim=True)) / x.std(dim=1, keepdim=True)
                with tracer.start_as_current_span("forward"):
                    y = model(x)
                with tracer.start_as_current_span("postprocess"):
                    top = torch.topk(y.softmax(dim=1), k=5, dim=1)
                    _ = top.indices.cpu()  # waits for the GPU
                print(f"request {i}: trace_id={sc.trace_id:032x} span_id={sc.span_id:016x}")
            time.sleep(args.sleep)

    provider.shutdown()


if __name__ == "__main__":
    main()
