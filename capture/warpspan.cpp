// Warpspan capture library.
//
// Loaded into any CUDA process when CUDA_INJECTION64_PATH points at this .so.
// During CUDA initialisation the driver dlopen()s the library, looks up
// InitializeInjection by name (dlsym) and calls it.
//
// Flow:
//   1. A span bridge calls warpspan_set_span() whenever the active OTel span on
//      a thread changes; it is stored in a thread_local.
//   2. CUPTI Callback API: on every kernel launch (app thread) we store
//      correlation_id -> current span in a lock-free table.
//   3. CUPTI Activity API: completed kernel records (CUPTI worker thread or our
//      flush thread) are joined with that table on correlation_id.
//   4. Joined records are batched and sent to the agent over a unix
//      SOCK_SEQPACKET socket, non-blocking; dropped if the agent is busy.
//
// This code runs inside other people's processes: no exceptions, no allocation
// or locks on the launch path, no getenv() after initialisation (it races with
// the app's setenv()), logging only to stderr. One exception: the library is
// dlopen()ed, so glibc allocates its thread_local block lazily, on a thread's
// first launch or span change. -ftls-model=initial-exec would avoid that, but
// dlopen() then fails when the static TLS reserve is used up.

#include "include/warpspan.h"
#include "protocol.h"

#include <cupti.h>

#include <atomic>
#include <cerrno>
#include <cstdarg>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <ctime>
#include <mutex>
#include <new>
#include <string>
#include <unordered_map>
#include <vector>

#include <pthread.h>
#include <signal.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <unistd.h>

#define WARPSPAN_EXPORT extern "C" __attribute__((visibility("default")))

