# 02. Архитектура и техническая аналитика

> Документ для **главы «Проектирование»**. Здесь — принципы, компоненты, стек,
> библиотеки, кодстайл, ключевые решения (ADR) и модель расширения.

---

## 1. Принципы проектирования

1. **Performance-first.** Быстрый путь (fast path) не аллоцирует память в steady
   state, не берёт блокировки, не использует reflection и `panic`.
2. **No CGO.** Всё ядро — чистый Go. Это упрощает кросс-сборку, статический
   бинарь и запуск в минимальных контейнерах. WASM-рантайм тоже без CGO.
3. **Extensibility без компромиссов.** Плагины изолированы (WASM sandbox) и
   подключаются через узкий ABI; оверхед на пустом плагине — единицы процентов.
4. **Zero-downtime по умолчанию.** Любое изменение — это публикация новой
   неизменяемой версии конфигурации (snapshot), а не мутация состояния.
5. **Наблюдаемость встроена.** Метрики/логи/трейсы — не «потом», а часть ядра.
6. **Безопасность по умолчанию.** Песочница для плагинов, лимиты ресурсов,
   принцип наименьших привилегий, отсутствие доверия к плагину.

---

## 2. Высокоуровневая архитектура

```
┌───────────────────────────── CONTROL PLANE (C# / .NET 8) ─────────────────────────────┐
│  ASP.NET Core + gRPC                                                                   │
│  ┌──────────────┐  ┌────────────────┐  ┌───────────────┐  ┌──────────┐  ┌───────────┐  │
│  │ Config Store │  │ Plugin Registry│  │ Validate/Pub. │  │ Audit Log│  │ Rollback  │  │
│  │ (версии)     │  │ (WASM + meta)  │  │ (health-gate) │  │          │  │ (N-1)     │  │
│  └──────────────┘  └────────────────┘  └───────────────┘  └──────────┘  └───────────┘  │
└───────────────────────────────────────────┬────────────────────────────────────────────┘
                                             │  gRPC (protobuf) over Unix Domain Socket
                                             │  push snapshot + ack + health
                                             ▼
┌──────────────────────────────── DATA PLANE (Go, pure) ─────────────────────────────────┐
│  ┌───────────────┐                                                                     │
│  │  accept loop  │  accept → max_conns → rate_limit → on_accept → TLS/SNI → on_data →   │
│  │  net + splice │  balance/dial (retry, breaker) → copy/splice → on_close → metrics    │
│  └───────┬───────┘                                                                     │
│          │  читают через  atomic.Pointer[Snapshot]  (RCU, без блокировок)              │
│  ┌───────▼────────┐  ┌──────────────┐  ┌──────────────┐  ┌───────────────┐  ┌─────────┐ │
│  │ config snapshot│  │ ratelimit    │  │ plugin host  │  │ metrics/logs  │  │ TLS     │ │
│  │ (immutable)    │  │ (ring window)│  │ (wazero)     │  │ (prom/pprof) │  │ peek/term│ │
│  └────────────────┘  └──────────────┘  └──────────────┘  └───────────────┘  └─────────┘ │
└──────────────────────────────────────────┬─────────────────────────────────────────────┘
                                            ▼
                              ┌───────────────────────────┐
                              │  Upstream pool (backend)  │
                              └───────────────────────────┘
```

### Компоненты

| Компонент | Ответственность | Язык |
|-----------|-----------------|------|
| **Data Plane** | Приём соединений, маршрутизация, балансировка, copy/splice, вызов плагинов | Go |
| **Control Plane** | Хранение версий конфига, реестр плагинов, валидация, публикация, откат, аудит | C#/.NET |
| **Plugin Host** | Загрузка `.wasm`, sandbox без WASI, ABI на стеке, пул инстансов, лимиты памяти/времени | Go (wazero) |
| **Config Snapshot** | Неизменяемый срез конфига, публикуемый атомарно | Go |
| **Rate Limiter** | Скользящее окно, шардированные счётчики, вердикт по соединению | Go |
| **TLS** | Passthrough (SNI peek) и опциональная терминация | Go (`crypto/tls`, `internal/tlspeek`) |
| **Observability** | Метрики, логи, трейсы, pprof | Go |
| **Transport CP↔DP** | gRPC по Unix Domain Socket (быстро, локально, без сети) | Go + C# |

