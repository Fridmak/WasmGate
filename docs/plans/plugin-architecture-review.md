# Plugin architecture review and ABI v4 proposal

- **Branch:** `stable`
- **Goal:** decide whether the current plugin surface is a sound architecture or a
  collection of mode-specific patches, and define the next version before adding
  more modes.

## 0. Implementation status

- **M0 — done:** `internal/plugin/abi4` contains the stable v4 core: versioned
  envelope, `Plan`/`Caps`/`Verdict`, and the `on_event` engine with load-time
  validation, pooled instances and per-call timeouts. Conformance, fuzz,
  benchmark and race tests are in the same package.
- **M1 — headers cutover done:** `data_mode: headers` is now a strict ABI v4
  path wired through `internal/proxy`. It requires `on_event`/`wg_*`; legacy
  ABI v3 `on_headers` modules are rejected at snapshot load. The request-header
  path preserves the complete-block collection, route verdicts and body
  `splice(2)` replay.
- **M2 — L7 cutover done:** `once` and `stream` also use ABI v4
  (`OnOpaqueFirst`/`OnOpaqueChunk`), with direction flags and no v3 L7
  fallback.
- **M3 — lifecycle cutover done:** L4 `on_accept`/`on_close` now use
  `abi4.Engine` (`Plan: InspectLifecycle`). The production proxy no longer
  imports `internal/plugin`; the v3 package remains historical/test-only.
- **Next:** M4 — OUT mutations, state slots, body/response phases and v4
  example ports. The v3 package can be deleted after the ports.
- **Not in M3:** OUT mutations, request/response body policies, opaque+HTTP
  composition, HTTP/2 and WebSocket framing. Those stay separate milestones.
- **Research artifacts:** the old `internal/plugin/v4` prototype and generated
  benchmark adapters were removed; they were not production code.

## 1. Current model

The host currently exports four overlapping L7 hooks and selects them from
`plugin.data_mode`:

| `data_mode` | Hook used | Buffer boundary | `splice` | Route |
|---|---|---|---|---|
| `once` | `on_data` / `on_data_ex` | first client `Read` | yes | yes |
| `headers` | `on_headers`, fallback `on_data` | complete HTTP/1 headers | yes | yes |
| `stream` | `on_data_state` or `on_data` | every chunk | no | no |

Lifecycle hooks are separate: `on_accept` (L4) and `on_close`.

This works, but it is **mode-oriented**, not **phase-oriented**. The same plugin
gets different semantics depending on a listener-level string, and every new
need (headers, body, response) adds another hook or another mode.

## 2. Findings

### A. `headers` is a special case, not a pipeline stage

`on_headers` is selected only by `data_mode: headers`; `once` calls `on_data`,
`stream` calls `on_data_state`. A plugin cannot express “I want request headers,
then body chunks, then response headers” as one model. Adding body/response
inspection would add `on_body`, `on_response_headers`, etc. — the same crutch
again.

### B. Four overlapping L7 hooks

`on_data`, `on_data_ex`, `on_data_state`, `on_headers` differ by:

- stateless vs per-direction state;
- request-only vs both directions;
- first-read vs complete block;
- whether route pools are allowed.

The host has fallback rules (`on_headers` → `on_data`, `on_data_state` → etc.),
and `on_data_state` takes priority inside `OnDataConn`. This is hard to reason
about and easy to misconfigure.

### C. No phase metadata

A plugin sees a pointer/len and must infer whether it is request headers, body,
response headers, a TLS record, or a chunk of a binary protocol. `headers` mode
fixes only one of those cases. Examples duplicate parsing logic because of this.

### D. State is per direction, not per phase

`StateBuf` is a single 4 KiB buffer per direction. There is no separate state for
request headers vs body vs response, and `on_headers` is stateless. A protocol
that needs headers state and body state must encode both into one buffer.

### E. Route pools are asymmetric

