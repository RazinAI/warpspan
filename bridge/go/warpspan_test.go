package warpspan

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// Tests run against the real libwarpspan.so (no GPU needed: CUDA never calls
// InitializeInjection here, only the span thread-local is exercised).
const libPath = "../../capture/libwarpspan.so"

func TestMain(m *testing.M) {
	if _, err := os.Stat(libPath); err != nil {
		fmt.Printf("skipping: %s not built (make -C capture)\n", libPath)
		os.Exit(0)
	}
	os.Unsetenv("CUDA_INJECTION64_PATH")
	if err := Install(""); !errors.Is(err, ErrNotConfigured) {
		fmt.Println("Install without a path:", err)
		os.Exit(1)
	}
	if err := Install("/nonexistent/libwarpspan.so"); err == nil {
		fmt.Println("Install accepted a missing library")
		os.Exit(1)
	}
	if err := Install(libPath); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	if err := Install(libPath); err != nil { // idempotent
		fmt.Println(err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func newTracer(sampler sdktrace.Sampler) (trace.Tracer, *tracetest.SpanRecorder) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sampler), sdktrace.WithSpanProcessor(rec))
	return tp.Tracer("test"), rec
}

// published returns the span ID the library holds for this goroutine's
// thread, or "" if none. The caller must have locked its OS thread.
func published() string {
	if _, sid, ok := current(); ok {
		return sid.String()
	}
	return ""
}

func TestDoPublishesAndRestores(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	tracer, rec := newTracer(sdktrace.AlwaysSample())

	ctx, outer := tracer.Start(context.Background(), "outer")
	innerCtx, inner := tracer.Start(ctx, "inner")
	Do(ctx, func() {
		if got := published(); got != outer.SpanContext().SpanID().String() {
			t.Errorf("in Do: published %q, want outer", got)
		}
		Do(innerCtx, func() {
			if got := published(); got != inner.SpanContext().SpanID().String() {
				t.Errorf("in nested Do: published %q, want inner", got)
			}
		})
		if got := published(); got != outer.SpanContext().SpanID().String() {
			t.Errorf("after nested Do: published %q, want outer", got)
		}
	})
	if got := published(); got != "" {
		t.Errorf("after Do: published %q, want none", got)
	}

	inner.End()
	outer.End()
	for _, s := range rec.Ended() {
		var id string
		for _, a := range s.Attributes() {
			if a.Key == ProfileIDAttribute {
				id = a.Value.AsString()
			}
		}
		if id != s.SpanContext().SpanID().String() {
			t.Errorf("span %s: %s = %q", s.Name(), ProfileIDAttribute, id)
		}
	}
}

func TestDoClearsForUnsampledRemoteAndNoSpan(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	tracer, _ := newTracer(sdktrace.AlwaysSample())
	outerCtx, outer := tracer.Start(context.Background(), "outer")
	defer outer.End()

	never, _ := newTracer(sdktrace.NeverSample())
	unsampled, span := never.Start(context.Background(), "dropped")
	defer span.End()
	remote := trace.ContextWithRemoteSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1},
		SpanID:     trace.SpanID{2},
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	}))
	want := outer.SpanContext().SpanID().String()
	Do(outerCtx, func() {
		for name, ctx := range map[string]context.Context{"unsampled": unsampled, "remote": remote, "none": context.Background()} {
			Do(ctx, func() {
				if got := published(); got != "" {
					t.Errorf("%s: published %q, want none", name, got)
				}
			})
			if got := published(); got != want {
				t.Errorf("after %s: published %q, want outer", name, got)
			}
		}
	})
}

// countingSpan counts SetAttributes calls on a real span.
type countingSpan struct {
	trace.Span
	calls *int
}

func (s countingSpan) SetAttributes(kv ...attribute.KeyValue) {
	*s.calls++
	s.Span.SetAttributes(kv...)
}

func TestDoTagsOncePerSpan(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	tracer, _ := newTracer(sdktrace.AlwaysSample())
	_, real := tracer.Start(context.Background(), "batch")
	defer real.End()
	calls := 0
	ctx := trace.ContextWithSpan(context.Background(), countingSpan{Span: real, calls: &calls})
	for range 3 {
		Do(ctx, func() {})
	}
	if calls != 1 {
		t.Errorf("SetAttributes called %d times, want 1", calls)
	}
}

func TestDoIsPerThread(t *testing.T) {
	tracer, _ := newTracer(sdktrace.AlwaysSample())
	ctx, span := tracer.Start(context.Background(), "a")
	defer span.End()
	done := make(chan string)
	Do(ctx, func() {
		go func() { // another goroutine, another thread: sees nothing
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			done <- published()
		}()
		if got := <-done; got != "" {
			t.Errorf("other thread: published %q, want none", got)
		}
	})
}

func TestFlushResolvedAndHarmlessBeforeCUDAInit(t *testing.T) {
	if !Flush() {
		t.Fatal("warpspan_flush not resolved")
	}
}
