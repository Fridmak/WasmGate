# Reference plugins

Eight reference plugins (Rust `#![no_std]`; `ipguard` also has a TinyGo
variant). They cover L4 filtering, HTTP guarding, body inspection, path and SNI
routing, protocol detection, Redis ACL, and stream masking. The compiled `.wasm`
files are committed, so Rust is not required to run them.

Documentation:

- [Plugins, ABI v3, sandbox limits, build instructions](../../docs/en/plugins.md)
- [Плагины, ABI v3, ограничения песочницы, сборка](../../docs/ru/plugins.md)

[`configs/showcase.yaml`](../../configs/showcase.yaml) wires every plugin into
one runnable configuration.
