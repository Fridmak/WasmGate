# Benchmarks

All benchmarks live next to the code they cover (`X.go` → `X_test.go`) and are
compiled only by `go test`; they are not part of the binary.

```bash
make bench                                            # everything
go test -run '^$' -bench . -benchmem ./...            # same manually
go test -run '^$' -bench BenchmarkThroughput -benchmem ./internal/proxy
```

Compare two runs with `benchstat`:

```bash
go install golang.org/x/perf/cmd/benchstat@latest
go test -run '^$' -bench . -benchmem -count=10 ./... > new.txt
benchstat old.txt new.txt
```

## Platforms

`splice(2)` exists only on Linux. On Windows/macOS the same `io.Copy` falls back
to a user-space copy; WasmGate gives that path one buffer per direction from a
pool instead of allocating 32 KiB per direction per connection. Absolute numbers
from WSL2 are not comparable with bare metal (4 vCPU, virtual NIC, 9p
filesystem), but relative results are: WSL2 runs a real Linux kernel.

## Throughput and latency

Linux (WSL2, kernel 6.6.87, 4 vCPU, `-benchtime=1s -benchmem`) against the same
TCP echo backend:

| Benchmark | Linux | Windows |
|---|---|---|
| `Throughput/zero_copy/64KiB` | **557–725 MB/s**, 0 allocs | 222–253 MB/s |
| `Throughput/idle_watchdog/64KiB` | 257–286 MB/s | 208–314 MB/s |
| `Throughput/zero_copy/1MiB` | **862–977 MB/s** | 262–336 MB/s |
| `Throughput/idle_watchdog/1MiB` | 248–265 MB/s | 282–310 MB/s |
| `Throughput/direct/1MiB` (no proxy) | 902–1092 MB/s | 620–627 MB/s |
| `ConnectionSetup/through_proxy` | 36.0 KB/op, 64 allocs | 37.8–38.5 KB/op, 80 allocs |
| `RelayLatency/through_proxy` | 56–60 µs | 109–120 µs |

On Linux zero-copy is **2.1–2.7×** faster than the buffered path at 64 KiB and
**3.3–3.7×** at 1 MiB — the signature of a working `splice(2)`. On Windows both
variants converge because both copy in user space.

## Comparison with nginx and HAProxy

Ubuntu 24.04 (WSL2), `nginx/1.24.0`, `HAProxy 2.8.30`, same echo backend:

| Metric | direct | WasmGate L4 | WasmGate full* | nginx stream | HAProxy TCP |
|---|---:|---:|---:|---:|---:|
| Throughput 64 KiB | 644.6 MB/s | **588.8 MB/s** | **734.1 MB/s** | 272.5 MB/s | 268.9 MB/s |
| Throughput 1 MiB | 865.6 MB/s | **688.3 MB/s** | **794.9 MB/s** | 230.9 MB/s | 248.3 MB/s |
| Relay latency (1B RTT) | 29.2 µs | **62.0 µs** | **61.1 µs** | 394.0 µs | 439.2 µs |
| Connection setup | 402.9 µs | **917.7 µs** | **1420.9 µs** | 1076.9 µs | 1163.8 µs |

\* full = `max_conns` + `rate_limit` + `ipguard.wasm`; no TLS termination,
`data_mode: stream`, circuit breaker or dial retries in this run.

nginx stream and HAProxy copy through user space on this configuration, which is
why they land at the same throughput as WasmGate's buffered path. WasmGate's
feature checks run once per connection, allocate nothing, and do not move the
bulk path off `splice`.

## Hot-path microbenchmarks

Linux, `0 B/op, 0 allocs/op` everywhere:

| Benchmark | Result |
|---|---|
| `ratelimit/BenchmarkAllow` | 65.4 ns/op (parallel: 20.4 ns/op) |
| `plugin/BenchmarkOnAccept` | 81.3 ns/op (parallel: 24.7 ns/op) |
| `plugin/BenchmarkOnData` (L7) | 76.0 ns/op |
| `plugin/BenchmarkOnDataState` (historical ABI v3 fixture) | 111–122 ns/op |
| `tlspeek/BenchmarkParseClientHelloSNI` | 26.0 ns/op |

The historical stateful fixture added roughly 30 ns over stateless `on_data`.
It is retained only as an old benchmark record; production guests use the ABI
v4 envelope and host-owned state. A complete HTTP body policy costs more
because it parses framing and dispatches typed phases.

## What the numbers mean

- `ConnLimiter/at_limit` is cheaper than `below_limit`: rejection is a load and a
  compare, acceptance needs two CAS operations.
- `least_conn` scans the pool (O(n)); at 64 servers it stays in tens of
  nanoseconds. `round_robin` is flat because the cost is one atomic increment.
- Connection setup allocates ~36–38 KB/op, mostly sockets and the echo backend,
  not relay buffers.
- `Load` is a YAML decode and happens on `SIGHUP`, not per request.
- Reload under live traffic is covered by a test with 12 reloads and zero dropped
  connections.

Raw output: [`linux-ubuntu-results.txt`](../../benchmarks/linux-ubuntu-results.txt).
