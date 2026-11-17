# Plugin HTTP header inspection — design and work plan

- **Status:** implemented in `main` and `stable` as the ABI v3 `headers` mode; historical design, superseded for new work by ABI v4
- **Branch:** `feat/plugin-http-headers` (merged)
- **Base:** `main` after the metrics merge (`8aa95f5`)
- **Scope owner:** data plane / plugin ABI
- **Goal release:** shipped in the next minor after the plan was approved
- **Execution plan:** [`plugin-http-headers-implementation.md`](plugin-http-headers-implementation.md) — locked v1 decisions, tasks, tests and security gates

## 1. Problem

`data_mode: once` gives a plugin exactly one client `Read` before dialing the
upstream. TCP does not preserve HTTP message boundaries, so a request whose
headers arrive in two segments is only partly visible. `http_guard` works around
this by running in `stream`, which calls the WASM hook for every chunk and
disables `splice(2)` for the whole connection.

We want the common L7 request-header use case to be both correct and cheap:

- the plugin sees the **complete HTTP/1.1 request header block**;
- the hook runs **once per connection**;
- the request body and the rest of the connection keep the `splice(2)` fast path;
- plugin authors do not have to reason about TCP segmentation;
- existing configurations and plugins keep working unchanged.

## 2. Non-goals

The first version deliberately does **not** solve:

- per-request inspection on a keep-alive connection (only the first request is
  inspected);
- response headers or response bodies;
- body inspection after the header block;
- HTTP/2, HPACK, WebSocket frames, CONNECT tunnels after the header block;
- TLS passthrough traffic (the plugin would see TLS records, not HTTP);
- a host-side HTTP parser or header normalisation;
- plugin-controlled “read more” loops. This was considered and rejected: it
  leaks transport framing into every plugin, adds an ABI failure mode, and needs
  the same hard caps anyway.

For full body/response/per-request filtering, `data_mode: stream` remains the
correct mode.

## 3. User-facing contract

### 3.1 Configuration

```yaml
listeners:
  - name: edge
    addr: "0.0.0.0:8080"
    upstream_pool: web
    plugin:
      path: "plugins/examples/http_headers_guard/http_headers_guard.wasm"
      data_mode: headers      # new; default remains "once"
      max_header_bytes: 16384
      inspect_timeout: 3s
      # v1: incomplete headers always deny; no fallback option
      route_pools: ["canary"]
```

Validation rules:

- `data_mode` accepts `once`, `headers`, `stream`; empty means `once`.
- `headers` requires a plugin with a data hook (`on_headers`, `on_data` or
  `on_data_ex`). A module that only exports `on_data_state` is rejected with a
  clear message for this mode.
- `headers` is rejected at config validation when `sni_routes` is configured and
  TLS termination is disabled: that listener is a TLS-passthrough listener and
  the plugin cannot see HTTP.
- `max_header_bytes` must be in `1..16389` (the existing peek buffer is
  `5 + 16 KiB`). Default `16384`.
- `inspect_timeout` must be positive. Default `3s`. It is a **total** deadline
  for the whole header block, not a per-read deadline.
- Incomplete headers (timeout, EOF, oversized, no terminator) **always deny** in
  v1 before dialing. There is no `fallback_once`: a fail-open fallback would
  reintroduce exactly the bypass this mode exists to close.
- The existing `plugin.timeout` stays the per-hook WASM timeout. For
  `data_mode: headers` it must be non-zero; validation rejects `timeout: 0`
  because a looping guest would otherwise freeze the process during header
  inspection. This also links the feature to the WG-02 fix.

### 3.2 New optional hook

Add an additive ABI v3 hook:

```c
on_headers(conn_id: i64, data_ptr: i32, data_len: i32) -> i32
```

- Called only in `data_mode: headers` and only when a complete header block is
  available.
- The buffer contains the request line and every header, **including the final
  empty line** (`\r\n\r\n`). The body is not included, even if it arrived in the
  same `Read`.
- The buffer is writable; in-place changes are copied back before dialing. The
  plugin cannot change the length, insert bytes or remove bytes.
- Return value has the existing verdict semantics: `0` allow, `1` deny,
  `>= 2` selects `route_pools[value - 2]`. An invalid route index denies.
