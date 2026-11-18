# Migrating plugins to ABI v4

ABI v4 replaced the v1–v3 hook surface. The v1–v3 runtimes were removed: the
loader rejects legacy exports and there is no fallback. A v1–v3 guest must be
rebuilt against the v4 contract, not just renamed or repackaged.

The loader does not infer an ABI version from hook names. A module that still
exports `on_headers`, `on_data`, `on_data_ex` or `on_data_state` fails to load.
v1/v2 modules are legacy as well. The mapping below covers the hook names the
current loader rejects; confirm the actual exports before starting.

The wire format is in the [ABI v4 contract](plugin-abi-v4.md). This guide covers
the migration steps an existing plugin author needs.

## Hook mapping

| Legacy (v1–v3) | ABI v4 | Notes |
|---|---|---|
| `on_accept(i64, i64, i32) -> i32` | same signature plus `wg_abi`, `wg_caps` | Return `0` to allow; any nonzero denies. L4 modules declare `CapLifecycle`. |
| `on_close(i64, i64, i64, i64)` | same signature, or `on_close_ex` | `on_close_ex` adds `conn_id`. After `on_accept` allows a connection, the close hook runs exactly once, including TLS, header, routing and dial failures. |
| `on_close_ex(i64, i64, i64, i64, i64)` | same signature | Preferred when the guest needs `conn_id`. |
| `on_data(i32, i32) -> i32` | `on_event` with `PhaseOpaqueStream` (5) | Read the IN envelope at the supplied pointer. Opaque events are read-only. |
| `on_data_ex(i64, i32, i32, i32) -> i32` | `on_event` with `PhaseOpaqueStream` and a direction flag | `FlagClientToServer` is `8`, `FlagServerToClient` is `16`. Both bits cannot be set. |
| `on_headers(i64, i32, i32) -> i32` | `on_event` with `PhaseRequestHeaders` (1) | The IN envelope carries the blob, header index and start line. Response headers use `PhaseResponseHeaders` (3) when `body_policy` selects them. |
| `on_data_state(...)` | no direct replacement | v4 has no stream state. State exists only for request/response header and body phases selected by `body_policy`, with `CapStateIn` and `CapStateOut`. |
| `on_data_ptr()`, `on_state_ptr()` | `wg_buf_ptr`, `wg_buf_len`, `wg_out_ptr`, `wg_out_len` | The host reads the IN window; the guest writes effects to the separate OUT window. There is no state pointer. |

`on_event` has the signature `(i64, i32, i32, i32, i32) -> i64`:
`conn_id, phase, flags, in_ptr, in_len`. The low 32 bits of the result are the
verdict: `0` allow, `1` deny, `2 + index` selects `route_pools[index]`. Negative
and out-of-range values are invalid and fail closed. Routing is available only
for `once` and `headers`.

## Configuration mapping

| Legacy setting | ABI v4 |
|---|---|
| L4 module without `data_mode` | Still supported. The module must export `wg_abi`, `wg_caps` and at least one of `on_accept`, `on_close`, `on_close_ex`, and declare `CapLifecycle`. |
| `data_mode: once` | Same mode, but the guest must implement the v4 `on_event` opaque phase. |
| `data_mode: headers` | Same mode, but `on_headers` is rejected. `timeout` must be greater than zero. |
| `data_mode: stream` | Same mode, read-only. `route_pools` is rejected. |
| `timeout: 0` | Selects the `1s` hook deadline. The timer cannot be disabled. |
| `max_memory_pages: 16` | The default is `17` pages (1.06 MiB). A module may still request fewer. |
| `max_instances` | Default `64`, hard cap `1024`, per listener. |
| `route_pools` | Supported for `once` and `headers`; rejected for `stream`. |
| `body_policy` | `request`, `response` or `both`. Requires `data_mode: headers`. |
| `max_body_bytes` | Default `1 MiB`; bounds the declared `Content-Length` before the body is read. |
| `mutation_policy` | `none` by default. `replace` requires `body_policy` and a guest with `CapMutation`. |
| `inspect_timeout` | Default `3s` for one header block. |
| `max_header_bytes` | Default `16 KiB`, hard cap `16389`; the header index accepts at most 512 fields. |

`headers` with `sni_routes` and without TLS termination is rejected: the proxy
cannot inspect plaintext HTTP through an encrypted passthrough stream.

