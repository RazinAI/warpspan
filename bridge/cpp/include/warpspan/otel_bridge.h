// Warpspan span bridge for OpenTelemetry C++.
//
// Call warpspan::otel::Install() once at startup, before other threads use
// OpenTelemetry (the API only allows replacing the runtime context storage
// then). It installs a storage that wraps the default thread-local one: after
// every Attach/Detach it hands the now-current span to libwarpspan.so, which
// tags the CUDA kernels launched on this thread with it.
//
// Spans are tracked per thread, so coroutines or fibers that switch spans
// without Attach/Detach get kernels attributed to the last attached span.
//
// Header-only. Needs the opentelemetry-cpp API headers, and -ldl on glibc
// older than 2.34.
#pragma once

#include <dlfcn.h>

#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <mutex>

#include "opentelemetry/context/runtime_context.h"
#include "opentelemetry/nostd/shared_ptr.h"
#include "opentelemetry/trace/context.h"
#include "opentelemetry/trace/span.h"

namespace warpspan {
namespace otel {

// Must match WARPSPAN_ABI_VERSION in capture/include/warpspan.h.
constexpr uint32_t kAbiVersion = 1;

// Span attribute Grafana needs to offer "Profiles for this span".
constexpr const char *kProfileIdAttribute = "pyroscope.profile.id";

namespace detail {

using SetSpanFn = void (*)(const uint8_t *trace_id, const uint8_t *span_id);

// Forwards to the default storage and publishes the current span after each
// change. Only sampled, local spans are published: unsampled spans never reach
// the trace backend and a remote parent belongs to another service.
class ContextStorage final : public opentelemetry::context::RuntimeContextStorage {
 public:
  explicit ContextStorage(SetSpanFn set_span) noexcept
      : inner_(new opentelemetry::context::ThreadLocalContextStorage()), set_span_(set_span) {}

  opentelemetry::context::Context GetCurrent() noexcept override { return inner_->GetCurrent(); }

  opentelemetry::nostd::unique_ptr<opentelemetry::context::Token> Attach(
      const opentelemetry::context::Context &context) noexcept override {
    auto token = inner_->Attach(context);
    Publish(context, /*tag=*/true);
    return token;
  }

  bool Detach(opentelemetry::context::Token &token) noexcept override {
    bool ok = inner_->Detach(token);
    Publish(inner_->GetCurrent(), /*tag=*/false);
    return ok;
  }

  void Publish(const opentelemetry::context::Context &context, bool tag) noexcept {
    namespace trace = opentelemetry::trace;
    // Read the value directly: trace::GetSpan() allocates a placeholder span
    // when there is none, which happens on every return to the root.
    const opentelemetry::context::ContextValue value = context.GetValue(trace::kSpanKey);
    const auto *span = opentelemetry::nostd::get_if<opentelemetry::nostd::shared_ptr<trace::Span>>(&value);
    if (!span || !*span) {
      set_span_(nullptr, nullptr);
      return;
    }
    const trace::SpanContext sc = (*span)->GetContext();
    if (!sc.IsValid() || !sc.IsSampled() || sc.IsRemote()) {
      set_span_(nullptr, nullptr);
      return;
    }
    set_span_(sc.trace_id().Id().data(), sc.span_id().Id().data());

    // Tag each span once per thread: SetAttribute on an SDK span takes a lock,
    // and the same span is re-attached whenever only other context values change.
    static thread_local trace::SpanId last_tagged;
    if (tag && sc.span_id() != last_tagged && (*span)->IsRecording()) {
      char hex[16];
      sc.span_id().ToLowerBase16(hex);
      (*span)->SetAttribute(kProfileIdAttribute, opentelemetry::nostd::string_view(hex, sizeof(hex)));
      last_tagged = sc.span_id();
    }
  }

 private:
  opentelemetry::nostd::shared_ptr<RuntimeContextStorage> inner_;
  SetSpanFn set_span_;
};

}  // namespace detail

// Installs the bridge. The library is library_path or $CUDA_INJECTION64_PATH.
// It must be the same file CUDA loads (CUDA_INJECTION64_PATH): dlopen() then
// returns that copy, so both share one thread-local. Returns false, logs why to
// stderr and changes nothing if the library is missing or has another ABI
// version; returns false silently if no path is given or set.
inline bool Install(const char *library_path = nullptr) noexcept {
  namespace context = opentelemetry::context;
  static std::mutex mu;
  static bool installed = false;
  std::lock_guard<std::mutex> lock(mu);
  if (installed) return true;

  const char *path = library_path ? library_path : std::getenv("CUDA_INJECTION64_PATH");
  if (!path || !*path) return false;
  void *lib = dlopen(path, RTLD_NOW | RTLD_LOCAL);
  if (!lib) {
    std::fprintf(stderr, "warpspan: cannot load %s: %s; span bridge disabled\n", path, dlerror());
    return false;
  }
  auto abi_version = reinterpret_cast<uint32_t (*)()>(dlsym(lib, "warpspan_abi_version"));
  auto set_span = reinterpret_cast<detail::SetSpanFn>(dlsym(lib, "warpspan_set_span"));
  const uint32_t abi = abi_version ? abi_version() : 0;
  if (!set_span || abi != kAbiVersion) {
    std::fprintf(stderr, "warpspan: %s: ABI version %u, bridge expects %u; span bridge disabled\n", path, abi,
                 kAbiVersion);
    dlclose(lib);
    return false;
  }

  // ThreadLocalContextStorage keeps its stack in one static thread_local, so
  // the new storage sees what the default one already holds.
  auto *storage = new detail::ContextStorage(set_span);
  context::RuntimeContext::SetRuntimeContextStorage(
      opentelemetry::nostd::shared_ptr<context::RuntimeContextStorage>(storage));
  storage->Publish(context::RuntimeContext::GetCurrent(), /*tag=*/false);  // a span may be active here
  installed = true;
  return true;
}

}  // namespace otel
}  // namespace warpspan
