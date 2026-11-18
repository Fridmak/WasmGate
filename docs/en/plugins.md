# Plugins (ABI v4)

WasmGate runs L4 lifecycle and L7 plugins through `wazero`. All nine prebuilt
[reference guests](../../plugins/examples/README.md) use ABI v4. The loader
rejects legacy modules; there is no v3 runtime or fallback. For exports,
capability bits, and wire layout, see the [ABI v4 contract](plugin-abi-v4.md).

## Choose an inspection mode

| `data_mode` | Dispatch | Routing |
|---|---|---|
| omitted | L4 `on_accept`, `on_close` or `on_close_ex` | No |
| `once` | First opaque client read, before dialing | Yes |
| `headers` | Complete, indexed HTTP/1 request head before dialing | Yes |
| `stream` | Opaque reads in both directions | No |

- Without a body policy, `headers` inspects **only the opening request**;
  later keep-alive messages are relayed without inspection. The head must
  be HTTP/1.0 or HTTP/1.1, use CRLF, and fit `max_header_bytes` (default 16 KiB)
  and 512 fields. `inspect_timeout` bounds head collection; `timeout` must
  be positive for a headers listener.
- `once` is a first-read routing hint, not complete message inspection.
  `stream` sees read boundaries, not messages. Both are read-only. With
  `idle_timeout: 0`, `once` and header-only listeners can resume Linux splice.
- TLS termination provides plaintext. TLS passthrough provides encrypted
  records; do not use `headers` with passthrough SNI routing.

## HTTP body policy

```yaml
plugin:
  path: "../plugins/examples/http_body_guard/http_body_guard.wasm"
  data_mode: headers
  timeout: 1s
  inspect_timeout: 3s
  body_policy: both          # request, response, or both
  mutation_policy: replace  # default none; replace requires CapMutation
```

**Any `body_policy` limits the connection to one HTTP/1 exchange.** Both
directions enforce framing; the policy only selects which body events the
guest sees. `response` and `both` also dispatch the response head. The proxy
closes both sockets after the first final response, even if the backend
keeps its socket open. Headers are not rewritten to advertise the close.

- A request without `Content-Length` has no body. A response must have a
  single valid `Content-Length`, except for HEAD and statuses 204, 205 and
  304. A length on HEAD or 304 describes a representation, not body bytes.
  `max_body_bytes` (default 1 MiB) bounds the declared length in both
  directions and is checked before any body bytes are read. HTTP/1 upgrades,
  CONNECT, `Expect`, informational responses, `Transfer-Encoding` (including
  chunked), close-delimited bodies, and ambiguous lengths are not supported.
- Response heads are held until validated and, if selected, inspected. A
  split response head has its own `inspect_timeout` measured from its first
  byte; set a listener `idle_timeout` to also bound a silent peer. Reads
  crossing the declared body end are rejected, and later messages are never
  forwarded. A transport error before the body ends closes the exchange. An
  early final response can end an unfinished upload.
- With `mutation_policy: replace`, a guest can replace an inspected body
  chunk with exactly the same number of bytes. Headers cannot be mutated.
  An unauthorized or invalid replacement closes the connection. Body checks
  are streaming, not atomic over the whole message: bytes already forwarded
  cannot be recalled on a later rejection. The proxy does not decode content
  encodings or update application-level checksums/signatures.
- State is optional. Guests declaring both state capabilities can keep small,
  separate state values per connection and direction. State is applied only
  after a valid allowed result. Do not use persisted Wasm globals as session
  state: pooled instances can be reused across connections.

## Sandbox and migration

The guests have no WASI, sockets, filesystem, clocks, or randomness. The
default memory cap is 17 pages (1.06 MiB) per instance: a ceiling, not an
initial size. The reference guests built with the repository's
`.cargo/config.toml` request 1–2 pages; the same cap still fits modules built
with Rust's default 1 MiB stack. The engine bounds idle instances
and `max_instances` (default 64, hard cap 1024) caps concurrent guest
instances per listener; requests beyond the limit wait for a slot or fail on
their deadline. `max_conns` bounds listener concurrency when configured. Load
has a 5s deadline and each hook has its own deadline (default 1s). A trap or
timeout fails closed and the instance is discarded. After `on_accept` allows a
connection, `on_close` or `on_close_ex` runs exactly once, including TLS,
header, routing and dial failures before the relay starts.

## Differences from the legacy v3 runtime

ABI v4 is a smaller, fail-closed contract, not a drop-in reimplementation of
every v3 feature. The following v3 behaviours are intentionally absent:

- no mutation of opaque `stream` chunks; replacements are limited to
  `body_policy` body events with `mutation_policy: replace`;
- no persistent state passed between arbitrary stream chunks, and no
  keep-alive request/response state across exchanges; state exists only for
  the header/body phases selected by `body_policy`;
- no fallback from `on_headers` to `on_data` hooks; a module must export the
  v4 `on_event` surface for its declared plan.

A v3 deployment that depends on these behaviours needs a guest or config
redesign, not just a rebuild.

To upgrade, rebuild v3 modules as v4: export `wg_abi` and `wg_caps`, replace
L7 hooks with `on_event`, and use the OUT envelope for state/replacements.
Declare `CapLifecycle` on L4 modules. Code written against the initial v4
body slice must now opt into replacements with `mutation_policy: replace`.
The [migration guide](migration-to-v4.md) maps each legacy hook and config key
to its v4 replacement.

Build and check the reference guests:

```bash
cd plugins/examples
cargo test --locked -p wg-common
cargo build --locked --workspace --release --target wasm32-unknown-unknown
```

See [`configs/showcase.yaml`](../../configs/showcase.yaml) for a runnable
loopback showcase. Validate it with `wasmgate -config configs/showcase.yaml -validate`.
