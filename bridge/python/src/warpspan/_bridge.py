"""Hook OpenTelemetry's context attach/detach and forward the active span.

How it works: every ``with tracer.start_as_current_span(...)`` (and any other
``opentelemetry.context.attach``/``detach``) goes through the SDK's runtime
context object ``opentelemetry.context._RUNTIME_CONTEXT``. We wrap that object;
after each attach/detach we read the span that is now current on this thread
and call ``warpspan_set_span(trace_id, span_id)`` in libwarpspan.so, which keeps
it in a thread-local that the CUPTI launch callback reads.

Known limitation: asyncio tasks switch context without attach/detach, so with
many concurrent requests on one event loop thread a kernel can be attributed
to the span that was attached last on that thread.
"""

from __future__ import annotations

import ctypes
import logging
import os
import threading
from typing import Any

_log = logging.getLogger("warpspan")

_ABI_VERSION = 1

_lock = threading.Lock()
_lib: ctypes.PyDLL | None = None
_wrapper: _WarpspanRuntimeContext | None = None


def _library_path(path: str | None) -> str | None:
    return path or os.environ.get("WARPSPAN_LIBRARY") or os.environ.get("CUDA_INJECTION64_PATH")


def _load(path: str) -> ctypes.PyDLL:
    # PyDLL keeps the GIL during the call: our functions are tiny, and not
    # releasing/re-acquiring the GIL makes each call much cheaper.
    # dlopen() returns the same copy CUDA loads via CUDA_INJECTION64_PATH, so the
    # bridge and the CUPTI callback share one thread-local.
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


try:  # subclass the SDK's interface when available (older APIs may differ)
    from opentelemetry.context.context import _RuntimeContext as _Base
except Exception:  # pragma: no cover
    _Base = object  # type: ignore[assignment,misc]


class _WarpspanRuntimeContext(_Base):  # type: ignore[misc,valid-type]
    def __init__(self, inner: Any, lib: ctypes.PyDLL) -> None:
        from opentelemetry import trace

        self._inner = inner
        self._set_span = lib.warpspan_set_span
        self._get_current_span = trace.get_current_span

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
            sc = self._get_current_span(context).get_span_context()
            if sc.is_valid:
                self._set_span(sc.trace_id.to_bytes(16, "big"), sc.span_id.to_bytes(8, "big"))
            else:
                self._set_span(None, None)
        except Exception:  # never break the app's tracing
            _log.debug("warpspan: failed to publish span", exc_info=True)


def install(library_path: str | None = None) -> bool:
    """Start forwarding the active span to libwarpspan.so.

    The library is found via ``library_path``, ``$WARPSPAN_LIBRARY`` or
    ``$CUDA_INJECTION64_PATH``. Returns True if the bridge is active. Never
    raises: if anything is missing the app runs as before.
    """
    global _lib, _wrapper
    with _lock:
        if _wrapper is not None:
            return True
        path = _library_path(library_path)
        if not path:
            _log.debug("warpspan: no library path (CUDA_INJECTION64_PATH unset); bridge disabled")
            return False
        try:
            import opentelemetry.context as otel_context
        except ImportError:
            _log.debug("warpspan: opentelemetry-api not installed; bridge disabled")
            return False
        try:
            lib = _load(path)
        except (OSError, AttributeError, RuntimeError) as e:
            _log.warning("warpspan: cannot load %s: %s; bridge disabled", path, e)
            return False

        wrapper = _WarpspanRuntimeContext(otel_context._RUNTIME_CONTEXT, lib)
        otel_context._RUNTIME_CONTEXT = wrapper
        _lib, _wrapper = lib, wrapper
        wrapper._publish(wrapper.get_current())  # a span may already be active
        return True


def uninstall() -> None:
    """Restore the original runtime context and clear this thread's span."""
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


def is_installed() -> bool:
    return _wrapper is not None


def current_span() -> tuple[int, int] | None:
    """(trace_id, span_id) libwarpspan.so sees for this thread, or None. For tests/debugging."""
    lib = _lib
    if lib is None:
        return None
    trace_buf = ctypes.create_string_buffer(16)
    span_buf = ctypes.create_string_buffer(8)
    if not lib.warpspan_get_span(ctypes.addressof(trace_buf), ctypes.addressof(span_buf)):
        return None
    return int.from_bytes(trace_buf.raw, "big"), int.from_bytes(span_buf.raw, "big")
