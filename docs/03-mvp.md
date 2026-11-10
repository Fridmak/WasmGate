# 03. MVP: базовая версия и замер производительности

> Документ для **глав «Реализация» и «Эксперименты»**. MVP — это **минимальный
> L4-TCP-прокси**, созданный ради двух целей: **(1)** проверить гипотезы и
> **(2)** снять **baseline-производительность**, относительно которой меряются
> все дальнейшие фичи. Документ также фиксирует, что из последующих этапов уже
> реализовано (M3–M5), чтобы текст ВКР не расходился с кодом.

---

## 1. Цель MVP

- Получить **работающее прокси-ядро** (L4 TCP) с корректным проксированием,
  балансировкой, таймаутами и drain.
- Снять **честный baseline**: throughput / latency / allocs / RSS.
- Заложить **точки расширения** (снапшот конфига, ABI WASM-плагинов),
  чтобы последующие фичи (Hot Reload, rate limiting, WASM, TLS) насаживались
  **без переписывания ядра**.

**Главный принцип:** MVP не «урезанный продукт на выброс», а **фундамент**,
к которому фичи добавляются инкрементально.

---

## 2. Scope

### 2.1. Входит (IN)

| # | Возможность | Статус |
|---|-------------|--------|
| 1 | Приём TCP-соединений (`net`, горутина на соединение; epoll под капотом) | ✅ M0 |
| 2 | Проксирование на upstream-пул (bidirectional copy, `splice` на Linux) | ✅ M1 |
| 3 | Балансировка: round-robin / least-conn, пропуск нездоровых бэкендов | ✅ M1 |
| 4 | Таймауты: connect, **idle** (общий для соединения); лимит **`max_conns`** | ✅ M2 |
| 5 | **TLS passthrough** через zero-alloc SNI-peek (`internal/tlspeek`, `sni_routes`) | ✅ M3 |
| 6 | **TLS-терминация** на листенере (`listeners[].tls`, TLS 1.2/1.3, hot reload сертификата) | ✅ M5 |
| 7 | Health-checks (активный TCP-connect + пассивный по ошибкам `dial`) | ✅ M3 |
| 8 | **Circuit breaker** (Closed/Open/Half-Open) и прозрачный `dial_retries` | ✅ M5 |
| 9 | Конфиг из YAML + **hot reload** (SIGHUP + `POST /reload`), reconcile слушателей | ✅ M2/M3 |
| 10 | Наблюдаемость: `log/slog`, `/metrics`, `/healthz`, `pprof`, `POST /reload` | ✅ M3 |
| 11 | Graceful shutdown & **drain** (перестаём принимать, доигрываем активные) | ✅ M2 |
| 12 | Валидация конфига (`-validate`, включая WASM и TLS) | ✅ M3/M5 |
| 13 | **Rate limiting** соединений: per-IP/subnet окно, allow-list, backoff-бан (F2) | ✅ M4 |
| 14 | **WASM-плагины** (`on_accept`/`on_data`/`on_data_ex`/`on_data_state`/`on_close`), L7 mutation, `route_pools`, ABI v3 state и 8 showcase-плагинов | ✅ M5 + ABI v3 |

### 2.2. Не входит в MVP (следующие направления)

- ❌ Zero-alloc JSON DPI **в ядре** (F3) — следующее направление. HTTP/1.x-разбор
  уже доступен через showcase-плагины `http_guard` и `http_body_guard`.
- ❌ eBPF-фильтрация (F4) — следующее направление.
- ❌ C# Control Plane как обязательный компонент (сейчас — файл + SIGHUP/`POST /reload`;
  gRPC/UDS — M6).
- ❌ Маркетплейс плагинов, GUI, multi-tenant.
- ❌ mTLS к control plane и клиентские сертификаты; Docker-упаковка
  (systemd-юнит и production-гайд уже есть — `deployments/`).

> Причина такого деления: сначала получить **воспроизводимый baseline** и
> **не раздувать ядро**, а фичи (Hot Reload, rate limiting, WASM, TLS) добавлять
> поверх, не ломая замеренные цифры.

---

## 3. Нефункциональные требования

| Требование | Цель |
|------------|------|
| Аллокации на соединение (steady state) | **0** |
| Throughput | ≥ 90 % от nginx `stream` на том же железе |
| Латентность p99 | +≤ 100–200 мкс к прямым соединениям |
| Память (RSS) под нагрузкой | ≤ nginx на сопоставимой нагрузке |
| Соединений/сек (accept rate) | не ниже аналога (в пределах ±10 %) |
| Простой при reload | 0 разорванных соединений |
| Кросс-сборка | linux/amd64, linux/arm64 (без CGO) |

---

## 4. Архитектура MVP (упрощённо)

