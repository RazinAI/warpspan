package io.github.zraisan.warpspan;

import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.SpanContext;
import io.opentelemetry.context.Context;
import io.opentelemetry.context.ContextStorage;
import io.opentelemetry.context.Scope;
import java.lang.foreign.Arena;
import java.lang.foreign.FunctionDescriptor;
import java.lang.foreign.Linker;
import java.lang.foreign.MemorySegment;
import java.lang.foreign.SymbolLookup;
import java.lang.foreign.ValueLayout;
import java.lang.invoke.MethodHandle;
import java.lang.invoke.MethodType;
import java.lang.invoke.MutableCallSite;
import java.nio.file.Path;
import java.util.Objects;
import java.util.Optional;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * Warpspan span bridge for OpenTelemetry Java.
 *
 * <p>Call {@link #install()} once at startup, after any other {@code ContextStorage.addWrapper}
 * call and before anything uses OpenTelemetry: the API only accepts wrappers until the storage is
 * first used, and install() initialises it. The wrapper publishes the current span to
 * libwarpspan.so after every attach and scope close, so CUDA kernels launched on that thread are
 * tagged with it. Only sampled, local spans are published, as in the other bridges.
 *
 * <p>Needs Java 22+ (foreign function API); on older JVMs loading this class fails. Run with
 * {@code --enable-native-access=ALL-UNNAMED} (or the module name): the JDK warns about native
 * access without it and will block it in future releases. Spans are tracked per OS thread, so
 * virtual threads are not supported (they move between carrier threads), and async code that
 * switches spans without attach/close is mis-attributed.
 */
public final class Warpspan {
  /** Must match WARPSPAN_ABI_VERSION in capture/include/warpspan.h. */
  static final int ABI_VERSION = 1;

  /** Span attribute Grafana needs to offer "Profiles for this span". */
  public static final String PROFILE_ID_ATTRIBUTE = "pyroscope.profile.id";

  private static final Logger log = Logger.getLogger(Warpspan.class.getName());

  // warpspan_set_span, bound at install(). A static final invoker of a call site lets the JIT
  // treat the target as a constant, which an ordinary field doesn't.
  private static final MutableCallSite setSpanSite =
      new MutableCallSite(MethodType.methodType(void.class, MemorySegment.class, MemorySegment.class));
  private static final MethodHandle setSpan = setSpanSite.dynamicInvoker();

  private static boolean installed; // guarded by Warpspan.class
  static MethodHandle getSpan; // warpspan_get_span, for tests

  private Warpspan() {}

  /** Installs the bridge with the library from {@code $CUDA_INJECTION64_PATH}. */
  public static boolean install() {
    return install(System.getenv("CUDA_INJECTION64_PATH"));
  }

  /**
   * Installs the bridge. {@code libraryPath} must be the file CUDA loads (CUDA_INJECTION64_PATH):
   * loading it gives that same copy, so both share one thread-local. Returns false, and logs why,
   * if the library can't be used or OpenTelemetry's context storage is already in use; returns
   * false silently if no path is given.
   */
  @SuppressWarnings("restricted") // calling into libwarpspan.so is the point
  public static synchronized boolean install(String libraryPath) {
    if (installed) {
      return true;
    }
    if (libraryPath == null || libraryPath.isEmpty()) {
      return false;
    }
    MethodHandle set;
    try {
      Linker linker = Linker.nativeLinker();
      SymbolLookup lib = SymbolLookup.libraryLookup(Path.of(libraryPath), Arena.global());
      Optional<MemorySegment> abiSym = lib.find("warpspan_abi_version");
      Optional<MemorySegment> setSym = lib.find("warpspan_set_span");
      if (abiSym.isEmpty() || setSym.isEmpty()) {
        log.warning("warpspan: " + libraryPath + " is not libwarpspan.so; span bridge disabled");
        return false;
      }
      int version =
          (int) linker.downcallHandle(abiSym.get(), FunctionDescriptor.of(ValueLayout.JAVA_INT)).invokeExact();
      if (version != ABI_VERSION) {
        log.warning(
            "warpspan: " + libraryPath + ": ABI version " + version + ", bridge expects "
                + ABI_VERSION + "; span bridge disabled");
        return false;
      }
      // critical(true): the IDs are passed straight from Java byte arrays.
      Linker.Option critical = Linker.Option.critical(true);
      set =
          linker.downcallHandle(
              setSym.get(), FunctionDescriptor.ofVoid(ValueLayout.ADDRESS, ValueLayout.ADDRESS), critical);
      getSpan =
          lib.find("warpspan_get_span")
              .map(
                  s ->
                      linker.downcallHandle(
                          s,
                          FunctionDescriptor.of(ValueLayout.JAVA_INT, ValueLayout.ADDRESS, ValueLayout.ADDRESS),
                          critical))
              .orElse(null);
    } catch (Throwable e) { // e.g. the file is missing, or native access is denied
      log.warning("warpspan: cannot load " + libraryPath + ": " + e + "; span bridge disabled");
      return false;
    }
    setSpanSite.setTarget(set);

    boolean[] wrapped = {false};
    ContextStorage.addWrapper(
        inner -> {
          wrapped[0] = true;
          return new PublishingStorage(inner);
        });
    ContextStorage.get(); // initialise now, so we know whether the wrapper took effect
    if (!wrapped[0]) {
      log.warning(
          "warpspan: OpenTelemetry context storage was already in use; call"
              + " Warpspan.install() earlier. Span bridge disabled");
      return false;
    }
    installed = true;
    return true;
  }

  /** Forwards to the wrapped storage and publishes the current span after each change. */
  private static final class PublishingStorage implements ContextStorage {
    // Span ID last tagged on each thread: setAttribute takes a lock in the SDK, and the same span
    // is re-attached whenever only other context values change.
    private static final ThreadLocal<String> lastTagged = new ThreadLocal<>();

    private final ContextStorage inner;

    PublishingStorage(ContextStorage inner) {
      this.inner = inner;
    }

    @Override
    public Scope attach(Context context) {
      Scope scope = inner.attach(context);
      if (scope == Scope.noop()) {
        return scope; // the current context didn't change (same context, or null)
      }
      publish(inner.current(), true);
      return () -> {
        try {
          scope.close();
        } finally {
          publish(inner.current(), false);
        }
      };
    }

    @Override
    public Context current() {
      return inner.current();
    }

    @Override
    public Context root() {
      return inner.root();
    }

    private static void publish(Context context, boolean tag) {
      try {
        Span span = context == null ? Span.getInvalid() : Span.fromContext(context);
        SpanContext sc = span.getSpanContext();
        if (!sc.isValid() || !sc.isSampled() || sc.isRemote()) {
          setSpan.invokeExact(MemorySegment.NULL, MemorySegment.NULL);
          return;
        }
        setSpan.invokeExact(
            MemorySegment.ofArray(sc.getTraceIdBytes()), MemorySegment.ofArray(sc.getSpanIdBytes()));
        String spanId = sc.getSpanId();
        if (tag && !Objects.equals(lastTagged.get(), spanId) && span.isRecording()) {
          span.setAttribute(PROFILE_ID_ATTRIBUTE, spanId);
          lastTagged.set(spanId);
        }
      } catch (Error e) {
        throw e;
      } catch (Throwable e) { // never break the app's tracing
        log.log(Level.FINE, "warpspan: failed to publish span", e);
      }
    }
  }
}
