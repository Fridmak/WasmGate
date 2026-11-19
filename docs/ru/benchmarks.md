# Бенчмарки

> **Актуальный прогон.** Таблицы ниже — от 2026-11-19, stable `2ff37ac`.
> Linux — контейнер на 4 vCPU в Docker Desktop/WSL2 (AMD Ryzen 5 3550H,
> Go 1.27.4); Windows — тот же хост с 8 логическими CPU (Go 1.27.1). Каждая
> строка — медиана трёх прогонов `go test -bench` с `-benchtime=1s -benchmem`.
> Абсолютные MB/s включают накладные расходы контейнера/WSL2 и не являются
> обещанием bare-metal; полезны относительные цифры. Исторический pre-v4 вывод
> остаётся в [`linux-ubuntu-results.txt`](../../benchmarks/linux-ubuntu-results.txt),
> актуальный — в [`linux-current-results.txt`](../../benchmarks/linux-current-results.txt)
> и [`windows-current-results.txt`](../../benchmarks/windows-current-results.txt).

Все бенчмарки лежат рядом с кодом (`X.go` → `X_test.go`) и собираются только
`go test`; в бинарник они не входят.

```bash
make bench                                            # всё
go test -run '^$' -bench . -benchmem ./...            # то же вручную
go test -run '^$' -bench BenchmarkThroughput -benchmem ./internal/proxy
```

Сравнить два прогона через `benchstat`:

```bash
go install golang.org/x/perf/cmd/benchstat@latest
go test -run '^$' -bench . -benchmem -count=10 ./... > new.txt
benchstat old.txt new.txt
```

## Платформы

`splice(2)` есть только на Linux. На Windows/macOS тот же `io.Copy` уходит в
user-space copy; WasmGate даёт этому пути один буфер на направление из пула,
а не выделяет 32 KiB на направление на каждое соединение. Linux- и
Windows-таблицы ниже используют один и тот же echo-бэкенд и один и тот же код
бенчмарков.

## Пути relay в WasmGate

Linux, MB/s в зависимости от размера чанка; все пути — 0 аллокаций на
операцию:

| Путь | 4 KiB | 64 KiB | 1 MiB |
|---|---:|---:|---:|
| direct (без прокси) | 154.5 | 581.2 | 822.9 |
| zero-copy (`splice(2)`) | **211.0** | **460.1** | **604.7** |
| idle watchdog (buffered) | 247.6 | 240.0 | 232.6 |

Windows использует user-space copy для обоих путей WasmGate:

| Путь | 4 KiB | 64 KiB | 1 MiB |
|---|---:|---:|---:|
| direct (без прокси) | 128.4 | 318.8 | 322.1 |
| WasmGate relay | 119.0 | 214.7 | 179.4 |
| idle watchdog | 105.3 | 213.8 | 248.6 |

На 4 KiB доминируют syscall’ы и планировщик; на 64 KiB и 1 MiB видно
преимущество Linux `splice(2)` над user-space copy.

## Сравнение с nginx и HAProxy

Linux-контейнер, nginx с stream-модулем и HAProxy в TCP-режиме, тот же
echo-бэкенд. Каждая колонка — медиана трёх прогонов:

| Метрика | direct | WasmGate L4 | WasmGate full* | nginx stream | HAProxy TCP |
|---|---:|---:|---:|---:|---:|
| Throughput 64 KiB | **580.6 MB/s** | 452.3 MB/s | 419.5 MB/s | 277.6 MB/s | 233.0 MB/s |
| Throughput 1 MiB | **793.5 MB/s** | 589.3 MB/s | 549.8 MB/s | 269.4 MB/s | 230.4 MB/s |
| Relay latency (1B RTT) | **28.9 µs** | 54.7 µs | 59.5 µs | 527.3 µs | 506.7 µs |

\* full = `max_conns` + `rate_limit` + ABI v4 lifecycle WASM hook; без TLS
termination, L7 body policy, circuit breaker и dial retries в этом прогоне.

nginx stream и HAProxy в этой конфигурации копируют через user space, поэтому
на bulk-трафике отстают от Linux-пути `splice(2)`. Проверки WasmGate
выполняются один раз на соединение и не аллоцируют на relay-пути.

Windows, где nginx/HAProxy не устанавливались, а оба пути WasmGate копируют в
user space:

| Метрика | direct | WasmGate L4 | WasmGate full* |
|---|---:|---:|---:|
| Throughput 64 KiB | **274.5 MB/s** | 218.4 MB/s | 168.3 MB/s |
| Throughput 1 MiB | **274.3 MB/s** | 182.1 MB/s | 157.5 MB/s |
| Relay latency (1B RTT) | **132.8 µs** | 229.0 µs | 204.6 µs |

## Режимы данных ABI v4

HTTP body relay на 64 KiB, тот же бэкенд и тот же v4 verdict-плагин. `once` и
`headers` инспектируют opening-часть и затем переходят на splice; `stream`
вызывает hook на каждом чанке.

| Режим | Linux | Windows | Аллокации/op |
|---|---:|---:|---:|
| `once` | **446.9 MB/s** | 192.4 MB/s | 0 |
| `headers` | 422.4 MB/s | **220.5 MB/s** | 0 |
| `stream` | 198.6 MB/s | 188.6 MB/s | 52 (≈3.8 KiB/op) |

`stream` дорогой по дизайну: каждый чанк заново собирает event-envelope и
вызывает guest.

## Масштабирование throughput

Чанки 64 KiB, суммарные MB/s при росте числа одновременных проксируемых
соединений:

| Соединений | Linux | Windows |
|---:|---:|---:|
| 1 | 433.4 MB/s | 206.5 MB/s |
| 4 | 1066.7 MB/s | 477.3 MB/s |
| 16 | **1103.0 MB/s** | **493.1 MB/s** |
| 64 | 950.3 MB/s | 445.5 MB/s |

Суммарный throughput растёт, пока не насыщается loopback/CPU, затем
выравнивается, а не падает.

## Hot path и микробенчмарки

Медиана ns/op; все строки без аллокаций, если не указано иное:

| Бенчмарк | Linux | Windows |
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

`OnRequestHeaders` включает разбор envelope, метаданные инициализации guest и
typed OUT response; это не стоимость на каждый байт. Сам relay-путь остаётся
без аллокаций.

## Что означают цифры

- `ConnHotPath` — CPU на соединение до того, как relay коснётся сокета:
  snapshot, listener, слот соединения, выбор пула и release.
- `round_robin` — один atomic increment; `least_conn` сканирует пул (O(n)) и
  остаётся в десятках наносекунд на 64 серверах.
- `Load` — разбор YAML на `SIGHUP`, не на каждый запрос. Reload под живым
  трафиком покрыт тестом с 12 reload и нулём потерянных соединений.
- Linux держит bulk-путь на `splice(2)`; проверки фич и ABI envelope не гоняют
  payload через user space, если только TLS termination или body policy не
  требуют обратного.

Сырой вывод: актуальный
[`linux-current-results.txt`](../../benchmarks/linux-current-results.txt) и
[`windows-current-results.txt`](../../benchmarks/windows-current-results.txt);
исторический pre-v4
[`linux-ubuntu-results.txt`](../../benchmarks/linux-ubuntu-results.txt).
