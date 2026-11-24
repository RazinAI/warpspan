"""Hook OpenTelemetry's context attach/detach and forward the active span.

Every ``with tracer.start_as_current_span(...)`` (and any other
``opentelemetry.context.attach``/``detach``) goes through the API's runtime
context object ``opentelemetry.context._RUNTIME_CONTEXT``. We wrap it: after
each attach/detach we read the span now current on this thread and call
``warpspan_set_span(trace_id, span_id)`` in libwarpspan.so, which keeps it in a
thread-local that the CUPTI launch callback reads.

Only sampled, local spans are published. Unsampled spans never reach the trace
backend, and a remote parent belongs to another service, so linking kernels to
them would only add span IDs nobody can look up.

When a span becomes current it also gets the attribute ``pyroscope.profile.id``
(its span ID in hex). Grafana shows "Profiles for this span" only on spans that
have it, and queries Pyroscope for that span ID.

asyncio task steps and thread pools running a copied context switch contexts
without attach/detach. On Python 3.14+ (main interpreter only) the bridge also
keeps the span's IDs in a ContextVar and registers a context watcher in
libwarpspan.so that republishes them on every switch; see ADR 0002.
"""

from __future__ import annotations

import contextvars
import ctypes
import logging
import os
import sys
import threading
from typing import Any

from opentelemetry import trace
from opentelemetry.context.context import _RuntimeContext

_log = logging.getLogger("warpspan")

_ABI_VERSION = 1
_PROFILE_ID_ATTR = "pyroscope.profile.id"

_lock = threading.Lock()
# trace_id + span_id (24 bytes) of the published span, or b"", for the context
# watcher. Only kept up to date while the watcher is registered.
_ids: contextvars.ContextVar[bytes] = contextvars.ContextVar("warpspan_span", default=b"")
_watcher_id: int | None = None
# Span ID last tagged on each thread: set_attribute takes a lock in the SDK, and
# the same span is re-attached whenever only other context values change.
_tagged = threading.local()
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
        self._publish(context, tag=True)
        return token

    def get_current(self) -> Any:
        return self._inner.get_current()

    def detach(self, token: Any) -> None:
        self._inner.detach(token)
        self._publish(self._inner.get_current())

    def _publish(self, context: Any, tag: bool = False) -> None:
        try:
            span = trace.get_current_span(context)
            sc = span.get_span_context()
            if sc.is_valid and sc.trace_flags.sampled and not sc.is_remote:
                trace_id, span_id = sc.trace_id.to_bytes(16, "big"), sc.span_id.to_bytes(8, "big")
                self._set_span(trace_id, span_id)
                if _watcher_id is not None:
                    _ids.set(trace_id + span_id)
                if tag and getattr(_tagged, "span_id", 0) != sc.span_id and span.is_recording():
                    span.set_attribute(_PROFILE_ID_ATTR, format(sc.span_id, "016x"))
                    _tagged.span_id = sc.span_id
            else:
                self._set_span(None, None)
                if _watcher_id is not None:
                    _ids.set(b"")
        except Exception:  # never break the app's tracing
            _log.debug("warpspan: failed to publish span", exc_info=True)


class _PythonApi(ctypes.Structure):
    """warpspan_python_api in capture/include/warpspan.h."""

    _fields_ = [
        ("contextvar", ctypes.py_object),
        ("add_watcher", ctypes.c_void_p),
        ("contextvar_get", ctypes.c_void_p),
        ("bytes_as_string", ctypes.c_void_p),
        ("bytes_size", ctypes.c_void_p),
        ("inc_ref", ctypes.c_void_p),
        ("dec_ref", ctypes.c_void_p),
        ("get_raised_exception", ctypes.c_void_p),
        ("set_raised_exception", ctypes.c_void_p),
    ]


_API_FUNCTIONS = (
    "PyContext_AddWatcher",
    "PyContextVar_Get",
    "PyBytes_AsString",
    "PyBytes_Size",
    "Py_IncRef",
    "Py_DecRef",
    "PyErr_GetRaisedException",
    "PyErr_SetRaisedException",
)


def _watch_contexts(lib: ctypes.PyDLL) -> int | None:
    """Registers the context watcher; returns its id, or None if unavailable."""
    if sys.version_info < (3, 14):
        return None
    try:
        watch = lib.warpspan_python_watch  # missing in older libraries
        funcs = [ctypes.cast(getattr(ctypes.pythonapi, f), ctypes.c_void_p).value for f in _API_FUNCTIONS]
    except AttributeError:
        return None
    watch.argtypes = [ctypes.POINTER(_PythonApi)]
    watch.restype = ctypes.c_int
    watcher = watch(ctypes.byref(_PythonApi(_ids, *funcs)))
    return watcher if watcher >= 0 else None  # -1: e.g. another interpreter has it


def install(library_path: str | None = None) -> bool:
    """Start forwarding the active span to libwarpspan.so.

    The library is ``library_path`` or ``$CUDA_INJECTION64_PATH``. Returns True
    if the bridge is active. Never raises: if anything is missing the app runs
    as before.
    """
    global _lib, _wrapper, _watcher_id
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
        _watcher_id = _watch_contexts(lib)
        wrapper._publish(wrapper.get_current())  # a span may already be active
        return True


# Test helpers.


def _uninstall() -> None:
    """Restore the original runtime context (clears only the calling thread's span)."""
    global _lib, _wrapper, _watcher_id
    with _lock:
        if _wrapper is None:
            return
        import opentelemetry.context as otel_context

        if otel_context._RUNTIME_CONTEXT is _wrapper:
            otel_context._RUNTIME_CONTEXT = _wrapper._inner
        if _watcher_id is not None:
            ctypes.pythonapi.PyContext_ClearWatcher(_watcher_id)
            _watcher_id = None
        _ids.set(b"")
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
