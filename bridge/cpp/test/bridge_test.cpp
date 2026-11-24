// Tests against the real libwarpspan.so (no GPU needed: CUDA never calls
// InitializeInjection here, only the span thread-local is exercised).
#include <dlfcn.h>

#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <map>
#include <string>
#include <thread>

#include "opentelemetry/trace/default_span.h"
#include "opentelemetry/trace/scope.h"
#include "warpspan.h"
#include "warpspan/otel_bridge.h"

namespace nostd = opentelemetry::nostd;
namespace trace = opentelemetry::trace;

static_assert(warpspan::otel::kAbiVersion == WARPSPAN_ABI_VERSION, "bridge and library ABI differ");

namespace {

int failures = 0;

#define CHECK(cond)                                                \
  do {                                                             \
    if (!(cond)) {                                                 \
      std::fprintf(stderr, "%s:%d: CHECK(%s)\n", __FILE__, __LINE__, #cond); \
      failures++;                                                  \
    }                                                              \
  } while (0)

int (*get_span)(uint8_t *, uint8_t *);

// A span that keeps its attributes, so the test can see them.
class TestSpan : public trace::DefaultSpan {
 public:
  TestSpan(trace::SpanContext sc, bool recording) noexcept : DefaultSpan(sc), recording_(recording) {}
  bool IsRecording() const noexcept override { return recording_; }
  void SetAttribute(nostd::string_view key, const opentelemetry::common::AttributeValue &value) noexcept override {
    set_attribute_calls++;
    if (auto *s = nostd::get_if<nostd::string_view>(&value)) attrs[std::string(key)] = std::string(*s);
  }
  std::map<std::string, std::string> attrs;
  int set_attribute_calls = 0;

 private:
  bool recording_;
};

trace::SpanContext make_context(uint8_t seed, bool sampled = true, bool remote = false) {
  uint8_t tid[16] = {}, sid[8] = {};
  tid[15] = seed;
  sid[7] = seed;
  return trace::SpanContext(trace::TraceId(tid), trace::SpanId(sid),
                            trace::TraceFlags(sampled ? trace::TraceFlags::kIsSampled : 0), remote);
}

nostd::shared_ptr<trace::Span> make_span(uint8_t seed, bool sampled = true, bool remote = false,
                                        TestSpan **out = nullptr, bool recording = true) {
  auto *span = new TestSpan(make_context(seed, sampled, remote), recording);
  if (out) *out = span;
  return nostd::shared_ptr<trace::Span>(span);
}

// Span ID byte 7 the library holds for this thread, or -1 if none.
int current() {
  uint8_t tid[16], sid[8];
  return get_span(tid, sid) ? sid[7] : -1;
}

void test_nested_spans() {
  CHECK(current() == -1);
  TestSpan *outer_span = nullptr;
  auto outer = make_span(1, true, false, &outer_span);
  {
    trace::Scope s1(outer);
    CHECK(current() == 1);
    {
      trace::Scope s2(make_span(2));
      CHECK(current() == 2);
    }
    CHECK(current() == 1);
  }
  CHECK(current() == -1);
  CHECK(outer_span->attrs["pyroscope.profile.id"] == "0000000000000001");
}

void test_unsampled_remote_and_null_spans_clear() {
  trace::Scope outer(make_span(3));
  CHECK(current() == 3);
  {
    trace::Scope s(make_span(4, /*sampled=*/false));
    CHECK(current() == -1);
  }
  CHECK(current() == 3);
  {
    trace::Scope s(make_span(5, /*sampled=*/true, /*remote=*/true));
    CHECK(current() == -1);
  }
  {
    trace::Scope s(nostd::shared_ptr<trace::Span>{});  // a context holding a null span
    CHECK(current() == -1);
  }
  CHECK(current() == 3);
}

void test_tagging() {
  TestSpan *recording = nullptr, *not_recording = nullptr;
  auto span = make_span(7, true, false, &recording);
  for (int i = 0; i < 3; i++) {  // re-attaching the same span tags it once
    trace::Scope s(span);
  }
  CHECK(recording->set_attribute_calls == 1);
  auto plain = make_span(8, true, false, &not_recording, /*recording=*/false);
  {
    trace::Scope s(plain);
    CHECK(current() == 8);  // published, but not tagged
  }
  CHECK(not_recording->set_attribute_calls == 0);
}

void test_threads_are_independent() {
  trace::Scope s(make_span(5));
  int other = 0;
  std::thread t([&] {
    trace::Scope s2(make_span(6));
    other = current();
  });
  t.join();
  CHECK(other == 6);
  CHECK(current() == 5);
}

}  // namespace

int main(int argc, char **argv) {
  if (argc != 2) {
    std::fprintf(stderr, "usage: %s path/to/libwarpspan.so\n", argv[0]);
    return 2;
  }
  void *lib = dlopen(argv[1], RTLD_NOW | RTLD_LOCAL);
  get_span = lib ? reinterpret_cast<int (*)(uint8_t *, uint8_t *)>(dlsym(lib, "warpspan_get_span")) : nullptr;
  if (!get_span) {
    std::fprintf(stderr, "cannot load warpspan_get_span from %s\n", argv[1]);
    return 1;
  }

  CHECK(!warpspan::otel::Install("/nonexistent/libwarpspan.so"));
  unsetenv("CUDA_INJECTION64_PATH");
  CHECK(!warpspan::otel::Install());
  setenv("CUDA_INJECTION64_PATH", argv[1], 1);
  {
    // A span already active on this thread at Install() is published, and its
    // scope, attached through the default storage, still detaches cleanly.
    trace::Scope before(make_span(9));
    CHECK(warpspan::otel::Install());
    CHECK(current() == 9);
  }
  CHECK(current() == -1);
  CHECK(warpspan::otel::Install("/nonexistent/libwarpspan.so"));  // already installed

  test_nested_spans();
  test_unsampled_remote_and_null_spans_clear();
  test_tagging();
  test_threads_are_independent();

  if (failures) {
    std::fprintf(stderr, "%d check(s) failed\n", failures);
    return 1;
  }
  std::puts("ok");
  return 0;
}
