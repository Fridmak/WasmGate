# M2 — v4-only L7 runtime

- **Branch:** `stable`
- **Status:** complete
- **Depends on:** ABI v4 headers cutover (M1, commit `e00ccb5`)

## Goal

Remove the remaining ABI v3 L7 data path from the proxy for the phases that v4
supports, without silently falling back between ABI versions. Each phase has
exactly one production implementation:

```text
data_mode: headers -> ABI v4 on_event (done)
data_mode: once    -> ABI v4 on_event opaque first chunk
data_mode: stream  -> ABI v4 on_event opaque chunks
L4 on_accept/close -> ABI v3 until M3
```

Unsupported v4 features fail at configuration/load time. They are not silently
disabled and they are not routed to v3.

## Non-goals for M2

- OUT mutations and in-place payload rewriting.
- Stateful stream/body policies (`on_data_state` equivalent).
- Response headers/bodies and keep-alive request framing.
- HTTP/2, WebSocket framing and CONNECT tunnels.
- Porting all examples to Rust v4 SDK; test fixtures may be hand-built WASM.

Those are M3/M4 work. If a feature is required before M3, it must be designed as
an explicit v4 phase, not added as another mode.

## Feature matrix

| Phase | M1 | M2 | M3 |
|---|---|---|---|
| request headers | v4 | v4 | v4 |
| opaque first chunk (`once`) | v3 | **v4** | v4 |
| opaque stream (`stream`) | v3 | **v4** | v4 |
| L4 accept/close | v3 | v3 | **v4** |
| in-place mutation | v3 | reject | M4 |
| stateful body | v3 | reject | M4 |

## Work breakdown

### M2.1 — direction-aware opaque engine (done)

- Add `FlagClientToServer` / `FlagServerToClient` to `Flags`.
- Reject events carrying both directions.
- Add `Engine.OnOpaqueChunk(ctx, connID, dir, flags, chunk)`.
- Keep `OnOpaqueFirst` and `OnStream` as convenience wrappers.
- Tests: both directions, invalid direction, unknown flags, dual-direction
  rejection.

### M2.2 — loader and plan resolution (done)

- `data_mode: once` and `stream` create an `abi4.Engine` with
  `Plan: InspectOpaqueStream`.
- `data_mode: headers` keeps `Plan: InspectRequestHeaders`.
- v3 modules with `on_data`/`on_data_ex`/`on_data_state` are rejected for these
  modes with an explicit error.
- L4-only modules use `data_mode: ""` and remain on the v3 lifecycle path.
- Cache key remains `(listener, config, wasm hash)`; `DataMode` is already part
  of config equality.
- Tests: v4 once/stream load, v3 L7 rejection, L4-only acceptance, cache hit.

### M2.3 — proxy `once` cutover (done)

- Replace the v3 `OnDataConn` call in the `once` branch with
  `Engine.OnOpaqueFirst`.
- Keep pre-dial route verdicts and `targetPool` selection.
- Replay the peeked prefix and keep `splice(2)` for the body.
- Deny/error/timeout close before dial.
- No mutation: the v4 envelope is read-only in M2.
- Tests: allow/deny/route, timeout, no dial, body bytes unchanged.

### M2.4 — proxy `stream` cutover (done)

- Introduce a small internal stream inspector interface so `copy.go` does not
  depend on the v3 `Plugin` type.
- Call `OnOpaqueChunk` for both directions, with direction flags.
- `stream` never routes; route verdicts are rejected or denied explicitly.
- Mutation and state are rejected at load; stream is allow/deny only in M2.
- Tests: both directions, deny, timeout, prefix replay, no v3 fallback.

### M2.5 — remove v3 L7 from production (done)

- Delete `on_data`, `on_data_ex`, `on_data_state` handling from the proxy path.
- Leave L4 lifecycle for M3.
- Update configs/examples: v3 L7 examples are either ported to v4 fixtures or
  marked legacy and removed from the runnable showcase.
- Add a guard test that no `data_mode: once|stream|headers` listener resolves a
  `*plugin.Plugin`.

## M3 — lifecycle and v3 removal (done)

- Added v4 `on_accept`/`on_close` support to the engine.
- Cut L4 over to v4.
- Removed `internal/plugin` from the proxy import graph.
- Kept the v3 package temporarily as historical/test-only code.
- Updated examples/docs to mark committed v3 binaries as legacy.

## M4 — mutations and state

- Add OUT sections/mutations to the envelope.
- Add state slots and body/response phases.
- Only then port stateful/body examples.

## Acceptance criteria for M2

1. `data_mode: once|stream` resolves only an `abi4.Engine`.
2. v3 L7 modules are rejected with a clear error.
3. No upstream dial on deny/error/timeout.
4. `once` route verdicts still select `route_pools`.
5. `stream` supports both directions and never routes.
6. Body replay remains byte-identical and preserves the fast path.
7. Mutation/state-requiring modules fail closed at load.
8. `go test ./...`, `go vet ./...`, race for `abi4`/`proxy` and
   `wasmgate -validate` pass.
9. A regression test asserts there is no v3 L7 fallback.

## Current progress

- M2.1–M2.5 are implemented and tested.
- `data_mode: once`, `stream` and `headers` resolve only `abi4.Engine`.
- ABI v3 is no longer in the production proxy import graph.
- v3 L7/L4 modules are rejected; committed v3 examples are marked legacy.
- M4 protocol primitives are complete. M5 owns HTTP framing and proxy body
  integration; M6 owns final legacy cleanup.
