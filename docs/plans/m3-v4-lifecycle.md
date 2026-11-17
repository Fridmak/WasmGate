# M3 — ABI v4 lifecycle and v3 removal

- **Branch:** `stable`
- **Status:** complete
- **Depends on:** M2 L7 cutover

## Goal

Move the last production v3 path — L4 `on_accept`/`on_close` — onto ABI v4 and
remove `internal/plugin` from the proxy import graph. After M3 the proxy has one
runtime type: `abi4.Engine`.

## Scope

- v4 lifecycle plan (`InspectLifecycle`) without an envelope or `on_event`;
- `on_accept` / `on_close` / `on_close_ex` validation and invocation;
- L4 loader path (`data_mode: ""`) uses `abi4.New`;
- v3 L4 modules are rejected;
- proxy lifecycle calls `Engine.OnAccept`/`OnCloseConn`;
- remove `plugin.Plugin` from `resolvedListener`, `cachedPlugin` and proxy code;
- tests for allow/deny, close pairing, IP encoding, timeout and rejection.

## Non-goals

- Mutations, state, body/response phases (M4).
- Porting Rust examples to v4; legacy v3 examples are marked legacy.
- Removing the `internal/plugin` package itself was deferred to M6 and is now
  complete; legacy example binaries remain until the v4 ports are rebuilt.

## Work breakdown

### M3.1 — lifecycle plan and validation (done)

- `InspectLifecycle` is a valid standalone plan.
- A lifecycle module requires `wg_abi`, `wg_caps` and at least one of
  `on_accept`/`on_close`/`on_close_ex`.
- Lifecycle-only modules do not need memory, `wg_buf_ptr/len` or `on_event`.
- Event modules under a lifecycle plan are rejected.

### M3.2 — Engine lifecycle calls (done)

- `Engine.OnAccept` packs IPv4 as `::ffff:a.b.c.d`, applies the hook timeout
  and treats any nonzero result as deny.
- `Engine.OnCloseConn` prefers `on_close_ex`, falls back to `on_close`, and
  discards an instance on trap/timeout.
- Lifecycle hooks use the same pooled instance model as L7 events.

### M3.3 — proxy and loader cutover (done)

- `data_mode: ""` loads `Plan: InspectLifecycle`.
- Every data_mode now resolves `*abi4.Engine`.
- `on_accept` deny closes before TLS/dial.
- `on_close` is paired with every accepted connection.
- ABI v3 L4 modules are rejected.

### M3.4 — guards (done)

- `TestV4L4ModuleAccepted`
- `TestV3L4ModuleRejected`
- `TestEngineLifecycle`
- `TestEngineLifecycleRejectsEventModule`
- `TestEncodeIP`
- `TestProxyV4L4LifecycleAllowsAndDenies`

## Acceptance criteria

1. No production proxy code imports `internal/plugin`.
2. `data_mode: ""` accepts only ABI v4 lifecycle modules.
3. `on_accept` allow/deny works before dial.
4. `on_close` is called for accepted connections, including early failures.
5. Lifecycle timeouts discard the instance and deny/close.
6. L7 M1/M2 tests remain green.
7. `go test ./...`, `go vet ./...`, race and config validation pass.

## Current progress

M3.1–M3.4 are implemented. The legacy Go v3 package was removed in M6 cleanup;
legacy example binaries remain outside the production configuration until the
v4 SDK/example ports are rebuilt.
