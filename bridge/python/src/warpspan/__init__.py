"""Warpspan span bridge for Python.

Publishes the active OpenTelemetry span of each thread to libwarpspan.so, so
GPU kernels launched while a span is active are linked to that span.

It enables itself without code changes, either through ``opentelemetry-instrument``
(when this package is installed) or through ``bridge/python/autoload`` on
``PYTHONPATH``. Explicitly::

    import warpspan
    warpspan.install()
"""

from ._bridge import install

__all__ = ["install"]
__version__ = "0.1.0"