---

## 3. Жизненный цикл соединения (поток данных)

```
accept()
  │
  ├─ 1. max_conns            (лимит одновременных соединений листенера)
  ├─ 2. rate_limit           (sliding window по IP/подсети → allow/deny/ban)
  ├─ 3. Plugin hook:on_accept(ip, port) → allow/deny
  ├─ 4. TLS                  (терминация tls.Server ИЛИ SNI peek + sni_routes)
  ├─ 5. L7 on_data           (once: первый пакет до dial; stream: в data loop)
  ├─ 6. Route resolve        (upstream_pool, sni_routes, plugin.route_pools)
  ├─ 7. Upstream dial        (RR/least-conn, dial_retries, circuit breaker)
  ├─ 8. Data loop            (bidirectional copy; на Linux — splice/zero-copy;
  │                           в stream-режиме — on_data на каждом чанке)
  ├─ 9. Plugin hook:on_close (rx/tx байты)
  └─ 10. close + metrics
```

**Важно:** `Handle` делает один `atomic.Pointer[Snapshot]` load в начале и
использует **зафиксированную версию** на всё время жизни соединения. Смена
конфига в середине сессии его не ломает; новые соединения видят новый snapshot.
`max_conns` и окна `rate_limit` живут в `registry` и переживают reload.

### 3.1. Уровни анализа: L4-ядро, поток, «пакеты»

Важно не путать три разных уровня — иначе кажется, что «L4» противоречит
«плагинам и анализу»:

| Уровень | Что видим | Что делаем | Где в проекте |
|---------|-----------|------------|---------------|
| **L4 (ядро)** | TCP-соединения, IP/порт | route/балансировка, rate limiting по частоте соединений (connection flood), copy | реализовано |
| **Поток (L7-lite)** | байты TCP-потока (**не** пакеты) | SNI-peek, TLS-терминация, `on_data` (inspect/mutate/route) | реализовано точечно |
| **Пакеты (L3/L4)** | отдельные сетевые пакеты | раннее отсечение SYN-flood и подобного в ядре ОС | F4 eBPF (future) |

Ключевые нюансы:

- Ядро — **L4**: по умолчанию payload не разбирается, мы просто переливаем байты.
- Но «L4» ≠ «слепой»: `splice`-путь в байты не смотрит, а вот SNI-peek и
  `on_data` — смотрят. Именно через эти швы L4-ядро умеет «L7», **не превращаясь
  целиком в L7-парсер**: ядро остаётся быстрым, а умная логика — в песочнице.
- Мы работаем с **потоком**, а не с пакетами: сегментацию и сборку TCP делает
  ядро ОС. Буквальный «анализ пакетов» по заголовкам — это eBPF (F4).
- **Стоимость инспекции.** `on_data` в режиме `once` и SNI peek читают только
  первый пакет, после чего `DrainTo` возвращает исходный сокет — остаток
  соединения снова идёт через zero-copy `splice(2)`. А вот `idle_timeout > 0`,
  `data_mode: stream` и TLS-терминация переводят соединение на буферизованный
  путь: это осознанная цена за watchdog/почанковую инспекцию/расшифровку.
  Решения по **метаданным** (`on_accept`/`on_close`, rate limit, балансировка)
  остаются на fast path.

---

## 4. Технологический стек

