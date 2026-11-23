"""Warpspan span bridge for Python.

Publishes the active OpenTelemetry span of each thread to libwarpspan.so, so
GPU kernels launched while a span is active are linked to that span.

Usually enabled without code changes by putting ``bridge/python/autoload`` and
``bridge/python/src`` on ``PYTHONPATH`` (see the README). It can also be
enabled explicitly::

    import warpspan
    warpspan.install()
"""

from ._bridge import current_span, install, is_installed, uninstall

__all__ = ["install", "uninstall", "is_installed", "current_span"]
__version__ = "0.1.0"
