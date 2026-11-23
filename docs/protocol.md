# Capture library → agent protocol (v1)

`libwarpspan.so` sends data to the node agent over a **unix `SOCK_SEQPACKET` socket**
(default `/var/run/warpspan/warpspan.sock`, override with `WARPSPAN_SOCKET`).

- SEQPACKET keeps message boundaries: one `send()` is one message, delivered whole or not at all.
- The library sends with `MSG_DONTWAIT | MSG_NOSIGNAL`. If the agent is slow, the message is
  dropped and counted. It never blocks the app or kills it with `SIGPIPE`.
- One connection per process. A closed connection means the process exited.
- All integers are little-endian. Strings are `u16 length` + bytes (no terminator).
- Messages are at most 60 KiB.

Source of truth: `capture/protocol.h` (C++) and `agent/wire` (Go). Change both together
and bump the version.

## Header (16 bytes, every message)

| offset | type | field |
|---|---|---|
| 0 | u32 | magic `0x4E505357` (`"WSPN"`) |
| 4 | u16 | version (`1`) |
| 6 | u16 | type: `1` = Hello, `2` = Kernels |
| 8 | u32 | payload length (bytes after the header) |
| 12 | u32 | reserved (0) |

## Hello (type 1)

Sent once, right after connecting.

```
u32 pid
u32 count
count × (str16 key, str16 value)
```

Keys sent today: `warpspan.version`, `process.pid`, `process.executable.path`,
`process.executable.name`, `host.name`, `container.id` (if found in `/proc/self/cgroup`),
`cupti.api_version`, `env.OTEL_SERVICE_NAME`, `env.OTEL_RESOURCE_ATTRIBUTES` (if set).
The agent derives the OTel resource (`service.name` etc.) from these.

## Kernels (type 2)

A batch of completed kernel executions. Kernel names are interned per message.

```
u64 dropped_total          running total of records not delivered (CUPTI overflow + agent busy)
u32 string_count
u32 record_count
string_count × str16       kernel names (mangled), index = position
record_count × record      64 bytes each:
```

| offset | type | field |
|---|---|---|
| 0 | u64 | GPU start, ns (CUPTI timestamp, Unix epoch) |
| 8 | u64 | GPU end, ns |
| 16 | u8[16] | trace_id (W3C byte order) |
| 32 | u8[8] | span_id |
| 40 | u32 | CUPTI correlation id |
| 44 | u32 | name index into the string table |
| 48 | u32 | device id |
| 52 | u32 | stream id |
| 56 | u32 | OS thread id that launched the kernel (0 if unknown) |
| 60 | u32 | flags: bit 0 = trace_id/span_id valid |