| Слой | Выбор | Почему | Альтернативы (и почему нет) |
|------|-------|--------|------------------------------|
| Networking (L4) | **стандартный `net`** (горутина на соединение) | Просто и предсказуемо; backpressure и per-conn состояние «из коробки»; под капотом epoll; `io.Copy` использует `splice(2)` | `gnet` (event-loop) — опция для миллионов лёгких соединений, но добавляет сложность; `fasthttp` — это HTTP |
| Zero-copy | `splice` (Linux) через `net.TCPConn.ReadFrom` | Копирование данных в ядре, а не в user-space | буферизованная копия из пула — включается при `idle_timeout > 0` (и вне Linux, ADR-7) |
| WASM runtime | **`tetratelabs/wazero`** | Чистый Go, **нет CGO**, быстрый старт, sandbox; работаем **без WASI** (ABI на стеке) | `wasmtime-go` (CGO), `wasmer-go` (CGO, тяжелее) |
| JSON (future DPI) | `valyala/fastjson` / `bytedance/sonic` | Zero/низкие аллокации, SIMD (sonic) | `encoding/json` (рефлексия, аллокации) |
| Config | **`gopkg.in/yaml.v3`** (`KnownFields(true)`) | Прямое декодирование в структуры и `time.Duration`, отсечение опечаток без лишних обёрток | Viper, koanf (избыточны для плоской схемы) |
| Logging | **`log/slog`** (stdlib) + `observe.Limiter` | Встроен в стандартную библиотеку Go, структурный вывод без внешних зависимостей | `logrus` (аллокации, медленнее) |
| Metrics | **`observe.Metrics`** (атомики + Prometheus text 0.0.4) | 0 аллокаций и ~60 нс на соединение, совместимость с любым Prometheus-scraper'ом без тяжёлых зависимостей | `client_golang` на каждом чанке (дороже) |
| Rate limit | **собственный sliding window** (`internal/ratelimit`) | 64 шарда, 10 слотов, 16-байтный ключ, allow-list, `/24` и `/64`, backoff-бан; 0 аллокаций в steady state (≈65 нс) | фикс-окно (всплески на границе окна) |
| eBPF (future) | `cilium/ebpf` | Современный, чистый Go | `iovisor/gobpf` (устарел) |
| Control plane | **.NET 8 / ASP.NET Core / gRPC** | Строгий контракт, генерация кода | REST (слабее контракт) |
| Тесты | стандартный **`testing`** (table-driven, `-race`, fuzz для SNI, bench) | Нулевые внешние зависимости, единый стиль со stdlib | — |
| Бенчмарки | `go test -bench`, **`benchstat`**, сравнение с `nginx stream` и HAProxy на Linux | Воспроизводимость + статистика | — |
| Линт | **`golangci-lint` v2** (схема v2, `v2.15.0`) | Один запуск — много анализаторов | — |
| Сборка | `go build` + **`Makefile`**; Docker/systemd — future | Кросс-сборка без CGO | — |

---

## 5. Кодстайл и соглашения

Базируется на: **Effective Go**, **Go Code Review Comments**, **Uber Go Style
Guide**, **Google Go Style Decisions**.

### 5.1. Форматирование и линт

- Форматтер: `gofmt`.
- Линтер: `golangci-lint` **v2** (конфиг в схеме v2). Базовый набор:

```yaml
# .golangci.yml (фрагмент, v2 schema)
version: "2"
linters:
  enable:            # v2 standard set (errcheck, govet, staticcheck, ineffassign, unused)
    - bodyclose      # закрытие HTTP-тел
    - exhaustive     # полнота switch по enum
    - gosec          # безопасность
    - noctx          # запросы без context
    - prealloc       # пред-аллокация слайсов в горячем коде
    - revive         # стиль
```

### 5.2. Раскладка проекта (standard layout)

