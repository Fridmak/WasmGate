# M6 — legacy cleanup

- **Status:** planned
- **Depends on:** M5 example and proxy migration

- Delete `internal/plugin` and its v3 tests.
- Remove legacy v3 `.wasm` artifacts and stale compatibility documentation.
- Keep only ABI v4 conformance, integration, fuzz, race and benchmark tests.
- Validate every committed example and configuration through the v4 loader.
