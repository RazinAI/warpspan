// Checks the NVTX side of libwarpspan.so without a GPU: NVTX routes ranges to
// the library (NVTX_INJECTION64_PATH), and warpspan_nvtx_labels reports the
// calling thread's label path. Run through `make test` in capture/.
#include <nvtx3/nvToolsExt.h>

#include <dlfcn.h>

#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <thread>

using labels_fn = int (*)(char *, size_t);
labels_fn g_labels;
int g_failures = 0;

void expect(const char *what, const char *want, int want_depth) {
  char buf[512];
  int depth = g_labels(buf, sizeof buf);
  if (depth != want_depth || std::strcmp(buf, want) != 0) {
    std::fprintf(stderr, "FAIL %s: got depth %d \"%s\", want depth %d \"%s\"\n", what, depth, buf, want_depth, want);
    g_failures++;
  }
}

void push_ex(const char *message) {
  nvtxEventAttributes_t attr{};
  attr.version = NVTX_VERSION;
  attr.size = NVTX_EVENT_ATTRIB_STRUCT_SIZE;
  attr.messageType = NVTX_MESSAGE_TYPE_ASCII;
  attr.message.ascii = message;
  nvtxRangePushEx(&attr);
}

// Pushes a range from a static destructor, after the main thread's TLS
// destructors ran (glibc runs those first in exit()). Must not touch freed TLS.
struct PushAtExit {
  ~PushAtExit() {
    nvtxRangePushA("from a static destructor");
    nvtxRangePop();
  }
} g_push_at_exit;

int main(int argc, char **argv) {
  const char *lib = std::getenv("NVTX_INJECTION64_PATH");
  if (!lib) {
    std::fprintf(stderr, "set NVTX_INJECTION64_PATH to libwarpspan.so\n");
    return 2;
  }
  // The first NVTX call loads the library; dlopen then returns the same copy.
  nvtxRangePushA("outer");
  void *h = dlopen(lib, RTLD_NOW | RTLD_NOLOAD);
  g_labels = h ? reinterpret_cast<labels_fn>(dlsym(h, "warpspan_nvtx_labels")) : nullptr;
  if (!g_labels) {
    std::fprintf(stderr, "FAIL: warpspan_nvtx_labels not found in %s\n", lib);
    return 1;
  }

  if (argc > 1 && std::strcmp(argv[1], "cap") == 0) {
    // WARPSPAN_NVTX_MAX_LABELS=2: "outer" and one more label path fit.
    nvtxRangePushA("a");
    expect("cap: second path recorded", "outer > a", 2);
    nvtxRangePop();
    nvtxRangePushA("b");
    expect("cap: past the cap, new labels share one node", "outer > (more NVTX labels)", 2);
    nvtxRangePushA("c");
    expect("cap: no nested overflow nodes", "outer > (more NVTX labels)", 2);
    nvtxRangePop();
    nvtxRangePop();
    nvtxRangePushA("d");
    expect("cap: same overflow node again", "outer > (more NVTX labels)", 2);
    nvtxRangePop();
    nvtxRangePushA("a");
    expect("cap: known paths still work", "outer > a", 2);
    nvtxRangePop();
    nvtxRangePop();
    expect("cap: empty", "", 0);
    std::printf(g_failures ? "nvtx cap test FAILED\n" : "nvtx cap test ok\n");
    return g_failures ? 1 : 0;
  }

  expect("push A", "outer", 1);

  // A second module (its own NVTX state) initialises NVTX again on first use.
  void *mod = dlopen(std::getenv("NVTX_TEST_MODULE"), RTLD_NOW);
  auto mpush = mod ? reinterpret_cast<void (*)(const char *)>(dlsym(mod, "module_push")) : nullptr;
  auto mpop = mod ? reinterpret_cast<void (*)()>(dlsym(mod, "module_pop")) : nullptr;
  if (!mpush || !mpop) {
    std::fprintf(stderr, "FAIL: cannot load NVTX_TEST_MODULE (%s)\n", dlerror());
    return 1;
  }
  mpush("from module");
  expect("second module", "outer > from module", 2);
  nvtxRangePushA("main again");
  expect("main inside module range", "outer > from module > main again", 3);
  nvtxRangePop();
  mpop();
  expect("module popped", "outer", 1);
  push_ex("infer s3 f120");
  expect("push Ex", "outer > infer s3 f120", 2);
  if (nvtxRangePop() != 1) std::fprintf(stderr, "FAIL pop level\n"), g_failures++;
  expect("pop", "outer", 1);

  // Domains: a range of another domain can be closed out of order.
  nvtxDomainHandle_t d = nvtxDomainCreateA("tensorrt");
  nvtxEventAttributes_t attr{};
  attr.version = NVTX_VERSION;
  attr.size = NVTX_EVENT_ATTRIB_STRUCT_SIZE;
  attr.messageType = NVTX_MESSAGE_TYPE_REGISTERED;
  attr.message.registered = nvtxDomainRegisterStringA(d, "layer conv1");
  nvtxDomainRangePushEx(d, &attr);
  nvtxRangePushA("inner");
  expect("domain + default", "outer > layer conv1 > inner", 3);
  nvtxDomainRangePop(d);
  expect("domain popped under default", "outer > inner", 2);
  nvtxRangePop();

  // Registered strings are interned: the same text gives the same handle.
  if (nvtxDomainRegisterStringA(d, "layer conv1") != attr.message.registered)
    std::fprintf(stderr, "FAIL registered strings not interned\n"), g_failures++;

  nvtxRangePushW(L"wide");
  expect("wide", "outer > wide", 2);
  nvtxRangePop();

  push_ex("");
  expect("empty message", "outer > (unnamed range)", 2);
  nvtxRangePop();

  // Each thread has its own stack.
  std::thread t([] {
    expect("new thread starts empty", "", 0);
    nvtxRangePushA("worker");
    expect("thread push", "worker", 1);
    nvtxRangePop();
  });
  t.join();
  expect("main unaffected", "outer", 1);

  // Deeper than the library records: extra levels are ignored, pops still match.
  for (int i = 0; i < 40; i++) nvtxRangePushA("deep");
  for (int i = 0; i < 40; i++) nvtxRangePop();
  expect("after deep nesting", "outer", 1);

  if (nvtxRangePop() != 0) std::fprintf(stderr, "FAIL last pop level\n"), g_failures++;
  expect("empty", "", 0);
  if (nvtxRangePop() != -1) std::fprintf(stderr, "FAIL pop on empty stack\n"), g_failures++;

  std::printf(g_failures ? "nvtx test FAILED (%d)\n" : "nvtx test ok\n", g_failures);
  return g_failures ? 1 : 0;
}