```
wasmgate/                         # ← фактическая структура репозитория
├── cmd/wasmgate/                 # бинарь data plane (+ signals_unix/windows.go)
├── internal/
│   ├── config/                   # схема YAML, Load()/Validate(), валидация
│   ├── proxy/                    # data plane: server, proxy, pool, upstream, state, copy, snapshot
│   ├── plugin/                   # wazero sandbox: ABI, пул инстансов, лимиты
│   ├── ratelimit/                # sliding window по IP/подсети (первая фича ядра, F2)
│   ├── tlspeek/                  # zero-alloc SNI peek и прозрачный replay ClientHello
│   └── observe/                  # логирование (log/slog), лог-лимитер, метрики, admin/pprof
├── configs/                      # примеры конфигурации (wasmgate.yaml, local-echo.yaml)
├── plugins/examples/             # примеры плагинов: Rust no_std, TinyGo + ipguard.wasm
├── benchmarks/                   # методика бенчмарков, сценарии и Linux-результаты
├── deployments/                  # (future) Dockerfile, compose, systemd
├── docs/                         # проектная документация (01–07)
├── README.md / DEVELOPMENT.md
├── go.mod / .golangci.yml / Makefile
└── (future)
    ├── internal/control/         # gRPC-клиент к control plane
    └── pkg/api/                  # сгенерированный protobuf (общий контракт)
```

