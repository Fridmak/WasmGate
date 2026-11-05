# WasmGate

Сетевой прокси-шлюз на Go. Ядро реализует L4/TCP-проксирование; в развитии
предусмотрены плагины на WebAssembly, применение конфигурации без разрыва
соединений и встроенная защита от всплесков соединений (rate limiting).

Репозиторий содержит рабочий прототип ядра и проектную документацию (материалы
ВКР).

| Параметр | Значение |
|----------|----------|
| Статус | ядро L4 + hot reload + метрики + rate limiting (этапы M0–M4) |
| Data plane | Go 1.23+ |
| Control plane (план) | C#/.NET 8, gRPC поверх Unix Domain Socket |
| Плагины | WebAssembly ([wazero](https://github.com/tetratelabs/wazero)) |
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

### Реализовано (этапы M0–M4)

- L4/TCP reverse proxy на стандартном `net` (горутина на соединение).
- Балансировка: `round_robin`, `least_conn`.
- Проксирование через `io.Copy`; на Linux используется `splice(2)` (zero-copy),
  на других платформах буфер копирования берётся из пула.
- Таймауты `dial_timeout` и `idle_timeout` (опционально), лимит `max_conns`.
- Защита от всплесков соединений (`rate_limit`, фича F2) на скользящем окне
  (64 шарда, 10 слотов, 0 аллокаций в steady state) до `dial` к апстриму.
- Полу-закрытие (half-close) и корректное завершение при ошибке либо отмене
  контекста.
- Graceful shutdown: сначала перестаём принимать, затем доигрываем активные
  соединения до `-shutdown-grace`; только после этого они закрываются принудительно.
- Перезагрузка конфигурации по `SIGHUP` (Unix) через атомарную подмену snapshot;
  слушатели добавляются/удаляются/переименовываются без перезапуска, счётчики
  лимитов и окна `rate_limit` не сбрасываются.
- Строгая валидация конфигурации.
- Структурное логирование (`log/slog`) с ограничением частоты «шумных» событий.
- Метрики в формате Prometheus (`/metrics`), `/healthz` и `net/http/pprof` на
  отдельном admin-листенере (`admin_addr`, опционально). Счётчики — атомики,
  одно обновление на соединение (не на чанк): ~60 нс и 0 аллокаций на соединение.
- Сборка без CGO; юнит- и интеграционные тесты.

### Планируется

- Хуки плагинов WASM: `on_accept`, `on_data`, `on_close`.
- TLS passthrough через SNI peek и опциональная терминация.
- Health-checks upstream.
- CLI.
- C# control plane: версии конфигурации, публикация, откат.
- DPI и eBPF (future work).

---

## Архитектура

```
клиенты
   |
   v
[ Data Plane (Go) ]  accept -> балансировка -> dial upstream -> copy (splice)
   |                        ^
   |                        +-- Snapshot (atomic.Pointer) <-- hot reload (SIGHUP)
   v
upstream pool

[ Control Plane (C#/.NET, gRPC/UDS) ]  версии конфигурации, публикация, откат (план)
```

Ядро работает на уровне L4: решения принимаются по IP/порту и метаданным
соединения, прикладной протокол не разбирается. Разбор содержимого потока
(SNI peek, хуки `on_data`, DPI) относится к последующим этапам. При переходе к
инспекции тела направление копирования переключается с zero-copy `splice` на
буферизованный путь.

Подробнее: [`docs/02-architecture-tech.md`](docs/02-architecture-tech.md).

---

## Состояние проекта

| Milestone | Содержание | Deliverable | Статус |
|-----------|------------|-------------|--------|
| M0. Skeleton | accept-loop, логирование, каркас | бинарь принимает соединения | готово |
| M1. Proxy core | пулы, балансировка, copy/splice, таймауты | проксирование TCP end-to-end | готово |
| M2. Config + reload + drain | YAML, snapshot, atomic swap, SIGHUP, reconcile слушателей, drain | reload без разрыва соединений | готово |
| M3. Observe + hardening | метрики, `pprof`, health-checks, CLI | наблюдаемый прокси | метрики/pprof готовы, baseline-замеры S0–S5 впереди |
| M4. Connection flood (F2) | скользящее окно по IP в fast path | фильтр в горячем пути, 0 allocs/op | готово |

---

## Требования

- Go 1.23 или новее.
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
`error`), `-shutdown-grace` (ожидание активных соединений при останове).

Останов: `Ctrl+C` (graceful drain). На Linux `SIGHUP` перечитывает конфигурацию
без перезапуска.

Перед запуском укажите в `configs/wasmgate.yaml` реальные адреса в
`pools.*.servers`. Для локальной проверки предусмотрен `configs/local-echo.yaml`
(порядок smoke-теста описан в [`DEVELOPMENT.md`](DEVELOPMENT.md), §5).

---

## Конфигурация

```yaml
version: 1

# Опционально: observability-эндпоинт (/metrics, /healthz, /debug/pprof/)
# на отдельном listener'е; пусто или отсутствует = выключен.
admin_addr: "127.0.0.1:9099"

listeners:
  - name: edge
    addr: "127.0.0.1:8080"
    upstream_pool: web
    max_conns: 10000
    dial_timeout: 3s
    idle_timeout: 0s
    rate_limit:
      max: 200
      window: 1s

pools:
  web:
    balance: round_robin
    servers:
      - "127.0.0.1:9001"
      - "127.0.0.1:9002"
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
- неизвестные ключи конфигурации отклоняются при загрузке.

По умолчанию `idle_timeout: 0` (таймаут простоя отключён), что сохраняет
zero-copy `splice`. При `idle_timeout > 0` включается буферизованное копирование
с общим для соединения watchdog'ом: дедлайн сдвигается при активности **любого**
из направлений, поэтому долгая односторонняя передача (медленное скачивание, SSE)
не рвётся. `splice` при этом отключается для всего соединения.

Полное описание полей: [`DEVELOPMENT.md`](DEVELOPMENT.md), §9.

---

## Структура репозитория

```
cmd/wasmgate/        точка входа и обработка сигналов (unix/windows)
internal/config/     схема YAML, загрузка, валидация
internal/proxy/      ядро data plane: сервер, прокси, пулы, балансировщики, copy, snapshot
internal/ratelimit/  скользящее окно ограничения частоты соединений по IP (F2)
internal/observe/    логирование (log/slog), лог-лимитер, метрики и admin-эндпоинт
configs/             примеры конфигурации
docs/                проектная документация
plugins/             примеры плагинов (в разработке)
benchmarks/          методика бенчмарков и сценарии нагрузки
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
