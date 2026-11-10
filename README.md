# WasmGate

**Быстрый TCP/L4 reverse proxy с расширением через WebAssembly.** Один статический
бинарник на Go: zero-copy на Linux, горячая перезагрузка без разрыва соединений,
защита периметра, TLS/SNI и готовый набор WASM-плагинов для типовых задач nginx.

![Go 1.25+](https://img.shields.io/badge/Go-1.25%2B-00ADD8?logo=go)
![WebAssembly](https://img.shields.io/badge/plugins-WebAssembly-654FF0?logo=webassembly)
![License MIT](https://img.shields.io/badge/license-MIT-green)

| Параметр | Значение |
|---|---|
| Состояние | рабочее ядро + 8 showcase-плагинов + ABI v3 + тесты + Linux-бенчмарки |
| Data plane | Go 1.25+, без CGO |
| Плагины | WebAssembly, [wazero](https://github.com/tetratelabs/wazero), без WASI |
| Zero-copy | `splice(2)` на Linux, буфер из пула на остальных платформах |
| Лицензия | MIT |

---

## Что это и для кого

WasmGate стоит на периметре и принимает TCP-соединения. По умолчанию он работает
как очень быстрый L4-прокси: принимает соединение, применяет лимиты, выбирает
бэкенд и перекладывает байты. Когда нужна логика прикладного уровня, включается
WASM-плагин: он видит первый пакет или каждый чанк, может пропустить трафик,
запретить его, изменить байты на месте или выбрать другой пул.

Это даёт «быстро по умолчанию, умно там, где нужно»: плата за L7-инспекцию
возникает только на тех листенерах, где она явно включена.

**Кому подходит:**

- TCP-шлюз для PostgreSQL, Redis, SSH, gRPC и других не-HTTP протоколов;
- маршрутизация TLS по SNI без расшифровки (`ssl_preread`-сценарий);
- canary/blue-green по `Host`, пути, SNI или первым байтам протокола;
- собственная политика на периметре без пересборки прокси;
- защита от connection flood на скользящем окне по IP и подсети;
- единая точка наблюдаемости для нескольких бэкендов.

**Когда лучше взять другое решение:** если нужен полноценный HTTP reverse proxy
с HTTP/2, кэшированием, раздачей статики и богатой экосистемой модулей — nginx
или Envoy уже это умеют. WasmGate силён там, где нужен быстрый TCP/L4-шлюз с
расширяемой логикой и предсказуемым reload.

---

## Сильные стороны

### Zero-copy на Linux

На Linux `io.Copy` доходит до `TCPConn.ReadFrom` и использует `splice(2)`: ядро
перекладывает байты между сокетами, не поднимая их в user space. В опубликованном
прогоне это дало **557–725 MB/s** на 64 KiB и **862–977 MB/s** на 1 MiB при
**0 allocs/op** на копирование.

### Быстрее nginx stream и HAProxy TCP на крупных потоках

В том же Linux-прогоне на одном эхо-бэкенде:

| Сценарий | WasmGate L4 | nginx stream | HAProxy TCP |
|---|---:|---:|---:|
| Throughput, 64 KiB | **588.8 MB/s** | 272.5 MB/s | 268.9 MB/s |
| Throughput, 1 MiB | **688.3 MB/s** | 230.9 MB/s | 248.3 MB/s |
| Relay latency, 1 B RTT | **62.0 µs** | 394.0 µs | 439.2 µs |

WasmGate с включёнными `max_conns`, `rate_limit` и `ipguard.wasm` показал
**734.1 MB/s** на 64 KiB и **794.9 MB/s** на 1 MiB (в пределах шума от чистого
L4): проверки выполняются один раз на соединение, а не на каждый байт.

### Дешёвые плагины

WASM-хуки не перекладывают данные через строки и JSON: метаданные идут через
стек регистров, буферы — через линейную память. На Linux:

- `on_accept` — **81 нс**, параллельно **25 нс**, 0 allocs/op;
- `on_data` — **76–94 нс**, 0 allocs/op;
- `on_data_state` (ABI v3) — **111–122 нс**, 0 allocs/op;
- разбор SNI из `ClientHello` — **26 нс**, 0 allocs/op.

### Горячая перезагрузка без потерь

Конфигурация живёт в неизменяемом snapshot. Reload строит новый snapshot,
проверяет плагины и сертификаты **до** публикации и атомарно подменяет указатель.
Старые соединения доигрывают на старом snapshot. В тесте под нагрузкой —
**12 reload, 0 разорванных соединений**. Счётчики лимитов, окна rate limit,
состояние circuit breaker и `least_conn` переживают reload.

### Восемь готовых плагинов

В репозитории лежат исходники и **уже собранные `.wasm`**: Rust для запуска не
нужен. Плагины покрывают L4-фильтрацию, HTTP-политики, маршрутизацию, SNI,
Redis ACL и stateful-инспекцию тела запроса.

### Безопасность по умолчанию

Плагин — это не нативная библиотека в процессе. Это WASM в песочнице:

- нет WASI: ни файлов, ни сети, ни часов, ни `rand`;
- жёсткий лимит линейной памяти (по умолчанию 1 MiB на инстанс);
- опциональный таймаут на вызов;
- пул инстансов, а не инстанс на соединение;
- состояние ABI v3 изолировано по соединению и направлению и зануляется после
  возврата инстанса в пул.

### Один бинарник

Чистый Go, без CGO и внешних зависимостей времени выполнения. Собирается под
Linux и Windows, кросс-компилируется одной командой.

---

## Возможности

### Data plane

- L4/TCP reverse proxy на стандартном `net`: горутина на соединение, честный
  backpressure.
- Балансировка `round_robin` и `least_conn`; нездоровые бэкенды пропускаются,
  при полном отказе пул работает fail-open, чтобы дать шанс восстановлению.
- Прозрачный `dial_retries`: дополнительные попытки на других серверах пула,
  пока клиентский payload ещё не отправлен.
- `dial_timeout`, `idle_timeout` (общий для соединения, а не по направлениям),
  `max_conns`.
- Half-close и корректный teardown: чистый EOF закрывает только запись, ошибка
  или таймаут — оба направления.
- Graceful shutdown: перестаём принимать, доигрываем активные соединения до
  `-shutdown-grace`, затем закрываем принудительно.

### Надёжность

- Трёхфазный circuit breaker на каждый upstream: `Closed → Open → Half-Open`,
  `error_threshold`, `open_duration`, `half_open_requests`.
- Пассивная отметка нездоровья по ошибкам `dial` и активный TCP-пробинг
  (`health_check.interval` / `timeout`).
- Локальные ошибки (`EMFILE`, `ENOMEM`, `EADDRNOTAVAIL`, отмена контекста) не
  помечают бэкенд мёртвым: проблема на стороне прокси, а не upstream.
- Fail-closed reload: битый конфиг, плагин или сертификат не публикуется; старый
  snapshot продолжает работать.
- Reconcile слушателей: добавление, удаление и переименование без перезапуска,
  с откатом при ошибке бинда.

### Периметр

- Скользящее окно по IP и подсети: 10 слотов, 64 шарда, **0 allocs/op**,
  ~65 нс на проверку.
- IPv4: точный адрес для хоста, `/24` для подсети.
- IPv6: настраиваемые префиксы, по умолчанию `/64` для хоста и `/56` для
  подсети — один адрес не обойдёт лимит ротацией внутри выделенной сети.
- CIDR allow-list, экспоненциальный бан `1x…16x`, ограничение одновременных
  соединений на IP и подсеть.
- Проверки выполняются до `dial`, поэтому отказ не занимает upstream-соединение.

### TLS и маршрутизация

- **TLS passthrough**: чтение SNI из `ClientHello` без расшифровки, маршрутизация
  по точному имени или `*.domain`, прозрачный replay исходных байтов.
- **TLS termination**: TLS 1.2/1.3, горячая перезагрузка сертификата, настраиваемый
  `handshake_timeout`; после расшифровки плагин видит открытый текст.
- Комбинация с L7-плагинами: HTTP-политики, маршрутизация по `Host`/пути,
  маскировка ответов.

### WASM-плагины

- ABI v1: `on_accept`, `on_data`, `on_close`.
- ABI v2: `on_data_ex`, `on_close_ex` — `conn_id` и направление (`0` — запрос,
  `1` — ответ).
- ABI v3: `on_data_state` — host-managed состояние до 4 KiB на направление,
  переживающее смену инстанса и keep-alive запросы.
- Режимы `once` (первый пакет, затем возврат на `splice`) и `stream` (каждый чанк
  в обе стороны).
- In-place мутация байтов и выбор пула через `route_pools` (в `once`).
- Пул инстансов, лимит памяти, опциональный таймаут, уничтожение «испорченного»
  инстанса вместо возврата в пул.

### Эксплуатация

- Строгая валидация YAML с отказом на неизвестные ключи; `-validate` проверяет
  конфиг, WASM-модули и TLS-сертификаты.
- `SIGHUP` (Unix) и `POST /reload` (admin) с single-flight защитой от
  параллельных reload.
- Prometheus-метрики, `/healthz`, опциональный `pprof` на отдельном admin-листенере.
- Структурные логи `log/slog` с ограничением частоты «шумных» событий, чтобы
  флуд не превратил лог в DoS.
- Админ-эндпоинт по умолчанию только на loopback; токен, проверка `Host`,
  same-origin для reload, лимит одновременных подключений.

---

## Готовые сценарии вместо nginx-фич

В `configs/showcase.yaml` собраны листенеры под каждый плагин. Конфиг валидируется
без запуска бэкендов:

```bash
go run ./cmd/wasmgate -config configs/showcase.yaml -validate
```

| Задача | Аналог в nginx | Плагин | Режим |
|---|---|---|---|
| Allow/deny по IP до соединения с бэкендом | `allow` / `deny` / `geo` | `ipguard` | L4 `on_accept` |
| Методы, пути, Basic auth, CL/TE-smuggling, маскировка заголовков | `access` + `proxy_hide_header` | `http_guard` | `stream` |
| Маршрутизация по `Host` и пути | `map` + `location` | `path_router` | `once` + `route_pools` |
| Маршрутизация TLS по SNI без расшифровки | `ssl_preread` + `map` | `sni_guard` | `once` |
| Диспетчеризация TLS/HTTP/SSH/PostgreSQL/Redis по первым байтам | `stream` + `map` | `protocol_router` | `once` |
| ACL для Redis: `CONFIG`, `FLUSHALL`, `EVAL`, `CLIENT KILL` | Redis ACL / `stream` | `redis_guard` | `stream` |
| Сигнатуры атак в теле и маскировка JSON-секретов в ответе | `sub_filter` + WAF | `stream_guard` | `stream` |
| Сборка разорванных HTTP-заголовков, `Content-Length`, бюджеты keep-alive, поиск сигнатур между чанками | stateful HTTP body inspection | `http_body_guard` | `stream`, ABI v3 |

Эталонный stateful-плагин — `http_body_guard`: он собирает заголовки по чанкам,
считает тело, ловит сигнатуры в rolling tail 64 байта, ограничивает запрос
64 KiB, соединение 128 KiB и 5 keep-alive запросов, а также маскирует секреты в
ответах.

---

## Быстрый старт

### Требования

- **Go 1.25+** (в `go.mod` зафиксирован toolchain 1.27.1).
- Rust **не нужен**: `.wasm` уже собраны и закоммичены.
- C-компилятор нужен только для `go test -race`; обычная сборка и запуск — без CGO.

### Сборка и проверка конфига

```bash
go build -o wasmgate ./cmd/wasmgate

# Linux/macOS
./wasmgate -config configs/wasmgate.yaml -validate

# Windows
.\wasmgate.exe -config configs/wasmgate.yaml -validate
```

### Запуск

```bash
./wasmgate -config configs/wasmgate.yaml
```

Параметры:

- `-config` — путь к YAML-конфигу (по умолчанию `configs/wasmgate.yaml`);
- `-log-level` — `debug` | `info` | `warn` | `error`;
- `-shutdown-grace` — сколько ждать активные соединения при останове (по умолчанию 15 с);
- `-validate` — проверить конфиг, плагины и сертификаты и выйти.

Останов: `Ctrl+C` — graceful drain. На Unix `SIGHUP` перечитывает конфиг; на всех
платформах доступен `POST /reload` на `admin_addr`.

### Smoke-тест

```bash
# в отдельном окне — эхо-бэкенд из DEVELOPMENT.md, §5
go run ./cmd/wasmgate -config configs/local-echo.yaml -log-level debug

curl http://127.0.0.1:9098/healthz
curl http://127.0.0.1:9098/metrics
```

### Production-сборка

```bash
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o wasmgate ./cmd/wasmgate
```

Готовый пример systemd-юнита и рекомендации по развёртыванию:
[`deployments/README.md`](deployments/README.md).

---

## Конфигурация

```yaml
version: 1

# Опциональный admin-листенер: /metrics, /healthz, /reload, /debug/pprof/.
# Пусто или отсутствует = выключен. Non-loopback требует token + allow_remote.
admin_addr: "127.0.0.1:9099"
# admin_token_file: "/etc/wasmgate/admin.token"   # или WASMGATE_ADMIN_TOKEN
admin_pprof: false          # профилировщик выключен по умолчанию
admin_allow_remote: false   # true требует admin_token_file/WASMGATE_ADMIN_TOKEN

listeners:
  - name: edge
    addr: "127.0.0.1:8080"
    upstream_pool: web
    max_conns: 10000          # 0 = без лимита (WARN при старте)
    dial_timeout: 3s          # по умолчанию 5s
    idle_timeout: 0s          # 0 = zero-copy splice; >0 = общий idle watchdog
    rate_limit:
      max: 200                # новых соединений с IP за окно (0 = выкл.)
      subnet_max: 1000        # новых соединений с подсети за окно (0 = выкл.)
      max_concurrent: 0       # одновременных с IP (0 = выкл.; WARN при старте)
      subnet_max_concurrent: 0
      ipv6_host_prefix: 64    # IPv6 host-группировка (по умолчанию 64)
      ipv6_subnet_prefix: 56  # IPv6 subnet-группировка (по умолчанию 56)
      window: 1s              # минимум 10ms
      ban_duration: 5s        # база бана, рост до 16x; 0 = без бана
      allow_cidrs: ["127.0.0.1/32"]
    plugin:                   # опциональный WASM-плагин
      path: "plugins/examples/ipguard/ipguard.wasm"
      max_memory_pages: 16    # 16 * 64 KiB = 1 MiB на инстанс
      timeout: 0s             # 0 = без таймера; только для доверенных плагинов
      data_mode: "once"       # once | stream
      route_pools: ["canary"] # on_data >= 2 -> route_pools[ret-2]
    sni_routes:               # TLS passthrough; игнорируется при listeners[].tls
      "api.example.com": web
      "*.example.com": web
    # tls:                    # TLS-терминация вместо passthrough
    #   cert_file: "certs/server.crt"
    #   key_file: "certs/server.key"
    #   min_version: "1.2"    # 1.2 | 1.3
    #   handshake_timeout: 10s

pools:
  web:
    balance: round_robin      # round_robin | least_conn
    dial_retries: 1           # всего попыток 1 + N
    health_check:
      interval: 2s            # 0 = только пассивная проверка
      timeout: 1s             # по умолчанию 1s
    circuit_breaker:
      error_threshold: 5      # 0 = размыкать после первой ошибки
      open_duration: 10s      # по умолчанию 10s
      half_open_requests: 2   # по умолчанию 1
    servers:
      - "127.0.0.1:9001"
      - "127.0.0.1:9002"

  canary:
    balance: least_conn
    servers:
      - "127.0.0.1:9003"
```

### Правила валидации

- `version` должен быть `1`; неизвестные ключи отклоняются.
- Имена листенеров и адреса уникальны; порт `0` — «любой свободный», исключён из
  проверки дублей.
- `upstream_pool`, `sni_routes` и `plugin.route_pools` должны ссылаться на
  существующие пулы.
- Пул не может быть пустым; дубли серверов и нечисловые порты отклоняются.
- `balance` — `round_robin` или `least_conn`.
- Таймауты, `max_conns`, `dial_retries`, `rate_limit.*` и поля breaker'а не могут
  быть отрицательными; `window` при включённом лимите — не меньше 10 мс.
- `ipv6_host_prefix`/`ipv6_subnet_prefix` — в диапазоне 1…128, host ≥ subnet.
- `plugin.data_mode` — `once` или `stream`; `max_memory_pages ≤ 65536`.
- `tls` требует одновременно `cert_file` и `key_file`; `min_version` — `1.2`/`1.3`;
  `handshake_timeout` по умолчанию 10 с.
- При `tls` включён, `sni_routes` на этом листенере игнорируется: терминация уже
  сняла TLS, passthrough-маршрутизация по SNI неприменима.

### Что включает буферизованный путь

| Настройка | Путь копирования | `splice(2)` |
|---|---|---|
| нет L7-хука, `idle_timeout: 0` | `io.Copy` (Linux) / пул-буфер | да (Linux) |
| `data_mode: once` | первый пакет в WASM, затем raw-сокет | да, после `DrainTo` |
| `data_mode: stream` | каждый чанк через плагин | нет |
| `listeners[].tls` | `*tls.Conn` + расшифровка | нет |
| `idle_timeout > 0` | буфер + watchdog на оба направления | нет |

Это осознанный trade-off: `splice` нельзя использовать, если нужно смотреть или
менять каждый байт. Дефолт `idle_timeout: 0` сохраняет zero-copy.

---

## Плагины

### Контракт ABI

| Экспорт | Сигнатура | Назначение |
|---|---|---|
| `on_accept` | `(i64, i64, i32) -> i32` | L4 allow/deny до `dial` |
| `on_data` | `(i32, i32) -> i32` | L7-инспекция первого пакета / потока |
| `on_data_ex` | `(i64, i32, i32, i32) -> i32` | то же + `conn_id` и направление |
| `on_data_state` | `(i64, i32, i32, i32, i32, i32, i32) -> i64` | то же + состояние до 4 KiB на направление |
| `on_close` | `(i64, i64, i64, i64) -> ()` | статистика соединения |
| `on_close_ex` | `(i64, i64, i64, i64, i64) -> ()` | то же + `conn_id` |

Вердикты: `0` — allow, `1` — deny, `>= 2` — `route_pools[verdict-2]` (только
`once`). Отрицательные значения и `INT32_MIN` трактуются как deny; индекс
маршрута вне диапазона — тоже deny, а не «default pool».

### Ограничения песочницы

- Нет WASI: файловая система, сеть, окружение, часы и `rand` недоступны.
- `memory.grow` сверх `max_memory_pages` возвращает ошибку внутри гостя, не
  затрагивая процесс Go.
- При `timeout > 0` зависший или упавший хук прерывается, а инстанс уничтожается.
- При `timeout: 0` таймера нет: это быстрее, но подходит только для доверенных
  плагинов. При загрузке пишется WARN.
- Инстансы переиспользуются через `sync.Pool`; глобальная память WASM не
  привязана к соединению. Для состояния используйте ABI v3.

### Сборка своих плагинов

```bash
cd plugins/examples
rustup target add wasm32-unknown-unknown
cargo build --release --target wasm32-unknown-unknown
```

Подробный контракт, ограничения и примеры: [`plugins/examples/README.md`](plugins/examples/README.md).

---

## Архитектура

```
клиенты
   │
   ▼
┌──────────────────────────── Data Plane (Go) ────────────────────────────┐
│ accept → max_conns / rate_limit / guard → on_accept                      │
│        → TLS termination | SNI peek (passthrough)                        │
│        → on_data (once | stream) → balance / dial → copy / splice        │
│                              ▲                                           │
│                              │ atomic.Pointer<Snapshot>                  │
│                    SIGHUP / POST /reload                                 │
└──────────────────────────────┬───────────────────────────────────────────┘
                               │
                               ▼
                        upstream pools
```

- Ядро оптимизировано под L4: по умолчанию payload не разбирается, байты идут
  через `splice(2)`.
- L7 включается точечно: SNI peek, TLS-терминация, `on_data` в `once`/`stream`.
  Это не «переключение всего прокси в медленный режим», а решение на уровне
  листенера.
- Конфигурация неизменяема на время соединения: reload не может поменять правила
  посреди запроса.
- Состояние, зависящее от истории (лимиты, окна, breaker, active-счётчики),
  живёт в registry и переживает reload.

Подробнее: [`docs/02-architecture-tech.md`](docs/02-architecture-tech.md).

---

## Производительность

Методика, сценарии и дословные результаты: [`benchmarks/README.md`](benchmarks/README.md).
Коротко, Linux/WSL2, AMD Ryzen 5 3550H:

| Метрика | Результат |
|---|---|
| `Throughput/zero_copy/64KiB` | 557–725 MB/s, 0 allocs |
| `Throughput/zero_copy/1MiB` | 862–977 MB/s, 0 allocs |
| `Throughput/idle_watchdog/1MiB` | 248–265 MB/s |
| `RelayLatency/through_proxy` | 56–60 µs |
| `ratelimit/BenchmarkAllow` | 65.4 ns, 0 allocs |
| `plugin/BenchmarkOnAccept` | 81.3 ns, 0 allocs |
| `plugin/BenchmarkOnData` | 76.0 ns, 0 allocs |
| `plugin/BenchmarkOnDataState` | 111–122 ns, 0 allocs |
| `tlspeek/BenchmarkParseClientHelloSNI` | 26.0 ns, 0 allocs |

Сравнение с `nginx stream` и `HAProxy TCP` снято на коммите `18d4249` (до батча
circuit breaker/TLS); в этом прогоне не задействованы TLS-терминация,
`stream`-режим и breaker, поэтому цифры относятся к чистому L4 + `once`-проверке.
Zero-copy и микрометрики воспроизводятся текущими бенчмарками. Для собственных
цифр запускайте на Linux:

```bash
go test -run '^$' -bench . -benchmem ./...
```

Важно: `splice(2)` есть только на Linux. На Windows/macOS используется буфер из
пула, поэтому абсолютные MB/s и разница `zero_copy` vs `idle_watchdog` там не
показательны; корректно сравнивать только на Linux.

---

## Наблюдаемость

Admin-листенер (`admin_addr`) отдаёт:

- `GET /metrics` — Prometheus-текст:
  `wasmgate_connections_accepted_total`, `wasmgate_connections_closed_total`,
  `wasmgate_connections_active`,
  `wasmgate_connections_limit_rejected_total`,
  `wasmgate_connections_rate_rejected_total`,
  `wasmgate_connections_plugin_rejected_total`,
  `wasmgate_plugin_instances_discarded_total`,
  `wasmgate_upstream_dial_failures_total`, `wasmgate_upstream_retries_total`,
  `wasmgate_relayed_bytes_total`, `wasmgate_reloads_total`,
  `wasmgate_config_version`, `wasmgate_uptime_seconds`,
  `wasmgate_upstream_active{server=...}`, `wasmgate_upstream_healthy{server=...}`,
  плюс `go_goroutines` и `go_mem_heap_alloc_bytes` для контроля утечек.
- `GET /healthz` — JSON со статусом, версией конфига, активными соединениями и
  uptime.
- `POST /reload` — атомарный reload из файла; параллельный вызов получает `409`.
- `GET /debug/pprof/` — только при `admin_pprof: true`.

Безопасность admin по умолчанию: loopback-only, проверка `Host`, same-origin для
reload, опциональный Bearer-токен, лимит одновременных подключений и таймауты
HTTP-сервера.

---

## Безопасность и production-настройки

Рекомендуемый минимум для периметра:

```yaml
listeners:
  - name: edge
    addr: "0.0.0.0:8443"
    upstream_pool: web
    max_conns: 20000
    dial_timeout: 3s
    idle_timeout: 60s          # осознанно отключает splice; 0 — если нужен zero-copy
    rate_limit:
      max: 200
      subnet_max: 2000
      max_concurrent: 100
      subnet_max_concurrent: 2000
      window: 1s
      ban_duration: 30s
    plugin:
      path: "plugins/examples/ipguard/ipguard.wasm"
      max_memory_pages: 16
      timeout: 50ms            # для недоверенных плагинов обязателен
      data_mode: "once"
```

- `max_conns: 0` и `max_concurrent: 0` — это «без лимита»; при старте будет WARN.
- Для недоверенных плагинов всегда задавайте `timeout`; `0` — только для своих.
- `idle_timeout > 0` защищает от полуоткрытых соединений, но отключает `splice`.
- TLS-терминация расшифровывает только клиентскую сторону; upstream — обычный TCP.
- Reload fail-closed: ошибка в новом конфиге не затрагивает работающий трафик.
- Секреты (ключи, токены) не логируются; admin-токен можно передать через
  `WASMGATE_ADMIN_TOKEN` или `admin_token_file`.

---

## Границы текущей версии

Это не «недоделки», а осознанно очерченный периметр. Важно понимать их до
внедрения:

- **Только TCP.** UDP, HTTP/2 и QUIC не проксируются.
- **TLS-терминация только клиентская.** После расшифровки трафик идёт к бэкенду
  обычным TCP; upstream TLS/mTLS — в дорожной карте.
- **SNI-маршрутизация только в passthrough.** При `listeners[].tls` `sni_routes`
  игнорируется.
- **`once` — не фильтр.** Проверяется первый пакет (до 16 389 байт). Клиент может
  прислать 1 байт и обойти проверку остального payload; для фильтрации — `stream`.
- **`stream` отключает `splice`.** Это цена инспекции каждого чанка.
- **`http_body_guard` не поддерживает `Transfer-Encoding: chunked`.** Любой
  такой запрос отклоняется; для chunked нужен отдельный парсер.
- **`least_conn` линейно сканирует пул.** Для типовых пулов это десятки наносекунд;
  на тысячах серверов понадобится heap.
- **Плагин не имеет WASI.** Нет файлов, сети, часов и `rand`; это
  детерминированная функция от переданных байтов и метаданных.
- **Rust/TinyGo нужны только для сборки своих плагинов.** Готовые `.wasm` уже в
  репозитории.

---

## Дорожная карта

- C#/.NET control plane: версии конфигурации, реестр плагинов, публикация,
  откат и аудит через gRPC/UDS.
- Upstream TLS/mTLS и клиентские сертификаты на TLS-листенерах.
- Zero-alloc JSON DPI в ядре и eBPF-фильтрация для раннего отсечения на уровне
  пакетов.
- Контейнерная упаковка (Docker/compose) и расширенные deployment-рецепты.

Уже сделано и доступно сегодня: L4 data plane, zero-copy, rate limiting, TLS/SNI,
health checks, circuit breaker, hot reload, метрики, ABI v3 и восемь плагинов.

---

## Документация

| Документ | Содержание |
|---|---|
| [`DEVELOPMENT.md`](DEVELOPMENT.md) | Рабочий гайд: команды, структура кода, рецепты, конфиг |
| [`plugins/examples/README.md`](plugins/examples/README.md) | ABI v1–v3, ограничения песочницы, сборка плагинов |
| [`benchmarks/README.md`](benchmarks/README.md) | Методика, сценарии, Linux-результаты, сравнение с nginx/HAProxy |
| [`deployments/README.md`](deployments/README.md) | Production-сборка, systemd, рекомендации |
| [`docs/01-problem.md`](docs/01-problem.md) | Постановка задачи, пользователи, метрики |
| [`docs/02-architecture-tech.md`](docs/02-architecture-tech.md) | Архитектура, стек, ADR, модель плагинов |
| [`docs/03-mvp.md`](docs/03-mvp.md) | Границы MVP, Definition of Done, методика замеров |
| [`docs/04-diploma-features.md`](docs/04-diploma-features.md) | Разбор фич и приоритизация |
| [`docs/05-roadmap-thesis.md`](docs/05-roadmap-thesis.md) | План работ и milestones |
| [`docs/06-dev-journal.md`](docs/06-dev-journal.md) | Журнал разработки |
| [`docs/07-glossary.md`](docs/07-glossary.md) | Глоссарий терминов |

---

## Разработка

```bash
gofmt -l .          # форматирование
go vet ./...        # статический анализ
go test ./...       # тесты
go build ./...      # сборка
```

CI гоняет lint (`gofmt`, `go vet`, `golangci-lint`) и тесты на Linux (с `-race`) и
Windows. Бенчмарки запускаются вручную через GitHub Actions, чтобы не мерить шум
общих раннеров на каждом коммите.

Команды для бенчмарков, профилирования и smoke-теста — в
[`DEVELOPMENT.md`](DEVELOPMENT.md).

---

## Лицензия

MIT. См. [`LICENSE`](LICENSE).
