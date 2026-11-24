/*
 * Public C API of libwarpspan.so, used by span bridges (Python ctypes, Go cgo, ...).
 *
 * The capture library keeps one "current span" per thread. A span bridge sets it
 * whenever the active OpenTelemetry span on that thread changes; every kernel
 * launched from the thread is then tagged with it.
 */
#ifndef WARPSPAN_H
#define WARPSPAN_H

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

#ifdef __cplusplus
}
#endif

#endif /* WARPSPAN_H */
