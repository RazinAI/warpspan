"""Tests run against the real libwarpspan.so (no GPU needed: CUDA never calls
InitializeInjection here, we only exercise the span thread-local).

Build it first: make -C capture
"""

from __future__ import annotations

import os
import subprocess
import sys
import threading
from pathlib import Path

import pytest
from opentelemetry import trace
from opentelemetry.sdk.trace import TracerProvider

import warpspan

REPO = Path(__file__).resolve().parents[3]
LIB = Path(os.environ.get("WARPSPAN_LIBRARY", REPO / "capture" / "libwarpspan.so"))

pytestmark = pytest.mark.skipif(not LIB.exists(), reason=f"{LIB} not built (make -C capture)")

provider = TracerProvider()
tracer = provider.get_tracer("test")


@pytest.fixture(autouse=True)
def bridge():
    assert warpspan.install(str(LIB))
    yield
    warpspan.uninstall()


def ids(span) -> tuple[int, int]:
    sc = span.get_span_context()
    return sc.trace_id, sc.span_id


def test_tracks_nested_spans():
    assert warpspan.current_span() is None
    with tracer.start_as_current_span("outer") as outer:
        assert warpspan.current_span() == ids(outer)
        with tracer.start_as_current_span("inner") as inner:
            assert warpspan.current_span() == ids(inner)
        assert warpspan.current_span() == ids(outer)
    assert warpspan.current_span() is None


def test_span_not_made_current_is_ignored():
    with tracer.start_as_current_span("current") as current:
        tracer.start_span("background").end()  # started but never attached
        assert warpspan.current_span() == ids(current)


def test_threads_are_independent():
    seen: dict[str, object] = {}

    def worker() -> None:
        seen["before"] = warpspan.current_span()
        with tracer.start_as_current_span("worker") as span:
            seen["inside"] = warpspan.current_span() == ids(span)

    with tracer.start_as_current_span("main") as main:
        t = threading.Thread(target=worker)
        t.start()
        t.join()
        assert warpspan.current_span() == ids(main)
    assert seen == {"before": None, "inside": True}


def test_install_is_idempotent_and_uninstall_restores():
    import opentelemetry.context as ctx

    wrapped = ctx._RUNTIME_CONTEXT
    assert warpspan.install(str(LIB))
    assert ctx._RUNTIME_CONTEXT is wrapped
    warpspan.uninstall()
    assert not warpspan.is_installed()
    with tracer.start_as_current_span("not tracked"):
        assert warpspan.current_span() is None
    assert warpspan.install(str(LIB))


def test_missing_library_does_not_raise(tmp_path):
    warpspan.uninstall()
    assert warpspan.install(str(tmp_path / "missing.so")) is False


def test_autoload_via_pythonpath():
    """sitecustomize installs the bridge before the app runs and chains to the next sitecustomize."""
    bridge_dir = REPO / "bridge" / "python"
    code = (
        "import warpspan, sys; "
        "from opentelemetry.sdk.trace import TracerProvider; "
        "t = TracerProvider().get_tracer('x'); "
        "s = t.start_as_current_span('req'); span = s.__enter__(); "
        "sc = span.get_span_context(); "
        "print(warpspan.is_installed(), warpspan.current_span() == (sc.trace_id, sc.span_id), "
        "getattr(sys.modules.get('_warpspan_next_sitecustomize'), 'MARK', None))"
    )
    chained = Path(__file__).parent / "chained"
    env = dict(os.environ)
    env["PYTHONPATH"] = os.pathsep.join([str(bridge_dir / "autoload"), str(bridge_dir / "src"), str(chained)])
    env["CUDA_INJECTION64_PATH"] = str(LIB)
    out = subprocess.run([sys.executable, "-c", code], env=env, capture_output=True, text=True, check=True)
    assert out.stdout.split() == ["True", "True", "chained-ok"], out.stderr
