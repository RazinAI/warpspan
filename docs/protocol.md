# Capture library → agent protocol (v1)

`libwarpspan.so` sends data to the node agent over a **unix `SOCK_SEQPACKET` socket**
(default `/var/run/warpspan/warpspan.sock`, override with `WARPSPAN_SOCKET`).

- SEQPACKET keeps message boundaries: one `send()` is one message, delivered whole or not at all.
- The library sends with `MSG_DONTWAIT | MSG_NOSIGNAL`. When the socket is full it polls up to
  50 ms for the agent to drain it (on CUPTI's worker thread or the library's flush thread, never
  on a launching app thread), then drops the message and counts it. It never kills the app with
  `SIGPIPE`.
- One connection per process. A closed connection means the process exited.
- All integers are little-endian. Strings are `u16 length` + bytes (no terminator).
- Messages are at most 60 KiB; the agent rejects larger ones.

Source of truth: `capture/protocol.h` (C++) and `agent/wire` (Go). Change both together.

## Header (16 bytes, every message)

| offset | type | field |
|---|---|---|
| 0 | u32 | magic `0x4E505357` (`"WSPN"`) |
| 4 | u16 | version (`2`; the agent also accepts `1`, from older libraries) |
| 6 | u16 | type: `1` = Hello, `2` = Kernels |
| 8 | u32 | payload length (bytes after the header) |
| 12 | u32 | pid of the sending process (in its own pid namespace) |

## Hello (type 1)

Sent once, right after connecting. The agent closes connections that send kernels first.

```
u32 count
count × (str16 key, str16 value)
```

Keys: `process.executable.name`, `host.name`, and, if set in the app's
environment at startup, `env.OTEL_SERVICE_NAME` and `env.OTEL_RESOURCE_ATTRIBUTES`.
The agent derives the OTel resource (`service.name` etc.) from these.

## Kernels (type 2)

A batch of completed kernel executions. Kernel names (mangled, at most 2048
bytes; the library truncates longer ones) and NVTX labels (at most 256 bytes)
share one string table; strings and label paths are interned per message. A
message with bytes after the last record is rejected.

```
u64 dropped_total     process lifetime total of records lost (CUPTI overflow, agent busy)
u32 string_count
u32 path_count
u32 record_count
string_count × str16
path_count × (u16 depth, depth × u32 string_index)   NVTX ranges open at launch, outermost first (depth 1..32)
record_count × 52-byte record:
```

| offset | type | field |
|---|---|---|
| 0 | u64 | GPU start, ns |
| 8 | u64 | GPU end, ns |
| 16 | u8[16] | trace_id (W3C byte order), zero if not linked |
| 32 | u8[8] | span_id, zero if not linked |
| 40 | u32 | name index into the string table |
| 44 | u32 | CUDA device id |
| 48 | u32 | path index, `0xFFFFFFFF` if launched outside any NVTX range |

Version 1 (libraries before NVTX labels) has no `path_count`, no paths, and
48-byte records without the path index. The agent reads both.

Timestamps are CUPTI's. On the test machine (CUDA 13.4, Linux) they matched
Unix-epoch nanoseconds, but this has not been verified across driver versions.

## Trust model

The socket is world-writable so that apps running as any user can connect. Any
local process (or any pod mounting the socket directory) can therefore send
records under any `service.name` or trace ID. The agent validates message
structure and sizes and limits what a client can make it hold: kernel names up
to 2048 bytes, a cap on samples per process per interval, at most 1024
connections. It does not check where the data came from. Treat the profiles as
being as trustworthy as the workloads on the node.
