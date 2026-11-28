// Wire format between libwarpspan.so and the agent. See docs/protocol.md.
// Must stay in sync with agent/wire.
#pragma once

#include <cstddef>
#include <cstdint>

#if !defined(__BYTE_ORDER__) || __BYTE_ORDER__ != __ORDER_LITTLE_ENDIAN__
#error "warpspan wire format is little-endian; big-endian hosts are not supported"
#endif

namespace warpspan::wire {

constexpr uint32_t kMagic = 0x4E505357;  // bytes "WSPN"
constexpr uint16_t kVersion = 2;         // the agent also accepts version 1 (no NVTX labels)

enum MessageType : uint16_t {
  kHello = 1,    // process metadata, sent once per connection
  kKernels = 2,  // batch of kernel executions
};

// Every message (one SOCK_SEQPACKET datagram):
//   u32 magic | u16 version | u16 type | u32 payload_len | u32 pid
constexpr size_t kHeaderSize = 16;

// Keep messages well below the default unix socket buffer (~208 KiB).
constexpr size_t kMaxMessage = 60 * 1024;

// Kernels payload (version 2):
//   u64 dropped_total | u32 string_count | u32 path_count | u32 record_count
//   string_count x (u16 len | bytes)              kernel names and NVTX labels
//   path_count x (u16 depth | depth x u32 string_index)   NVTX ranges, outermost first
//   record_count x record:
//     u64 start_ns | u64 end_ns | u8 trace_id[16] | u8 span_id[8] | u32 name_index | u32 device_id
//     | u32 path_index (kNoPath: launched outside any NVTX range)
// A record is linked to a span iff span_id is non-zero. Strings and paths are
// interned per message.
constexpr size_t kKernelsFixedSize = 20;
constexpr size_t kKernelRecordSize = 52;
constexpr uint32_t kNoPath = 0xFFFFFFFFu;

constexpr size_t kMaxStringLen = 2048;  // longer kernel names are truncated
constexpr size_t kMaxLabelLen = 256;    // longer NVTX messages are truncated
constexpr int kMaxNvtxDepth = 32;       // deeper NVTX ranges are not recorded

}  // namespace warpspan::wire