> Control Plane (C#) — **отдельный репозиторий/решение** `wasmgate-control`
> (src/WasmGate.Control.Api, src/WasmGate.Control.Domain, tests/).

### 5.3. Именование и структура пакетов

- Пакеты: короткие, без `util`/`common`/`helpers`, без подчёркиваний.
- Интерфейсы — по роли (`Reader`, `Balancer`, `PluginHost`), определяются
  **на стороне потребителя** (accept interfaces, return structs).
- Экспорт — только необходимое; всё внутри `internal/`.

### 5.4. Обработка ошибок

```go
// Оборачиваем с контекстом, сравниваем через errors.Is/As.
if err := dialUpstream(ctx, addr); err != nil {
    return fmt.Errorf("dial upstream %s: %w", addr, err)
}
```

- Никаких `panic` в hot path. Паника — только фатальная инициализация.
- Sentinel-ошибки для ожидаемых кейсов, `%w` для обёртки, `errors.Join` для
  агрегации.

### 5.5. Конкурентность

- `context.Context` первым аргументом; отмена — везде, где есть I/O.
- `errgroup.Group` для связанных задач.
- **`sync.Pool`** для переиспользуемых буферов; никаких аллокаций на соединение
  в steady state.
- Предпочтение **атомикам** и неизменяемым снапшотам перед мьютексами в hot path.

### 5.6. Правила горячего пути (hot path)

| Правило | Почему |
|---------|--------|
| Не аллоцировать в steady state | Меньше GC-давления → ровнее p99 |
| Не использовать `reflect` | Непредсказуемые аллокации/CPU |
| Не брать мьютексы | Конкуренция → деградация под нагрузкой |
| Переиспользовать буферы (`sync.Pool`, ring) | Ноль аллокаций на соединение |
| Избегать интерфейсов там, где горячо | Меньше косвенных вызовов |
| Ограничивать работу плагинов по бюджету | Плагин не должен «съесть» соединение |

### 5.7. Тестирование

- Table-driven тесты, `go test -race`.
- **Fuzz** для парсеров (сейчас — SNI/ClientHello; JSON — future).
- Benchmarks с `-benchmem` + `benchstat` для регрессий.
- Интеграционные тесты с реальным upstream (TCP-эхо), chaos-reload под
  нагрузкой, e2e против nginx/HAProxy на Linux.

---

## 6. Модель плагинов (WebAssembly)

**Киллер-фича:** плагин пишется на **любом языке**, компилируется в `.wasm`,
исполняется в песочнице `wazero`.

### 6.1. Контракт / ABI

ABI **не использует WASI и не предоставляет host functions**. Метаданные
передаются числовыми аргументами через стек `wazero` (`CallWithStack`), а
payload `on_data` — в фиксированном scratch-регионе линейной памяти инстанса.
Это даёт 0 B/op и 0 allocs/op на горячем пути.

| Хук | Сигнатура | Семантика |
|-----|-----------|-----------|
| `on_accept` | `(i64, i64, i32) -> i32` | `ip_hi`, `ip_lo` (16-байтный канонический IP, IPv4 = `::ffff:a.b.c.d`), `port`; `0` = allow, **любое ненулевое** = deny |
| `on_data` | `(i32, i32) -> i32` | `ptr`, `len` — payload в линейной памяти; `0` = allow/default pool, `1` = deny, `>= 2` = allow и `route_pools[ret-2]` |
| `on_close` | `(i64, i64, i64, i64) -> ()` | `ip_hi`, `ip_lo`, `rx` (server→client), `tx` (client→server) |

- Плагин обязан экспортировать хотя бы один хук; сигнатуры проверяются при
  компиляции (fail-fast на `Start`/`Reload`, а не на первом клиенте).
- `on_data` может **мутировать байты на месте**; хост копирует изменённый
  префикс обратно в payload до возврата инстанса в пул.
- Инспектируется/мутируется не более `32 KiB` (scratch). Свой буфер задаётся
  обязательным экспортом `on_data_ptr() -> i32`; модуль с `on_data` без него
  отвергается при загрузке.

### 6.2. Режимы `on_data`

- `once` (**по умолчанию**): первый пакет клиента читается до `dial`,
  проверяется/мутируется в WASM, затем `DrainTo` переигрывает его в апстрим и
  возвращает исходный `*net.TCPConn` — остаток сессии снова идёт через
  `splice(2)`. `route_pools` работает только в этом режиме.
- `stream`: каждый чанк `client → server` проходит через `on_data` в data loop.
  `splice` не используется; вердикт `>= 2` трактуется как allow (пул уже выбран
  до `dial`).

### 6.3. Безопасность и ресурсы песочницы

- Изоляция линейной памяти; **нет** файловой системы, сети, env, часов и RNG —
  только чистые вычисления над переданными аргументами.
- `max_memory_pages` (по умолчанию 16 страниц = 1 MiB) ограничивает память;
  модуль с `min > limit` отвергается при загрузке, `memory.grow` сверх лимита
  получает `-1` внутри гостя.
- `timeout > 0` включает `WithCloseOnContextDone(true)`: зависший/trap-инстанс
  прерывается, закрывается и **не возвращается** в пул. `timeout: 0` не создаёт
  таймер на вызов и сохраняет 0 allocs/op.
- Инстансы живут в `sync.Pool` (Reactor-модель): глобальная память не
  сбрасывается между соединениями и **не синхронизирована** между инстансами.
- Версионирование ABI: несовместимый модуль отвергается на этапе
  загрузки/reload, до публикации snapshot.

### 6.4. SDK и примеры

| Язык | Как собирать | Пример |
|------|--------------|--------|
| Rust | `cargo build --target wasm32-unknown-unknown` (`#![no_std]`) | `plugins/examples/ipguard/` |
| TinyGo | `tinygo build -target=wasm-unknown -no-debug` | `plugins/examples/ipguard/tinygo/` |
| C# / AssemblyScript | не проверялись | — |

---

## 7. Zero-Downtime Hot Reload (механика ядра)

**Идея:** конфигурация — это **неизменяемый снапшот**; переключение — атомарная
замена указателя. Читатели (обработчики соединений) не блокируются никогда.

```go
// internal/proxy/snapshot.go — текущая реализация (упрощённо)
type resolvedListener struct {
    cfg         config.ListenerConfig
    pool        *pool                  // upstream_pool
    limiter     *connLimiter           // max_conns (живёт в registry)
    rateLimiter *ratelimit.Limiter     // rate_limit (живёт в registry)
    plugin      *plugin.Plugin         // скомпилированный WASM
    sniRoutes   map[string]*pool       // lower-case host -> pool
    routePools  []*pool                // on_data >= 2
    tlsConfig   *tls.Config            // терминация; nil = passthrough
}

type Snapshot struct {
    Version   uint64
    Config    *config.Config
    listeners map[string]resolvedListener
}

type Runtime struct {
    cur      atomic.Pointer[Snapshot]
    mu       sync.Mutex // сериализует Reload: версия — read-modify-write
    registry *registry  // состояние, живущее дольше snapshot (см. ниже)
}

// Load — один атомарный load; вызывается на КАЖДОМ соединении.
func (r *Runtime) Load() *Snapshot { return r.cur.Load() }

// Reload строит новый снапшот и публикует его (SIGHUP / POST /reload / CP).
func (r *Runtime) Reload(cfg *config.Config) (uint64, error) {
    r.mu.Lock()
    defer r.mu.Unlock()
    next, err := r.build(r.cur.Load().Version+1, cfg)
    if err != nil {
        return 0, err // старый snapshot остаётся активным
    }
    r.cur.Store(next) // RCU: старые читатели держат прошлый, новые видят новый
    return next.Version, nil
}
```

> **Что НЕ кладём в snapshot.** Всё, что зависит от истории — счётчик активных
> соединений листенера (`max_conns`) и счётчики `active` у бэкендов — живёт в
> `registry` (`internal/proxy/state.go`), который переживает reload. Иначе каждый
> reload обнулял бы лимит (N reload — N лимитов) и ломал `least_conn`. Snapshot
> хранит только неизменяемую конфигурацию и ссылки на эти долгоживущие объекты.

> Сама замена указателя атомарна и в блокировке не нуждается, но **инкремент
> версии** — это read-modify-write: без `mu` два одновременных reload могли бы
> опубликовать одну и ту же версию. `mu` берётся только в `Reload`, на hot path
> его нет.
>
> `listeners` — предвычисленный индекс: горячий путь достаёт листенер, пул,
> лимитеры, плагин, SNI-таблицу и TLS-конфиг **одним** обращением к map, вместо
> линейного поиска по списку. Поэтому поля `Routes`/`Plugins`/`Limits` не нужны
> отдельно: всё уже разрешено внутри `resolvedListener`.
>
> **Ограничение замены плагина.** Runtime старого `plugin.Plugin` не закрывается
> при замене/удалении в новом snapshot (известный открытый вопрос, `docs/06`);
> уже взятые инстансы доживают на старом runtime.

**Reconcile слушателей.** `Start` открывает порты; `Server.Reload` при смене
конфига добавляет новые адреса, закрывает удалённые и **не трогает** неизменённые.
Порядок безопасен: сначала биндятся новые сокеты, при ошибке — откат без
публикации snapshot; затем публикуется snapshot; затем стартуют accept-loop'ы
новых адресов; затем закрываются старые сокеты.

**Drain-политика:**

- новые соединения получают **новый** snapshot;
- уже установленные — доживают на **своём** snapshot'е (без принудительного
  разрыва), пока не истечёт `-shutdown-grace`; только после этого они закрываются
  принудительно (и `Shutdown` возвращает ошибку таймаута);
