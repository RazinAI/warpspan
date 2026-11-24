"""Tests run against the real libwarpspan.so (no GPU needed: CUDA never calls
InitializeInjection here, we only exercise the span thread-local).

Build it first: make -C capture
"""

from __future__ import annotations

import asyncio
import contextvars
import os
import shutil
import subprocess
import sys
import threading
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

import pytest
from opentelemetry import trace
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.sampling import ALWAYS_OFF

import warpspan
from warpspan._bridge import _current_span, _uninstall

REPO = Path(__file__).resolve().parents[3]
LIB = REPO / "capture" / "libwarpspan.so"

pytestmark = pytest.mark.skipif(not LIB.exists(), reason=f"{LIB} not built (make -C capture)")

provider = TracerProvider()
tracer = provider.get_tracer("test")


@pytest.fixture(autouse=True)
def bridge():
    assert warpspan.install(str(LIB))
    yield
    _uninstall()


def ids(span) -> tuple[int, int]:
    sc = span.get_span_context()
    return sc.trace_id, sc.span_id


def test_tracks_nested_spans():
    assert _current_span() is None
    with tracer.start_as_current_span("outer") as outer:
        assert _current_span() == ids(outer)
        with tracer.start_as_current_span("inner") as inner:
            assert _current_span() == ids(inner)
        assert _current_span() == ids(outer)
    assert _current_span() is None


def test_current_span_gets_profile_id_for_grafana():
    with tracer.start_as_current_span("outer") as outer:
        background = tracer.start_span("background")  # never made current
        background.end()
    assert outer.attributes["pyroscope.profile.id"] == format(ids(outer)[1], "016x")
    assert "pyroscope.profile.id" not in background.attributes


def test_profile_id_is_set_once_per_span(monkeypatch):
    span = tracer.start_span("reattached")
    calls = []
    original = span.set_attribute
    monkeypatch.setattr(span, "set_attribute", lambda k, v: (calls.append(k), original(k, v)))
    for _ in range(3):
        with trace.use_span(span):
            pass
    span.end()
    assert calls == ["pyroscope.profile.id"]


def test_unsampled_and_remote_spans_are_not_published():
    unsampled = TracerProvider(sampler=ALWAYS_OFF).get_tracer("test")
    with unsampled.start_as_current_span("dropped"):
        assert _current_span() is None

    remote = trace.SpanContext(
        trace_id=1, span_id=2, is_remote=True, trace_flags=trace.TraceFlags(trace.TraceFlags.SAMPLED)
    )
    with trace.use_span(trace.NonRecordingSpan(remote)):
        assert _current_span() is None


def test_span_not_made_current_is_ignored():
    with tracer.start_as_current_span("current") as current:
        tracer.start_span("background").end()  # started but never attached
        assert _current_span() == ids(current)


def test_threads_are_independent():
    seen: dict[str, object] = {}

    def worker() -> None:
        seen["before"] = _current_span()
        with tracer.start_as_current_span("worker") as span:
            seen["inside"] = _current_span() == ids(span)

    with tracer.start_as_current_span("main") as main:
        t = threading.Thread(target=worker)
        t.start()
        t.join()
        assert _current_span() == ids(main)
    assert seen == {"before": None, "inside": True}


def test_install_is_idempotent_and_uninstall_restores():
    import opentelemetry.context as ctx

    wrapped = ctx._RUNTIME_CONTEXT
    assert warpspan.install(str(LIB))
    assert ctx._RUNTIME_CONTEXT is wrapped
    _uninstall()
    with tracer.start_as_current_span("not tracked"):
        assert _current_span() is None
    assert warpspan.install(str(LIB))


def test_missing_library_does_not_raise(tmp_path):
    _uninstall()
    assert warpspan.install(str(tmp_path / "missing.so")) is False


def test_autoload_via_pythonpath():
    """sitecustomize installs the bridge before the app runs and chains to the next sitecustomize."""
    bridge_dir = REPO / "bridge" / "python"
    code = (
        "import sys; "
        "from warpspan._bridge import _current_span, _wrapper; "
        "from opentelemetry.sdk.trace import TracerProvider; "
        "t = TracerProvider().get_tracer('x'); "
        "s = t.start_as_current_span('req'); span = s.__enter__(); "
        "sc = span.get_span_context(); "
        "print(_wrapper is not None, _current_span() == (sc.trace_id, sc.span_id), "
        "getattr(sys.modules.get('_warpspan_next_sitecustomize'), 'MARK', None))"
    )
    chained = Path(__file__).parent / "chained"
    env = dict(os.environ)
    env["PYTHONPATH"] = os.pathsep.join([str(bridge_dir / "autoload"), str(bridge_dir / "src"), str(chained)])
    env["CUDA_INJECTION64_PATH"] = str(LIB)
    out = subprocess.run([sys.executable, "-c", code], env=env, capture_output=True, text=True, check=True)
    assert out.stdout.split() == ["True", "True", "chained-ok"], out.stderr


