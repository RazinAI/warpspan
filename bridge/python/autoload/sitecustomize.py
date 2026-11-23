"""Enable the warpspan span bridge at Python startup, with no app changes.

Python imports the first ``sitecustomize`` module it finds on ``sys.path``
while starting up. Putting this directory (and ``bridge/python/src``) on
``PYTHONPATH`` therefore installs the bridge before the app's code runs.

Only one ``sitecustomize`` is ever imported, so afterwards we run the next one
on ``sys.path`` (e.g. the one ``opentelemetry-instrument`` adds) to avoid
breaking other tools that rely on the same mechanism.
"""


def _warpspan_autoload() -> None:
    import os
    import sys

    if os.environ.get("CUDA_INJECTION64_PATH") or os.environ.get("WARPSPAN_LIBRARY"):
        try:
            import warpspan

            warpspan.install()
        except Exception as e:  # never break app startup
            print(f"warpspan: python bridge disabled: {e}", file=sys.stderr)

    import importlib.util

    here = os.path.dirname(os.path.abspath(__file__))
    seen_self = False
    for entry in list(sys.path):
        entry_abs = os.path.abspath(entry or os.getcwd())
        if entry_abs == here:
            seen_self = True
            continue
        if not seen_self:
            continue
        candidate = os.path.join(entry_abs, "sitecustomize.py")
        if os.path.isfile(candidate):
            spec = importlib.util.spec_from_file_location("_warpspan_next_sitecustomize", candidate)
            if spec and spec.loader:
                module = importlib.util.module_from_spec(spec)
                sys.modules[spec.name] = module
                try:
                    spec.loader.exec_module(module)
                except Exception as e:
                    print(f"warpspan: error in {candidate}: {e}", file=sys.stderr)
            break


_warpspan_autoload()
del _warpspan_autoload
