# 03. MVP: базовая версия и замер производительности

> Документ для **глав «Реализация» и «Эксперименты»**. MVP — это **минимальный
> L4-TCP-прокси** без сложных фич, созданный ради двух целей:
> **(1)** проверить гипотезы и **(2)** снять **baseline-производительность**,
> относительно которой меряются все дальнейшие фичи.

---

## 1. Цель MVP

- Получить **работающее прокси-ядро** (L4 TCP) с корректным проксированием,
  балансировкой, таймаутами и drain.
- Снять **честный baseline**: throughput / latency / allocs / RSS.
- Заложить **точки расширения** (снапшот конфига, host-functions для плагинов),
  чтобы последующие фичи (Hot Reload, DDoS, WASM, DPI) насаживались **без
  переписывания ядра**.

**Главный принцип:** MVP не «урезанный продукт на выброс», а **фундамент**,
к которому фичи добавляются инкрементально.

---

## 2. Scope

### 2.1. Входит (IN)

| # | Возможность | Статус |
|---|-------------|--------|
| 1 | Приём TCP-соединений (`net`, горутина на соединение; epoll под капотом) | ✅ M0 |
| 2 | Проксирование на upstream-пул (bidirectional copy, `splice` на Linux) | ✅ M1 |
| 3 | Балансировка: round-robin / least-conn | ✅ M1 |
| 4 | Таймауты: connect, **idle** (общий для соединения); лимит **`max_conns`** | ✅ M2 |
| 5 | **TLS passthrough** через SNI-peek (без терминации); опц. терминация | ⏳ план |
| 6 | Health-checks (активный TCP-connect + пассивный по ошибкам) | ⏳ план |
| 7 | Конфиг из YAML + **hot reload** (SIGHUP — прообраз RCU), reconcile слушателей | ✅ M2 |
| 8 | Наблюдаемость: структурные логи (`log/slog`), метрики (`/metrics`), `/healthz`, `pprof` | ✅ M3 |
| 9 | Graceful shutdown & **drain** (перестаём принимать, доигрываем активные) | ✅ M2 |
| 10 | CLI `wasmgate-ctl` (валидация/применение конфига) | ⏳ план |

### 2.2. Не входит (OUT — в следующей волне)

- ❌ L7/HTTP-парсинг и JSON DPI (F3) — future work.
- ❌ eBPF-фильтрация (F4) — future work.
- ❌ Полноценный WASM-рантайм (в MVP — **заглушка-интерфейс** без реального
  исполнения; настоящий wazero — Milestone M2).
- ❌ C# Control Plane как обязательный компонент (в MVP — файл+watch; gRPC — M2).
- ❌ Маркетплейс плагинов, GUI, multi-tenant.

> Причина такого деления: сначала получить **воспроизводимый baseline** и
> **не раздувать ядро**, а фичи ядра (Hot Reload / защита от connection flood)
> добавлять поверх, не ломая замеренные цифры.

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
clients ─▶ accept-loop ─▶ balancer ─▶ upstream pool ─▶ copy/splice
                │                            ▲
                └── snapshot (atomic) ◀── config loader ┘
                        ▲
                   SIGHUP / file watch / Control Plane (позже)
```

Модули MVP (фактическая структура; `proxy` — это и есть data plane):

```
internal/
├── proxy/       # server, proxy.Handle, pool, balancer, copy/splice, snapshot (RCU)
├── config/      # схема YAML, загрузчик, Validate(), валидация (чистые данные)
├── ratelimit/   # скользящее окно по IP (F2)
└── observe/     # логирование (log/slog), лог-лимитер, метрики Prometheus, admin/pprof
# future: plugin/ (wazero), tlspeek/ (SNI), control/ (gRPC)
```

---

## 5. Модель конфигурации (пример)

```yaml
# wasmgate.yaml
version: 1
listeners:
  - name: edge
    addr: "0.0.0.0:443"
    protocol: tcp
    tls:
      mode: passthrough        # passthrough | terminate
    limits:
      max_conns: 200000
      idle_timeout: 60s

routes:
  - match: { sni: "api.example.com" }
    upstream_pool: api_pool
  - match: { sni: "*.example.com" }
    upstream_pool: default_pool

upstream_pools:
  api_pool:
    balance: least_conn
    health_check: { type: tcp, interval: 2s, timeout: 500ms }
    servers:
      - "10.0.0.10:8443"
      - "10.0.0.11:8443"
  default_pool:
    balance: round_robin
    servers:
      - "10.0.0.20:8443"