namespace {

namespace wire = warpspan::wire;

constexpr const char *kDefaultSocket = "/var/run/warpspan/warpspan.sock";
constexpr size_t kActivityBufferSize = 2u << 20;  // 2 MiB per CUPTI buffer
constexpr uint32_t kLaunchSlots = 1u << 17;       // must be a power of two
constexpr uint64_t kReconnectIntervalMs = 2000;
constexpr int kFlushIntervalMs = 1000;

// ---------------------------------------------------------------------------
// Logging (stderr only; stdout belongs to the app)

bool g_quiet = false;

__attribute__((format(printf, 1, 2))) void log(const char *fmt, ...) {
  if (g_quiet) return;
  char buf[512];
  va_list ap;
  va_start(ap, fmt);
  int n = vsnprintf(buf, sizeof buf, fmt, ap);
  va_end(ap);
  if (n >= 0) std::fprintf(stderr, "warpspan: %s\n", buf);
}

bool env_true(const char *name) {
  const char *v = std::getenv(name);
  return v && *v && std::strcmp(v, "0") != 0 && std::strcmp(v, "false") != 0;
}

uint64_t monotonic_ms() {
  timespec ts{};
  clock_gettime(CLOCK_MONOTONIC, &ts);
  return static_cast<uint64_t>(ts.tv_sec) * 1000u + static_cast<uint64_t>(ts.tv_nsec) / 1000000u;
}

// ---------------------------------------------------------------------------
// Current span per thread (set by the span bridge)

struct SpanCtx {
  uint64_t w[3];  // raw bytes: trace_id[16] then span_id[8]
  bool valid() const { return (w[0] | w[1]) != 0 && w[2] != 0; }
};

thread_local SpanCtx t_span{};

// ---------------------------------------------------------------------------
// Launch table: correlation_id -> span. Direct-mapped ring written by app
// threads at launch and read by the activity thread. Each slot is a seqlock:
// the writer clears corr, writes the span, then publishes corr; the reader
// accepts the span only if corr is the same before and after reading it.

struct LaunchSlot {
  std::atomic<uint32_t> corr;
  std::atomic<uint64_t> w[3];
};

LaunchSlot *g_launches = nullptr;

inline void record_launch(uint32_t corr) {
  LaunchSlot &slot = g_launches[corr & (kLaunchSlots - 1)];
  // A runtime launch can also fire the driver callback with the same id.
  if (slot.corr.load(std::memory_order_relaxed) == corr) return;
  const SpanCtx s = t_span;
  slot.corr.store(0, std::memory_order_relaxed);
  std::atomic_thread_fence(std::memory_order_release);
  for (int i = 0; i < 3; i++) slot.w[i].store(s.w[i], std::memory_order_relaxed);
  slot.corr.store(corr, std::memory_order_release);
}

bool lookup_launch(uint32_t corr, SpanCtx *span) {
  LaunchSlot &slot = g_launches[corr & (kLaunchSlots - 1)];
  if (slot.corr.load(std::memory_order_acquire) != corr) return false;
  for (int i = 0; i < 3; i++) span->w[i] = slot.w[i].load(std::memory_order_relaxed);
  std::atomic_thread_fence(std::memory_order_acquire);
  return slot.corr.load(std::memory_order_relaxed) == corr;
}

// ---------------------------------------------------------------------------
// Little-endian byte writer

struct Bytes {
  std::vector<uint8_t> buf;
  void u16(uint16_t v) { raw(&v, sizeof v); }
  void u32(uint32_t v) { raw(&v, sizeof v); }
  void u64(uint64_t v) { raw(&v, sizeof v); }
  void raw(const void *p, size_t n) {
    const auto *b = static_cast<const uint8_t *>(p);
    buf.insert(buf.end(), b, b + n);
  }
  void str16(const char *s, size_t n) {
    u16(static_cast<uint16_t>(n));
    raw(s, n);
  }
};

void put_header(Bytes &out, wire::MessageType type, size_t payload_len) {
  out.u32(wire::kMagic);
  out.u16(wire::kVersion);
  out.u16(type);
  out.u32(static_cast<uint32_t>(payload_len));
  out.u32(static_cast<uint32_t>(getpid()));
}

// HELLO: process metadata the agent turns into OTel resource attributes.
// Built once at init, so no getenv() happens on our threads later.
Bytes build_hello() {
  std::vector<std::pair<std::string, std::string>> kv;
  char exe[4096];
  ssize_t n = readlink("/proc/self/exe", exe, sizeof exe - 1);
  if (n > 0) {
    exe[n] = '\0';
    const char *slash = std::strrchr(exe, '/');
    kv.emplace_back("process.executable.name", slash ? slash + 1 : exe);
  }
  char host[256] = {};
  if (gethostname(host, sizeof host - 1) == 0) kv.emplace_back("host.name", host);
  for (const char *name : {"OTEL_SERVICE_NAME", "OTEL_RESOURCE_ATTRIBUTES"}) {
    const char *v = std::getenv(name);
    if (v && *v) kv.emplace_back(std::string("env.") + name, std::string(v).substr(0, 4096));
  }

  Bytes payload;
  payload.u32(static_cast<uint32_t>(kv.size()));
  for (auto &[k, v] : kv) {
    payload.str16(k.data(), k.size());
    payload.str16(v.data(), v.size());
  }
  Bytes msg;
  put_header(msg, wire::kHello, payload.buf.size());
  msg.raw(payload.buf.data(), payload.buf.size());
  return msg;
}

// ---------------------------------------------------------------------------
// Global state. Heap-allocated and never freed: CUPTI threads may still run
// while static destructors execute at exit.

struct State {
  std::string socket_path;  // empty: don't send
  FILE *debug = nullptr;
  Bytes hello;

  std::atomic<bool> disabled{false};

  std::atomic<uint64_t> kernels{0};
  std::atomic<uint64_t> kernels_linked{0};
  std::atomic<uint64_t> dropped{0};  // CUPTI overflow + agent busy
  std::atomic<uint64_t> unsent{0};   // no agent connected

  std::mutex flush_mu;  // serialises cuptiActivityFlushAll calls

