// Warpspan capture library.
//
// Loaded into any CUDA process when CUDA_INJECTION64_PATH points at this .so.
// During CUDA initialisation the driver dlopen()s the library, looks up
// InitializeInjection by name (dlsym) and calls it.

#include <cstdio>
#include <unistd.h>

// Built with -fvisibility=hidden, so only symbols marked with this are exported.
// extern "C" keeps the name unmangled so dlsym("InitializeInjection") finds it.
#define WARPSPAN_EXPORT extern "C" __attribute__((visibility("default")))

namespace {
bool g_initialized = false;
} // namespace

// Called by the CUDA driver. Return 1 on success, 0 on failure.
WARPSPAN_EXPORT int InitializeInjection(void) {
  if (g_initialized) {
    return 1;
  }
  g_initialized = true;

  // stderr only: stdout belongs to the app.
  std::fprintf(stderr, "warpspan: loaded (pid %d)\n", static_cast<int>(getpid()));
  return 1;
}
