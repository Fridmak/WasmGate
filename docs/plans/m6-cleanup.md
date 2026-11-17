# M6 — legacy cleanup

- **Status:** in progress (legacy Go runtime removed)
- **Depends on:** M5 example and proxy migration

- Delete `internal/plugin` and its v3 tests. (done)
- Remove legacy v3 `.wasm` artifacts and stale compatibility documentation.
- Keep only ABI v4 conformance, integration, fuzz, race and benchmark tests.
- Validate every committed example and configuration through the v4 loader.

The remaining work is the Rust v4 example port/rebuild. Until it is complete,
legacy `.wasm` files remain as explicitly marked reference artifacts and are
rejected by the production loader.