  // Guarded by out_mu.
  std::mutex out_mu;
  int fd = -1;
  uint64_t last_connect_attempt_ms = 0;
  bool logged_connect_failure = false;
  Bytes strings, records;
  uint32_t string_count = 0, record_count = 0;
  std::unordered_map<const char *, uint32_t> string_index;  // CUPTI's name pointer -> index
};

State *g = nullptr;
// Set once InitializeInjection has finished. CUPTI callbacks can only run after
// start_cupti, but warpspan_flush can be called from any thread at any time.
std::atomic<bool> g_ready{false};

// --- socket (out_mu held) ---------------------------------------------------

void close_socket_locked() {
  if (g->fd >= 0) close(g->fd);
  g->fd = -1;
}

bool send_locked(const Bytes &msg) {
  if (g->fd < 0) return false;
  // SOCK_SEQPACKET sends the whole message or fails.
  if (send(g->fd, msg.buf.data(), msg.buf.size(), MSG_DONTWAIT | MSG_NOSIGNAL) >= 0) return true;
  if (errno == EAGAIN || errno == EWOULDBLOCK) return false;  // agent busy: drop
  log("lost connection to agent (%s), will reconnect", std::strerror(errno));
  close_socket_locked();
  return false;
}

void connect_locked() {
  if (g->fd >= 0 || g->socket_path.empty()) return;
  uint64_t now = monotonic_ms();
  if (g->last_connect_attempt_ms != 0 && now - g->last_connect_attempt_ms < kReconnectIntervalMs) return;
  g->last_connect_attempt_ms = now;

  sockaddr_un addr{};
  addr.sun_family = AF_UNIX;
  std::memcpy(addr.sun_path, g->socket_path.c_str(), g->socket_path.size() + 1);
  int fd = socket(AF_UNIX, SOCK_SEQPACKET | SOCK_CLOEXEC | SOCK_NONBLOCK, 0);
  if (fd < 0) return;
  if (connect(fd, reinterpret_cast<sockaddr *>(&addr), sizeof addr) != 0) {
    if (!g->logged_connect_failure) {
      log("agent not reachable at %s (%s); retrying in the background", g->socket_path.c_str(),
          std::strerror(errno));
      g->logged_connect_failure = true;
    }
    close(fd);
    return;
  }
  g->fd = fd;
  if (!send_locked(g->hello)) {
    close_socket_locked();  // the agent rejects kernels without a hello
    return;
  }
  log("connected to agent at %s", g->socket_path.c_str());
  g->logged_connect_failure = false;
}

// --- kernel batches (out_mu held) -------------------------------------------

void flush_batch_locked() {
  if (g->record_count > 0) {
    Bytes msg;
    const size_t payload_len = wire::kKernelsFixedSize + g->strings.buf.size() + g->records.buf.size();
    msg.buf.reserve(wire::kHeaderSize + payload_len);
    put_header(msg, wire::kKernels, payload_len);
    msg.u64(g->dropped.load(std::memory_order_relaxed));
    msg.u32(g->string_count);
    msg.u32(g->record_count);
    msg.raw(g->strings.buf.data(), g->strings.buf.size());
    msg.raw(g->records.buf.data(), g->records.buf.size());
    if (!send_locked(msg)) g->dropped.fetch_add(g->record_count, std::memory_order_relaxed);
  }
  g->strings.buf.clear();
  g->records.buf.clear();
  g->string_count = g->record_count = 0;
  g->string_index.clear();
}

void add_kernel_locked(const char *name, uint64_t start, uint64_t end, uint32_t device, const SpanCtx &span) {
  if (!name) name = "<unknown kernel>";
  auto it = g->string_index.find(name);
  size_t name_len = it == g->string_index.end() ? strnlen(name, wire::kMaxStringLen) : 0;
  size_t need = wire::kKernelRecordSize + (it == g->string_index.end() ? 2 + name_len : 0);
  if (wire::kHeaderSize + wire::kKernelsFixedSize + g->strings.buf.size() + g->records.buf.size() + need >
      wire::kMaxMessage) {
    flush_batch_locked();
    it = g->string_index.end();
    name_len = strnlen(name, wire::kMaxStringLen);
  }
  uint32_t name_index;
  if (it == g->string_index.end()) {
    name_index = g->string_count++;
    g->strings.str16(name, name_len);
    g->string_index.emplace(name, name_index);
  } else {
    name_index = it->second;
  }

  Bytes &r = g->records;
  r.u64(start);
  r.u64(end);
  r.raw(span.w, sizeof span.w);  // trace_id[16] + span_id[8], zero if unlinked
  r.u32(name_index);
  r.u32(device);
  g->record_count++;
}

void debug_kernel(const char *name, uint64_t start, uint64_t end, uint32_t corr, uint32_t device,
                  const SpanCtx &span, bool linked) {
  char ids[80] = "trace_id=- span_id=-";  // linked: 9 + 32 + 9 + 16 + NUL = 67 bytes
  if (linked) {
    uint8_t b[24];
    std::memcpy(b, span.w, sizeof b);
    int o = std::snprintf(ids, sizeof ids, "trace_id=");
    for (int i = 0; i < 16; i++) o += std::snprintf(ids + o, sizeof ids - o, "%02x", b[i]);
    o += std::snprintf(ids + o, sizeof ids - o, " span_id=");
    for (int i = 16; i < 24; i++) o += std::snprintf(ids + o, sizeof ids - o, "%02x", b[i]);
  }
  std::fprintf(g->debug, "kernel name=%s start_ns=%llu end_ns=%llu duration_ns=%llu correlation_id=%u device=%u %s\n",
               name ? name : "<unknown kernel>", static_cast<unsigned long long>(start),
               static_cast<unsigned long long>(end), static_cast<unsigned long long>(end - start), corr, device, ids);
}

// ---------------------------------------------------------------------------
// CUPTI callbacks

bool cupti_ok(CUptiResult r, const char *what) {
  if (r == CUPTI_SUCCESS) return true;
  const char *msg = nullptr;
  cuptiGetResultString(r, &msg);
  log("%s failed: %s", what, msg ? msg : "unknown error");
  return false;
}
#define CUPTI_OK(call) cupti_ok((call), #call)

// Hot path: runs on the app's thread inside every kernel launch.
void CUPTIAPI on_api_callback(void *, CUpti_CallbackDomain, CUpti_CallbackId, const void *data) {
  const auto *cb = static_cast<const CUpti_CallbackData *>(data);
  if (cb->callbackSite == CUPTI_API_ENTER) record_launch(cb->correlationId);
}

void CUPTIAPI on_buffer_requested(uint8_t **buffer, size_t *size, size_t *max_records) {
  void *p = nullptr;
  if (posix_memalign(&p, 8, kActivityBufferSize) != 0) p = nullptr;
  *buffer = static_cast<uint8_t *>(p);
  *size = p ? kActivityBufferSize : 0;
  *max_records = 0;  // as many as fit
}

void CUPTIAPI on_buffer_completed(CUcontext ctx, uint32_t stream_id, uint8_t *buffer, size_t, size_t valid) {
  if (g && !g->disabled.load(std::memory_order_relaxed) && valid > 0) {
    std::lock_guard<std::mutex> lock(g->out_mu);
    const bool connected = g->fd >= 0;
    CUpti_Activity *rec = nullptr;
    while (cuptiActivityGetNextRecord(buffer, valid, &rec) == CUPTI_SUCCESS) {
      if (rec->kind != CUPTI_ACTIVITY_KIND_CONCURRENT_KERNEL) continue;
      // CUpti_ActivityKernel4..13 all have these fields at the same offsets
      // (checked against the CUDA 13.4 headers), so the oldest layout still
      // shipped works with CUDA 11, 12 and 13.
      const auto *k = reinterpret_cast<const CUpti_ActivityKernel4 *>(rec);
      SpanCtx span{};
      const bool linked = lookup_launch(k->correlationId, &span) && span.valid();
      if (!linked) span = SpanCtx{};
      g->kernels.fetch_add(1, std::memory_order_relaxed);
      if (linked) g->kernels_linked.fetch_add(1, std::memory_order_relaxed);
      if (g->debug) debug_kernel(k->name, k->start, k->end, k->correlationId, k->deviceId, span, linked);
      if (connected) {
        add_kernel_locked(k->name, k->start, k->end, k->deviceId, span);
      } else {
        g->unsent.fetch_add(1, std::memory_order_relaxed);
      }
    }
    size_t dropped = 0;
    if (cuptiActivityGetNumDroppedRecords(ctx, stream_id, &dropped) == CUPTI_SUCCESS && dropped > 0)
      g->dropped.fetch_add(dropped, std::memory_order_relaxed);
    flush_batch_locked();
    if (g->debug) std::fflush(g->debug);
  }
  std::free(buffer);
}

void flush_activity(uint32_t flags) {
  std::lock_guard<std::mutex> lock(g->flush_mu);
  if (g->disabled.load(std::memory_order_relaxed)) return;  // exiting: CUDA may be torn down
  cuptiActivityFlushAll(flags);
}

bool start_cupti(CUpti_SubscriberHandle *subscriber) {
  if (!CUPTI_OK(cuptiSubscribe(subscriber, on_api_callback, nullptr))) return false;

  const CUpti_CallbackId runtime_launches[] = {
      CUPTI_RUNTIME_TRACE_CBID_cudaLaunchKernel_v7000,
      CUPTI_RUNTIME_TRACE_CBID_cudaLaunchKernel_ptsz_v7000,
      CUPTI_RUNTIME_TRACE_CBID_cudaLaunchCooperativeKernel_v9000,
      CUPTI_RUNTIME_TRACE_CBID_cudaLaunchCooperativeKernel_ptsz_v9000,
      CUPTI_RUNTIME_TRACE_CBID_cudaGraphLaunch_v10000,
      CUPTI_RUNTIME_TRACE_CBID_cudaGraphLaunch_ptsz_v10000,
#if CUDA_VERSION >= 11060
      CUPTI_RUNTIME_TRACE_CBID_cudaLaunchKernelExC_v11060,
      CUPTI_RUNTIME_TRACE_CBID_cudaLaunchKernelExC_ptsz_v11060,
#endif
  };
  // Triton and other JIT kernels are launched through the driver API.
  const CUpti_CallbackId driver_launches[] = {
      CUPTI_DRIVER_TRACE_CBID_cuLaunchKernel,
      CUPTI_DRIVER_TRACE_CBID_cuLaunchKernel_ptsz,
      CUPTI_DRIVER_TRACE_CBID_cuLaunchCooperativeKernel,
      CUPTI_DRIVER_TRACE_CBID_cuLaunchCooperativeKernel_ptsz,
      CUPTI_DRIVER_TRACE_CBID_cuGraphLaunch,
      CUPTI_DRIVER_TRACE_CBID_cuGraphLaunch_ptsz,
#if CUDA_VERSION >= 12000
      CUPTI_DRIVER_TRACE_CBID_cuLaunchKernelEx,
      CUPTI_DRIVER_TRACE_CBID_cuLaunchKernelEx_ptsz,
#endif
  };
  for (CUpti_CallbackId id : runtime_launches)
    CUPTI_OK(cuptiEnableCallback(1, *subscriber, CUPTI_CB_DOMAIN_RUNTIME_API, id));
  for (CUpti_CallbackId id : driver_launches)
    CUPTI_OK(cuptiEnableCallback(1, *subscriber, CUPTI_CB_DOMAIN_DRIVER_API, id));

  if (!CUPTI_OK(cuptiActivityRegisterCallbacks(on_buffer_requested, on_buffer_completed)) ||
      !CUPTI_OK(cuptiActivityEnable(CUPTI_ACTIVITY_KIND_CONCURRENT_KERNEL))) {
    cuptiUnsubscribe(*subscriber);
    return false;
  }
  return true;
}

// ---------------------------------------------------------------------------
// Background thread (flush + reconnect), exit and fork handling

void *flush_thread_main(void *) {
  while (!g->disabled.load(std::memory_order_relaxed)) {
    {
      std::lock_guard<std::mutex> lock(g->out_mu);
      connect_locked();
    }
    flush_activity(0);
    timespec ts{kFlushIntervalMs / 1000, (kFlushIntervalMs % 1000) * 1000000L};
    nanosleep(&ts, nullptr);
  }
  return nullptr;
}

bool start_flush_thread() {
  // Block all signals in our thread so the app's signal handling is unaffected.
  sigset_t all, old;
  sigfillset(&all);
  pthread_sigmask(SIG_SETMASK, &all, &old);
  pthread_t thread;
  int rc = pthread_create(&thread, nullptr, flush_thread_main, nullptr);
  pthread_sigmask(SIG_SETMASK, &old, nullptr);
  if (rc != 0) return false;
  pthread_setname_np(thread, "warpspan");
  pthread_detach(thread);
  return true;
}

// Runs on the exiting app thread: one last flush so short-lived processes are seen.
void on_exit() {
  if (!g || g->disabled.load()) return;
  {
    std::lock_guard<std::mutex> lock(g->out_mu);
    connect_locked();
  }
  {
    // Last flush; then stop the flush thread from calling CUPTI while CUDA's
    // own exit handlers and destructors run.
    std::lock_guard<std::mutex> lock(g->flush_mu);
    cuptiActivityFlushAll(CUPTI_ACTIVITY_FLAG_FLUSH_FORCED);
    g->disabled.store(true);
  }
  log("exiting: %llu kernels, %llu linked to spans, %llu dropped, %llu not sent (no agent)",
      static_cast<unsigned long long>(g->kernels.load()), static_cast<unsigned long long>(g->kernels_linked.load()),
      static_cast<unsigned long long>(g->dropped.load()), static_cast<unsigned long long>(g->unsent.load()));
}

void on_fork_child() {
  // The background thread does not exist in the child and CUDA can't be used
  // after fork; stop, and don't share the parent's socket.
  if (!g) return;
  g->disabled.store(true);
  if (g->fd >= 0) close(g->fd);
  g->fd = -1;
}

}  // namespace

