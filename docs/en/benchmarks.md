# Benchmarks

> **Current run.** The tables below are from 2026-11-19, stable `2ff37ac`.
> Linux was a 4-vCPU container on Docker Desktop/WSL2 (AMD Ryzen 5 3550H,
> Go 1.27.4); Windows was the same host with 8 logical CPUs (Go 1.27.1). Each
> row is the median of three `go test -bench` runs with `-benchtime=1s
> -benchmem`. Absolute MB/s includes container/WSL2 overhead and is not a
> bare-metal claim; relative numbers are the useful part.
>
> The historical pre-v4 raw output remains in
> [`linux-ubuntu-results.txt`](../../benchmarks/linux-ubuntu-results.txt). The
> current raw output is
> [`linux-current-results.txt`](../../benchmarks/linux-current-results.txt),
> [`yandex-current-results.txt`](../../benchmarks/yandex-current-results.txt)
> and [`windows-current-results.txt`](../../benchmarks/windows-current-results.txt).
> The Yandex Cloud table is a clean 2-vCPU reference; CPU generation and core
> count differ from the local 4-vCPU container, so compare shapes rather than
> absolute MB/s across those two tables.

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
pool instead of allocating 32 KiB per direction per connection. The Linux and
Windows tables below use the same echo backend and the same benchmark code.

## WasmGate relay paths

Linux, MB/s by chunk size; every path reports 0 allocations per operation:

| Path | 4 KiB | 64 KiB | 1 MiB |
|---|---:|---:|---:|
| direct (no proxy) | 154.5 | 581.2 | 822.9 |
| zero-copy (`splice(2)`) | **211.0** | **460.1** | **604.7** |
| idle watchdog (buffered) | 247.6 | 240.0 | 232.6 |

Windows uses a user-space copy for both WasmGate paths:

| Path | 4 KiB | 64 KiB | 1 MiB |
|---|---:|---:|---:|
| direct (no proxy) | 128.4 | 318.8 | 322.1 |
| WasmGate relay | 119.0 | 214.7 | 179.4 |
| idle watchdog | 105.3 | 213.8 | 248.6 |

The 4 KiB case is dominated by syscalls and scheduling; the 64 KiB and 1 MiB
cases show the Linux `splice(2)` advantage over a user-space copy.

## Comparison with nginx and HAProxy

Linux container, `nginx` stream module and HAProxy in TCP mode, same echo
backend. Every column is the median of three runs:

| Metric | direct | WasmGate L4 | WasmGate full* | nginx stream | HAProxy TCP |
|---|---:|---:|---:|---:|---:|
| Throughput 64 KiB | **580.6 MB/s** | 452.3 MB/s | 419.5 MB/s | 277.6 MB/s | 233.0 MB/s |
| Throughput 1 MiB | **793.5 MB/s** | 589.3 MB/s | 549.8 MB/s | 269.4 MB/s | 230.4 MB/s |
| Relay latency (1B RTT) | **28.9 µs** | 54.7 µs | 59.5 µs | 527.3 µs | 506.7 µs |

\* full = `max_conns` + `rate_limit` + an ABI v4 lifecycle WASM hook; no TLS
termination, L7 body policy, circuit breaker or dial retries in this run.

nginx stream and HAProxy copy through user space on this configuration, which is
why they trail the Linux `splice(2)` path on bulk traffic. WasmGate's feature
checks run once per connection and allocate nothing on the relay path.

Windows, where nginx/HAProxy were not installed and both WasmGate paths copy in
user space:

| Metric | direct | WasmGate L4 | WasmGate full* |
|---|---:|---:|---:|
| Throughput 64 KiB | **274.5 MB/s** | 218.4 MB/s | 168.3 MB/s |
| Throughput 1 MiB | **274.3 MB/s** | 182.1 MB/s | 157.5 MB/s |
| Relay latency (1B RTT) | **132.8 µs** | 229.0 µs | 204.6 µs |

## Clean Yandex Cloud run

A dedicated preemptible VM (2 vCPU Intel Ice Lake, 4 GiB, 15 GiB HDD,
Ubuntu 24.04, Go 1.27.4, nginx 1.24, HAProxy 2.8.30) gives a cleaner view than
the nested Docker/WSL2 container. Median of three runs:

