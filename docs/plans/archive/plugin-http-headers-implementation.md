# HTTP header inspection — implementation plan (v1)

- **Status:** implemented; historical execution plan, superseded for new work by ABI v4
- **Branch:** `feat/plugin-http-headers` (merged)
- **Base:** `main` @ `8aa95f5`
- **Design rationale:** [`plugin-http-headers.md`](plugin-http-headers.md)
- **Scope owner:** data plane / plugin ABI

This document records the execution plan that was used for the ABI v3
`data_mode: headers` feature. It is kept for design history and security
rationale; new plugin work targets ABI v4.

## 1. Locked v1 decisions

| Area | Decision |
|---|---|
| Config name | `data_mode: headers` |
| Protocol | HTTP/1.x request headers only |
| First request only | Yes; keep-alive requests after the first are spliced without inspection |
| Host parsing | Host finds `\r\n\r\n` only; it does not parse or normalise HTTP |
| Plugin input | Complete request header block, including the final empty line; body is not passed |
| Plugin call count | Exactly one call per connection |
| New hook | Optional `on_headers(i64 conn_id, i32 ptr, i32 len) -> i32` |
| Old plugins | Fallback to `on_data` / `on_data_ex(dir=0)` with the full block |
| Body path | Replayed from the existing peek buffer; `splice(2)` where available |
| Header cap | Default `16384`; hard max `16389` (existing `tlspeek` buffer) |
| Total deadline | `inspect_timeout`, default `3s`, covering all reads |
| Incomplete headers | Always fail closed in v1; no `fallback_once` option |
| Dial timing | After the complete header block and route decision |
| HTTP/2 | Detect the connection preface and deny in headers mode |
| TLS passthrough | Reject `headers` when `sni_routes` is configured without TLS termination |
| TLS termination | Supported; no `splice` in that case, but still one plugin call |
| Hook timeout | `plugin.timeout` must be `> 0` for `headers` mode; validation error otherwise |
| Scratch hygiene | `OnHeaders` clears the full 32 KiB scratch before copying the header block |
| Response/body | Out of scope |
| Keep-alive rejection | Not implemented; documented as a limitation |

### 1.1 Why these limits are deliberate

- No `fallback_once`: a silent fail-open would turn an incomplete header block
  into the exact bypass the feature exists to prevent. Operators who need the
  old behaviour keep `data_mode: once`.
- No `More` verdict: transport framing stays in the host; the plugin gets one
  complete block and does not need to know about TCP segmentation.
- 16 KiB cap: the existing `tlspeek` pool is `5 + 16 KiB`; increasing it would
  add memory to every connection. A larger cap needs a separate buffer design.
- First request only: full per-request inspection needs HTTP message framing
  (Content-Length/chunked), which is a different, larger feature.

## 2. Work breakdown

Each task has a concrete file target, tests and acceptance criteria. Tasks are
ordered by dependency; commit boundaries are suggested in Â§5.

### T0 — prerequisites and baseline (0.5 day)

**T0.1 Baseline benchmark harness**

- Add `internal/proxy/bench_headers_test.go` (or extend an existing benchmark)
  with four cases: no plugin, `once`, `headers`, `stream`.
- Payloads: 1 KiB request, 16 KiB headers split into 2–3 writes, 1 MiB upload.
- Record `ns/op`, allocs/op, bytes/op and, on Linux, whether the body path used
  `splice(2)` (counter or `strace` during the manual run).
- Acceptance: numbers are reproducible and committed only as a comment/PR
  table, not as generated profiles.

**T0.2 Guard rails for this mode**

- `Plugin.OnHeaders` must clear `inst.dataCap` bytes of scratch before copying
  the header block; add a test that a previous call's bytes are not visible.
- `config.Validate` rejects `data_mode: headers` with `plugin.timeout == 0`.
- Acceptance: a headers-mode plugin cannot observe stale scratch and cannot
  freeze the process through a zero hook timeout.

**T0.3 Decision record**

- Keep this file and the design file in sync; add any new decisions to Â§1.
- Acceptance: no implementation detail contradicts the locked decisions.

### T1 — buffered prefix support in `tlspeek` (1 day)

Files: `internal/tlspeek/sni.go`, `internal/tlspeek/sni_test.go`.

