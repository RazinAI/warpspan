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
constexpr uint16_t kVersion = 1;

enum MessageType : uint16_t {
  kHello = 1,    // process metadata, sent once per connection
  kKernels = 2,  // batch of kernel executions
};

// Every message (one SOCK_SEQPACKET datagram):
//   u32 magic | u16 version | u16 type | u32 payload_len | u32 reserved
constexpr size_t kHeaderSize = 16;

// Keep messages well below the default unix socket buffer (~208 KiB).
constexpr size_t kMaxMessage = 60 * 1024;

// Kernels payload:
//   u64 dropped_total | u32 string_count | u32 record_count
//   string_count x (u16 len | bytes)
//   record_count x KernelRecord (kKernelRecordSize bytes, layout below)
constexpr size_t kKernelsFixedSize = 16;

// KernelRecord:
//   u64 start_ns | u64 end_ns | u8 trace_id[16] | u8 span_id[8]
//   u32 correlation_id | u32 name_index | u32 device_id | u32 stream_id
//   u32 thread_id | u32 flags
constexpr size_t kKernelRecordSize = 64;
constexpr uint32_t kFlagLinked = 1u << 0;  // trace_id/span_id are valid

constexpr size_t kMaxStringLen = 2048;  // longer kernel names are truncated

}  // namespace warpspan::wire
