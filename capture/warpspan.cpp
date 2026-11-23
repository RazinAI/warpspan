// Warpspan capture library.
//
// Loaded into any CUDA process when CUDA_INJECTION64_PATH points at this .so.
// During CUDA initialisation the driver dlopen()s the library, looks up
// InitializeInjection by name (dlsym) and calls it.
//
// Flow:
//   1. Span bridge (in the app's language) calls warpspan_set_span() whenever the
//      active OTel span on a thread changes -> stored in a thread_local.
//   2. CUPTI Callback API: on every kernel launch (app thread) we store
//      correlation_id -> (current span, thread id) in a lock-free table.
//   3. CUPTI Activity API: completed kernel records (CUPTI worker thread / our
//      flush thread) are joined with that table on correlation_id.
//   4. Joined records are batched and sent to the agent over a unix
//      SOCK_SEQPACKET socket (non-blocking; dropped if the agent is slow or absent).
//
// Rules for this file: no exceptions, nothing that can block or allocate on the
// launch path (on_api_callback), log only to stderr.

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
#include <string>
#include <unordered_map>
#include <vector>

#include <pthread.h>
#include <signal.h>
#include <sys/socket.h>
#include <sys/syscall.h>
#include <sys/un.h>
#include <unistd.h>

#define WARPSPAN_EXPORT extern "C" __attribute__((visibility("default")))

