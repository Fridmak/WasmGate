# Plugins (WASM ABI v3)

Plugins are WebAssembly modules executed by the Go host through `wazero`. Every
plugin runs in a pure sandbox: no WASI, no filesystem, no sockets, no clocks and
no randomness. A module is a deterministic function of the connection metadata it
receives.

> **ABI status:** `data_mode: headers` is now a strict ABI v4 path. It requires
> a module with `on_event`/`wg_*`; legacy ABI v3 `on_headers` modules are
> rejected at snapshot load. `once` and `stream` still use the legacy ABI v3
> runtime until their v4 phases are implemented.

Nine reference plugins live in [`plugins/examples/`](../../plugins/examples/); their
`.wasm` files are committed, so Rust is not required to run them.

| Plugin | Demonstrates | Mode | Verdict |
|---|---|---|---|
| `ipguard` | `on_accept`: L4 allow/deny before `dial` | L4 | `0` allow, `≠0` deny |
| `http_guard` | `on_data_ex`: methods/paths, Basic auth, CL+TE smuggling, header masking | `stream` | `0` / `1` |
| `http_headers_guard` | legacy ABI v3 `on_headers`; not accepted in `headers` mode until ported to v4 | legacy | `0` / `1` |
| `http_body_guard` | `on_data_state` (ABI v3): split headers, `Content-Length`, cross-chunk body signatures, keep-alive counters | `stream` | `0` / `1` |
| `path_router` | `route_pools` by `Host` and path | `once` | `2..4` → pools |
| `sni_guard` | SNI from ClientHello without terminating TLS | `once` | `2..4` → pools |
| `protocol_router` | Detect TLS/HTTP/SSH/PostgreSQL/Redis from the first bytes | `once` | `2..6` → pools |
| `redis_guard` | RESP and inline parsing, ACL for `CONFIG`/`FLUSHALL`/`EVAL` | `stream` | `0` / `1` |
| `stream_guard` | Attack signatures in the request body, in-place JSON secret masking | `stream` | `0` / `1` |

Full showcase: [`configs/showcase.yaml`](../../configs/showcase.yaml).

## Minimal configuration

```yaml
listeners:
  - name: edge
    addr: "127.0.0.1:8080"
    upstream_pool: web
    plugin:
      path: "plugins/examples/ipguard/ipguard.wasm"
      max_memory_pages: 16    # 16 * 64 KiB = 1 MiB per instance
      timeout: 1s             # per-hook timeout; 0 selects the safe 1s default
      data_mode: "once"       # "once" (ABI v3), "headers" (ABI v4), "stream" (ABI v3)
      max_header_bytes: 16384 # headers: max request header block
      inspect_timeout: 3s     # headers: total deadline for the block
      route_pools: ["canary"] # on_event/on_data >= 2 -> route_pools[ret-2]
```

## Hook contract

The host passes connection metadata through a `uint64` stack (`wazero`
`CallWithStack`), without allocating strings or copying into linear memory.

| Export | Signature | Arguments | Result |
|---|---|---|---|
| `on_event` | `(i64, i32, i32, i32, i32) -> i64` | ABI v4: `conn_id`, `phase`, `flags`, `ptr`, `len` into the envelope | Low 32 bits: `0` allow, `1` deny, `>= 2` route; high 32 bits reserved |
| `on_accept` | `(i64, i64, i32) -> i32` | `ip_hi`, `ip_lo` (16-byte canonical IP, big-endian; IPv4 is `::ffff:a.b.c.d`), client port | `0` allow, any nonzero denies before `dial` |
| `on_headers` | `(i64, i32, i32) -> i32` | legacy ABI v3 only; rejected in `data_mode: headers` | `0` / `1` |
| `on_data` | `(i32, i32) -> i32` | `ptr`, `len` in linear memory (**`on_data_ptr() -> i32` is required**); up to 32 KiB, may mutate in place | `0` allow, `1` deny, `>= 2` → `route_pools[ret-2]` (`once` only) |
| `on_data_ex` | `(i64, i32, i32, i32) -> i32` | `conn_id`, `dir` (0 client→server, 1 server→client), `ptr`, `len` | Same as `on_data` |
| `on_data_state` | `(i64, i32, i32, i32, i32, i32, i32) -> i64` | `conn_id`, `dir`, `state_ptr`, `state_len`, `state_cap` (4 KiB), `data_ptr`, `data_len`; requires `on_data_ptr()` and `on_state_ptr()` | Low 32 bits: verdict; high 32 bits: new state length (`0..4096`) |
| `on_close` | `(i64, i64, i64, i64) -> ()` | `ip_hi`, `ip_lo`, `rx_bytes`, `tx_bytes` | — |
| `on_close_ex` | `(i64, i64, i64, i64, i64) -> ()` | `conn_id`, `ip_hi`, `ip_lo`, `rx_bytes`, `tx_bytes` | — |