## State and replacements

v4 effects travel in the OUT envelope. The guest must export both
`wg_out_ptr` and `wg_out_len` when it declares `CapStateOut` or `CapMutation`.

- State is optional. The proxy keeps it only for guests that declare both state
  capabilities, and only for the header/body phases selected by `body_policy`.
  Omitted state leaves the previous value unchanged; an explicitly empty state
  clears the bytes but remains present.
- Replacements are limited to inspected body chunks and require both
  `mutation_policy: replace` and `CapMutation`. The replacement must have exactly
  the same length as the input chunk. Header mutation is not supported.
- Proxy replacements use chunks up to 2 KiB. With mutation enabled, the state
  slot allows 1,984 bytes so both effects fit in the default 4 KiB OUT window.
- An omitted or truncated result does not commit state or forward replacement
  bytes. A deny or an invalid effect closes the connection.

The default IN window uses the first 40 KiB of the guest buffer; the guest must
declare at least 44 KiB at `wg_buf_ptr` and 4 KiB at `wg_out_ptr`, with no
overlap between the active IN region and OUT.

## Features that are intentionally absent

A legacy deployment that depends on these behaviours needs a guest or
configuration redesign:

- mutation of opaque `stream` chunks;
- persistent state between arbitrary stream chunks, or state across keep-alive
  HTTP exchanges;
- header mutation;
- fallback from `on_headers` to `on_data`;
- HTTP/1 upgrades, `CONNECT`, `Expect`, informational responses,
  `Transfer-Encoding` (including chunked), close-delimited bodies and ambiguous
  lengths when `body_policy` is enabled;
- HTTP/2 inspection.

`body_policy` also limits the connection to one HTTP/1 exchange. Later
keep-alive messages are not forwarded.

## Minimal Rust guest

The reference SDK is `plugins/examples/common`. An event guest exports the v4
surface through `wg_v4_plugin!`:

```rust
#![no_std]

use wg_common::{self as wg, v4};

wg::wg_v4_plugin!(v4::CAP_HTTP_HEADERS, inspect);

fn inspect(call: v4::Call, event: v4::Event<'_>, _out: &mut v4::Output<'_>) -> i32 {
    if call.phase != v4::Phase::RequestHeaders {
        return v4::DENY;
    }
    // Inspect event.request_line() and event.headers(); return ALLOW or DENY.
    v4::ALLOW
}
```

A lifecycle-only guest uses `wg_v4_metadata!(v4::CAP_LIFECYCLE)` and exports
`on_accept`/`on_close` or `on_close_ex` directly, as in
[`ipguard`](../../plugins/examples/ipguard/src/lib.rs). The macro adds
`CapBlob`; lifecycle metadata does not.

## Build and validate

Run Cargo from `plugins/examples`; Cargo reads `.cargo/config.toml` only from
the working directory, and that file pins the 16 KiB wasm stack used by the
committed guests.

```bash
cd plugins/examples
cargo test --locked -p wg-common
cargo build --locked --workspace --release --target wasm32-unknown-unknown
```

Validate the rebuilt module in a real configuration:

```bash
wasmgate -config configs/showcase.yaml -validate
```

`-validate` compiles every referenced module through the ABI v4 loader and
rejects legacy exports, wrong signatures, missing capabilities and invalid
windows. A module that passes validation still needs a request-level test: the
loader cannot check policy logic.

## Rollout checklist

1. Inventory every listener with a plugin and its `data_mode`.
2. Rebuild each v1–v3 guest against the v4 SDK. Do not keep legacy exports as a
   fallback: the loader rejects them.
3. Move stream mutation or state to `headers` + `body_policy`, or remove the
   behaviour if the protocol cannot be framed with `Content-Length`.
4. Set `timeout` explicitly for `headers` listeners and review
   `max_body_bytes`, `max_header_bytes` and `mutation_policy`.
5. Run `-validate` on the target configuration.
6. Deploy one listener or environment at a time and watch
   `wasmgate_plugin_header_incomplete_total` and
   `wasmgate_plugin_instances_discarded_total`.
7. Keep the previous release available for rollback.

See the [plugin guide](plugins.md) for inspection modes and limits, and the
[ABI v4 contract](plugin-abi-v4.md) for the wire layout.