Routing works only before dial (`once`/`headers`), not in `stream`. That is
reasonable at the transport level, but it is not expressed in the API; it is a
mode side effect.

### F. Fail-open configuration was possible

Before the stable fix, an `on_headers`-only module could be configured with
`once`/`stream` and silently do nothing. That is now rejected, but the root cause
is that hook selection is implicit.

### G. Examples are not all safe as drop-in policies

This is important because users copy examples:

- `http_headers_guard` had a raw-prefix path check; absolute-form, dot segments,
  `//` and percent-encoding bypassed it. Fixed in `stable` and covered by tests.
- `http_guard` is stateless and runs in `stream`; on a split request its
  `parse_request` fails open. A client can split the request line/headers so the
  first chunk is “non-HTTP” and the policy is skipped. It is a demonstration,
  not a WAF.
- `path_router` uses `once`: a split request line yields the default pool.
- `protocol_router` is fine for first-byte protocol detection.
- `http_body_guard` is the closest to a real state machine, but its state is
  packed manually and it is still responsible for framing.
- `stream_guard` signature matching cannot see signatures split across chunks
  unless the plugin maintains state; the host does not reassemble.

The examples are useful, but the host should not imply that “stream” equals
“safe HTTP filter” or that “headers” is a full WAF.

### H. Lifecycle pairing

`on_close` is invoked only after a successful dial/join. If a connection is
rejected after `on_accept` (rate limit, plugin deny, header timeout), there is no
paired close event. The host should define a single lifecycle contract.

### I. Legacy API footgun

`OnData` uses `connID = 0`; before the stable fix it did not clear scratch because
the cross-connection check saw the same ID. The production proxy uses unique
IDs, but the public/embedding API should not have a special case.

### J. No capability negotiation

The host introspects exported function names, but there is no explicit ABI
version or feature set. Adding a hook is a new host-side special case, and an
old plugin can be silently run in an incompatible mode.

## 3. Proposed ABI v4: one event stream, phases instead of hooks

### 3.1 Lifecycle

Keep two lifecycle hooks:

```c
on_accept(conn_id: i64, ip_hi: i64, ip_lo: i64, port: i32) -> i32
on_close(conn_id: i64, ip_hi: i64, ip_lo: i64, rx_bytes: i64, tx_bytes: i64) -> ()
```

`on_accept` runs immediately after accept; `on_close` runs for every accepted
connection, including early rejects, exactly once.

### 3.2 One L7 event hook

```c
on_event(
    conn_id: i64,
    phase: i32,
    flags: i32,
    data_ptr: i32,
    data_len: i32,
    state_ptr: i32,
    state_len: i32,
    state_cap: i32
) -> i64
```

`phase`:

| Value | Meaning |
|---|---|
| 1 | request headers (complete, one event) |
| 2 | request body chunk |
| 3 | response headers (complete, one event) |
| 4 | response body chunk |
| 5 | opaque stream chunk (generic protocols; no framing promise) |

`flags`:

- `FIRST` / `LAST` for the phase;
- `TRUNCATED` if the host hit a configured cap;
- `DIRECTION` bits are implied by phase; no separate `dir`.

Return value: low 32 bits = verdict/route, high 32 bits = new state length.
State is one buffer per connection and phase (or a small fixed array of phase
states); the host owns allocation and bounds. `route` is valid only from request
headers; route from a later phase denies.

### 3.3 Inspection plan, not `data_mode`

The listener config becomes an explicit plan:

```yaml
plugin:
  path: ...
  inspect:
    request_headers: true
    request_body: false
    response_headers: false
    response_body: false
    stream: false
```

Or the named plans for common cases:

```yaml
inspection: request_headers   # l4 | request_headers | stream | full
```

The host determines which phases exist. A plugin exports `on_event`; it does not
need to know whether a phase arrives as one chunk or many. If a phase is absent,
the host never calls it. No fallback and no silent no-op: an `on_event` plugin
with `inspection: l4` is a configuration error.

### 3.4 Framing guarantees

