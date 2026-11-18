# ABI v4 reference plugins

These Rust `no_std` guests are built for `wasm32-unknown-unknown` without WASI.
The adjacent `.wasm` files are prebuilt; Rust is only needed to rebuild them.
The [showcase config](../../configs/showcase.yaml) demonstrates all nine on
loopback. They are examples, not a production authentication or WAF boundary.

| Guest | Mode | Demonstrates |
|---|---|---|
| `ipguard` | L4, omit `data_mode` | Accept/deny and close hooks |
| `http_guard` | `headers` | Indexed method/path/headers and public demo Basic credentials |
| `http_headers_guard` | `headers` | Request-head validation and a path policy |
| `http_body_guard` | `headers`, `body_policy: both`, `mutation_policy: replace` | Cross-chunk request state and same-length response replacement |
| `path_router` | `headers` | Host/path routing to `[api, static, ws]` |
| `sni_guard` | `once` | TLS ClientHello SNI routing; configure a native SNI route to enable the full record peek |
| `protocol_router` | `once` | First-read classification to `[tls, http, ssh, postgres, redis]` |
| `redis_guard` | `stream` | Inspect the first command in a client chunk, not a complete Redis ACL |
| `stream_guard` | `stream` | Read-only byte signature inspection, no reassembly |

`http_body_guard` replaces ASCII digits with `*` only in an identity-encoded,
`text/plain` 200 response bearing `X-WG-Mask-Digits: yes`. It demonstrates the
OUT contract; it is not a JSON or secret-redaction implementation. Opaque
`stream` guests do not mutate traffic. `once` can see incomplete messages.

## Build and update the prebuilt guests

```bash
cd plugins/examples
cargo test --locked -p wg-common
cargo build --locked --workspace --release --target wasm32-unknown-unknown
for guest in */*.wasm; do
  cp "target/wasm32-unknown-unknown/release/${guest##*/}" "$guest"
done
```

The Rust toolchain and target are pinned in `rust-toolchain.toml`. The
`ipguard/tinygo` source is an alternative lifecycle example; it is not used
to build the committed Rust artifact. See the [ABI contract](../../docs/en/plugin-abi-v4.md)
and the plugin guides ([EN](../../docs/en/plugins.md), [RU](../../docs/ru/plugins.md)).