- при ошибке применения (валидация, занятый адрес) — прежняя конфигурация
  остаётся активной целиком (**откат**: snapshot всегда доступен).

**Поток применения (план для CP; сейчас — SIGHUP/POST /reload):**

```
CP: validate → build snapshot → publish → ждёт ack + health от DP
DP: Apply(snapshot) → atomic store → ack → метрика wasmgate_config_version
```

**Транспорт CP→DP (план):** gRPC по Unix Domain Socket (низкая латентность,
локально, без сетевого стека), push-модель + периодический heartbeat.

---

## 8. Защита от всплесков соединений (скользящее окно)

**Область применимости (честно).** Проверка выполняется **после `accept()`**, то
есть в user-space. Это защита от *connection flood* (много соединений в единицу
времени), а **не** от объёмных атак: SYN-flood и подобное гасится ядром ОС до
`accept()`, и для этого нужен отдельный слой (eBPF/XDP — F4). В тексте ВКР это
надо называть именно rate limiting'ом соединений, а не «DDoS-защитой» целиком.

**Задача:** в hot path считать частоту новых соединений по ключу (IP и/или
подсеть) за окно времени и принимать вердикт **без блокировок**.

> Состояние окон живёт в `registry` (как `max_conns`), а не в snapshot, — иначе
> reload сбрасывал бы накопленные счётчики, открывая ту же дыру, что и со
> сбросом лимита соединений.

**Алгоритм:** *sliding window* на кольцевых слотах (bucketed ring counter) —
комбинация точности и скорости.

