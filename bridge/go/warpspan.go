// Package warpspan links CUDA kernels launched from Go (through cgo) to the
// OpenTelemetry span in a context.
//
// libwarpspan.so keeps the current span per OS thread, and Go has no "current
// span" per thread: a goroutine carries its span in a context.Context and can
// move between threads. So instead of hooking span changes, wrap the calls that
// launch GPU work:
//
//	warpspan.Install("") // once, at startup
//	...
//	warpspan.Do(ctx, func() { C.run_model(...) })
//
// Do pins the goroutine to its OS thread, publishes the span from ctx, runs fn
// and restores the previous span. Only kernels launched on that OS thread are
// tagged: not those launched from goroutines fn starts, nor from threads a C
// library or a Go CUDA binding runs its launches on.
//
// Go exits without running C atexit handlers, so the library's exit flush
// never happens. Call Flush before exiting (os.Exit and log.Fatal skip deferred
// calls), or the kernels of the last second are lost.
package warpspan

/*
#cgo LDFLAGS: -ldl
#include <dlfcn.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

// dlopen and dlerror in one call: dlerror is per thread, and a goroutine can
// move between threads between two cgo calls.
static void *open_lib(const char *path, const char **err) {
	void *h = dlopen(path, RTLD_NOW | RTLD_LOCAL);
	*err = h ? NULL : dlerror();
	return h;
}

static uint32_t call_abi_version(void *fn) { return ((uint32_t (*)(void))fn)(); }

typedef void (*set_span_fn)(const uint8_t *, const uint8_t *);
typedef int (*get_span_fn)(uint8_t *, uint8_t *);

// Span ID last tagged with pyroscope.profile.id on this thread.
static __thread uint8_t last_tagged[8];

// Saves the thread's span into prev_trace/prev_span (returns 1 if there was
// one) and publishes trace_id/span_id (NULL clears). Sets *tag when span_id
// differs from the span last tagged on this thread, and remembers it.
static int swap_span(void *set, void *get, const uint8_t *trace_id, const uint8_t *span_id,
                     uint8_t *prev_trace, uint8_t *prev_span, int *tag) {
	int had_prev = ((get_span_fn)get)(prev_trace, prev_span);
	((set_span_fn)set)(trace_id, span_id);
	*tag = span_id != NULL && memcmp(span_id, last_tagged, 8) != 0;
	if (*tag) memcpy(last_tagged, span_id, 8);
	return had_prev;
}

static void call_set_span(void *fn, const uint8_t *trace_id, const uint8_t *span_id) {
	((set_span_fn)fn)(trace_id, span_id);
}
static int call_get_span(void *fn, uint8_t *trace_id, uint8_t *span_id) {
	return ((get_span_fn)fn)(trace_id, span_id);
}
static void call_flush(void *fn) { ((void (*)(void))fn)(); }
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// abiVersion must match WARPSPAN_ABI_VERSION in capture/include/warpspan.h.
const abiVersion = 1

// ProfileIDAttribute is the span attribute Grafana needs to offer "Profiles
// for this span". Do sets it to the span ID on recording spans.
const ProfileIDAttribute = "pyroscope.profile.id"

// ErrNotConfigured is returned by Install when no path is given and
// CUDA_INJECTION64_PATH is unset, i.e. warpspan isn't in use. Apps that also
// run without warpspan can ignore it.
var ErrNotConfigured = errors.New("warpspan: CUDA_INJECTION64_PATH is not set")

// library holds the resolved functions of libwarpspan.so.
type library struct {
	setSpan, getSpan unsafe.Pointer
	flush            unsafe.Pointer // optional: older libraries lack warpspan_flush
}

var (
	installMu sync.Mutex
	lib       atomic.Pointer[library] // nil until Install succeeds
)

// Install loads libwarpspan.so from path, or from $CUDA_INJECTION64_PATH if
// path is empty. It must be the file CUDA loads (CUDA_INJECTION64_PATH):
// dlopen then returns that copy, so both share one thread-local. Until Install
// succeeds, Do just runs fn. Calling it again after success does nothing.
func Install(path string) error {
	installMu.Lock()
	defer installMu.Unlock()
	if lib.Load() != nil {
		return nil
	}
	if path == "" {
		path = os.Getenv("CUDA_INJECTION64_PATH")
	}
	if path == "" {
		return ErrNotConfigured
	}
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	var cerr *C.char
	handle := C.open_lib(cpath, &cerr)
	if handle == nil {
		return fmt.Errorf("warpspan: %s", C.GoString(cerr))
	}
	sym := func(name string) unsafe.Pointer {
		cname := C.CString(name)
		defer C.free(unsafe.Pointer(cname))
		return C.dlsym(handle, cname)
	}
	abi, set, get := sym("warpspan_abi_version"), sym("warpspan_set_span"), sym("warpspan_get_span")
	if abi == nil || set == nil || get == nil {
		C.dlclose(handle)
		return fmt.Errorf("warpspan: %s is not libwarpspan.so", path)
	}
	if v := C.call_abi_version(abi); v != abiVersion {
		C.dlclose(handle)
		return fmt.Errorf("warpspan: %s has ABI version %d, bridge expects %d", path, v, abiVersion)
	}
	lib.Store(&library{setSpan: set, getSpan: get, flush: sym("warpspan_flush")})
	return nil
}

// Do runs fn with the span from ctx published to libwarpspan.so on the
// current OS thread, then restores the thread's previous span. Only sampled,
// local spans are published, as in the other bridges; otherwise kernels
// launched by fn are recorded without a span. Each call costs a few cgo calls,
// so wrap a request's or a batch's GPU work, not every kernel.
func Do(ctx context.Context, fn func()) {
	l := lib.Load()
	if l == nil {
		fn()
		return
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	span := trace.SpanFromContext(ctx)
	sc := span.SpanContext()
	var tid trace.TraceID
	var sid trace.SpanID
	tp, sp := (*C.uint8_t)(nil), (*C.uint8_t)(nil) // NULL clears the span
	if sc.IsValid() && sc.IsSampled() && !sc.IsRemote() {
		tid, sid = sc.TraceID(), sc.SpanID()
		tp, sp = (*C.uint8_t)(&tid[0]), (*C.uint8_t)(&sid[0])
	}

	var prevTrace [16]byte
	var prevSpan [8]byte
	var tag C.int
	hadPrev := C.swap_span(l.setSpan, l.getSpan, tp, sp,
		(*C.uint8_t)(&prevTrace[0]), (*C.uint8_t)(&prevSpan[0]), &tag) != 0
	defer func() {
		if hadPrev {
			C.call_set_span(l.setSpan, (*C.uint8_t)(&prevTrace[0]), (*C.uint8_t)(&prevSpan[0]))
		} else {
			C.call_set_span(l.setSpan, nil, nil)
		}
	}()
	if tag != 0 && span.IsRecording() {
		span.SetAttributes(attribute.String(ProfileIDAttribute, sid.String()))
	}
	fn()
}

// Flush hands the records of all finished kernels to the agent now, as the
// library does at exit in other languages. Synchronise the GPU first: kernels
// still running are not included. It blocks until CUDA has delivered the
// records and does nothing before CUDA is initialised. It returns false if
// Install hasn't succeeded or the library predates warpspan_flush.
func Flush() bool {
	l := lib.Load()
	if l == nil || l.flush == nil {
		return false
	}
	C.call_flush(l.flush)
	return true
}

// current returns the span libwarpspan.so holds for the calling OS thread
// (for tests; the caller must have locked its OS thread).
func current() (trace.TraceID, trace.SpanID, bool) {
	var tid trace.TraceID
	var sid trace.SpanID
	l := lib.Load()
	if l == nil {
		return tid, sid, false
	}
	ok := C.call_get_span(l.getSpan, (*C.uint8_t)(&tid[0]), (*C.uint8_t)(&sid[0])) != 0
	return tid, sid, ok
}