**T1.1 Add `Bytes()`**

```go
// Bytes returns the unread prefix without consuming it.
func (c *Conn) Bytes() []byte
```

- Returns `nil` or an empty slice when the buffer is released.
- No allocation.

**T1.2 Add `Fill()`**

```go
// Fill reads more bytes into the existing buffer, up to capacity.
// It returns (0, io.EOF) at peer EOF and does not consume the prefix.
func (c *Conn) Fill() (int, error)
```

- Reads into `c.data[len:cap(c.data)]` from `c.Conn`.
- Extends `c.data` only by the number of bytes read.
- Returns `(0, nil)` only if there is no space; callers treat that as overflow.
- Preserves the pooled buffer ownership: it is released exactly once, by
  `DrainTo` or `Close`.
- Existing `Read` continues to consume the prefix; `Fill` and `Read` must not be
  mixed on the same connection.
- Tests:
  - append within capacity;
  - fill to capacity and overflow;
  - EOF on a closed pipe;
  - `DrainTo` after `Fill` writes every byte exactly once;
  - double `Close` does not double-release;
  - `Fill` after `DrainTo` returns an error rather than panicking.

**T1.3 Header reader helper**

Add `internal/proxy/headers.go`:

```go
// readHeaderBlock reads until the HTTP/1 header terminator, the byte cap or the
// total deadline. It returns the complete prefix, the header slice and an error.
func readHeaderBlock(c *tlspeek.Conn, max int, deadline time.Time) (prefix, headers []byte, err error)
```

- Use `bytes.Index(prefix, []byte("\r\n\r\n"))`.
- Set one absolute read deadline; do not reset it per read.
- Detect the HTTP/2 preface before scanning:
  `PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n`. If the prefix is shorter than the
  preface, keep reading until it matches, diverges, or the deadline/cap is
  reached; only the exact full preface is denied as HTTP/2. This avoids
  rejecting a legitimate HTTP/1 request whose first bytes are a prefix of the
  HTTP/2 marker.
- Return a typed error or sentinel for: overflow, timeout, EOF, malformed
  preface. The proxy maps all of them to deny.
- Unit tests with `net.Pipe`:
  - headers in one write;
  - split at every byte boundary around the terminator;
  - header exactly at cap;
  - cap + 1;
  - no terminator + timeout;
  - EOF before terminator;
  - HTTP/2 preface;
  - binary payload containing CRLFCRLF (must be rejected by the HTTP/2 check or
    by the plugin; the host does not claim HTTP parsing).

Acceptance: no new allocation per connection beyond the existing peek buffer;
`go test ./internal/tlspeek ./internal/proxy` passes.

### T2 — configuration and validation (0.5 day)

Files: `internal/config/listener_config.go`,
`internal/config/listener_config_test.go`, `internal/config/config_test.go`,
`internal/proxy/state.go` (`samePluginConfig`).

**T2.1 Add fields to `PluginConfig`**

```go
MaxHeaderBytes int           `yaml:"max_header_bytes"`
InspectTimeout time.Duration `yaml:"inspect_timeout"`
```

`DataMode` already exists; extend its comment.

**T2.2 Defaults**

- `MaxHeaderBytes == 0` â†’ `16384`.
- `InspectTimeout == 0` â†’ `3s`.
- Reject negative values and values outside `1..16389`.
- `data_mode: headers` + `plugin.timeout == 0` â†’ error with a message pointing
  to `plugin.timeout` and WG-02.
- `data_mode: headers` + no plugin â†’ error.
- `data_mode: headers` + `sni_routes` and no TLS termination â†’ error.
- `data_mode: headers` + `sni_routes` and TLS termination â†’ allowed, because
  `sni_routes` is ignored on terminated listeners; document this.

**T2.3 Cache identity**

Update `samePluginConfig` in `internal/proxy/state.go` to include
`MaxHeaderBytes` and `InspectTimeout`; otherwise a reload with changed caps
would reuse the old compiled plugin and the old config semantics.

**T2.4 Tests**

- table-driven `Validate` tests for every new field;
- YAML parse test with the full plugin block;
- reload test: changing `max_header_bytes` invalidates the cached plugin;
- reload test: invalid headers config is rejected without publishing a snapshot.