// ---------------------------------------------------------------------------
// Exported API

WARPSPAN_EXPORT uint32_t warpspan_abi_version(void) { return WARPSPAN_ABI_VERSION; }

WARPSPAN_EXPORT void warpspan_set_span(const uint8_t *trace_id, const uint8_t *span_id) {
  SpanCtx s{};
  if (trace_id && span_id) {
    std::memcpy(&s.w[0], trace_id, 16);
    std::memcpy(&s.w[2], span_id, 8);
  }
  t_span = s.valid() ? s : SpanCtx{};
}

WARPSPAN_EXPORT int warpspan_get_span(uint8_t *trace_id, uint8_t *span_id) {
  const SpanCtx s = t_span;
  if (trace_id) std::memcpy(trace_id, &s.w[0], 16);
  if (span_id) std::memcpy(span_id, &s.w[2], 8);
  return s.valid() ? 1 : 0;
}

WARPSPAN_EXPORT void warpspan_flush(void) {
  if (!g_ready.load(std::memory_order_acquire) || g->disabled.load()) return;
  {
    std::lock_guard<std::mutex> lock(g->out_mu);
    connect_locked();
  }
  flush_activity(CUPTI_ACTIVITY_FLAG_FLUSH_FORCED);
}

namespace {
warpspan_python_api g_py{};  // set by warpspan_python_watch; read by the watcher

// Called by CPython on the thread whose context switched, with its thread
// state attached. It can run while an exception passes through Context.run,
// so it saves and restores the current exception around its own C API calls.
int on_python_context_switch(int event, void *) {
  constexpr int kContextSwitched = 1;  // Py_CONTEXT_SWITCHED
  if (event != kContextSwitched) return 0;
  void *exception = g_py.get_raised_exception();
  SpanCtx s{};
  void *value = nullptr;
  if (g_py.contextvar_get(g_py.contextvar, nullptr, &value) == 0 && value) {
    if (g_py.bytes_size(value) == 24) {
      if (const char *bytes = g_py.bytes_as_string(value)) std::memcpy(s.w, bytes, 24);
    }
    g_py.dec_ref(value);
  }
  t_span = s.valid() ? s : SpanCtx{};
  g_py.set_raised_exception(exception);  // also drops any error raised above
  return 0;
}
}  // namespace