def test_opentelemetry_instrument_entry_point():
    """With the package installed, opentelemetry-instrument loads the bridge via its entry point."""
    exe = shutil.which("opentelemetry-instrument", path=os.path.dirname(sys.executable))
    if exe is None:
        pytest.skip("opentelemetry-instrumentation not installed")
    try:
        from importlib.metadata import entry_points

        if not list(entry_points(group="opentelemetry_pre_instrument", name="warpspan")):
            pytest.skip("warpspan package not installed (pip install -e bridge/python)")
    except Exception:
        pytest.skip("entry points unavailable")
    code = "import warpspan._bridge as b; print(b._wrapper is not None)"
    env = dict(os.environ, CUDA_INJECTION64_PATH=str(LIB), OTEL_TRACES_EXPORTER="none",
               OTEL_METRICS_EXPORTER="none", OTEL_LOGS_EXPORTER="none")
    env.pop("PYTHONPATH", None)
    out = subprocess.run([exe, sys.executable, "-c", code], env=env, capture_output=True, text=True, check=True)
    assert out.stdout.strip() == "True", out.stderr


needs_context_watchers = pytest.mark.skipif(sys.version_info < (3, 14), reason="context watchers need Python 3.14")


@needs_context_watchers
def test_asyncio_tasks_each_get_their_own_span():
    results = {}

    async def worker(name):
        with tracer.start_as_current_span(name) as span:
            await asyncio.sleep(0)  # the other task attaches its span meanwhile
            await asyncio.sleep(0)
            results[name] = (_current_span(), ids(span))

    async def main():
        await asyncio.gather(worker("a"), worker("b"))

    asyncio.run(main())
    for name, (published, expected) in results.items():
        assert published == expected, name
    assert _current_span() is None


@needs_context_watchers
def test_copied_context_in_thread_pool():
    with tracer.start_as_current_span("request") as span:
        ctx = contextvars.copy_context()
    with ThreadPoolExecutor(max_workers=1) as pool:
        assert pool.submit(ctx.run, _current_span).result() == ids(span)
        assert pool.submit(_current_span).result() is None  # cleared when the context exits


@needs_context_watchers
def test_exceptions_pass_through_context_switches():
    def boom():
        raise ValueError("app error")

    with pytest.raises(ValueError, match="app error"):
        contextvars.copy_context().run(boom)  # no span in this context
    with pytest.raises(ValueError, match="app error"):
        asyncio.run(asyncio.to_thread(boom))
    with pytest.raises(StopIteration):
        contextvars.copy_context().run(next, iter(()))


@needs_context_watchers
def test_span_attached_inside_copied_context():
    def work():
        with tracer.start_as_current_span("inside") as span:
            inner = _current_span()
        return inner, ids(span), _current_span()

    inner, expected, after = contextvars.copy_context().run(work)
    assert inner == expected
    assert after is None
    assert _current_span() is None


@needs_context_watchers
def test_sub_interpreter_install_keeps_main_working(tmp_path):
    # In a subprocess: a crash there must fail the test, not pytest.
    script = tmp_path / "subinterp.py"
    script.write_text(
        f"""
import contextvars
from concurrent import interpreters
from opentelemetry.sdk.trace import TracerProvider
import warpspan
from warpspan._bridge import _current_span
assert warpspan.install({str(LIB)!r})
tracer = TracerProvider().get_tracer("t")

sub = interpreters.create()
sub.exec("import warpspan; warpspan.install({str(LIB)!r})")
with tracer.start_as_current_span("main") as span:
    ctx = contextvars.copy_context()
want = (span.get_span_context().trace_id, span.get_span_context().span_id)
assert ctx.run(_current_span) == want, "main interpreter lost its watcher"
sub.close()
assert ctx.run(_current_span) == want
print("ok")
"""
    )
    out = subprocess.run([sys.executable, str(script)], capture_output=True, text=True, timeout=60)
    assert out.returncode == 0 and out.stdout.strip() == "ok", out.stderr