```go
// internal/ratelimit/window.go — фактическая реализация (упрощённо)
const (
    slotCount  = 10   // окно делится на 10 слотов
    shardCount = 64   // независимых таблиц, чтобы IP редко конкурировали
    shardCap   = 4096 // ключей на шард; при переполнении шард очищается
)

type Limiter struct {
    window   time.Duration
    shards   [shardCount]shard
    allow    []Prefix // CIDR allow-list
    banBase  time.Duration // 0 = ban выключен
}
```

- Ключ — канонический 16-байтный IP; IPv4 и IPv4-mapped IPv6 дают один ключ.
  Отдельно считается ключ подсети: `/24` для IPv4 и `/64` для IPv6.
- Счётчик окна — сумма 10 атомарных слотов; слот выбирается по времени.
  Свод по окну — O(1) на вердикт, без мьютексов.
- Память ограничена: `shardCap` ключей на шард, при переполнении шард
  очищается (это осознанный компромисс против неограниченного роста).
- **CIDR allow-list** проверяется до подсчёта: такие адреса не банятся.
- При превышении — бан на `ban_duration`; повторные нарушения дают
  экспоненциальный backoff `1x, 2x, 4x, 8x, 16x` (потолок).
- Метрики отказов раздельные: `max_conns`, rate limit и plugin deny.

**Почему в фаст-пасе это не бьёт по перфу:** одна атомарная инкрементация +
деление на кольцо; свод по окну — O(1) на вердикт; никаких блокировок.
Измерено на Linux: ≈65 нс на вызов (≈20 нс в параллельном тесте), 0 B/op,
0 allocs/op (`BenchmarkAllow` / `BenchmarkAllowParallel`).

**Чего здесь нет (осознанно):** token-bucket throttle, EWMA-аномалий и
LRU-эвикции по каждому ключу. Окно и бан уже покрывают connection flood;
более сложные модели добавили бы состояние и цену без доказанной пользы для
дипломного сценария.

---

## 9. Безопасность

- **TLS:** реализованы passthrough (zero-alloc SNI-peek и `sni_routes`) и
  опциональная терминация (`listeners[].tls`, TLS 1.2/1.3, горячая перезагрузка
  сертификата). mTLS к control plane — план.
- **Плагины:** песочница WASM без WASI + лимиты памяти/времени (см. §6).
- **Секреты:** пути к сертификатам/ключам в конфиге; сами ключи не логируются.
  Вынос в env/secret store — план.
- **OWASP API Security Top-10** — чек-лист для будущего L7-этапа.
- **Supply chain:** фиксированные версии в `go.mod`; `govulncheck`/подпись
  артефактов — план.

---

## 10. Ключевые решения (ADR)