- `on_data_ptr` is required, exactly as for `on_data`.
- If a module exports `on_headers` and `on_data`, `headers` mode uses
  `on_headers`; `stream`/`once` continue to use `on_data`.
- If a module does not export `on_headers`, the host falls back to
  `on_data`/`on_data_ex` with `dir = 0` and the same full block. This keeps old
  plugins usable without rebuilding.
- `on_headers` is request-only; there is no `dir` argument. Response inspection
  is out of scope.

### 3.3 Plugin author experience

The existing `plugins/examples/common` crate already contains a zero-allocation
HTTP helper. Extend it with a strict parser:

```rust
pub fn parse_request_headers(payload: &[u8]) -> Option<Request<'_>>;
```

Differences from today's `parse_request`:

- requires a complete `\r\n\r\n` terminator;
- rejects LF-only line endings;
- validates the request line and header names;
- exposes duplicate-header detection and `Content-Length`/`Transfer-Encoding`
  helpers;
- returns `None` for malformed input; a `headers` plugin should then deny.

Example plugin:

```rust
#[no_mangle]
pub extern "C" fn on_headers(_conn_id: i64, ptr: i32, len: i32) -> i32 {
    let Some(buf) = (unsafe { wg::payload(ptr, len) }) else {
        return 1;
    };
    let Some(req) = wg::parse_request_headers(buf) else {
        return 1; // fail closed: headers mode promises a complete HTTP/1 block
    };
    if req.method == b"TRACE" || req.target.starts_with(b"/admin") {
        return 1;
    }
    0
}
```

## 4. Host algorithm

For a listener with `data_mode: headers` and a data-capable plugin:

```text
prefix = peeked bytes (may already contain the first client read)
if plugin is absent or has no data hook: fall through to normal once path

deadline = now + inspect_timeout
loop:
    if "\r\n\r\n" in prefix:
        headerEnd = index + 4
        break
    if len(prefix) >= max_header_bytes:
        return incomplete(...)          # deny by default
    if deadline expired:
        return incomplete(...)
    read more bytes into the existing peek buffer
    if EOF or error:
        return incomplete(...)

headers = prefix[:headerEnd]            # body bytes stay in prefix, not passed
verdict = plugin.on_headers(connID, headers) or on_data(connID, 0, headers)

if deny: close; return
if route: routePool = routePools[verdict-2]
dial upstream (selected route pool)
write(prefix)                            # headers (possibly mutated) + body
join(client, upstream)                   # splice where the platform allows it
```

Important properties:

- `prefix` is stored in the existing pooled `tlspeek.Conn` buffer. No new
  per-connection allocation in the common case.
- The plugin is called **once**.
- Only header bytes are passed to the plugin; the body starts after `headerEnd`
  and is replayed untouched.
- If the client sends headers and body in one read, the body is not exposed to
  the header hook.
- Dial happens after the complete header block, so a route decision on
  `Host`/path cannot use a partial value.
- `prefix` bytes are written to the upstream before `join`; the rest of the
  session can use `splice(2)` on Linux/raw TCP.

### 4.1 Buffer support in `tlspeek`

The existing `tlspeek.Conn` owns a pooled `5 + 16 KiB` buffer and already has
`Bytes()`, `Read()`, `DrainTo()` and `Close()`.

Add a narrow API rather than exposing internals:

```go
// Bytes returns the unread prefix.
func (c *Conn) Bytes() []byte

// Fill reads more bytes into the buffer, up to its capacity.
// It returns io.EOF when the peer closed.
func (c *Conn) Fill() (int, error)
```

`Fill` must:

- read into the unused capacity, not consume the existing prefix;
- never grow beyond the pooled buffer;
- release the buffer exactly once;
- preserve `DrainTo` semantics (write all buffered bytes, release, return raw
  conn);
- be safe with the existing `Read` path and `CloseWrite`.

A helper in `internal/proxy` can own the delimiter scan and deadline:

```go
func readHeaderPrefix(c *tlspeek.Conn, max int, deadline time.Time) ([]byte, bool, error)
```

### 4.2 Dial and replay

Reuse the existing `DrainTo` path:

