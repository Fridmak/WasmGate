# Reference plugins

Nine reference plugins (Rust `#![no_std]`; `ipguard` also has a TinyGo
variant). They cover L4 filtering, HTTP guarding, body inspection, path and SNI
routing, protocol detection, Redis ACL, and stream masking. The compiled `.wasm`
files are committed, so Rust is not required to run them.

Documentation:

- [Plugins, ABI status, sandbox limits, build instructions](../../docs/en/plugins.md)
- [Плагины, статус ABI, ограничения песочницы, сборка](../../docs/ru/plugins.md)

All committed example `.wasm` files are legacy ABI v3 and are rejected by the
current ABI v4 proxy until ported. The test suite uses hand-built ABI v4
fixtures for `on_event` and L4 lifecycle.

[`configs/showcase.yaml`](../../configs/showcase.yaml) currently wires only the
native SNI baseline; plugin examples are pending v4 ports.
