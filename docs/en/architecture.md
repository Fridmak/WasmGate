# Architecture

WasmGate is split into a **data plane** (the proxy process) and an optional
**control plane** that only stores desired state and pushes releases. The data
plane never calls the control plane on the request path.

```
client ── TCP ──► listener ──► rate limit / max_conns ──► pool selection
                                   │                          │
                                   ▼                          ▼
                             WASM on_accept               upstream dial
                                   │                          │
                                   └──────── relay ───────────┘
                                      io.Copy / splice(2)
```

## Connection lifecycle

1. **Accept.** The listener checks `max_conns`, then the per-IP/subnet sliding
   window (`rate_limit`). Rejections are counted in metrics and never reach a
   plugin or a backend.
2. **Plugins (L4).** `on_accept` receives the canonical client IP and port and may
   deny the connection before any upstream dial.
3. **Pool selection.** `round_robin` or `least_conn` picks a healthy member. Dial
   failures are retried on other members according to `dial_retries`.
4. **TLS.** With `tls` the proxy terminates TLS and the relay sees plaintext; with
   `sni_routes` it reads only the ClientHello, picks a pool and replays the bytes,
   so end-to-end TLS and `splice(2)` are preserved.
5. **Relay.** On Linux the plain path uses `io.Copy`, which reaches
   `TCPConn.ReadFrom` → `splice(2)`: payload never enters user space. On other
   platforms the same loop gets one buffer per direction from a pool.
6. **Close.** Bytes sent and received are recorded, `on_close` runs, race-free
   counters are updated.

## Hot reload

The running configuration is an immutable **snapshot**: listeners, pools,
limiters and plugins. `SIGHUP` or `POST /reload` parses and validates a new
document into a new snapshot, then swaps it atomically; in-flight connections
finish on the old snapshot. A document that fails validation is rejected before
the swap, so a typo cannot take the process down. Reload under load is covered by
a test with zero dropped connections.

## Packages

| Package | Responsibility |
|---|---|
| `internal/config` | YAML schema (`KnownFields`), defaults, validation, `Resolve` for relative paths |
| `internal/proxy` | listeners, snapshot, pools/balancers, limiter, relay, drain |
| `internal/plugin` | `wazero` runtime, instance pool, ABI v1–v3, memory/timeout limits |
| `internal/plugin/abi4` | ABI v4 core: envelope, inspection plan, `on_event` engine; used by `data_mode: headers` |
| `internal/tlspeek` | ClientHello parser for SNI routing |
| `internal/ratelimit` | Sliding-window limiter and concurrent-connection guard |
| `internal/observe` | Structured logs, Prometheus metrics, admin HTTP endpoint |
| `internal/controlplane` | Enroll, mTLS session, heartbeats, release apply, certificate renewal |

## Design constraints

- **No CGO.** The binary is static and cross-compiles; the sandbox is pure Go
  (`wazero`), with no WASI and no syscalls for plugin guests.
- **Allocation-free fast path.** Balancer, limiter, snapshot lookup and plugin
  hooks perform zero allocations per connection; buffers are pooled.
- **Fail fast.** Invalid configuration, plugin signatures, memory limits or TLS
  material abort validation before any listener changes.
- **Bounded resources.** `max_conns` per listener, WASM memory pages, plugin
  timeout, connection rate limits and idle timeout are all explicit.
- **Control plane is out of band.** It stores configuration and artifacts, signs
  releases and sends commands over a gRPC session; if it is unavailable, the data
  plane keeps serving the last applied configuration.

See [control plane](control-plane.md) for the managed mode and
[configuration](configuration.md) for every knob.