namespace {

namespace wire = warpspan::wire;

constexpr const char *kVersion = "0.1.0";
constexpr const char *kDefaultSocket = "/var/run/warpspan/warpspan.sock";
constexpr size_t kActivityBufferSize = 2u << 20;  // 2 MiB per CUPTI buffer
constexpr uint32_t kLaunchSlots = 1u << 17;       // must be a power of two
constexpr int kReconnectIntervalMs = 2000;

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
  if (n < 0) return;
  std::fprintf(stderr, "warpspan: %s\n", buf);
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

void to_hex(const uint8_t *b, size_t n, char *out) {
  static const char digits[] = "0123456789abcdef";
  for (size_t i = 0; i < n; i++) {
    out[2 * i] = digits[b[i] >> 4];
    out[2 * i + 1] = digits[b[i] & 0xF];
  }
  out[2 * n] = '\0';
}

// ---------------------------------------------------------------------------
// Current span per thread (set by the span bridge)

struct SpanCtx {
  uint64_t w[3];  // raw bytes: trace_id[16] then span_id[8]
  bool valid() const { return (w[0] | w[1]) != 0 && w[2] != 0; }
};

thread_local SpanCtx t_span{};
thread_local uint32_t t_tid = 0;

uint32_t current_tid() {
  if (t_tid == 0) t_tid = static_cast<uint32_t>(syscall(SYS_gettid));
  return t_tid;
}

// ---------------------------------------------------------------------------
// Launch table: correlation_id -> (span, tid). Direct-mapped ring, one writer per
// launch (app thread), read by the activity thread. Each slot is a small seqlock:
// the writer clears corr, writes data, then publishes corr; the reader accepts
// the data only if corr is unchanged before and after reading it.

struct LaunchSlot {
  std::atomic<uint32_t> corr;
  std::atomic<uint32_t> tid;
  std::atomic<uint64_t> w[3];
};

LaunchSlot *g_launches = nullptr;

inline void record_launch(uint32_t corr) {
  const SpanCtx s = t_span;
  LaunchSlot &slot = g_launches[corr & (kLaunchSlots - 1)];
  slot.corr.store(0, std::memory_order_relaxed);
  std::atomic_thread_fence(std::memory_order_release);
  slot.tid.store(current_tid(), std::memory_order_relaxed);
  for (int i = 0; i < 3; i++) slot.w[i].store(s.w[i], std::memory_order_relaxed);
  slot.corr.store(corr, std::memory_order_release);
}

bool lookup_launch(uint32_t corr, SpanCtx *span, uint32_t *tid) {
  LaunchSlot &slot = g_launches[corr & (kLaunchSlots - 1)];
  if (slot.corr.load(std::memory_order_acquire) != corr) return false;
  *tid = slot.tid.load(std::memory_order_relaxed);
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
  void str16(const std::string &s) {
    u16(static_cast<uint16_t>(s.size()));
    raw(s.data(), s.size());
  }
};

void put_header(Bytes &out, wire::MessageType type, size_t payload_len) {
  out.u32(wire::kMagic);
  out.u16(wire::kVersion);
  out.u16(type);
  out.u32(static_cast<uint32_t>(payload_len));
  out.u32(0);
}

// ---------------------------------------------------------------------------
// Process metadata (sent in the HELLO message)

std::string read_link(const char *path) {
  char buf[4096];
  ssize_t n = readlink(path, buf, sizeof buf - 1);
  if (n <= 0) return {};
  return std::string(buf, static_cast<size_t>(n));
}

std::string container_id() {
  // Look for a 64-hex-char run in /proc/self/cgroup (docker, containerd, cri-o).
  FILE *f = std::fopen("/proc/self/cgroup", "r");
  if (!f) return {};
  char line[1024];
  std::string found;
  while (found.empty() && std::fgets(line, sizeof line, f)) {
    size_t run = 0;
    for (size_t i = 0; line[i]; i++) {
      char c = line[i];
      bool hex = (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f');
      run = hex ? run + 1 : 0;
      if (run == 64) {
        bool ends = !line[i + 1] || !((line[i + 1] >= '0' && line[i + 1] <= '9') ||
                                      (line[i + 1] >= 'a' && line[i + 1] <= 'f'));
        if (ends) {
          found.assign(line + i - 63, 64);
          break;
        }
      }
    }
  }
  std::fclose(f);
  return found;
}

std::vector<std::pair<std::string, std::string>> process_metadata() {
  std::vector<std::pair<std::string, std::string>> kv;
  kv.emplace_back("warpspan.version", kVersion);
  kv.emplace_back("process.pid", std::to_string(getpid()));
  std::string exe = read_link("/proc/self/exe");
  if (!exe.empty()) {
    kv.emplace_back("process.executable.path", exe);
    size_t slash = exe.rfind('/');
    kv.emplace_back("process.executable.name", slash == std::string::npos ? exe : exe.substr(slash + 1));
  }
  char host[256] = {};
  if (gethostname(host, sizeof host - 1) == 0) kv.emplace_back("host.name", host);
  std::string cid = container_id();
  if (!cid.empty()) kv.emplace_back("container.id", cid);
  uint32_t cupti_version = 0;
  if (cuptiGetVersion(&cupti_version) == CUPTI_SUCCESS)
    kv.emplace_back("cupti.api_version", std::to_string(cupti_version));
  for (const char *name : {"OTEL_SERVICE_NAME", "OTEL_RESOURCE_ATTRIBUTES"}) {
    const char *v = std::getenv(name);
    if (v && *v) kv.emplace_back(std::string("env.") + name, v);
  }
  return kv;
}

// ---------------------------------------------------------------------------
// Global state. Heap-allocated and never freed: CUPTI threads may still run
// while static destructors execute at exit.

struct State {
  // config
  std::string socket_path;
  FILE *debug = nullptr;
  int flush_interval_ms = 1000;

  CUpti_SubscriberHandle subscriber = nullptr;
  std::atomic<bool> disabled{false};
  std::atomic<bool> stopping{false};

  // stats
  std::atomic<uint64_t> kernels{0};
  std::atomic<uint64_t> kernels_linked{0};
  std::atomic<uint64_t> dropped{0};  // CUPTI dropped + not delivered to agent

  std::mutex flush_mu;  // serialises cuptiActivityFlushAll calls

  // Everything below is guarded by out_mu.
  std::mutex out_mu;
  int fd = -1;
  uint64_t last_connect_attempt_ms = 0;
  bool logged_connect_failure = false;
  // current batch
  Bytes strings;
  Bytes records;
  uint32_t string_count = 0;
  uint32_t record_count = 0;
  std::unordered_map<std::string, uint32_t> string_index;
};

State *g = nullptr;

// --- socket (out_mu held) ---------------------------------------------------

void close_socket_locked() {
  if (g->fd >= 0) close(g->fd);
  g->fd = -1;
}

bool send_locked(const Bytes &msg) {
  if (g->fd < 0) return false;
  ssize_t n = send(g->fd, msg.buf.data(), msg.buf.size(), MSG_DONTWAIT | MSG_NOSIGNAL);
  if (n == static_cast<ssize_t>(msg.buf.size())) return true;
  if (n < 0 && (errno == EAGAIN || errno == EWOULDBLOCK)) return false;  // agent busy: drop
  log("lost connection to agent (%s), will reconnect", n < 0 ? std::strerror(errno) : "short write");
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
  if (g->socket_path.size() >= sizeof addr.sun_path) {
    log("socket path too long: %s", g->socket_path.c_str());
    g->socket_path.clear();
    return;
  }
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

  Bytes payload;
  payload.u32(static_cast<uint32_t>(getpid()));
  auto kv = process_metadata();
  payload.u32(static_cast<uint32_t>(kv.size()));
  for (auto &[k, v] : kv) {
    payload.str16(k);
    payload.str16(v.size() > 4096 ? v.substr(0, 4096) : v);
  }
  Bytes msg;
  put_header(msg, wire::kHello, payload.buf.size());
  msg.raw(payload.buf.data(), payload.buf.size());
  if (send_locked(msg)) {
    log("connected to agent at %s", g->socket_path.c_str());
    g->logged_connect_failure = false;
  }
}

// --- kernel batches (out_mu held) -------------------------------------------

void reset_batch_locked() {
  g->strings.buf.clear();
  g->records.buf.clear();
  g->string_count = 0;
  g->record_count = 0;
  g->string_index.clear();
}

void flush_batch_locked() {
  if (g->record_count == 0) return;
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
  reset_batch_locked();
}

void add_kernel_locked(const char *name_ptr, uint64_t start, uint64_t end, uint32_t corr,
                       uint32_t device, uint32_t stream, const SpanCtx &span, uint32_t tid, bool linked) {
  std::string name = name_ptr ? name_ptr : "<unknown kernel>";
  if (name.size() > wire::kMaxStringLen) name.resize(wire::kMaxStringLen);

  auto it = g->string_index.find(name);
  size_t need = wire::kKernelRecordSize + (it == g->string_index.end() ? 2 + name.size() : 0);
  if (wire::kHeaderSize + wire::kKernelsFixedSize + g->strings.buf.size() + g->records.buf.size() + need >
      wire::kMaxMessage) {
    flush_batch_locked();
    it = g->string_index.end();
  }
  uint32_t name_index;
  if (it == g->string_index.end()) {
    name_index = g->string_count++;
    g->strings.str16(name);
    g->string_index.emplace(std::move(name), name_index);
  } else {
    name_index = it->second;
  }

  Bytes &r = g->records;
  r.u64(start);
  r.u64(end);
  r.raw(span.w, sizeof span.w);  // trace_id[16] + span_id[8]
  r.u32(corr);
  r.u32(name_index);
  r.u32(device);
  r.u32(stream);
  r.u32(tid);
  r.u32(linked ? wire::kFlagLinked : 0u);
  g->record_count++;
}

void debug_kernel(const char *name, uint64_t start, uint64_t end, uint32_t corr, uint32_t device,
                  uint32_t stream, const SpanCtx &span, uint32_t tid, bool linked) {
  char trace_hex[33] = "-", span_hex[17] = "-";
  if (linked) {
    uint8_t b[24];
    std::memcpy(b, span.w, sizeof b);
    to_hex(b, 16, trace_hex);
    to_hex(b + 16, 8, span_hex);
  }
  std::fprintf(g->debug,
               "kernel name=%s start_ns=%llu end_ns=%llu duration_ns=%llu correlation_id=%u device=%u "
               "stream=%u tid=%u trace_id=%s span_id=%s\n",
               name ? name : "<unknown kernel>", static_cast<unsigned long long>(start),
               static_cast<unsigned long long>(end), static_cast<unsigned long long>(end - start), corr, device,
               stream, tid, trace_hex, span_hex);
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
  if (cb->callbackSite != CUPTI_API_ENTER) return;
  record_launch(cb->correlationId);
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
    CUpti_Activity *rec = nullptr;
    while (cuptiActivityGetNextRecord(buffer, valid, &rec) == CUPTI_SUCCESS) {
      if (rec->kind != CUPTI_ACTIVITY_KIND_CONCURRENT_KERNEL && rec->kind != CUPTI_ACTIVITY_KIND_KERNEL) continue;
      // Every CUpti_ActivityKernelN (N = 4..13) has the fields we read at the same
      // offsets, so the oldest still-shipped layout works across CUDA 11/12/13.
      const auto *k = reinterpret_cast<const CUpti_ActivityKernel4 *>(rec);
      SpanCtx span{};
      uint32_t tid = 0;
      bool found = lookup_launch(k->correlationId, &span, &tid);
      bool linked = found && span.valid();
      if (!found) span = SpanCtx{};
      g->kernels.fetch_add(1, std::memory_order_relaxed);
      if (linked) g->kernels_linked.fetch_add(1, std::memory_order_relaxed);
      if (g->debug)
        debug_kernel(k->name, k->start, k->end, k->correlationId, k->deviceId, k->streamId, span, tid, linked);
      add_kernel_locked(k->name, k->start, k->end, k->correlationId, k->deviceId, k->streamId, span, tid, linked);
    }
    size_t dropped = 0;
    if (cuptiActivityGetNumDroppedRecords(ctx, stream_id, &dropped) == CUPTI_SUCCESS && dropped > 0) {
      g->dropped.fetch_add(dropped, std::memory_order_relaxed);
    }
    flush_batch_locked();
    if (g->debug) std::fflush(g->debug);
  }
  std::free(buffer);
}

void flush_activity(uint32_t flags) {
  std::lock_guard<std::mutex> lock(g->flush_mu);
  cuptiActivityFlushAll(flags);
}

bool start_cupti() {
  if (!CUPTI_OK(cuptiSubscribe(&g->subscriber, on_api_callback, nullptr))) return false;

  const CUpti_CallbackId runtime_launches[] = {
      CUPTI_RUNTIME_TRACE_CBID_cudaLaunch_v3020,
      CUPTI_RUNTIME_TRACE_CBID_cudaLaunch_ptsz_v7000,
      CUPTI_RUNTIME_TRACE_CBID_cudaLaunchKernel_v7000,
      CUPTI_RUNTIME_TRACE_CBID_cudaLaunchKernel_ptsz_v7000,
      CUPTI_RUNTIME_TRACE_CBID_cudaLaunchCooperativeKernel_v9000,
      CUPTI_RUNTIME_TRACE_CBID_cudaLaunchCooperativeKernel_ptsz_v9000,
      CUPTI_RUNTIME_TRACE_CBID_cudaLaunchCooperativeKernelMultiDevice_v9000,
      CUPTI_RUNTIME_TRACE_CBID_cudaGraphLaunch_v10000,
      CUPTI_RUNTIME_TRACE_CBID_cudaGraphLaunch_ptsz_v10000,
#if CUDA_VERSION >= 11060
      CUPTI_RUNTIME_TRACE_CBID_cudaLaunchKernelExC_v11060,
      CUPTI_RUNTIME_TRACE_CBID_cudaLaunchKernelExC_ptsz_v11060,
#endif
  };
  const CUpti_CallbackId driver_launches[] = {
      CUPTI_DRIVER_TRACE_CBID_cuLaunch,
      CUPTI_DRIVER_TRACE_CBID_cuLaunchGrid,
      CUPTI_DRIVER_TRACE_CBID_cuLaunchGridAsync,
      CUPTI_DRIVER_TRACE_CBID_cuLaunchKernel,
      CUPTI_DRIVER_TRACE_CBID_cuLaunchKernel_ptsz,
      CUPTI_DRIVER_TRACE_CBID_cuLaunchCooperativeKernel,
      CUPTI_DRIVER_TRACE_CBID_cuLaunchCooperativeKernel_ptsz,
      CUPTI_DRIVER_TRACE_CBID_cuLaunchCooperativeKernelMultiDevice,
      CUPTI_DRIVER_TRACE_CBID_cuGraphLaunch,
      CUPTI_DRIVER_TRACE_CBID_cuGraphLaunch_ptsz,
#if CUDA_VERSION >= 12000
      CUPTI_DRIVER_TRACE_CBID_cuLaunchKernelEx,
      CUPTI_DRIVER_TRACE_CBID_cuLaunchKernelEx_ptsz,
#endif
  };
  for (CUpti_CallbackId id : runtime_launches)
    CUPTI_OK(cuptiEnableCallback(1, g->subscriber, CUPTI_CB_DOMAIN_RUNTIME_API, id));
  for (CUpti_CallbackId id : driver_launches)
    CUPTI_OK(cuptiEnableCallback(1, g->subscriber, CUPTI_CB_DOMAIN_DRIVER_API, id));

  if (!CUPTI_OK(cuptiActivityRegisterCallbacks(on_buffer_requested, on_buffer_completed)) ||
      !CUPTI_OK(cuptiActivityEnable(CUPTI_ACTIVITY_KIND_CONCURRENT_KERNEL))) {
    cuptiUnsubscribe(g->subscriber);
    return false;
  }
  return true;
}

// ---------------------------------------------------------------------------
// Background thread, exit and fork handling

void *flush_thread_main(void *) {
  while (!g->stopping.load(std::memory_order_relaxed) && !g->disabled.load(std::memory_order_relaxed)) {
    {
      std::lock_guard<std::mutex> lock(g->out_mu);
      connect_locked();
    }
    flush_activity(0);
    timespec ts{g->flush_interval_ms / 1000, static_cast<long>(g->flush_interval_ms % 1000) * 1000000L};
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
  if (rc != 0) {
    log("could not start background thread: %s", std::strerror(rc));
    return false;
  }
  pthread_setname_np(thread, "warpspan");
  pthread_detach(thread);
  return true;
}

void on_exit() {
  if (!g || g->disabled.load()) return;
  g->stopping.store(true);
  {
    std::lock_guard<std::mutex> lock(g->out_mu);
    connect_locked();  // short-lived processes may exit before the first flush tick
  }
  flush_activity(CUPTI_ACTIVITY_FLAG_FLUSH_FORCED);
  log("exiting: %llu kernels captured, %llu linked to spans, %llu dropped",
      static_cast<unsigned long long>(g->kernels.load()),
      static_cast<unsigned long long>(g->kernels_linked.load()),
      static_cast<unsigned long long>(g->dropped.load()));
}

void on_fork_child() {
  // The background thread does not exist in the child and CUDA cannot be used
  // after fork anyway; stop doing anything and don't share the parent's socket.
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
  if (!trace_id || !span_id) {
    t_span = SpanCtx{};
    return;
  }
  SpanCtx s;
  std::memcpy(&s.w[0], trace_id, 16);
  std::memcpy(&s.w[2], span_id, 8);
  t_span = s.valid() ? s : SpanCtx{};
}

WARPSPAN_EXPORT int warpspan_get_span(uint8_t *trace_id, uint8_t *span_id) {
  const SpanCtx s = t_span;
  if (trace_id) std::memcpy(trace_id, &s.w[0], 16);
  if (span_id) std::memcpy(span_id, &s.w[2], 8);
  return s.valid() ? 1 : 0;
}

// Called by the CUDA driver. Return 1 on success, 0 on failure. We always return
// 1: if profiling can't start, the app must keep running unaffected.
WARPSPAN_EXPORT int InitializeInjection(void) {
  static std::atomic<bool> initialized{false};
  if (initialized.exchange(true)) return 1;

  g_quiet = env_true("WARPSPAN_QUIET");
  if (env_true("WARPSPAN_DISABLE")) {
    log("disabled by WARPSPAN_DISABLE");
    return 1;
  }

  g = new State();
  const char *sock = std::getenv("WARPSPAN_SOCKET");
  g->socket_path = sock ? sock : kDefaultSocket;  // WARPSPAN_SOCKET="" disables sending
  if (const char *ms = std::getenv("WARPSPAN_FLUSH_INTERVAL_MS")) {
    int v = std::atoi(ms);
    if (v >= 10) g->flush_interval_ms = v;
  }
  if (const char *path = std::getenv("WARPSPAN_DEBUG_FILE")) {
    g->debug = std::strcmp(path, "-") == 0 ? stderr : std::fopen(path, "a");
    if (!g->debug) log("cannot open WARPSPAN_DEBUG_FILE %s: %s", path, std::strerror(errno));
  }

  g_launches = new LaunchSlot[kLaunchSlots]();

  if (!start_cupti()) {
    log("CUPTI setup failed; profiling disabled for pid %d", static_cast<int>(getpid()));
    g->disabled.store(true);
    return 1;
  }
  pthread_atfork(nullptr, nullptr, on_fork_child);
  std::atexit(on_exit);
  start_flush_thread();

  log("loaded (pid %d, agent socket %s)", static_cast<int>(getpid()),
      g->socket_path.empty() ? "<disabled>" : g->socket_path.c_str());
  return 1;
}
