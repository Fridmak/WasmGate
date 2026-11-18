# Бенчмарки

> **Исторические числа.** Результаты ниже сняты 2026-11-05 на коммите
> `18d4249`, до перехода на ABI v4. У движка v4 другой путь хуков и другая
> стоимость dispatch. Перед решением перезапустите бенчмарки на текущем дереве.

Все бенчмарки лежат рядом с кодом, который покрывают (`X.go` → `X_test.go`), и
компилируются только `go test`; в бинарник они не попадают.

```bash
make bench                                            # всё
go test -run '^$' -bench . -benchmem ./...            # то же вручную
go test -run '^$' -bench BenchmarkThroughput -benchmem ./internal/proxy
```

Сравнение прогонов через `benchstat`:

```bash
go install golang.org/x/perf/cmd/benchstat@latest
go test -run '^$' -bench . -benchmem -count=10 ./... > new.txt
benchstat old.txt new.txt
```

## Платформы

`splice(2)` есть только на Linux. На Windows/macOS тот же `io.Copy` уходит в
копирование в user space; WasmGate даёт этому пути по одному буферу на
направление из пула вместо 32 KiB на направление на каждое соединение.
Абсолютные числа из WSL2 нельзя сравнивать с bare metal (4 vCPU, виртуальный NIC,
ФС через 9p), но относительные корректны: WSL2 — настоящее ядро Linux.

## Пропускная способность и задержка

Linux (WSL2, ядро 6.6.87, 4 vCPU, `-benchtime=1s -benchmem`), один и тот же
TCP echo-бэкенд:

| Бенчмарк | Linux | Windows |
|---|---|---|
| `Throughput/zero_copy/64KiB` | **557–725 MB/s**, 0 allocs | 222–253 MB/s |
| `Throughput/idle_watchdog/64KiB` | 257–286 MB/s | 208–314 MB/s |
| `Throughput/zero_copy/1MiB` | **862–977 MB/s** | 262–336 MB/s |
| `Throughput/idle_watchdog/1MiB` | 248–265 MB/s | 282–310 MB/s |
| `Throughput/direct/1MiB` (без прокси) | 902–1092 MB/s | 620–627 MB/s |
| `ConnectionSetup/through_proxy` | 36.0 KB/op, 65 allocs | 37.8–38.5 KB/op, 80 allocs |
| `RelayLatency/through_proxy` | 56–60 µs | 109–120 µs |

На Linux zero-copy быстрее буферизованного пути в **2.1–2.7×** на 64 KiB и
**3.3–3.7×** на 1 MiB — это сигнатура работающего `splice(2)`. На Windows
варианты сходятся, потому что оба копируют в user space.

## Сравнение с nginx и HAProxy

Ubuntu 24.04 (WSL2), `nginx/1.24.0`, `HAProxy 2.8.30`, тот же echo-бэкенд:

| Метрика | direct | WasmGate L4 | WasmGate full* | nginx stream | HAProxy TCP |
|---|---:|---:|---:|---:|---:|
| Throughput 64 KiB | 644.6 MB/s | **588.8 MB/s** | **734.1 MB/s** | 272.5 MB/s | 268.9 MB/s |
| Throughput 1 MiB | 865.6 MB/s | **688.3 MB/s** | **794.9 MB/s** | 230.9 MB/s | 248.3 MB/s |
| Задержка релея (RTT 1B) | 29.2 µs | **62.0 µs** | **61.1 µs** | 394.0 µs | 439.2 µs |
| Установка соединения | 402.9 µs | **917.7 µs** | **1420.9 µs** | 1076.9 µs | 1163.8 µs |

\* full = `max_conns` + `rate_limit` + `ipguard.wasm`; терминации TLS,
`data_mode: stream`, circuit breaker и dial retries в этом прогоне нет.

nginx stream и HAProxy в этой конфигурации копируют через user space — поэтому
они попадают в ту же пропускную способность, что и буферизованный путь WasmGate.
Проверки WasmGate выполняются один раз на соединение, не аллоцируют и не уводят
основной поток с `splice`.

## Исторические микробенчмарки горячего пути (pre-v4)

Linux, `0 B/op, 0 allocs/op` в том прогоне:

| Бенчмарк | Результат |
|---|---|
| `ratelimit/BenchmarkAllow` | 65.4 ns/op (параллельно: 20.4 ns/op) |
| `plugin/BenchmarkOnAccept` | 81.3 ns/op (параллельно: 24.7 ns/op) |
| `plugin/BenchmarkOnData` (L7) | 76.0 ns/op |
| `plugin/BenchmarkOnDataState` (исторический ABI v3 fixture) | 111–122 ns/op |
| `tlspeek/BenchmarkParseClientHelloSNI` | 26.0 ns/op |

Строки `plugin/Benchmark*` относятся к удалённому пакету v3 и не
воспроизводятся текущим деревом. Бенчмарки движка v4 —
`BenchmarkOnRequestHeaders`, `BenchmarkSplitHeaders`, `BenchmarkWriteEnvelope`
и `BenchmarkHeadersBody`; для них нужен новый прогон на Linux. Полная HTTP body
policy дороже старого opaque-пути из-за разбора фрейминга и typed-фаз.

## Как читать числа

- `ConnLimiter/at_limit` дешевле `below_limit`: отказ — это load и сравнение,
  приём требует двух CAS.
- `least_conn` сканирует пул (O(n)); на 64 серверах это десятки наносекунд.
  `round_robin` плоский: цена — один атомарный инкремент.
- Установка соединения аллоцирует ~36–38 KB/op — это сокеты и эхо-бэкенд, а не
  буферы релея.
- `Load` — это разбор YAML, он происходит на `SIGHUP`, а не на запрос.
- Reload под живым трафиком покрыт тестом: 12 reload, 0 потерянных соединений.

Сырой вывод: [`linux-ubuntu-results.txt`](../../benchmarks/linux-ubuntu-results.txt).