Acceptance: `-validate` fails fast with a useful message; no partial snapshot.

### T3 — plugin ABI and snapshot validation (1 day)

Files: `internal/plugin/sandbox.go`, `internal/plugin/sandbox_test.go`,
`internal/proxy/snapshot.go`, `internal/proxy/state.go`.

**T3.1 New hook**

- `hookOnHeaders = "on_headers"`.
- Validate signature `(i64, i32, i32) -> i32`.
- Add `hasHeaders` to `exportFlags` and `Plugin`.
- Require `on_data_ptr` when `hasHeaders` is set.
- Export `HasHeaders()` and `HasDataHook()` (or one `CanInspectHeaders()`).

**T3.2 `OnHeaders`**

```go
func (p *Plugin) OnHeaders(ctx context.Context, connID uint64, payload []byte) (Verdict, error)
```

- If `hasHeaders`: copy payload into scratch, call `on_headers`, copy back only
  `len(payload)` bytes, apply the same verdict/route handling as `OnDataConn`.
- Clear the full `dataCap` before the copy, including the fallback path, so a
  previous connection's scratch bytes are never visible.
- If no `on_headers`: delegate to `OnDataConn(ctx, connID, 0, payload)` **after**
  the clear (or make `OnDataConn` take a `clearScratch` flag used only by the
  headers path).
- Reject payloads larger than `ScratchCapacity` with a deny/error.
- Clear `dataCap` bytes of scratch before the copy (T0.2).
- Apply `p.timeout` exactly as `OnDataConn` does.
- Preserve the existing discarded-instance behaviour on trap/timeout.

**T3.3 Snapshot-time validation**

- In `registry.preparePlugin` or `Runtime.build`, after `plugin.New`, check:
  - `data_mode: headers` requires `CanInspectHeaders()`;
  - `data_mode: headers` with only `on_data_state` is rejected;
  - `data_mode: once|stream` keep the existing requirements.
- `samePluginConfig` already updated in T2.3.
- Acceptance: an incompatible plugin fails `-validate` and reload before any
  socket changes.

**T3.4 Tests**

- signature valid/invalid;
- `on_headers` called once with the full block;
- fallback to `on_data_ex(dir=0)`;
- mutation is copied back;
- route verdict selects the expected pool;
- missing `on_data_ptr`;
- empty payload;
- payload > scratch capacity;
- timeout/trap discards the instance;
- stale scratch is zeroed;
- module with only `on_data_state` rejected in headers mode;
- `on_headers` plus `on_data` uses the right hook per mode.

Acceptance: `go test ./internal/plugin` and race tests pass.

### T4 — proxy integration (1.5 days)

Files: `internal/proxy/proxy.go`, `internal/proxy/proxy_test.go`,
`internal/proxy/headers.go`, `internal/proxy/copy_test.go` (bench).

**T4.1 Branch in `Handle`**

Current order: TLS termination / SNI routes / plugin once / dial / replay / join.
Insert a headers branch before the existing once branch:

```text
if plugin != nil && plugin.CanInspectHeaders():
    switch dataMode:
    case "headers":
        prefix, headers, err := readHeaderBlock(...)
        if err != nil: metric/log; return          // fail closed
        verdict := plugin.OnHeaders(ctx, connID, headers)
        if deny: metric/log; return
        if route: select route pool, validate index
        dial
        write prefix to upstream (via DrainTo)
        prefixCount = len(prefix)
        join (streamPlugin = nil)
    case "stream": existing path
    default: existing once path
```

- Keep the existing `once` path untouched for compatibility.
- Keep the existing `stream` path untouched.
- For `headers`, the plugin must be called before dial.
- If the plugin mutates headers, `DrainTo` writes the mutated prefix.
- Count `prefixCount` in `tx` for `on_close`.
- Reuse the existing `RoutePools` and `join` code; do not duplicate route logic
  if it can be extracted into a small helper.

**T4.2 Failure handling**

- Overflow, timeout, EOF, HTTP/2 preface â†’ deny/close before dial.
- Plugin error/trap â†’ `PluginDiscard`, `RejectPlugin`, close.
- Unknown route â†’ deny and reject.
- No upstream dial in any failure path.
- Log through `p.sampler` to avoid log flooding.
- Add `wasmgate_plugin_header_incomplete_total` (no labels) to `observe.Metrics`
  and a `HeaderIncomplete()` method; expose it in the metrics endpoint/docs.
  This is optional but useful for detecting slowloris/oversized headers.
