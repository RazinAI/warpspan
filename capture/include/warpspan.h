/*
 * Public C API of libwarpspan.so, used by span bridges (Python ctypes, Go cgo, ...).
 *
 * The capture library keeps one "current span" per thread. A span bridge sets it
 * whenever the active OpenTelemetry span on that thread changes; every kernel
 * launched from the thread is then tagged with it.
 */
#ifndef WARPSPAN_H
#define WARPSPAN_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

/* Bumped when the functions below change incompatibly. */
#define WARPSPAN_ABI_VERSION 1u

uint32_t warpspan_abi_version(void);

/*
 * Set the span active on the calling thread.
 * trace_id: 16 bytes, span_id: 8 bytes, both in W3C / OTLP byte order.
 * Passing NULL for either (or all-zero IDs) clears the current span.
 */
void warpspan_set_span(const uint8_t *trace_id, const uint8_t *span_id);

/*
 * Copy the calling thread's current span into trace_id[16] / span_id[8].
 * Returns 1 if a span is set, 0 otherwise. Intended for tests and debugging.
 */
int warpspan_get_span(uint8_t *trace_id, uint8_t *span_id);

/*
 * Hand all records of finished kernels to the agent now, as the library does at
 * exit through atexit(), which runtimes such as Go skip. Synchronise the GPU
 * first: kernels still running are not included. Records are dropped, as
 * always, if the agent isn't connected or is busy. Blocks until CUPTI has
 * delivered its buffers; does nothing before CUDA is initialised. Added after
 * ABI version 1 was published: look it up as an optional symbol.
 */
void warpspan_flush(void);

/*
 * Python 3.14+ context watcher, used by the Python span bridge (see ADR 0002).
 * contextvar holds the current span's IDs (24 bytes: trace_id, span_id) or
 * b"". On every context switch the watcher publishes the value in the new
 * context. The Python C API functions are passed in, so the library needs no
 * Python headers or symbols; PyObject * is void * here.
 */
typedef struct {
  void *contextvar; /* the library keeps a reference */
  int (*add_watcher)(int (*callback)(int event, void *context));
  int (*contextvar_get)(void *var, void *default_value, void **value);
  char *(*bytes_as_string)(void *bytes);
  intptr_t (*bytes_size)(void *bytes);
  void (*inc_ref)(void *object);
  void (*dec_ref)(void *object);
  void *(*get_raised_exception)(void);
  void (*set_raised_exception)(void *exception);
} warpspan_python_api;

/*
 * Register the watcher; call with the GIL held. Returns the watcher id, or -1
 * if PyContext_AddWatcher fails or a different contextvar is already
 * registered: the library has one slot, so only the first interpreter that
 * registers gets the watcher (sub-interpreters keep the pre-3.14 behaviour).
 * Registering the same contextvar again (after PyContext_ClearWatcher) is
 * allowed. Added after ABI version 1 was published: look it up as an optional
 * symbol.
 */
int warpspan_python_watch(const warpspan_python_api *api);

/*
 * The calling thread's open NVTX ranges as "outer > inner", written to buf
 * (NUL-terminated, truncated to size). Returns the number of ranges, or -1
 * when NVTX isn't routed to this library (NVTX_INJECTION64_PATH unset). Meant
 * for tests and debugging; look it up as an optional symbol.
 */
int warpspan_nvtx_labels(char *buf, size_t size);

#ifdef __cplusplus
}
#endif

#endif /* WARPSPAN_H */