WARPSPAN_EXPORT int warpspan_python_watch(const warpspan_python_api *api) {
  if (g_py.contextvar && g_py.contextvar != api->contextvar) return -1;  // another interpreter
  if (!g_py.contextvar) api->inc_ref(api->contextvar);  // kept for the life of the process
  g_py = *api;
  return g_py.add_watcher(on_python_context_switch);
}

// Called by the CUDA driver. Return 1 on success. We always return 1: if
// profiling can't start, the app must keep running unaffected.
WARPSPAN_EXPORT int InitializeInjection(void) {
  static std::atomic<bool> initialized{false};
  if (initialized.exchange(true)) return 1;

  g_quiet = env_true("WARPSPAN_QUIET");
  if (env_true("WARPSPAN_DISABLE")) {
    log("disabled by WARPSPAN_DISABLE");
    return 1;
  }

  // calloc: the table's pages are only touched as correlation ids reach them.
  g_launches = static_cast<LaunchSlot *>(std::calloc(kLaunchSlots, sizeof(LaunchSlot)));
  g = new (std::nothrow) State();
  if (!g || !g_launches) {
    log("out of memory; profiling disabled");
    g = nullptr;
    return 1;
  }

  const char *sock = std::getenv("WARPSPAN_SOCKET");
  g->socket_path = sock ? sock : kDefaultSocket;
  if (g->socket_path.size() >= sizeof(sockaddr_un::sun_path)) {
    log("WARPSPAN_SOCKET path too long (max %zu bytes); not sending", sizeof(sockaddr_un::sun_path) - 1);
    g->socket_path.clear();
  }
  if (const char *path = std::getenv("WARPSPAN_DEBUG_FILE")) {
    g->debug = std::strcmp(path, "-") == 0 ? stderr : std::fopen(path, "ae");
    if (!g->debug) log("cannot open WARPSPAN_DEBUG_FILE %s: %s", path, std::strerror(errno));
  }
  g->hello = build_hello();

  CUpti_SubscriberHandle subscriber = nullptr;
  if (!start_cupti(&subscriber)) {
    log("CUPTI setup failed; profiling disabled for pid %d", static_cast<int>(getpid()));
    g->disabled.store(true);
    return 1;
  }
  pthread_atfork(nullptr, nullptr, on_fork_child);
  std::atexit(on_exit);
  if (!start_flush_thread()) log("could not start background thread; kernels are sent only when buffers fill");

  g_ready.store(true, std::memory_order_release);
  log("loaded (pid %d, agent socket %s)", static_cast<int>(getpid()),
      g->socket_path.empty() ? "<none>" : g->socket_path.c_str());
  return 1;
}