```

Смена конфига → новый `Snapshot` → `atomic.Store` (прообраз Hot Reload).

> **Важно:** пример выше — **целевая** схема (с `routes`, `tls`, `health_check`).
> Текущая реализация поддерживает подмножество (`listeners` + `pools`) — живой
> пример см. в [`configs/wasmgate.yaml`](../configs/wasmgate.yaml). Остальные поля
> включаются по мере реализации фич.

---

## 6. Этапы (milestones)

| Milestone | Содержание | Deliverable | DoD |
|-----------|------------|-------------|-----|
| **M0. Skeleton** | accept-loop (`net`), логирование, каркас | Бинарь принимает соединения | ✅ done |
| **M1. Proxy core** | пулы, балансировка, copy/splice, таймауты | Проксирует TCP end-to-end | ✅ done |
| **M2. Config+reload+drain** | YAML, Snapshot, atomic swap, SIGHUP, reconcile слушателей, drain | Reload без разрыва соединений | ✅ done |
| **M3. Observe+hardening** | метрики, pprof, health-checks, CLI | Наблюдаемый прокси | ⏳ baseline |

> **После M3** — ядровые фичи: защита от connection flood (F2, скользящее окно),
> затем WASM host + C# Control Plane (F1); DPI (F3) и eBPF (F4) — future work.

---

## 7. Definition of Done (MVP)

- [x] Проксирует TCP end-to-end, корректно закрывает соединения (half-close).
- [ ] Балансировка и health-checks работают, мёртвые upstream исключаются (балансировка — ✅, health-checks — в плане).
- [x] Reload под нагрузкой **не рвёт** соединения (механика и юнит-тесты — ✅; внешний chaos-прогон — M3).
- [x] `go test -race ./...` зелёный; нет утечек горутин/буферов.
- [x] Есть метрики, логи, `pprof`-эндпоинт.
- [x] `go test -benchmem` показывает **0 alloc** в steady state на fast path.
- [ ] Сняты и задокументированы внешние baseline-замеры S0–S5 (микробенчмарки и Linux/Windows сравнение сняты — см. `benchmarks/README.md`).
- [x] Кросс-сборка без CGO проходит (linux/amd64, linux/arm64).

> **Fast path** — это `idle_timeout: 0` (по умолчанию): `io.Copy` идёт через
> `splice`. Если включить `idle_timeout`, включается буферизованное копирование с
> read-deadline, и zero-copy/splice для этого направления отключается. Бенчмарки
> baseline снимаются с `idle_timeout: 0`.

---

## 8. Методика замеров baseline (ядро этой главы)

### 8.1. Стенд

- Отдельный хост/VM (или изолированный netns), **опрос синхронный** по времени.
- Никаких «шумных соседей»; CPU governor = performance; отключены deep C-states.
- Прокси и генератор нагрузки — на **разных** ядрах (CPU pinning).
- Версии фиксированы; каждый сценарий — N прогонов с warmup.

### 8.2. Инструменты

| Что меряем | Инструмент |
|------------|------------|
| L4 throughput (Gbps) | `iperf3` (TCP) |
| HTTP RPS/latency через L4 | `wrk2` или `vegeta` / `k6` |
| Connection rate | `wrk2 -c`, custom accept-bench |
| Go-аллокации | `go test -bench . -benchmem` |
| Статистика прогонов | `benchstat` |
| Профили CPU/heap | `pprof` |
| Ресурсы | `pidstat`, `ps`, `/proc` |

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

### 8.5. Шаблон таблицы результатов

| Сценарий | Throughput | RPS | p50 | p99 | Allocs/conn | RSS |
|----------|-----------|-----|-----|-----|-------------|-----|
| S0 прямой | TBD | TBD | TBD | TBD | 0 | TBD |
| S1 proxy | TBD | TBD | TBD | TBD | **0** | TBD |
| S5 vs nginx | TBD | TBD | TBD | TBD | — | TBD |

> Заполняется после M3. Именно эти числа — «baseline», от которого считаются
> оверхеды плагинов (H1), DDoS (H3) и т.д.

### 8.6. Go-специфика профилирования

```bash
# Аллокации и бенчи
go test -run '^$' -bench . -benchmem -cpuprofile cpu.out -memprofile mem.out ./internal/proxy

# Сравнение до/после
go test -bench . -benchmem ./... > new.txt
benchstat old.txt new.txt

# Живой профиль под нагрузкой
go tool pprof http://127.0.0.1:6060/debug/pprof/profile?seconds=30
```

---

## 9. Стратегия тестирования

| Уровень | Что проверяем |
|---------|---------------|
| Unit | балансировщик, парсер SNI, Snapshot/atomic swap, таймауты |
| Integration | проксирование end-to-end, health-check failover, drain |
| Race | `go test -race ./...` |
| Fuzz | парсер ClientHello/SNI, загрузчик конфига |
| Load | сценарии S0–S5 (§8.3) |
| Chaos | reload под нагрузкой, kill upstream, обрыв клиента |

---

## 10. Риски MVP

| Риск | Вероятность | Влияние | Митигация |
|------|-------------|---------|-----------|
| Движок event-loop (`gnet`) добавляет сложность, если взять его преждевременно | Средняя | Среднее | Решено: старт на `net`+`splice`; `gnet` — только если замеры покажут узкое место |
| Drain/reload рвёт соединения на гонках | Средняя | Высокое | Chaos-тест + race; фиксировать snapshot per-conn |
| `splice` не кросс-платформенный | Высокая | Низкое | Linux — fast path; на других платформах копия через пул буферов (подробно — benchmarks/README.md) |
| Baseline «загрязнён» окружением | Средняя | Среднее | pinning, netns, ≥5 прогонов, benchstat |
| Разрастание скоупа (feature creep) | Высокая | Среднее | Жёсткий scope §2.2 + milestones |

---

## 11. Что даёт MVP для диплома

- **Доказуемый артефакт:** работающий прокси + воспроизводимые числа.
- **База для гипотез:** каждая фича меряется против baseline (H1–H4).
- **Точки расширения:** Snapshot/RCU и plugin-интерфейс → главы по Hot Reload и WASM.