- If the metric is added, update `internal/observe/metrics_test.go` and both
  metrics docs.

**T4.3 Tests**

- split headers in 1, 2 and many reads;
- header exactly at cap, cap + 1;
- body in the same read is forwarded byte-for-byte but not passed to the plugin;
- plugin mutation of method/header reaches the upstream;
- body bytes after headerEnd remain untouched;
- allow, deny, route, invalid route;
- no dial on deny/timeout/EOF/HTTP/2;
- TLS-terminated listener;
- `sni_routes` + headers rejected at config validation;
- keep-alive second request is not inspected (assert current documented behavior);
- `on_close` sees the prefix in tx bytes;
- reload while a connection is in flight uses the old snapshot;
- race: `go test -race ./internal/proxy`.

**T4.4 Benchmarks**

- Compare no plugin / once / headers / stream for 1 KiB, 16 KiB and 1 MiB.
- Confirm the body path is still the fast path on Linux.
- Acceptance: `headers` is close to `once` for one-read headers and clearly
  cheaper than `stream` for large uploads; record the numbers in the PR.

### T5 — strict SDK parser and example (1 day)

Files: `plugins/examples/common/src/lib.rs`, new
`plugins/examples/http_headers_guard/`, `plugins/examples/Cargo.toml`,
`plugins/examples/README.md`, `configs/showcase.yaml`.

**T5.1 Strict parser**

Add to the common crate:

```rust
pub fn parse_request_headers(payload: &[u8]) -> Option<Request<'_>>;
```

- Requires `\r\n\r\n`.
- Rejects LF-only lines.
- Validates method token, target non-empty, version `HTTP/1.x`.
- Validates header-name token characters and no bare CR/LF.
- Keeps `Request` zero-copy.
- Adds helpers if missing: `header_count`, `header_value`, `content_length`,
  `transfer_encoding`, `is_http`.
- Unit tests or host-loaded example tests for the cases in the design doc.

**T5.2 Example plugin**

- New `http_headers_guard` with `on_headers` + `on_data_ptr`.
- Policy: allowed methods, blocked paths, required auth, duplicate CL/TE
  rejection, malformed headers deny.
- Committed `.wasm` built from the source.
- Add a config block to `configs/showcase.yaml`.
- Keep `http_guard` as the `stream` example (it also filters responses).
- Acceptance: `wasmgate -validate -config configs/showcase.yaml` passes and the
  example is loadable from the proxy tests.

**T5.3 Docs for plugin authors**

- Update `docs/en/plugins.md` and `docs/ru/plugins.md`:
  - new hook table row;
  - data-mode table;
  - header-mode limitations;
  - build command for the new example;
  - warning that `stream` is required for body/response/per-request policy.

### T6 — documentation and config samples (0.5 day)

- `docs/en/configuration.md` / `docs/ru/configuration.md`: new fields and
  defaults.
- `configs/wasmgate.yaml`: commented `data_mode: headers` example.
- `configs/showcase.yaml`: working example.
- `deployments/observability/README.md` and dashboard: only if
  `wasmgate_plugin_header_incomplete_total` is added.
- Update `docs/plans/archive/plugin-http-headers.md` if any decision changes.

### T7 — verification and hardening (1 day)

Run in this order:

```bash
gofmt -l cmd internal
go vet ./...
go test ./...
CGO_ENABLED=1 go test -race ./...      # Linux or Docker golang image if gcc is absent
go test -run '^$' -bench 'Headers|Plugin|Proxy' ./internal/proxy ./internal/plugin
git diff --check
```

Manual/e2e checks:

1. Build `wasmgate`; `wasmgate -validate -config configs/showcase.yaml`.
2. Start a local HTTP upstream and a listener with `data_mode: headers`.
3. Send headers split into 2 and 3 writes; verify the plugin sees the complete
   block and the upstream receives the exact bytes.
