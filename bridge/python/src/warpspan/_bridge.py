"""Hook OpenTelemetry's context attach/detach and forward the active span.

Every ``with tracer.start_as_current_span(...)`` (and any other
``opentelemetry.context.attach``/``detach``) goes through the API's runtime
context object ``opentelemetry.context._RUNTIME_CONTEXT``. We wrap it: after
each attach/detach we read the span now current on this thread and call
``warpspan_set_span(trace_id, span_id)`` in libwarpspan.so, which keeps it in a
thread-local that the CUPTI launch callback reads.

Known limitation: asyncio tasks switch context without attach/detach, so with
many concurrent requests on one event-loop thread a kernel can be attributed to
the span attached last on that thread.
"""

from __future__ import annotations

import ctypes
import logging
import os
import threading
from typing import Any

from opentelemetry import trace
from opentelemetry.context.context import _RuntimeContext

_log = logging.getLogger("warpspan")

_ABI_VERSION = 1

_lock = threading.Lock()
_lib: ctypes.PyDLL | None = None
_wrapper: _WarpspanRuntimeContext | None = None


def _load(path: str) -> ctypes.PyDLL:
    # PyDLL keeps the GIL during the call: the functions are tiny, and not
    # releasing/re-acquiring the GIL makes each call much cheaper. dlopen()
    # returns the copy CUDA loads via CUDA_INJECTION64_PATH, so the bridge and
    # the CUPTI callback share one thread-local.
    lib = ctypes.PyDLL(path)
    lib.warpspan_abi_version.argtypes = []
    lib.warpspan_abi_version.restype = ctypes.c_uint32
    lib.warpspan_set_span.argtypes = [ctypes.c_char_p, ctypes.c_char_p]
    lib.warpspan_set_span.restype = None
    lib.warpspan_get_span.argtypes = [ctypes.c_void_p, ctypes.c_void_p]
    lib.warpspan_get_span.restype = ctypes.c_int
    abi = lib.warpspan_abi_version()
    if abi != _ABI_VERSION:
        raise RuntimeError(f"{path}: ABI version {abi}, bridge expects {_ABI_VERSION}")
    return lib


class _WarpspanRuntimeContext(_RuntimeContext):
    def __init__(self, inner: _RuntimeContext, lib: ctypes.PyDLL) -> None:
        self._inner = inner
        self._set_span = lib.warpspan_set_span

    def attach(self, context: Any) -> Any:
        token = self._inner.attach(context)
        self._publish(context)
        return token

    def get_current(self) -> Any:
        return self._inner.get_current()

    def detach(self, token: Any) -> None:
        self._inner.detach(token)
        self._publish(self._inner.get_current())

    def _publish(self, context: Any) -> None:
        try:
            sc = trace.get_current_span(context).get_span_context()
            if sc.is_valid:
                self._set_span(sc.trace_id.to_bytes(16, "big"), sc.span_id.to_bytes(8, "big"))
            else:
                self._set_span(None, None)
        except Exception:  # never break the app's tracing
            _log.debug("warpspan: failed to publish span", exc_info=True)


def install(library_path: str | None = None) -> bool:
    """Start forwarding the active span to libwarpspan.so.

    The library is ``library_path`` or ``$CUDA_INJECTION64_PATH``. Returns True
    if the bridge is active. Never raises: if anything is missing the app runs
    as before.
    """
    global _lib, _wrapper
    with _lock:
        if _wrapper is not None:
            return True
        path = library_path or os.environ.get("CUDA_INJECTION64_PATH")
        if not path:
            return False
        try:
            lib = _load(path)
        except (OSError, AttributeError, RuntimeError) as e:
            _log.warning("warpspan: cannot load %s: %s; span bridge disabled", path, e)
            return False

        import opentelemetry.context as otel_context

        wrapper = _WarpspanRuntimeContext(otel_context._RUNTIME_CONTEXT, lib)
        otel_context._RUNTIME_CONTEXT = wrapper
        _lib, _wrapper = lib, wrapper
        wrapper._publish(wrapper.get_current())  # a span may already be active
        return True


# Test helpers.


def _uninstall() -> None:
    """Restore the original runtime context (clears only the calling thread's span)."""
    global _lib, _wrapper
    with _lock:
        if _wrapper is None:
            return
        import opentelemetry.context as otel_context

        if otel_context._RUNTIME_CONTEXT is _wrapper:
            otel_context._RUNTIME_CONTEXT = _wrapper._inner
        if _lib is not None:
            _lib.warpspan_set_span(None, None)
        _lib, _wrapper = None, None


def _current_span() -> tuple[int, int] | None:
    """(trace_id, span_id) libwarpspan.so holds for this thread, or None."""
    lib = _lib
    if lib is None:
        return None
    trace_buf = ctypes.create_string_buffer(16)
    span_buf = ctypes.create_string_buffer(8)
    if not lib.warpspan_get_span(ctypes.addressof(trace_buf), ctypes.addressof(span_buf)):
        return None
    return int.from_bytes(trace_buf.raw, "big"), int.from_bytes(span_buf.raw, "big")
