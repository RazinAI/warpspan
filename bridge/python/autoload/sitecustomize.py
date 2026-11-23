"""Enable the warpspan span bridge at Python startup, with no app changes.

Python imports the first ``sitecustomize`` module on ``sys.path`` at startup,
so putting this directory (and ``bridge/python/src``) first on ``PYTHONPATH``
installs the bridge before the app's code runs. Only one ``sitecustomize`` is
ever imported, so afterwards we run the next one on ``sys.path`` too.

Not needed with ``opentelemetry-instrument``: it prepends its own
``sitecustomize`` (which does not chain), but it calls the
``opentelemetry_pre_instrument`` entry point of the installed ``warpspan``
package instead.
"""


def _warpspan_autoload() -> None:
    import importlib.util
    import os
    import sys

    if os.environ.get("CUDA_INJECTION64_PATH"):
        try:
            import warpspan

            warpspan.install()
        except Exception as e:  # never break app startup
            print(f"warpspan: python span bridge disabled: {e}", file=sys.stderr)

    here = os.path.dirname(os.path.abspath(__file__))
    entries = [os.path.abspath(p or os.getcwd()) for p in sys.path]
    if here not in entries:
        return
    for entry in entries[entries.index(here) + 1 :]:
        candidate = os.path.join(entry, "sitecustomize.py")
        if os.path.isfile(candidate):
            spec = importlib.util.spec_from_file_location("_warpspan_next_sitecustomize", candidate)
            if spec and spec.loader:
                module = importlib.util.module_from_spec(spec)
                sys.modules[spec.name] = module
                try:
                    spec.loader.exec_module(module)
                except Exception as e:
                    print(f"warpspan: error in {candidate}: {e}", file=sys.stderr)
            return


_warpspan_autoload()
del _warpspan_autoload