Negative verdicts and `INT32_MIN` deny; only `0` allows. A route index outside
`route_pools` denies instead of falling back. A module that exports no hook, or
whose signature does not match, fails validation before the snapshot is swapped.

## Sandbox limits

- **No WASI or syscalls.** Filesystem, sockets, environment, clocks and randomness
  are unavailable.
- **Memory cap.** Default 16 pages (1 MiB) per instance. A module requesting more
  `min` memory is rejected at load; `memory.grow` beyond the cap returns `-1`.
- **Reactor model.** Instances live in a `sync.Pool` and are reused, so global
  WASM state is not reset per connection and is not shared between instances.
- **Timeout.** Every hook call has a deadline (`plugin.timeout`, default `1s`
  when omitted); a hung hook is interrupted, the connection is denied and the
  instance is destroyed. Stack loading and instantiation are bounded separately
  by a 5s load deadline.

## Data modes and TLS

- `data_mode` is **required** for any module exporting `on_data`, `on_data_ex`,
  `on_data_state` or `on_event`. `once` inspects the first client read (up to
  16 389 bytes) before `dial` and then resumes `splice(2)`; it remains ABI v3.
  It is a routing/mutation hint, not a full filter: use `stream` for filtering.
- `data_mode: headers` is an ABI v4 `on_event` path. It gathers one complete
  HTTP/1.x request header block (through `\r\n\r\n`) before `dial`, calls the
  plugin once, and then resumes `splice(2)` for the body. The module must export
  `on_event`, `wg_abi`, `wg_caps`, `wg_buf_ptr` and `wg_buf_len`; legacy ABI v3
  `on_headers` modules are rejected. It is intended for connection-level
  routing/auth/header policy, not for per-request or body filtering. Only the
  first request of a keep-alive connection is inspected. The header index is
  capped at 512 entries; a request with more fails closed. The host rejects
  incomplete, oversized, timed-out, malformed and HTTP/2-preface input before
  dialing. `plugin.timeout` must be greater than zero, and `sni_routes` without
  TLS termination is rejected.
- `data_mode: stream` runs the legacy ABI v3 hook for every chunk in both
  directions (with `on_data_ex`), disables `splice`, and does not apply
  `route_pools`.
- TLS termination decrypts the stream before `on_data`; TLS passthrough keeps it
  encrypted, so the plugin sees `ClientHello`/records. `sni_routes` is ignored on
  a listener with termination.

## ABI v3 state

`on_data_state` gives each direction of a connection a bounded 4 KiB state buffer,
managed by the host and copied in/out around the call. It exists for split
headers, keep-alive framing and body inspection, where stateless hooks are not
enough. The reference implementation is
[`http_body_guard`](../../plugins/examples/http_body_guard).

## Building a plugin

Rust (`#![no_std]`, recommended):

```bash
cd plugins/examples
rustup target add wasm32-unknown-unknown
cargo build --release --target wasm32-unknown-unknown
cp target/wasm32-unknown-unknown/release/ipguard.wasm ipguard/ipguard.wasm
```

TinyGo:

```bash
cd plugins/examples/ipguard/tinygo
tinygo build -o ../ipguard.wasm -target=wasm-unknown -no-debug .
```

Validate the result together with the configuration:

```bash
wasmgate -config configs/showcase.yaml -validate
```
