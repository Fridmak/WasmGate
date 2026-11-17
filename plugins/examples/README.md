# Reference plugins

Nine reference plugins (Rust `#![no_std]`; `ipguard` also has a TinyGo
variant). They cover L4 filtering, HTTP guarding, body inspection, path and SNI
routing, protocol detection, Redis ACL, and stream masking. The compiled `.wasm`
files are committed, so Rust is not required to run them.

Documentation:

- [Plugins, ABI status, sandbox limits, build instructions](../../docs/en/plugins.md)
- [Плагины, статус ABI, ограничения песочницы, сборка](../../docs/ru/plugins.md)

`http_headers_guard` still exports the legacy ABI v3 `on_headers` hook. It is
kept for reference, but `data_mode: headers` is now ABI v4-only and rejects it
until the example is ported to `on_event`.

[`configs/showcase.yaml`](../../configs/showcase.yaml) wires the supported
plugins into one runnable configuration.