```
clients ─▶ accept-loop ─▶ max_conns/rate_limit ─▶ on_accept ─▶ TLS/SNI
              │                                              │
              │                                              ▼
              │                                   on_data (once) ─▶ balancer
              │                                              │        │
              │                                              ▼        ▼
              └── snapshot (atomic) ◀── config loader ── copy/splice ◀─ upstream pool
                        ▲
          SIGHUP / POST /reload / Control Plane (позже)
```

Модули (фактическая структура; `proxy` — это и есть data plane):

```
internal/
├── proxy/       # server, proxy.Handle, pool, balancer, copy/splice, snapshot (RCU)
├── config/      # схема YAML, загрузчик, Validate(), валидация (чистые данные)
├── plugin/      # wazero sandbox: ABI v1/v2/v3, пул инстансов, ConnState
├── ratelimit/   # скользящее окно по IP/подсети (F2)
├── tlspeek/     # zero-alloc SNI peek и прозрачный replay ClientHello
└── observe/     # логирование (log/slog), лог-лимитер, метрики Prometheus, admin/pprof
# future: control/ (gRPC Control Plane client)
```

---

## 5. Модель конфигурации (пример)

```yaml
# configs/wasmgate.yaml (сокращённо; полный — в репозитории)
version: 1
admin_addr: "127.0.0.1:9099"     # /metrics, /healthz, /reload, /debug/pprof

listeners:
  - name: edge
    addr: "0.0.0.0:443"
    upstream_pool: default_pool
    max_conns: 200000
    dial_timeout: 3s
    idle_timeout: 60s            # >0 отключает splice (idle watchdog)
    rate_limit:
      max: 200
      subnet_max: 1000
      window: 1s
      ban_duration: 5s
      allow_cidrs: ["127.0.0.1/32"]
    plugin:
      path: "plugins/examples/ipguard/ipguard.wasm"
      max_memory_pages: 16
      timeout: 0s
      data_mode: "once"
      route_pools: ["canary"]
    sni_routes:                  # TLS passthrough; игнорируется при tls:
      "api.example.com": api_pool
      "*.example.com": default_pool
    # tls:                       # опциональная терминация
    #   cert_file: "certs/server.crt"
    #   key_file: "certs/server.key"
    #   min_version: "1.2"

pools:
  api_pool:
    balance: least_conn
    dial_retries: 1
    health_check: { interval: 2s, timeout: 1s }
    circuit_breaker: { error_threshold: 5, open_duration: 10s, half_open_requests: 2 }
    servers:
      - "10.0.0.10:8443"
      - "10.0.0.11:8443"
  default_pool:
    balance: round_robin
    servers:
      - "10.0.0.20:8443"
  canary:
    balance: least_conn
    servers:
      - "10.0.0.30:8443"
```

Смена конфига → новый `Snapshot` → `atomic.Store` (Hot Reload). Прежний
snapshot остаётся активным, если сборка/валидация нового упала.

> Схема в коде — источник истины: [`internal/config/listener_config.go`](../internal/config/listener_config.go)
> и [`internal/config/pool_config.go`](../internal/config/pool_config.go).
> Живой пример — [`configs/wasmgate.yaml`](../configs/wasmgate.yaml).

---

## 6. Этапы (milestones)

| Milestone | Содержание | Deliverable | DoD |
|-----------|------------|-------------|-----|
| **M0. Skeleton** | accept-loop (`net`), логирование, каркас | Бинарь принимает соединения | ✅ done |
| **M1. Proxy core** | пулы, балансировка, copy/splice, таймауты | Проксирует TCP end-to-end | ✅ done |
| **M2. Config+reload+drain** | YAML, Snapshot, atomic swap, SIGHUP, reconcile слушателей, drain | Reload без разрыва соединений | ✅ done |
| **M3. Observe+hardening** | метрики, pprof, health-checks, CLI, `-validate`, `POST /reload`, SNI peek | Наблюдаемый прокси + Linux-baseline | ✅ done |
| **M4. Connection flood (F2)** | скользящее окно per-IP/subnet, allow-list, backoff-бан | Фильтр в горячем пути, 0 allocs/op | ✅ done |
| **M5. WASM + L7 + TLS** | wazero sandbox, ABI v1/v2/v3, `on_data`/`on_data_ex`/`on_data_state`, `route_pools`, TLS passthrough/termination, circuit breaker, dial retries, 8 showcase-плагинов | Плагин без рестарта, e2e и сравнение с nginx/HAProxy | ✅ done |
| **M6. Control Plane** | C#/.NET 8 + gRPC/UDS: версии, публикация, откат, аудит | Hot reload из CP | ⏳ plan |

> После M5 — C# control plane; zero-alloc JSON DPI в ядре (F3) и eBPF (F4) —
> следующие направления.

---

## 7. Definition of Done (MVP)