1. after the verdict, dial the upstream;
2. write the complete prefix to the upstream (with the plugin's header edits);
3. `DrainTo` returns the raw connection;
4. `join` starts with no plugin (or with the stream plugin only if the listener
   is in `stream` mode, which is a different path).

The prefix byte count must be included in `tx` for `on_close` accounting, as the
`once` path already does.

## 5. Detailed work breakdown

### Phase 0 — baseline and benchmarks (0.5 day)

- Add a repeatable benchmark matrix: no plugin, `once`, `headers`, `stream`.
- Payloads: 1 KiB request with one-chunk headers, 16 KiB headers split across
  2–3 reads, 1 MiB upload.
- Record requests/sec, p50/p99, CPU, allocations, and whether `splice(2)` is
  used (Linux, `strace` or a counter).
- Store results in the PR description; do not commit generated profiles.

### Phase 1 — buffer primitives (1 day)

- `internal/tlspeek/sni.go`: add `Bytes()`, `Fill()`, and tests for:
  - append into free capacity;
  - full buffer;
  - `Fill` after `Read`/`DrainTo`;
  - double `Close`;
  - EOF and partial reads.
- Add a small `readHeaderPrefix` helper in `internal/proxy` with a total
  deadline; unit-test it with `net.Pipe`.
- No extra allocation per connection; verify with an allocation test.

### Phase 2 — configuration and validation (0.5 day)

Files:

- `internal/config/listener_config.go`: `DataMode`, `MaxHeaderBytes`,
  `InspectTimeout`, `Incomplete`.
- `internal/config/listener_config_test.go`: table tests for valid/invalid
  values, defaults, `sni_routes` rejection, plugin hook mismatch.
- `internal/config/config_test.go`: parse a real YAML with the new fields.

Validation must run before a snapshot is published. A bad reload must be
rejected without disturbing existing connections (the current fail-closed
semantics already provide this).

### Phase 3 — plugin ABI (1 day)

Files:

- `internal/plugin/sandbox.go`:
  - constant `hookOnHeaders = "on_headers"`;
  - `hasHeaders` in `exportFlags`/`Plugin`;
  - signature validation `(i64, i32, i32) -> i32`;
  - `on_data_ptr` requirement when only `on_headers` is present;
  - `func (p *Plugin) OnHeaders(ctx context.Context, connID uint64, payload []byte) (Verdict, error)`;
  - fallback to `OnDataConn(ctx, connID, 0, payload)` when the hook is absent;
  - copy-back semantics identical to `OnDataConn`;
  - reject/ignore when the payload exceeds the scratch capacity.
- `internal/plugin/sandbox_test.go`:
  - valid and invalid `on_headers` signatures;
  - fallback to `on_data`;
  - mutation and route verdicts;
  - missing `on_data_ptr`;
  - module with only `on_data_state` rejected in headers mode.
- Keep ABI v3: the new export is additive and optional. Document it as an
  extension, not a breaking version bump.

### Phase 4 — proxy integration (1.5 days)

Files:

- `internal/proxy/proxy.go`:
  - branch on `rl.cfg.Plugin.DataMode == "headers"`;
  - call the header prefix helper;
  - apply `incomplete` policy;
  - call `OnHeaders`/legacy fallback;
  - route selection, dial, prefix replay, `join`;
  - count incomplete/rejected events and log with the existing sampler.
- `internal/proxy/proxy_test.go`:
  - split headers at every boundary;
  - body in the same read is not passed to the plugin but is forwarded intact;
  - allow/deny/route;
  - mutation is forwarded;
  - cap boundary (`max-1`, `max`, `max+1`);
  - timeout, EOF and non-HTTP traffic;
  - TLS termination;
  - `sni_routes` + headers is rejected at startup;
  - keep-alive second request is not inspected (documented assertion, not a
    security test);
  - all incomplete-header cases deny before dial.
- `internal/proxy/copy_test.go` or a new benchmark file: verify the body path
  still uses the fast path on Linux.

### Phase 5 — SDK and example (1 day)

- `plugins/examples/common/src/lib.rs`:
  - strict `parse_request_headers`;
  - header-name validation, duplicate helpers, CL/TE helpers;
  - tests in the crate or host tests that load the example.
- New `plugins/examples/http_headers_guard/`:
  - `Cargo.toml`, `src/lib.rs`, committed `.wasm`;
  - uses `on_headers`, denies malformed/blocked requests, demonstrates route.
- Existing `http_guard` stays as the `stream` example (it also masks response
  headers). Update its doc comment to point header-only policies at the new
  example.
- Add the example to `configs/showcase.yaml` and `docs/en|ru/plugins.md`.

### Phase 6 — documentation (0.5 day)

- `docs/en/plugins.md` / `docs/ru/plugins.md`:
  - ABI table: `on_headers`;
  - data-mode table: `once`, `headers`, `stream`;
  - limitations: HTTP/1.1, first request, no response/body, TLS passthrough
    rejected, keep-alive bypass;
  - security warning: `headers` is connection-level, not a full WAF.
- `docs/en/configuration.md` / `docs/ru/configuration.md`: new fields.
- `configs/wasmgate.yaml` and `configs/showcase.yaml`: commented examples.
- `deployments/observability/README.md` and dashboard: only if new metrics are
  added.

### Phase 7 — verification and hardening (1 day)

- `go vet ./...`, `go test ./...`, `go test -race ./...` (Linux container for
  race), `golangci-lint` if available.
- `dotnet` untouched; no control-plane change.
- Fuzz tests for the strict parser and the split-boundary reader.
- Manual security matrix (see Â§6).
- Benchmarks and comparison table in the PR.
- Review against the round-2 report: WG-02 and NEW-2 are prerequisites (see
  Â§7).

| Phase | Deliverable | Depends on | Estimate |
|---|---|---|---|
| 0 | benchmark baseline | — | 0.5 d |
| 1 | `Bytes`/`Fill`, prefix reader, tests | 0 | 1 d |
| 2 | config fields, validation, defaults | 1 | 0.5 d |
| 3 | `on_headers`, fallback, ABI tests | 1 | 1 d |
| 4 | proxy `headers` path, routing, integration tests | 1–3 | 1.5 d |
| 5 | strict SDK parser + example plugin + `.wasm` | 3 | 1 d |
| 6 | en/ru docs, config samples | 2–5 | 0.5 d |
| 7 | fuzz, race, security matrix, benchmarks | all | 1 d |

Overall estimate: **5–7 working days** including tests and documentation, not
counting the WG-02/NEW-2 prerequisites.

## 6. Security analysis

Threat model: the client is untrusted; the plugin is operator-supplied code
(trusted for policy, but may be buggy or memory-unsafe inside the WASM sandbox);
the upstream HTTP parser may differ from the plugin's; TLS and reload are part of
the data-plane surface.

| Threat | Mitigation in this design | Residual risk |
|---|---|---|
| Split headers bypass (`once`) | Host gathers the complete `\r\n\r\n` block; plugin sees it once | None for the first request; keep-alive below |
| Keep-alive second request bypass | Explicitly out of scope; `headers` is connection-level | **High if sold as a full L7 firewall.** Document bluntly; use `stream` for per-request |
| HTTP/2 / binary preface | Detect `PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n` and deny in headers mode | Other binary protocols that happen to contain CRLFCRLF; plugin must reject non-HTTP |
| TLS passthrough | Startup validation rejects `headers` with `sni_routes` and no termination | TLS termination with ALPN h2 still needs the HTTP/2 check |
| Slowloris / pre-dial hold | `max_header_bytes`, total `inspect_timeout`, existing per-IP/concurrency guards | Guards are off by default; recommend enabling them |
| Oversized headers | Hard cap at the existing 16,389-byte peek buffer; fail closed | None; larger headers need a new buffer policy |
| Incomplete headers | Always deny in v1 before dial; no fallback option | None in v1; operators who need the old behavior keep `data_mode: once` |
| Request smuggling / parser differential | Host does not normalise or parse; passes raw bytes; strict SDK parser | Upstream may still interpret CL/TE differently; `stream`/normalisation is the only complete answer |
| Header mutation injection | In-place only; no length change; plugin is trusted policy code | A malicious plugin can already rewrite headers |
| Plugin timeout 0 freezes process | `headers` mode requires a non-zero hook timeout; tie to WG-02 fix | Existing plugin loads/reloads still need WG-26 |
| Scratch/state leak across connections (NEW-2/NEW-1) | Clear the full scratch before header inspection, or land the global NEW-2 fix first | Deterministic plugin input requires the leak fix |
| Plugin instance pool / guest escaping memory | Existing wazero sandbox, memory cap, no imports, `on_data_ptr` bounds | Standard WASM sandbox assumptions |
| Route to the wrong pool | Route decision only after complete headers | Plugin policy bugs remain |
| Reload with changed mode/caps | Snapshot captured per connection; new config applies to new connections | Existing connections keep old policy |
| Log flooding | Existing sampler; incomplete events logged at debug/sampled warn | None |
| Metric cardinality | Aggregate counters only; never label by Host/path | None |
| Response/body bypass | Out of scope | **High for a WAF claim**; use `stream` |
| Client half-close / EOF mid-headers | Fail closed before dial | None |
| `Expect: 100-continue` | Headers are complete before dial; body and 100-continue are spliced | Plugin cannot inject a custom 100 response in v1 |
| CONNECT/Upgrade | Plugin decides on headers; tunnel content is not inspected | Document as tunnel semantics |

### 6.1 Security positioning

`headers` mode is a **connection-level L7 policy hook**, not a replacement for
`stream`. It is suitable for:

- routing by `Host`/path;
- Basic-auth or API-key checks on the first request;
- blocking obviously bad methods/headers before dial;
- hiding/rewriting request headers.

It is **not** suitable for:

- per-request policy on keep-alive;
- body-based WAF rules;
- response filtering;
- HTTP/2;
- TLS passthrough.

The docs must state this next to the config example, not only in a footnote. If
we cannot accept the keep-alive gap for a given deployment, the answer is
`stream`, not `headers`.

### 6.2 Main downsides to accept explicitly

- **Only the first request of a connection.** This is the largest semantic
  limitation and the easiest way to create a false sense of security.
- **No response or body coverage.** A header-only plugin cannot implement a full
  WAF, response masking, or body signatures.
- **Dial is delayed until the header block is complete.** For routing plugins
  this is required for correctness; for non-routing plugins it is pure latency.
- **16 KiB header ceiling.** Large cookies or JWT headers may exceed it. The
  safe default is to deny; increasing the buffer costs memory on every active
  connection and needs a separate design.
- **HTTP/1.1 only.** HTTP/2, WebSocket frames, CONNECT tunnels and arbitrary
  binary protocols are not covered.
- **TLS passthrough is unsupported.** The feature works on plain HTTP or on a
  TLS-terminating listener, not on `sni_routes` passthrough.
- **A plugin can still be trusted-but-buggy.** The sandbox prevents host memory
  access, but policy mistakes (wrong method list, permissive fallback) remain
  application bugs.
- **More configuration surface.** `headers` is a third mode; docs, validation,
  examples and support must explain when not to use it.

### 6.3 Definition of done

- [ ] `data_mode: headers` is documented in both languages with the limitations
      above.
- [ ] `on_headers` is optional, validated, and covered by tests; old plugins
      still work through fallback.
- [ ] Split headers are collected exactly once and the plugin is called once.
- [ ] The body path is byte-identical and uses the fast path on Linux.
- [ ] Oversized/incomplete/timeout/EOF cases fail closed by default.
- [ ] TLS passthrough + `headers` is rejected at config validation.
- [ ] HTTP/2 preface is rejected in headers mode.
- [ ] `timeout: 0` and NEW-2 scratch leakage are addressed before release.
- [ ] Benchmarks show the expected cost relative to `once` and `stream`.
- [ ] `go test ./...`, race tests and fuzz tests pass.

## 7. Dependencies on the round-2 security report

- **WG-02** (`timeout:0` freezes the process): headers mode must reject a
  zero hook timeout with a default, and the general fix should land before this
  feature is advertised.
- **NEW-2** (scratch residual payload): the header hook must not see bytes from
  a previous connection. Either clear the full scratch before `on_headers`, or
  land the global per-connection clear first. Prefer the global fix.
- **WG-26** (plugin load deadline): not caused by this feature, but a slow/hung
  plugin load still affects reload; keep as a separate prerequisite for
  production readiness.
- **CPSEC-1 / CPDATA-1 / CPDATA-2**: unrelated to the data-plane feature; track
  separately.

## 8. Testing matrix

Host and proxy:

- headers in one read, split at every byte boundary, split after `\r`, `\r\n`,
  before the final empty line;
- body in the same read; body after headers; body absent;
- `max_header_bytes - 1`, exactly max, max + 1;
- EOF, timeout, reset, half-close (all deny);
- plugin allow/deny/route, invalid route;
- plugin mutation of method/header;
- plugin error/panic/trap;
- `timeout: 0` config rejection;
- TLS termination; TLS passthrough with `sni_routes` rejected;
- HTTP/2 preface denied;
- keep-alive second request bypass (documented);
- reload while connections are in flight.

Plugin ABI:

- `on_headers` correct/incorrect signature;
- fallback to `on_data`/`on_data_ex`;
- missing `on_data_ptr`;
- payload > scratch capacity;
- state-only module rejected in headers mode;
- `on_close` accounting includes the prefix.

SDK/parser:

- LF-only, obs-fold, trailing whitespace, duplicate `Content-Length`,
  `Content-Length` + `Transfer-Encoding`, long header line, NUL/control bytes,
  empty header value, no terminator, body after terminator;
- fuzz `parse_request_headers` and the header-boundary reader.

Performance:

- no plugin, `once`, `headers`, `stream`;
- 1 KiB / 16 KiB / 1 MiB;
- req/s, p99, CPU, allocations;
- Linux `strace`/counter confirmation that the body path uses `splice(2)`.

Acceptance criteria:

- default config and all existing plugins behave exactly as before;
- `headers` invokes the plugin once per connection;
- the upstream receives byte-identical headers/body when the plugin does not
  mutate;
- `headers` overhead vs `once` is small for one-read requests and clearly lower
  than `stream` for large bodies;
- all caps/timeouts/error paths fail closed;
- `go test ./...`, race tests, fuzz tests and docs pass.

## 9. Rollout and compatibility

- Default remains `once`; no migration is required.
- New config fields are optional and validated before publish.
- Old plugins work in `headers` mode through the `on_data` fallback; new plugins
  can adopt `on_headers` incrementally.
- New example `.wasm` is committed; Rust is not required to run it.
- No control-plane, database, deployment, or API changes.
- The branch should stay separate from the security fixes; if WG-02/NEW-2 land
  first, rebase on top of them.

## 10. Open questions

Resolved for v1 (see the execution plan):

1. **Mode name:** `headers`.
2. **`fallback_once`:** not implemented; all incomplete headers deny.
3. **Keep-alive:** not rejected; the first-request-only limitation is documented,
   and `stream` remains the per-request option.
4. **`on_headers`:** a separate optional hook, with fallback to `on_data`.
5. **Body bytes:** never passed to the header hook; pass exactly through
   `\r\n\r\n`.

Still open for later versions:

6. Do we need a `max_header_lines` cap in addition to bytes? The byte cap is
   sufficient for memory; the SDK can enforce a line cap.
7. Is 16,389 bytes enough for real-world headers? If not, a separate 32 KiB
   header pool is a follow-up; do not silently grow the existing per-connection
   peek buffer.
8. Do we need a per-request HTTP mode for keep-alive? That is a separate,
   larger HTTP-aware proxy feature.

## 11. Future work

- `data_mode: requests` for HTTP/1.1 keep-alive: host parses request framing,
  calls the plugin per request headers, and relays each body without per-chunk
  WASM calls. This is a larger HTTP-aware proxy feature, not a small extension.
- response-header inspection;
- HTTP/2 support;
- generic `prefix` mode for non-HTTP protocols;
- plugin-controlled incremental inspection only if a real custom-protocol use
  case appears; it should be a separate experimental mode with hard caps, not a
  change to `once`.

## 12. Summary

The cheapest useful feature is not “let the plugin ask for more chunks”. It is
“let the host deliver one complete header block, then get out of the way”. That
keeps the fast path, keeps plugins simple, preserves `stream` for full
inspection, and has a small, auditable security surface.