| Metric | direct | WasmGate L4 | WasmGate full* | nginx stream | HAProxy TCP |
|---|---:|---:|---:|---:|---:|
| Throughput 64 KiB | **2104.2 MB/s** | 1519.3 MB/s | 1571.9 MB/s | 726.6 MB/s | 691.2 MB/s |
| Throughput 1 MiB | **1903.8 MB/s** | 1508.2 MB/s | 1474.0 MB/s | 808.5 MB/s | 697.3 MB/s |
| Relay latency (1B RTT) | **10.3 µs** | 21.4 µs | 21.4 µs | 57.1 µs | 61.4 µs |

The L4/full 64 KiB difference is inside the run-to-run noise: full 1.541–1.587
GB/s, L4 1.492–1.538 GB/s. On 1 MiB L4 is slightly ahead. This run also shows
the 2-vCPU saturation point: throughput scaling is 1563 / 1536 / 1419 MB/s at
1 / 4 / 16 connections, then drops to 409 MB/s at 64 connections.

## ABI v4 data modes

HTTP body relay at 64 KiB, same backend and the same v4 verdict plugin. `once`
and `headers` inspect the opening section and then splice; `stream` invokes the
event hook for every chunk.

| Mode | Linux | Windows | Allocs/op |
|---|---:|---:|---:|
| `once` | **446.9 MB/s** | 192.4 MB/s | 0 |
| `headers` | 422.4 MB/s | **220.5 MB/s** | 0 |
| `stream` | 198.6 MB/s | 188.6 MB/s | 52 (≈3.8 KiB/op) |

`stream` is the expensive mode by design: every chunk rebuilds an event
envelope and calls the guest.

## Throughput scaling

64 KiB chunks, aggregate MB/s as concurrent proxied connections grow:

| Connections | Linux | Windows |
|---:|---:|---:|
| 1 | 433.4 MB/s | 206.5 MB/s |
| 4 | 1066.7 MB/s | 477.3 MB/s |
| 16 | **1103.0 MB/s** | **493.1 MB/s** |
| 64 | 950.3 MB/s | 445.5 MB/s |

Aggregate throughput rises until loopback/CPU saturates, then flattens rather
than collapsing.

## Hot path and microbenchmarks

Median ns/op; all rows are allocation-free unless noted:

| Benchmark | Linux | Windows |
|---|---:|---:|
| `proxy/ConnHotPath` | 191.8 ns | 166.4 ns |
| `proxy/ConnHotPathParallel` | 143.4 ns | 123.1 ns |
| `ratelimit/GuardAcquireRelease` | 263.6 ns | 301.7 ns |
| `ratelimit/Allow` | 80.3 ns | 95.7 ns |
| `ratelimit/AllowParallel` | 33.2 ns | 22.7 ns |
| `abi4/SplitHeaders` | 290.5 ns (327 MB/s) | 362.6 ns (262 MB/s) |
| `abi4/WriteEnvelope` | 175.6 ns (541 MB/s) | 144.9 ns (656 MB/s) |
| `abi4/OnRequestHeaders` | 5267 ns (18.0 MB/s), 944 B/13 allocs | 5237 ns (18.1 MB/s), 944 B/13 allocs |
| `tlspeek/ParseClientHelloSNI` | 38.0 ns | 40.3 ns |
| `tlspeek/MatchRouteWildcard` | 120.1 ns | 116.5 ns |

`OnRequestHeaders` includes envelope parsing, guest initialization metadata and
the typed OUT response; it is not a per-byte cost. The relay path itself stays
allocation-free.

## What the numbers mean

- `ConnHotPath` is the per-connection CPU before the relay touches a socket:
  snapshot lookup, listener resolution, connection slot, pool pick and release.
- `round_robin` is one atomic increment; `least_conn` scans the pool (O(n)) and
  stays in tens of nanoseconds at 64 servers.
- `Load` is a YAML decode on `SIGHUP`, not per request. Reload under live traffic
  is covered by a test with 12 reloads and zero dropped connections.
- Linux keeps the bulk path on `splice(2)`; feature checks and the ABI envelope
  do not move payload through user space unless TLS termination or a body policy
  requires it.

Raw output: current
[`linux-current-results.txt`](../../benchmarks/linux-current-results.txt) and
[`windows-current-results.txt`](../../benchmarks/windows-current-results.txt);
historical pre-v4
[`linux-ubuntu-results.txt`](../../benchmarks/linux-ubuntu-results.txt).
