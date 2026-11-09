# WasmGate

Сетевой прокси-шлюз на Go. Ядро реализует L4/TCP-проксирование, а прикладная
логика подключается как WebAssembly-плагин; конфигурация применяется без разрыва
соединений, на периметре работает rate limiting соединений.

Репозиторий содержит рабочее ядро с плагинами, TLS/SNI, health-checks,
circuit breaker и проектную документацию (материалы ВКР).

| Параметр | Значение |
|----------|----------|
| Статус | ядро L4 + WASM L7-lite + SNI/TLS + health-checks + circuit breaker + hot reload + метрики (M0–M5) |
| Data plane | Go 1.25+ |
| Control plane (план) | C#/.NET 8, gRPC поверх Unix Domain Socket |
| Плагины | WebAssembly, чистый Go-рантайм ([wazero](https://github.com/tetratelabs/wazero)), без WASI |
| Лицензия | MIT |

---

## Обзор

WasmGate проектируется как прокси-шлюз с расширяемостью через WebAssembly.
Целевые свойства:

- расширяемость без пересборки ядра: плагин компилируется в `.wasm` и
  исполняется в sandbox;
- применение конфигурации и версий плагинов без разрыва активных соединений
  (Zero-Downtime Hot Reload);
- защита периметра на скользящем окне, работающая в горячем пути без блокировок.

Постановка задачи и обоснование выбора ядра приведены в
[`docs/01-problem.md`](docs/01-problem.md) и
[`docs/04-diploma-features.md`](docs/04-diploma-features.md).

---

## Возможности

### Реализовано (этапы M0–M5)

- L4/TCP reverse proxy на стандартном `net` (горутина на соединение).
- Балансировка: `round_robin`, `least_conn`; мёртвые бэкенды пропускаются.
- Прозрачный `dial_retries`: при ошибке `dial` соединение переподключается к
  другому серверу пула, пока клиентский payload ещё не отправлен.
- Трёхфазный Circuit Breaker (`Closed → Open → Half-Open`) на каждом сервере пула:
  `error_threshold`, `open_duration`, `half_open_requests`; всё на атомиках.
- Health-checks upstream: пассивная отметка при ошибке `dial` и активный TCP-пробинг
  (`health_check.interval` / `timeout`).
- Проксирование через `io.Copy`; на Linux используется `splice(2)` (zero-copy),
  на других платформах буфер копирования берётся из пула.
- Таймауты `dial_timeout` и `idle_timeout` (опционально), лимит `max_conns`.
- Защита от всплесков соединений (`rate_limit`, фича F2) на скользящем окне
  (64 шарда, 10 слотов, 0 аллокаций в steady state) до `dial` к апстриму:
  per-IP и per-subnet (`/24`, `/64`), CIDR allow-list и экспоненциальный бан
  (`1x..16x`).
- WASM-плагины на `wazero` без WASI и без CGO: хуки `on_accept`, `on_data`,
  `on_close`; пул инстансов, `CallWithStack`, лимит памяти и опциональный timeout.
- L7-инспекция `on_data`:
  - режим `once` (по умолчанию) — первый пакет клиента проверяется до `dial`, затем
    `DrainTo` возвращает соединение на `splice(2)`;
  - режим `stream` — каждый чанк `client → server`;
  - плагин может менять байты на месте и возвращать `>= 2`, выбирая пул из
    `plugin.route_pools` (L7-роутинг, canary).
- TLS passthrough через zero-alloc SNI peek и `sni_routes` (exact + `*.domain`),
  при этом исходный `ClientHello` прозрачно переигрывается в апстрим.
- Опциональная TLS-терминация на листенере (`listeners[].tls`): TLS 1.2/1.3,
  горячая перезагрузка сертификата при `SIGHUP` / `POST /reload`, расшифрованный
  трафик виден WASM `on_data`.
- Полу-закрытие (half-close) и корректное завершение при ошибке либо отмене
  контекста.
- Graceful shutdown: сначала перестаём принимать, затем доигрываем активные
  соединения до `-shutdown-grace`; только после этого они закрываются принудительно.
- Перезагрузка конфигурации по `SIGHUP` (Unix) или `POST /reload` (admin) через
  атомарную подмену snapshot; слушатели добавляются/удаляются/переименовываются
  без перезапуска, счётчики лимитов и окна `rate_limit` не сбрасываются.
- Строгая валидация конфигурации и `-validate` (включая компиляцию WASM и загрузку
  TLS-сертификатов).
- Структурное логирование (`log/slog`) с ограничением частоты «шумных» событий.
- Метрики в формате Prometheus (`/metrics`), `/healthz` и `net/http/pprof` на
  отдельном admin-листенере (`admin_addr`, опционально): соединения, отказы,
  байты, retry, discarded WASM-инстансы, `wasmgate_upstream_healthy`.
  Счётчики — атомики, одно обновление на соединение (не на чанк): ~60 нс и
  0 аллокаций на соединение.
- Сборка без CGO; юнит-, интеграционные, chaos- и e2e-тесты; замеры против
  `nginx stream` и `HAProxy`.

### Планируется

- C# control plane: версии конфигурации, реестр плагинов, публикация, откат, аудит.
- Полноценный HTTP-парсинг, JSON DPI (F3) и eBPF-фильтрация (F4).
- Упаковка: Dockerfile, compose, systemd (см. [`deployments/`](deployments/README.md)).
- mTLS к control plane и клиентские сертификаты на TLS-листенерах.

---

## Архитектура

```
клиенты
   |
   v
[ Data Plane (Go) ]  accept -> max_conns/rate_limit -> on_accept -> TLS/SNI
   |                     -> on_data (once|stream) -> balance/dial -> copy (splice)
   |                        ^
   |                        +-- Snapshot (atomic.Pointer) <-- hot reload (SIGHUP / POST /reload)
   v
upstream pool

[ Control Plane (C#/.NET, gRPC/UDS) ]  версии конфигурации, публикация, откат (план)
```

Ядро остаётся L4: по умолчанию payload не разбирается, решения принимаются по
IP/порту и метаданным соединения. L7-lite включается точечно — `sni_routes`,
`on_data` и TLS-терминация; это единственные места, где ядро смотрит в тело
потока (и только там, где это явно настроено). Полноценный HTTP-парсинг и DPI
по-прежнему относятся к future work.

Подробнее: [`docs/02-architecture-tech.md`](docs/02-architecture-tech.md).

---

## Состояние проекта

| Milestone | Содержание | Deliverable | Статус |
|-----------|------------|-------------|--------|
| M0. Skeleton | accept-loop, логирование, каркас | бинарь принимает соединения | готово |
| M1. Proxy core | пулы, балансировка, copy/splice, таймауты | проксирование TCP end-to-end | готово |
| M2. Config + reload + drain | YAML, snapshot, atomic swap, SIGHUP, reconcile слушателей, drain | reload без разрыва соединений | готово |
| M3. Observe + hardening | метрики, `pprof`, health-checks, CLI, baseline-замеры S0–S5 | наблюдаемый прокси + Linux-бенчмарки | готово |
| M4. Connection flood (F2) | скользящее окно по IP/подсети, allow-list, backoff-ban | фильтр в горячем пути, 0 allocs/op | готово |
| M5. WASM + L7-lite | `wazero`, ABI, пул инстансов, `on_data`, `route_pools`, TLS/SNI | плагин работает без рестарта, e2e-тест | готово |
| M6. Control Plane (план) | C#/.NET + gRPC/UDS, версии, rollback, аудит | hot reload из CP | запланировано |

---

## Требования

- Go 1.25 или новее.
- C-компилятор нужен только для `go test -race` (CGO). На Windows удобнее
  запускать race-тесты в WSL; на Linux компилятор обычно уже доступен.
- Windows: PowerShell. Unix: любой shell.

---

## Быстрый старт

```bash
# сборка
go build -o bin/wasmgate.exe ./cmd/wasmgate

# запуск с примером конфигурации
./bin/wasmgate.exe -config configs/wasmgate.yaml

# либо без сборки
go run ./cmd/wasmgate -config configs/wasmgate.yaml
```

Параметры: `-config` (путь к конфигурации), `-log-level` (`debug`/`info`/`warn`/
`error`), `-shutdown-grace` (ожидание активных соединений при останове),
`-validate` (проверить конфиг, WASM-плагины и TLS-сертификаты и выйти).

Останов: `Ctrl+C` (graceful drain). На Linux `SIGHUP` перечитывает конфигурацию
без перезапуска; на всех платформах доступен `POST /reload` на `admin_addr`.

Перед запуском укажите в `configs/wasmgate.yaml` реальные адреса в
`pools.*.servers`. Для локальной проверки предусмотрен `configs/local-echo.yaml`
(порядок smoke-теста описан в [`DEVELOPMENT.md`](DEVELOPMENT.md), §5).

---

## Конфигурация

```yaml
version: 1

# Опционально: observability-эндпоинт (/metrics, /healthz, /reload, /debug/pprof/)
# на отдельном listener'е; пусто или отсутствует = выключен.
admin_addr: "127.0.0.1:9099"

listeners:
  - name: edge
    addr: "127.0.0.1:8080"
    upstream_pool: web
    max_conns: 10000
    dial_timeout: 3s
    idle_timeout: 0s            # 0 = zero-copy splice; >0 = buffered idle watchdog
    rate_limit:
      max: 200                  # per IP per window
      subnet_max: 1000          # per /24 (IPv4) or /64 (IPv6) per window
      window: 1s
      ban_duration: 5s
      allow_cidrs: ["127.0.0.1/32"]
    plugin:                     # optional WASM sandbox
      path: "plugins/examples/ipguard/ipguard.wasm"
      max_memory_pages: 16      # 1 MiB
      timeout: 0s               # 0 = no per-call timer (keeps 0 allocs/op)
      data_mode: "once"         # once | stream
      route_pools: ["canary"]   # on_data >= 2 -> route_pools[ret-2]
    sni_routes:                 # TLS passthrough only (ignored with listeners[].tls)
      "api.example.com": web
      "*.example.com": web
    # tls:                      # optional TLS termination instead of SNI passthrough
    #   cert_file: "certs/server.crt"
    #   key_file: "certs/server.key"
    #   min_version: "1.2"      # 1.2 | 1.3

pools:
  web:
    balance: round_robin        # round_robin | least_conn
    dial_retries: 1             # extra dial attempts on other pool servers
    health_check:
      interval: 2s
      timeout: 1s
    circuit_breaker:
      error_threshold: 5        # consecutive dial failures to open
      open_duration: 10s        # Open -> Half-Open after this
      half_open_requests: 2     # consecutive successes to close
    servers:
      - "127.0.0.1:9001"
      - "127.0.0.1:9002"

  canary:
    balance: least_conn
    servers:
      - "127.0.0.1:9003"
```

Правила:

- `version` должен быть равен `1`;
- `admin_addr` опционален: если задан — корректный `host:port` с числовым портом;
- имена listeners уникальны, `upstream_pool` должен существовать;
- `addr` и `servers` задаются как корректный `host:port` с числовым портом;
- адреса слушателей не дублируются (адрес с портом `0` исключён: это «любой
  свободный порт»);
- пул не может быть пустым и не может содержать дубли серверов;
- `balance` принимает значения `round_robin` или `least_conn`;
- `plugin.data_mode` — `once` (по умолчанию) или `stream`; `plugin.route_pools`
  и `sni_routes` обязаны ссылаться на существующие пулы;
- `tls` требует одновременно `cert_file` + `key_file` (либо inline PEM в коде) и
  `min_version` `1.2`/`1.3`; если `tls` включён, `sni_routes` на этом листенере
  игнорируется — терминация уже снимает TLS, и passthrough-маршрутизация по SNI
  не применяется;
- отрицательные таймауты, `max_conns`, `rate_limit.*`, `dial_retries` и поля
  `circuit_breaker` отклоняются;
- неизвестные ключи конфигурации отклоняются при загрузке.

По умолчанию `idle_timeout: 0` (таймаут простоя отключён), что сохраняет
zero-copy `splice`. При `idle_timeout > 0` включается буферизованное копирование
с общим для соединения watchdog'ом: дедлайн сдвигается при активности **любого**
из направлений, поэтому долгая односторонняя передача (медленное скачивание, SSE)
не рвётся. `splice` при этом отключается для всего соединения.

`plugin.data_mode: once` тоже сохраняет `splice`: после первого пакета
`DrainTo` отдаёт исходный сокет. `stream`, `TLS`-терминация и `idle_timeout > 0`
переключают соответствующее соединение на буферизованный путь — это осознанная
цена за инспекцию.

Полное описание полей: [`DEVELOPMENT.md`](DEVELOPMENT.md), §9.

---

## Структура репозитория

```
cmd/wasmgate/        точка входа и обработка сигналов (unix/windows)
internal/config/     схема YAML, загрузка, валидация
internal/proxy/      ядро data plane: сервер, прокси, пулы, балансировщики, copy, snapshot
internal/plugin/     песочница wazero: пул инстансов, ABI on_accept/on_data/on_close
internal/ratelimit/  скользящее окно ограничения частоты соединений по IP/подсети (F2)
internal/tlspeek/    zero-alloc SNI peek и прозрачный replay ClientHello
internal/observe/    логирование (log/slog), лог-лимитер, метрики и admin-эндпоинт
configs/             примеры конфигурации
docs/                проектная документация
plugins/examples/    примеры WASM-плагинов (Rust, TinyGo) и ABI
benchmarks/          методика бенчмарков, сценарии и Linux-результаты
deployments/         конфигурации развёртывания (в разработке)
```

---

## Документация

| Документ | Содержание |
|----------|------------|
| [`docs/01-problem.md`](docs/01-problem.md) | Постановка задачи, пользователи, гипотезы, метрики успеха |
| [`docs/02-architecture-tech.md`](docs/02-architecture-tech.md) | Архитектура, стек, соглашения, ADR, модель плагинов |
| [`docs/03-mvp.md`](docs/03-mvp.md) | Границы MVP, Definition of Done, методика замеров |
| [`docs/04-diploma-features.md`](docs/04-diploma-features.md) | Разбор фич, приоритизация, выбор ядра |
| [`docs/05-roadmap-thesis.md`](docs/05-roadmap-thesis.md) | План работ, milestones, структура ВКР |
| [`docs/06-dev-journal.md`](docs/06-dev-journal.md) | Журнал разработки: шаги, задачи, проблемы и решения |
| [`docs/07-glossary.md`](docs/07-glossary.md) | Глоссарий терминов |
| [`plugins/examples/README.md`](plugins/examples/README.md) | ABI WASM-плагинов, ограничения песочницы, сборка примеров |
| [`DEVELOPMENT.md`](DEVELOPMENT.md) | Рабочий гайд: команды, структура кода, рецепты |

Рекомендуемый порядок чтения: 01, 02, 03, 04, 05, затем `DEVELOPMENT.md`.

---

## Разработка

```bash
gofmt -l .          # проверка форматирования
go vet ./...        # статический анализ
go test ./...       # тесты
go build ./...      # сборка
```

Команды для бенчмарков, профилирования и smoke-теста, а также соглашения о коде и
коммитах приведены в [`DEVELOPMENT.md`](DEVELOPMENT.md).

---

## Лицензия

MIT. См. [`LICENSE`](LICENSE).
