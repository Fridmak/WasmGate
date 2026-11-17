# Reference plugins

Nine reference plugins (Rust `#![no_std]`; `ipguard` also has a TinyGo
variant). They cover L4 filtering, HTTP guarding, body inspection, path and SNI
routing, protocol detection, Redis ACL, and stream masking. The compiled `.wasm`
files are committed, so Rust is not required to run them.

Documentation:

- [Plugins, ABI status, sandbox limits, build instructions](../../docs/en/plugins.md)
- [Плагины, статус ABI, ограничения песочницы, сборка](../../docs/ru/plugins.md)

`ipguard` remains runnable on the legacy ABI v3 L4 `on_accept` path. The L7
examples (`http_guard`, `http_headers_guard`, `http_body_guard`, `path_router`,
`sni_guard`, `protocol_router`, `redis_guard`, `stream_guard`) still export ABI
v3 hooks and are not runnable in M2; they will be ported to `on_event` in M3/M4.

[`configs/showcase.yaml`](../../configs/showcase.yaml) wires the currently
runnable plugins.
