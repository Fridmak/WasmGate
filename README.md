# WasmGate

**WasmGate** is a TCP/L4 reverse proxy written in Go with WebAssembly plugins and
an optional control plane for fleets of edge nodes. It is a single static binary
(no CGO), keeps the fast path allocation-free, and uses Linux `splice(2)` so a
plain relay never copies payload through user space.

[Русская версия](README.ru.md) · [Documentation](#documentation)

## Why

- **Raw TCP stays raw.** No HTTP parsing on the relay path, so any protocol works:
  PostgreSQL, Redis, SSH, gRPC, arbitrary binary framing.
- **Zero-copy on Linux.** A plain relay reaches `splice(2)` through the standard
  `io.Copy` path; buffers are not allocated per connection.
- **Programmable when needed.** WASM hooks run on accept, on data and on close,
  with a strict sandbox: no WASI, a hard memory cap and a per-hook deadline
  (1s by default).
- **Operations built in.** Hot reload on `SIGHUP`/`POST /reload`, Prometheus
  metrics, health endpoint, rate limiting, health checks and a circuit breaker.
- **Fleet management (optional).** A .NET control plane stores configuration
  versions, signs releases, enrolls agents over mTLS and deploys in waves.

## Features

| Area | Capability |
|---|---|
| Listeners | TCP, multiple listeners, per-listener dial/idle timeouts and connection limits |
| Pools | `round_robin` and `least_conn`, dial retries across healthy members |
| Health | Active TCP probes, passive marking of failed upstreams |
| Resilience | Optional three-state circuit breaker (closed/open/half-open) |
| Rate limiting | Per-IP and per-subnet sliding window, concurrent limits, bans, CIDR allow list |
| TLS | SNI passthrough with pool routing, or termination with certificate hot reload |
| WASM | ABI v4 `on_event` for `once`, `headers` and `stream`, plus ABI v4 `on_accept`/`on_close` lifecycle |
| Observability | Admin endpoint (`/metrics`, `/healthz`, `/reload`), structured logs, pprof on demand |
| Control plane | Enrollment, certificates, immutable configurations, signed releases, wave deployments, rollback, audit, admin UI |

## Performance

Linux (WSL2, kernel 6.6.87, 4 vCPU, `-benchtime=1s -benchmem`), same TCP echo
backend for every row:

| Scenario / Metric | direct | WasmGate L4 | nginx stream | HAProxy TCP |
|---|---:|---:|---:|---:|
| Throughput, 64 KiB | 644.6 MB/s | **588.8 MB/s** | 272.5 MB/s | 268.9 MB/s |
| Throughput, 1 MiB | 865.6 MB/s | **688.3 MB/s** | 230.9 MB/s | 248.3 MB/s |
| Relay latency (1 byte RTT) | 29.2 µs | **62.0 µs** | 394.0 µs | 439.2 µs |
| Connection setup | 402.9 µs | **917.7 µs** | 1076.9 µs | 1163.8 µs |

These numbers were captured on 2026-11-05 (commit `18d4249`), before the ABI v4
cutover. Re-run `make bench` or the Benchmarks workflow on the current tree before
using them; the v4 hook path differs.

WSL2 runs a real Linux kernel, so `splice(2)` and the relative numbers are valid;
absolute MB/s are not comparable with bare metal. Reproduce with `make bench` or
see [benchmarks](docs/en/benchmarks.md) for methodology, raw output and the
Windows comparison.

## Quick start

```bash
# Build a static binary
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o wasmgate ./cmd/wasmgate

# Validate the example configuration (also compiles every referenced plugin)
./wasmgate -config configs/wasmgate.yaml -validate

# Run it
./wasmgate -config configs/wasmgate.yaml

# The example listens on 127.0.0.1:8080 and exposes metrics on 127.0.0.1:9099
curl http://127.0.0.1:8080/
curl http://127.0.0.1:9099/metrics
```

Minimal configuration:

```yaml
version: 1
admin_addr: "127.0.0.1:9099"

listeners:
  - name: edge
    addr: "0.0.0.0:443"
    upstream_pool: web
    max_conns: 10000

pools:
  web:
    balance: round_robin
    servers:
      - "10.0.0.10:8080"
      - "10.0.0.11:8080"
```

The full reference is [configuration](docs/en/configuration.md); the example file
`configs/wasmgate.yaml` documents every field.

## Control plane

The optional control plane (`.NET 10`, SQLite) turns the proxy into a managed
fleet: agents enroll with a one-time token, receive an mTLS certificate and
renew it automatically; configuration versions become signed releases deployed in
waves. A built-in admin UI is served on the admin port.

On one machine, `WASMGATE_CP_LOCAL=true` and `wasmgate -cp-local` skip TLS,
agent certificates and enrollment tokens entirely; the control plane and data
plane talk over loopback. This mode trusts every local process, so use it only
on a single-user host.

```bash
cd control-plane
dotnet build WasmGate.Control.sln
dotnet run --project src/WasmGate.Control.Api
# open http://127.0.0.1:9097/
```

See [control plane](docs/en/control-plane.md) for the schema, API, certificates,
agent protocol and security model.

## Project layout

```
cmd/wasmgate/          entry point and flags
internal/config/       YAML schema, validation, hot reload
internal/proxy/        compatibility facade and end-to-end tests
internal/proxy/runtime/ snapshots, plugin registry and limits
internal/proxy/conn/    connection pipeline and dialing
internal/proxy/server/  listener lifecycle, reload and probes
internal/proxy/backend/ pools, balancing, health and breakers
internal/proxy/relay/  bidirectional relay and idle/splice paths
internal/proxy/httpinspect/ HTTP/1 framing and body policy
internal/plugin/abi4/  ABI v4 runtime: envelope, plan, engine, lifecycle
internal/tlspeek/      SNI parser for passthrough
internal/ratelimit/    sliding-window limiter
internal/observe/      logs, metrics, admin endpoint
internal/controlplane/ control-plane Agent facade
internal/controlplane/options/ shared options
internal/controlplane/identity/ enrollment and certificate files
internal/controlplane/release/ signed releases and artifacts
internal/controlplane/session/ transport, heartbeat and dispatch
control-plane/         C# control plane and tests
plugins/examples/      nine reference plugins (Rust, prebuilt .wasm)
configs/               example and showcase configurations
deployments/           deployment scenarios, Dockerfiles, systemd, dev certificates
benchmarks/            raw benchmark output
docs/en, docs/ru       product documentation in both languages
```

## Documentation

English:
[Architecture](docs/en/architecture.md) ·
[Configuration](docs/en/configuration.md) ·
[Plugins](docs/en/plugins.md) ·
[ABI v4](docs/en/plugin-abi-v4.md) ·
[Migration to v4](docs/en/migration-to-v4.md) ·
[Benchmarks](docs/en/benchmarks.md) ·
[Deployment](docs/en/deployment.md) ·
[Control plane](docs/en/control-plane.md) ·
[Metrics](docs/en/metrics.md) ·
[Development](docs/en/development.md)

Русский:
[Архитектура](docs/ru/architecture.md) ·
[Конфигурация](docs/ru/configuration.md) ·
[Плагины](docs/ru/plugins.md) ·
[Бенчмарки](docs/ru/benchmarks.md) ·
[Развёртывание](docs/ru/deployment.md) ·
[Control plane](docs/ru/control-plane.md) ·
[Метрики](docs/ru/metrics.md) ·
[Разработка](docs/ru/development.md)

## Requirements

- Go 1.26+ (the module pins a 1.27 toolchain).
- Linux is required for zero-copy `splice(2)`; other platforms use a pooled
  buffered relay and work correctly, just slower on bulk transfer.
- The control plane requires .NET 10; its SQLite store is embedded.

## License

MIT. See [LICENSE](LICENSE).