- [x] Проксирует TCP end-to-end, корректно закрывает соединения (half-close).
- [x] Балансировка и health-checks работают, мёртвые upstream исключаются.
- [x] Reload под нагрузкой **не рвёт** соединения: chaos-тест `TestReloadUnderLoadZeroDroppedConnections` (12 reload, 0 dropped).
- [x] `go test -race ./...` зелёный; нет утечек горутин/буферов.
- [x] Есть метрики, логи, `pprof`-эндпоинт.
- [x] `go test -benchmem` показывает **0 alloc** в steady state на fast path.
- [x] Сняты и задокументированы baseline-замеры S0–S5 против `nginx stream` и `HAProxy` на Linux (см. §8.5 и `benchmarks/README.md`).
- [x] Кросс-сборка без CGO проходит (linux/amd64, linux/arm64).
- [x] Rate limiting: per-IP/subnet окно, allow-list, backoff-бан; `BenchmarkAllow` — 0 allocs/op.
- [x] WASM-плагин загружается, фильтрует соединения и переживает reload; есть e2e-тест `TestProxyExampleIPGuardWasmBlocks127_0_0_99`.
- [x] L7 `on_data` (once/stream), in-place mutation и `route_pools` работают; `once` сохраняет `splice`.
- [x] TLS passthrough по SNI и TLS-терминация с hot reload сертификата.
- [x] Circuit breaker и `dial_retries` покрыты тестами; `/metrics` отдаёт health/retry/discard.

> **Fast path** — это `idle_timeout: 0` (по умолчанию), без `tls`,
> `data_mode: once` и без stream-плагина: `io.Copy` идёт через `splice`.
> Включение idle watchdog, stream-инспекции или терминации переключает
> соединение на буферизованный путь. Baseline-замеры снимаются на fast path.

---

## 8. Методика замеров baseline (ядро этой главы)

### 8.1. Стенд

Фактический прогон для ВКР: Ubuntu 24.04 в WSL2, ядро
`6.6.87.2-microsoft-standard-WSL2`, AMD Ryzen 5 3550H (4 vCPU), Go 1.27.2,
nginx 1.24.0, HAProxy 2.8.30. Прокси, бэкенд и генератор нагрузки — на одном
хосте (loopback), поэтому числа сравнимы **относительно**, а не как абсолютный
сетевой throughput. Идеальный стенд с CPU pinning/netns описан ниже как план.

- Отдельный хост/VM (или изолированный netns), **опрос синхронный** по времени.
- Никаких «шумных соседей»; CPU governor = performance; отключены deep C-states.
- Прокси и генератор нагрузки — на **разных** ядрах (CPU pinning).
- Версии фиксированы; каждый сценарий — N прогонов с warmup.

### 8.2. Инструменты

| Что меряем | Инструмент |
|------------|------------|
| L4 throughput / latency / setup rate | `go test -bench` (`BenchmarkProxyComparison`, `BenchmarkThroughput`, `BenchmarkRelayLatency`, `BenchmarkConnectionSetup`) |
| Сравнение с внешними прокси | реальные процессы `nginx stream` и `HAProxy TCP` в тесте (Linux) |
| Go-аллокации | `go test -bench . -benchmem` |
| Статистика прогонов | `benchstat` |
| Профили CPU/heap | `pprof` |
| Ресурсы | `pidstat`, `ps`, `/proc` |
| HTTP RPS (future) | `wrk2` / `k6` |

### 8.3. Сценарии (матрица)

| # | Сценарий | Метрики |
|---|----------|---------|
| S0 | **Прямое соединение** (без прокси) — эталон | RPS, p50/p99, Gbps |
| S1 | Проксирование TCP (1 upstream) | RPS, p50/p99, Gbps, allocs |
| S2 | Проксирование + балансировка (N upstream) | RPS, распределение, p99 |
| S3 | Проксирование под нагрузкой + **reload** в середине | dropped conns, p99 spike |
| S4 | Concurrency sweep (1k→100k conns) | accept-rate, RSS, p99 |
| S5 | Сравнение с **nginx stream** и **HAProxy** | throughput/latency/RSS |

### 8.4. Протокол прогона

1. Warmup (30–60 с), затем измерение (2–5 мин).
2. **≥ 5 прогонов** на сценарий; результат — `benchstat` (медиана + CI).
3. Фиксировать: `GOMAXPROCS`, `GOGC`, версию Go, ядро ОС, NIC, offload-настройки.
4. Любое изменение ядра (фича, оптимизация) — **обязательный** повтор S1/S3 и
   сравнение через `benchstat` (регресс-gate).

### 8.5. Результаты замеров на Linux (Ubuntu 24.04, ядро 6.6.87, Go 1.27.2)