| ID | Решение | Альтернативы | Обоснование | Статус |
|----|---------|--------------|-------------|--------|
| ADR-1 | **`net`** (горутина на соединение) как L4-движок | `gnet`, `fasthttp` | прост, `splice`/backpressure бесплатны, достаточно быстр; `gnet` — опция после замеров | Accepted (было: gnet) |
| ADR-2 | wazero как WASM-рантайм | wasmtime/wasmer | чистый Go, **нет CGO**, быстро, sandbox | Accepted |
| ADR-3 | Snapshot + `atomic.Pointer` для reload | double-buffer с мьютексом | non-blocking RCU, 0 разрывов | Accepted |
| ADR-4 | C# control plane + gRPC/UDS | REST, файл+watch, Go control plane | строгий контракт, быстрый локальный канал | Accepted |
| ADR-5 | Стек стартует как **L4 (TCP)** | сразу L7 HTTP | быстрее получить baseline-перф | Accepted |
| ADR-6 | Ядро: L4 + connection flood (sliding window) + Hot Reload; L7-lite (WASM `on_data`, SNI, TLS) реализован точечно; DPI JSON и eBPF — future work | всё сразу | фокус и измеримость | Accepted (обновлён) |
| ADR-7 | Релей: на Linux — `io.Copy` → `splice(2)`; вне Linux — стандартная петля `io.CopyBuffer` с буфером из пула, выбор по одной проверке `runtime.GOOS` в `copyPlain` | build-теги (`copy_linux.go`/`copy_other.go`); свой цикл копирования; голый `io.Copy` везде | целевая платформа не тронута (splice, 0 аллокаций); в data plane нет своего цикла (это семантика stdlib); цена вне Linux — 4 wrapper'а ~100 B на соединение против 64 KiB у голого `io.Copy`; выбор платформы остаётся в одном файле; цены и альтернативы — benchmarks/README.md, §«Платформа» | Accepted (было: сплит по build-тегам) |
| ADR-8 | WASM ABI **без WASI и host functions**: `CallWithStack` над числовыми аргументами + scratch-память для `on_data` | WASI (`wasip1`) + host functions; строки через линейную память | WASI тянет системные вызовы и лишние копии; числовой ABI даёт 0 allocs/op и узкую поверхность атаки; ценою становится ограниченный набор аргументов | Accepted |
| ADR-9 | TLS: **passthrough через zero-alloc SNI peek** по умолчанию; терминация — opt-in на листенере | всегда терминация; только passthrough; внешний terminator | passthrough не ломает end-to-end TLS и сохраняет `splice`; терминация нужна лишь там, где плагин должен видеть открытый текст; при терминации `splice` отключается | Accepted |
| ADR-10 | L7-инспекция: `on_data` в режиме `once` (первый пакет) по умолчанию, `stream` — opt-in; L7-роутинг — через `route_pools` | всегда per-chunk; отдельный L7-парсер в ядре | `once` покрывает routing/mutation первого запроса и сохраняет zero-copy; `stream` — осознанная цена; ядро не превращается в HTTP-парсер | Accepted |

> **ADR-4, оговорка.** Второй рантайм (.NET) ради управления версиями конфига —
> это осознанный overhead: он оправдан, только если нужен **строгий контракт**
> (protobuf/gRPC), реестр версий и аудит «из коробки», и если у команды есть
> компетенция в .NET. Иначе тот же контур дешевле сделать на Go или «файл +
> watch». В ВКР это стоит защищать именно так, а не как «так модно».

---

## 11. Бюджет производительности (perf budget)

| Показатель | Бюджет | Факт (Linux, Ryzen 5 3550H) |
|------------|--------|-----------------------------|
| Аллокации на fast path (steady state) | **0 alloc / conn** | 0 allocs/op у rate limit, `on_accept`, `on_data`, SNI-парсера |
| Оверхед пустого/лёгкого плагина | ≤ 5 % p99 | `on_accept` ≈ 81 нс, `on_data` ≈ 76 нс на вызов |
| Оверхед rate limiter | ≤ 5 % throughput | ≈ 65 нс на соединение (≈ 20 нс parallel) |
| Латентность прокси (L4, мкс) | +≤ 100–200 мкс | relay latency: 62 мкс (WasmGate) против 394 мкс (nginx) и 439 мкс (HAProxy) |
| Throughput (64 KiB / 1 MiB) | не хуже nginx/HAProxy | 588.8 / 688.3 MB/s (L4), 734.1 / 794.9 MB/s (full) против 272.5 / 230.9 (nginx), 268.9 / 248.3 (HAProxy) |
| Память на снапшот конфига | O(числа маршрутов/плагинов) | O(listeners + pools + upstreams) |
| Время применения конфига | < 100 мс (локально) | reload под нагрузкой: 12 перезагрузок, 0 разорванных соединений |
| Время загрузки плагина | < 50 мс на плагин | компиляция wazero один раз при старте/reload |

> Методика измерения — в [`03-mvp.md`](03-mvp.md), §8; дословный вывод —
> [`benchmarks/linux-ubuntu-results.txt`](../benchmarks/linux-ubuntu-results.txt).
> Числа сняты до батча circuit breaker/TLS (06.11) и не включают TLS. `full` =
> `max_conns` + `rate_limit` + `ipguard.wasm` (без circuit breaker/TLS).
