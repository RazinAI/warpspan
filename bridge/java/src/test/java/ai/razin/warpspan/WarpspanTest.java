package ai.razin.warpspan;

import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.SpanContext;
import io.opentelemetry.api.trace.TraceFlags;
import io.opentelemetry.api.trace.TraceState;
import io.opentelemetry.api.trace.Tracer;
import io.opentelemetry.context.Context;
import io.opentelemetry.context.ContextStorage;
import io.opentelemetry.context.Scope;
import io.opentelemetry.sdk.trace.ReadWriteSpan;
import io.opentelemetry.sdk.trace.ReadableSpan;
import io.opentelemetry.sdk.trace.SdkTracerProvider;
import io.opentelemetry.sdk.trace.SpanProcessor;
import io.opentelemetry.sdk.trace.samplers.Sampler;
import java.lang.foreign.MemorySegment;
import java.lang.reflect.InvocationHandler;
import java.lang.reflect.InvocationTargetException;
import java.lang.reflect.Proxy;
import java.util.HexFormat;
import java.util.List;
import java.util.concurrent.CopyOnWriteArrayList;

/**
 * Runs against the real libwarpspan.so (no GPU needed: CUDA never calls InitializeInjection
 * here, only the span thread-local is exercised). Usage: WarpspanTest path/to/libwarpspan.so
 * [--late], where --late checks that installing after OpenTelemetry is in use fails.
 */
public final class WarpspanTest {
  private static int failures = 0;
  private static final List<ReadableSpan> ended = new CopyOnWriteArrayList<>();

  private static void check(boolean ok, String what) {
    if (!ok) {
      System.err.println("FAIL: " + what);
      failures++;
    }
  }

  /** "traceid/spanid" libwarpspan.so holds for this thread, or "" if none. */
  private static String published() throws Throwable {
    byte[] traceId = new byte[16];
    byte[] spanId = new byte[8];
    int ok =
        (int) Warpspan.getSpan.invokeExact(MemorySegment.ofArray(traceId), MemorySegment.ofArray(spanId));
    return ok != 0 ? HexFormat.of().formatHex(traceId) + "/" + HexFormat.of().formatHex(spanId) : "";
  }

  private static String ids(Span span) {
    return span.getSpanContext().getTraceId() + "/" + span.getSpanContext().getSpanId();
  }

  /**
   * Wraps span, counting calls to the abstract setAttribute. Default methods run on the proxy, so
   * that e.g. storeInContext stores the proxy and the String overload ends up counted once.
   */
  private static Span counting(Span span, int[] calls) {
    return (Span)
        Proxy.newProxyInstance(
            Span.class.getClassLoader(),
            new Class<?>[] {Span.class},
            (proxy, method, args) -> {
              if (method.isDefault()) {
                return InvocationHandler.invokeDefault(proxy, method, args);
              }
              if (method.getName().equals("setAttribute")) {
                calls[0]++;
              }
              try {
                Object result = method.invoke(span, args);
                return result == span ? proxy : result;
              } catch (InvocationTargetException e) {
                throw e.getCause();
              }
            });
  }

  private static Tracer tracer(Sampler sampler) {
    SpanProcessor recorder =
        new SpanProcessor() {
          @Override
          public void onStart(Context parent, ReadWriteSpan span) {}

          @Override
          public boolean isStartRequired() {
            return false;
          }

          @Override
          public void onEnd(ReadableSpan span) {
            ended.add(span);
          }

          @Override
          public boolean isEndRequired() {
            return true;
          }
        };
    return SdkTracerProvider.builder().setSampler(sampler).addSpanProcessor(recorder).build().get("test");
  }

  public static void main(String[] args) throws Throwable {
    String lib = args[0];
    if (args.length > 1 && args[1].equals("--late")) {
      Context.current(); // OpenTelemetry already in use
      check(!Warpspan.install(lib), "install after first context use must fail");
      finish();
      return;
    }

    check(!Warpspan.install("/nonexistent/libwarpspan.so"), "missing library rejected");
    check(Warpspan.install(lib), "install");
    check(Warpspan.install(lib), "install is idempotent");

    Tracer tracer = tracer(Sampler.alwaysOn());
    Span outer = tracer.spanBuilder("outer").startSpan();
    try (Scope s1 = outer.makeCurrent()) {
      check(published().equals(ids(outer)), "outer published");
      Span inner = tracer.spanBuilder("inner").startSpan();
      try (Scope s2 = inner.makeCurrent()) {
        check(published().equals(ids(inner)), "inner published");
      }
      inner.end();
      check(published().equals(ids(outer)), "outer restored");

      Span unsampled = tracer(Sampler.alwaysOff()).spanBuilder("dropped").startSpan();
      try (Scope s3 = unsampled.makeCurrent()) {
        check(published().isEmpty(), "unsampled span not published");
      }
      SpanContext remote =
          SpanContext.createFromRemoteParent(
              "0af7651916cd43dd8448eb211c80319c", "b7ad6b7169203331", TraceFlags.getSampled(),
              TraceState.getDefault());
      try (Scope s4 = Span.wrap(remote).makeCurrent()) {
        check(published().isEmpty(), "remote span not published");
      }
      check(published().equals(ids(outer)), "outer restored again");

      try (Scope s5 = ContextStorage.get().attach(null)) {
        check(published().equals(ids(outer)), "attach(null) keeps the current span");
      }

      int[] calls = {0};
      Span batch = tracer.spanBuilder("batch").startSpan();
      Span countingBatch = counting(batch, calls);
      for (int i = 0; i < 3; i++) {
        try (Scope s6 = Context.current().with(countingBatch).makeCurrent()) {
          check(published().equals(ids(batch)), "batch published");
        }
      }
      batch.end();
      check(calls[0] == 1, "tagged once per span, got " + calls[0]);

      String[] other = new String[1];
      Thread t =
          new Thread(
              () -> {
                try {
                  other[0] = published();
                } catch (Throwable e) {
                  other[0] = "error";
                }
              });
      t.start();
      t.join();
      check(other[0].isEmpty(), "other thread sees no span");
    }
    outer.end();
    check(published().isEmpty(), "cleared at root");

    check(ended.size() == 3, "3 spans recorded, got " + ended.size());
    for (ReadableSpan s : ended) {
      String id = s.getAttribute(io.opentelemetry.api.common.AttributeKey.stringKey(Warpspan.PROFILE_ID_ATTRIBUTE));
      check(s.getSpanContext().getSpanId().equals(id), s.getName() + " has " + Warpspan.PROFILE_ID_ATTRIBUTE);
    }
    finish();
  }

  private static void finish() {
    if (failures > 0) {
      System.err.println(failures + " check(s) failed");
      System.exit(1);
    }
    System.out.println("ok");
  }
}
