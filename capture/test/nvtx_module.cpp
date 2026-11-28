// A second NVTX module for nvtx_test: a shared library compiled with its own
// copy of the header-only NVTX, like TensorRT. Its ranges must reach the same
// per-thread stack as the test program's.
#include <nvtx3/nvToolsExt.h>

extern "C" __attribute__((visibility("default"))) void module_push(const char *message) { nvtxRangePushA(message); }
extern "C" __attribute__((visibility("default"))) void module_pop() { nvtxRangePop(); }
