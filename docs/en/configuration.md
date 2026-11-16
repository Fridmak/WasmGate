# Configuration

WasmGate reads one YAML document. The same document is used by `-validate`, the
initial load and every hot reload, so a configuration that passes validation will
load identically at runtime.

```bash
wasmgate -config /etc/wasmgate/wasmgate.yaml -validate
wasmgate -config /etc/wasmgate/wasmgate.yaml
kill -HUP $(pidof wasmgate)          # or: curl -X POST http://127.0.0.1:9099/reload
```

Annotated example: [`configs/wasmgate.yaml`((../configs/wasmgate.yaml).
Showcase with every plugin: [`configs/showcase.yaml`((../configs/showcase.yaml).

## Top level

| Key | Type | Default | Meaning |
|---|---|---|---|
| `version` | int | — | Schema version; must be `1` |
| `admin_addr` | string | empty (disabled) | `host:port` for `/metrics`, `/healthz`, `/reload`, optional pprof |
| `admin_token_file` | string | empty | File with a bearer token required by the admin endpoint (or `WASMGATE_ADMIN_TOKEN`) |
| `admin_pprof` | bool | `false` | Enable `/debug/pprof/` (local debugging only) |
| `admin_allow_remote` | bool | `false` | Allow a non-loopback `admin_addr` |
| `listeners` | list | — | Front-facing TCP listeners, see below |
| `pools` | map | — | Backend groups keyed by name |

Unknown keys are rejected (`KnownFields(true)`), duplicate listener names,
duplicate addresses and references to missing pools fail validation.

## Listeners

| Key | Type | Default | Meaning |
|---|---|---|---|
| `name` | string | — | Unique name used in logs, metrics and errors |
| `addr` | string | — | Listen address, e.g. `0.0.0.0:443` |
| `upstream_pool` | string | — | Pool that receives the connection |
| `max_conns` | int | `0` (unlimited) | Concurrent connection cap; excess is rejected and counted |
| `dial_timeout` | duration | `5s` | Upstream dial timeout |
| `idle_timeout` | duration | `0` (off) | Idle deadline refreshed by activity in either direction; `0` keeps `splice(2)` |
| `rate_limit` | object | off | Per-IP/subnet sliding window, see below |
| `plugin` | object | off | WASM plugin for this listener |
| `sni_routes` | map | empty | `host` or `*.domain` → pool, for TLS passthrough |
| `tls` | object | off | TLS termination (disables `splice` and `sni_routes` for this listener) |

### `rate_limit`

| Key | Default | Meaning |
|---|---:|---|
| `max` | `0` (off) | New connections per IP per window |
| `subnet_max` | `0` (off) | New connections per subnet per window |
| `max_concurrent` | `0` (off) | Concurrent connections per IP |
| `subnet_max_concurrent` | `0` (off) | Concurrent connections per subnet |
| `window` | `1s` | Sliding window length (minimum 10 ms) |
| `ban_duration` | `0` (off) | Base ban on violation; doubles up to 16× |
| `allow_cidrs` | empty | CIDRs that bypass counting |
| `ipv6_host_prefix` | `64` | IPv6 grouping for the per-IP limit |
| `ipv6_subnet_prefix` | `56` | IPv6 grouping for the per-subnet limit |

### `plugin`

| Key | Default | Meaning |
|---|---|---|
| `path` | — | `.wasm` file; empty disables the plugin |
| `max_memory_pages` | `16` | Linear-memory cap per instance (16 × 64 KiB = 1 MiB) |
| `timeout` | `0` | Per-hook timeout; `0` disables the timer (trusted plugins only) |
| `data_mode` | `once` | `once` inspects the first client packet and keeps `splice`; `stream` inspects every chunk |
| `route_pools` | empty | Pools selectable by an `on_data` return value `2..N` |

See [plugins((plugins.md) for the ABI and sandbox limits.

### `tls`

| Key | Default | Meaning |
|---|---|---|
| `cert_file` | — | PEM certificate chain |
| `key_file` | — | PEM private key |
| `min_version` | `1.2` | `1.2` or `1.3` |
| `handshake_timeout` | `10s` | Handshake deadline |

Certificates are hot-reloaded with the rest of the document.

## Pools

| Key | Default | Meaning |
|---|---|---|
| `balance` | `round_robin` | `round_robin` or `least_conn` |
| `servers` | — | `host:port` list |
| `dial_retries` | `0` | Extra attempts on other healthy members |
| `health_check` | off | Active TCP probes |
| `circuit_breaker` | off | Three-state passive breaker |

`health_check`:

| Key | Default | Meaning |
|---|---|---|
| `interval` | `0` (off) | Probe period |
| `timeout` | `1s` | Per-probe dial timeout |

`circuit_breaker`:

| Key | Default | Meaning |
|---|---|---|
| `error_threshold` | `0` | Consecutive failures to open; `0` opens after the first |
| `open_duration` | `10s` | Time in Open before a half-open probe |
| `half_open_requests` | `1` | Consecutive successes to close |

## Semantics

- **Relative paths.** Plugin and TLS paths are resolved against the directory of
  the configuration file, not the process working directory. A configuration
  applied by the control plane therefore works with artifacts stored next to it.
- **Atomic reload.** A new document is validated completely, then swapped into a
  new snapshot; in-flight connections finish on the old one.
- **Fail fast.** Invalid YAML, unknown fields, missing pools, broken TLS material
  or a plugin that violates the ABI/memory limits abort the reload with the old
  configuration still serving.
- **Admin endpoint.** Keep `admin_addr` on loopback. A non-loopback address
  requires `admin_allow_remote: true` **and** a token; otherwise startup fails.