4. Send a body in the same write; verify body bytes are unchanged.
5. Deny a request; verify no upstream connection was made.
6. Route by `Host`; verify the selected pool receives the request.
7. HTTP/2 preface; verify deny.
8. Oversized headers; verify deny and metric.
9. Slowloris (no terminator); verify deny within `inspect_timeout`.
10. Keep-alive second request; verify it bypasses the plugin (documented).
11. TLS termination; verify headers mode works.
12. `sni_routes` + headers; verify startup rejection.
13. Metrics endpoint contains the new counter if added.
14. Race test on `internal/proxy`, `internal/plugin`, `internal/tlspeek`.

Acceptance:

- all existing tests pass;
- new tests cover every failure path;
- `headers` calls the plugin once per connection;
- default `once`/`stream` behavior is unchanged;
- docs are consistent in English and Russian;
- no new dependency, no control-plane change, no host imports.

## 3. Commit and PR strategy

Keep the PR reviewable with focused commits:

1. `feat(tlspeek): add buffered prefix support`
2. `feat(config): add headers data mode settings`
3. `feat(plugin): add optional on_headers hook`
4. `feat(proxy): inspect complete HTTP request headers`
5. `feat(plugins): add strict HTTP header parser and example`
6. `docs(plugins): document headers mode`
7. `test(proxy): cover headers mode and benchmarks` (or fold into 4)

Before opening the PR:

- rebase on `main`;
- run the full verification suite;
- include benchmark numbers and the security limitations in the PR description;
- mention that the design doc is the rationale and this file is the executed plan.

## 4. Security gates (must be green before merge)

- [ ] `headers` + `timeout: 0` rejected.
- [ ] `OnHeaders` clears the full scratch before use.
- [ ] `headers` + `sni_routes` without TLS termination rejected.
- [ ] HTTP/2 preface rejected.
- [ ] Oversized, incomplete, EOF and timeout paths deny before dial.
- [ ] No upstream dial on plugin deny/error.
- [ ] Header block passed to the plugin excludes the body.
- [ ] Body bytes are forwarded unchanged when the plugin does not mutate headers.
- [ ] New hook has no host imports; sandbox limits unchanged.
- [ ] Fuzz tests for the host header reader and the strict parser.
- [ ] Docs explicitly say “first request only, HTTP/1.x, no body/response”.
- [ ] No `fallback_once` in v1.

## 5. Risks and fallbacks

| Risk | Mitigation |
|---|---|
| Header mode creates a false sense of full L7 security | Docs and PR text state the scope; `stream` remains the full filter |
| Dial latency increases for split headers | Measure; if unacceptable, add a separate non-routing “dial in parallel” optimization, not a semantic change |
| 16 KiB cap too small | Fail closed; a 32 KiB header pool is a separate design with memory accounting |
| Plugin parser differs from upstream | Host does not parse or normalise; SDK parser is strict; recommend `stream` where differential risk matters |
| Keep-alive bypass | Document; do not claim WAF coverage; future `requests` mode |
| Plugin bug causes deny/freeze | Hook timeout mandatory; WG-02 general fix still tracked separately |
| Scratch leak | Clear full scratch in `OnHeaders`; NEW-2 global fix remains separate |
| HTTP/2 or TLS passthrough | Startup/runtime rejection; tests |
| Reload cache misses new fields | Update `samePluginConfig`; reload tests |

## 6. Estimated schedule

| Day | Work |
|---|---|
| 1 | T0 baseline, T1 `tlspeek` buffer + tests |
| 2 | T1 header reader, T2 config/validation |
| 3 | T3 ABI + snapshot validation + tests |
| 4 | T4 proxy integration + core tests |
| 5 | T4 remaining tests, T5 parser/example |
| 6 | T5 example build, T6 docs |
| 7 | T7 fuzz/race/bench/e2e, PR cleanup |

Total: **5–7 working days**; add buffer for review and for any WG-02/NEW-2
interaction.

## 7. Definition of done

- `data_mode: headers` works end to end on plain HTTP and TLS termination.
- Complete headers are delivered once; body is untouched and uses the fast path
  where available.
- All security gates in Â§4 pass.
- Old plugins and configurations are unchanged.
- English and Russian docs describe the exact scope and limitations.
- Benchmarks are recorded and the PR explains the trade-off versus `once` and
  `stream`.
- The branch is rebased on `main`, tests are green, and the PR is ready for
  review.
