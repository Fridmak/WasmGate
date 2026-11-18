# WasmGate ABI v4 wire contract

The guest has one call at a time per instance. `i32` pointers are byte offsets
in Wasm linear memory. Use [`wg_v4_plugin!`](../../plugins/examples/common/src/v4.rs)
for a Rust event guest, or `wg_v4_metadata!` for lifecycle metadata.

## Exports and capabilities

| Export | Signature | Purpose |
|---|---|---|
| `wg_abi` | `() -> i32` | `0x0400`; high byte is the ABI major |
| `wg_caps` | `() -> i64` | Capability bits below |
| `wg_buf_ptr`, `wg_buf_len` | `() -> i32` | IN window location and capacity for event guests |
| `wg_out_ptr`, `wg_out_len` | `() -> i32` | Separate OUT window for event guests declaring OUT capabilities |
| `on_event` | `(i64, i32, i32, i32, i32) -> i64` | `conn_id, phase, flags, in_ptr, in_len` |
| `on_accept` | `(i64, i64, i32) -> i32` | `ip_hi, ip_lo, port`; nonzero denies |
| `on_close` | `(i64, i64, i64, i64) -> ()` | `ip_hi, ip_lo, rx_bytes, tx_bytes` |
| `on_close_ex` | `(i64, i64, i64, i64, i64) -> ()` | `conn_id, ip_hi, ip_lo, rx_bytes, tx_bytes`; preferred over `on_close` |

IP words are big-endian and IPv4 is encoded as `::ffff:a.b.c.d`. Events use
the low 32 bits of the return value: `0` allow, `1` deny, `2 + index` route
before dial in `once` or request headers. Negative and out-of-range values
are invalid. Route and deny verdicts never apply OUT effects.

| Bit | Capability | Use |
|---|---|---|
| 0 | `CapBlob` | All events |
| 1 | `CapHeaderIndex` | HTTP header phases |
| 2 | `CapStartLine` | HTTP header phases |
| 3 | `CapConnInfo` | Optional connection metadata; not emitted by the current proxy |
| 4 | `CapStateIn` | Accept state in IN |
| 5 | `CapStateOut` | Produce next state in OUT |
| 6 | `CapOpaque` | Opaque stream phase |
| 7 | `CapMutation` | Produce a replacement in OUT; the listener must also opt in |
| 8 | `CapLifecycle` | Lifecycle plan with at least one lifecycle hook |

Body phases do not require state or mutation. With the default engine, reserve
at least 44 KiB at `wg_buf_ptr` (first 40 KiB used for IN); an OUT-capable guest
also needs 4 KiB at `wg_out_ptr` that does not overlap the active IN region.
The engine clears OUT before a call. All zero bytes or a valid zero-section
OUT means no effects. A valid section can be explicitly present with length
zero. The proxy stores state only for guests that opt into both state bits.
An omitted state leaves it unchanged; an empty state clears its bytes but
remains present in the next IN. Proxy replacements use chunks up to 2 KiB;
the state slot then allows 1,984 bytes so both effects can fit in one OUT.

## Phases and flags

| Phase | ID |
|---|---|
| Request headers | 1 |
| Request body | 2 |
| Response headers | 3 |
| Response body | 4 |
| Opaque stream | 5 |

Flags: `First = 1`, `Last = 2`, `Truncated = 4`, `ClientToServer = 8`,
`ServerToClient = 16`. A complete head carries `First | Last`. A body event
carries `First` on the first block and `Last` on the final block. No body
event is emitted for a zero-length body. A short transport read can emit a
terminal `Last | Truncated` event with an empty blob and `First` if no body
event preceded it. Truncated events never commit effects or forward their
bytes. Direction flags are for opaque events; both bits cannot be set.

## Envelope layout

All integers are little-endian and offsets are from the envelope base, not
the blob. The 24-byte header has `magic:u32, version:u16=1, header_len:u16=24,
total_len:u32, section_count:u16, stride:u16=16, arena_off:u32, arena_len:u32`.
Magic is numeric `0x57473445` for IN and `0x5747344f` for OUT. `total_len` and
`arena_off` are 8-byte aligned. The table follows the header; at most 8
sections are allowed. Each entry is `type:u16, reserved:u16=0, offset:u32,
length:u32, count:u32`. Types are strictly increasing. Section offsets are
4-byte aligned and lie past the table; nonempty sections do not overlap. The
header index is capped at 512 entries by the engine.

| ID | Section | Location |
|---|---|---|
| 1 | Blob (required in IN) | Starts at `arena_off` |
| 2 | Header index | Outside the arena, after it |
| 3 | Start line | Outside the arena, after it |
| 4 | Connection info | Outside the arena, after it |
| 5 | State | In the arena, after the blob in IN; optional in OUT |
| 6 | Replacement | OUT arena only |

`count` is zero on known sections other than the header index. Each index
entry is `name_off:u32, value_off:u32, name_len:u16, value_len:u16, aux:u32`;
the host emits `aux=0`. Name and value lie inside the blob and duplicate
names remain separate. Start line is 24 bytes: three `u32` offsets followed
by three `u16` lengths, then target form (`u8`), flags (`u8`), and four reserved
bytes. Request fields are method, target, version; response fields at the
same positions are reason phrase, three-digit status, version.

Connection info is an optional [80-byte struct](../../internal/plugin/abi4/envelope/envelope.go)
whose SNI and ALPN offsets, if nonempty, point into the blob. Unknown IN
sections may be skipped after structural validation. Unknown OUT sections
are rejected. OUT is copied to host memory before the instance is reused.