| Сценарий | Throughput (64 KiB / 1 MiB) | Setup rate | RTT (1B warm) | Allocs/conn (steady) | Примечание |
|----------|-----------------------------|------------|---------------|----------------------|------------|
| S0 `direct` | 644.6 / 865.6 MB/s | ~2 480 conn/s (403 µs) | 29.2 µs | **0** | Прямой loopback к эхо-серверу |
| S1 `wasmgate_l4` | **588.8 / 688.3 MB/s** | ~1 090 conn/s (918 µs) | **62.0 µs** | **0** (65 при setup) | Ядерный `splice(2)` zero-copy |
| S1+F2+M5 `wasmgate_full` | **734.1 / 794.9 MB/s** | ~704 conn/s (1 421 µs) | **61.1 µs** | **0** (65 при setup) | `max_conns` + F2 `rate_limit` + WASM `ipguard.wasm` |
| S3 Reload под нагрузкой | 12 live reloads | 0 dropped | без просадки | **0** | `TestReloadUnderLoadZeroDroppedConnections` |
| S5 `nginx stream` 1.24.0 | 272.5 / 230.9 MB/s | ~928 conn/s (1 077 µs) | 394.0 µs | — | User-space буферы (без `splice`) |
| S5 `HAProxy TCP` 2.8.30 | 268.9 / 248.3 MB/s | ~859 conn/s (1 164 µs) | 439.2 µs | — | Дефолтный `mode tcp` (без `splice-auto`) |

> Полный разбор цифр и воспроизводимая команда `BenchmarkProxyComparison` — в [`benchmarks/README.md`](../benchmarks/README.md), §2.
>
> Числа сняты на коммите `18d4249` (до батча circuit breaker/TLS от 06.11) и
> **не включают** TLS, circuit breaker и `dial_retries`: `wasmgate_full` — это
> `max_conns` + `rate_limit` + `ipguard.wasm`. Дословный вывод консоли —
> [`benchmarks/linux-ubuntu-results.txt`](../benchmarks/linux-ubuntu-results.txt).

### 8.6. Go-специфика профилирования

```bash
# Аллокации и бенчи
go test -run '^$' -bench . -benchmem -cpuprofile cpu.out -memprofile mem.out ./internal/proxy

# Сравнение до/после
go test -bench . -benchmem ./... > new.txt
benchstat old.txt new.txt

# Живой профиль под нагрузкой (admin_addr + admin_pprof: true)
go tool pprof http://127.0.0.1:9099/debug/pprof/profile?seconds=30
```

---

## 9. Стратегия тестирования

| Уровень | Что проверяем |
|---------|---------------|
| Unit | балансировщик, circuit breaker, парсер SNI, Snapshot/atomic swap, таймауты, rate limiter |
| Integration | проксирование end-to-end, health-check failover, drain, TLS/SNI, WASM-хуки |
| Race | `go test -race ./...` |
| Fuzz | парсер ClientHello/SNI (`FuzzParseClientHello`) |
| Load | сценарии S0–S5 (§8.3) |
| Chaos | reload под нагрузкой, kill upstream, обрыв клиента |
| E2E | `ipguard.wasm` на реальных source IP, сравнение с nginx/HAProxy (Linux) |

---

## 10. Риски MVP

| Риск | Вероятность | Влияние | Митигация |
|------|-------------|---------|-----------|
| Движок event-loop (`gnet`) добавляет сложность, если взять его преждевременно | Средняя | Среднее | Решено: старт на `net`+`splice`; `gnet` — только если замеры покажут узкое место |
| Drain/reload рвёт соединения на гонках | Средняя | Высокое | Chaos-тест + race; фиксировать snapshot per-conn |
| `splice` не кросс-платформенный | Высокая | Низкое | Linux — fast path; на других платформах копия через пул буферов (подробно — benchmarks/README.md) |
| TLS-терминация или `stream`-плагин отключают `splice` | Высокая | Среднее | Держать TLS/`stream` только там, где нужна инспекция; `on_data: once` сохраняет zero-copy |
| Плагин съедает CPU/память | Средняя | Высокое | `max_memory_pages`, опциональный `timeout`, выбраковка инстанса при trap |
| Старый WASM-runtime не закрывается при reload | Средняя | Низкое | Задокументировано в `docs/06`; инстансы доживают на старом snapshot |
| Baseline «загрязнён» окружением | Средняя | Среднее | pinning, netns, ≥5 прогонов, benchstat |
| Разрастание скоупа (feature creep) | Высокая | Среднее | Жёсткий scope §2.2 + milestones |

---

## 11. Что даёт MVP для диплома

- **Доказуемый артефакт:** работающий прокси + воспроизводимые числа.
- **База для гипотез:** каждая фича меряется против baseline (H1–H4).
- **Точки расширения:** Snapshot/RCU и ABI WASM-плагинов → главы по Hot Reload,
  rate limiting, L7-инфильтрации и TLS.