- Request/response headers: host parses HTTP/1.1 framing and delivers one
  complete block through `CRLFCRLF`; caps/timeouts fail closed.
- Request/response body: host parses `Content-Length`/chunked; emits exact body
  chunks; no WASM call for framing metadata. This is required for keep-alive
  per-request inspection.
- `stream`: host emits chunks without reassembly; plugin owns framing.
- HTTP/2 is rejected until a separate framing implementation exists.
- TLS passthrough cannot use HTTP phases; config rejects it.

This is a larger HTTP-aware proxy feature, but it removes the `once`/`headers`/
`stream` special cases from the plugin API.

### 3.5 Compatibility

- ABI v3 remains supported through an adapter: `on_data` → `REQUEST_BODY` or
  `STREAM`, `on_headers` → `REQUEST_HEADERS`, `on_data_state` → stateful event.
- New v4 plugins use only `on_event`; the host rejects ambiguous mixes at load.
- Deprecate v3 in docs; remove it only after a major version and migration.
- `on_accept`/`on_close` stay compatible.

### 3.6 SDK

The common crate exposes an event enum and a dispatcher:

```rust
match event.phase {
    Phase::RequestHeaders => ...,
    Phase::RequestBody => ...,
    _ => Verdict::Allow,
}
```

Examples no longer need mode-specific code. `http_guard` becomes request/response
events; `http_body_guard` uses body events; `path_router` uses request headers;
`ipguard` remains L4.

## 4. Work plan

### Phase A — freeze the current surface

- Require explicit mode for L7 hooks (done).
- Reject `on_headers`-only outside headers (done).
- Fix example bypasses and lifecycle pairing (done in `stable`).
- Cut all `data_mode` values and L4 lifecycle over to ABI v4 and reject legacy
  v3 modules (done). `once`/`stream` use `OnOpaqueFirst`/`OnOpaqueChunk`;
  `on_accept`/`on_close` use `Plan: InspectLifecycle`.

### Phase B — host pipeline abstraction (done as M0 core)

- Add `internal/plugin/abi4` with typed phases (`Plan`), capability bits and
  route/verdict validation.
- Resolve the plan once at snapshot build; the data path must not switch on
  mode strings.
- Keep v3 adapters so behavior does not change yet.
- Tests: plan validation, envelope conformance, engine load/verdict/timeout,
  fuzz and race.

### Phase C — wire v4 into the proxy (M1 headers cutover done)

- Add `on_event` dispatch to the proxy for the request-header phase (done).
- Resolve the plan and capabilities during snapshot validation; reject a module
  whose declared caps do not satisfy the plan (done through `abi4.New`).
- All `data_mode` values and L4 lifecycle use ABI v4; v3 modules are rejected.
  The v3 package remains historical/test-only and is not imported by the proxy.
- Defer request/response body phases, framing, keep-alive and OUT mutations to
  later milestones.
- Integration tests cover allow/deny/route verdicts, malformed/oversized
  headers, header-count limits and timeout fail-closed behavior.

### Phase D — migration and removal (3–4 days)

- Update all examples and docs; add compatibility matrix.
- Deprecate v3 hooks; keep adapter for one major release.
- Remove the old mode strings after migration.
- Fuzz framing and event boundaries; race and benchmarks.

Overall: **~2 weeks** for the pipeline and v4, plus the HTTP framing work. It is
larger than the headers patch, but it prevents a fourth/fifth mode from being
bolted on later.

## 5. Recommendation

Do **not** add another hook for the next feature (body/response). Instead:

1. Use ABI v4 for every `data_mode`; v3 L7 hooks are no longer a compatibility
   path (done).
2. Cut L4 lifecycle over to v4 and remove the v3 runtime (M3).
3. Migrate examples and plugin authors to phases.
4. Leave `on_accept`/`on_close` as the only lifecycle hooks.

The ABI v3 L7 modes are gone; only the L4 lifecycle remains on v3 as an
explicit M3 item.
