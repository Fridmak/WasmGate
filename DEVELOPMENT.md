# DEVELOPMENT.md — гайд по репозиторию WasmGate

Практическое руководство: команды, структура кода, как ориентироваться и куда
вставлять новые фичи. Дополняет [`README.md`](README.md) и проектную
документацию в [`docs/`](docs/).

---

## Текущий статус и следующий шаг

**Реализовано (M0–M5 + ABI v3):** L4 TCP reverse proxy на `net` — приём соединений,
балансировка `round_robin`/`least_conn` с пропуском нездоровых бэкендов,
прозрачный `dial_retries` и трёхфазный circuit breaker (`Closed/Open/Half-Open`),
проксирование (`io.Copy` → `splice` на Linux; fast path при `idle_timeout: 0`),
таймауты (`dial_timeout`, `idle_timeout` — общий для соединения, а не по
направлениям), лимит соединений `max_conns`, ограничение частоты новых
соединений по IP/подсети на скользящем окне (`rate_limit`, пакет
`internal/ratelimit`, фича F2) с allow-list и backoff-баном, half-close и
корректный teardown при ошибке/принудительном закрытии, graceful shutdown
(сначала перестаём принимать, затем drain до grace, и только потом —
принудительное закрытие), reload конфига по SIGHUP и `POST /reload` через
атомарный snapshot, **reconcile слушателей** (добавление/удаление/переименование
без перезапуска), сохранение счётчиков лимитов, окон `rate_limit` и
балансировщика между reload, ограничение частоты «шумных» логов, строгая
валидация конфига, структурные логи, сборка без CGO.

Поверх ядра: **WASM-плагины** (`internal/plugin`, wazero без WASI) с ABI
v1/v2/v3 (`on_accept`, `on_data`, `on_data_ex`, `on_data_state`, `on_close`,
`on_close_ex`), L7-инспекцией `once`/`stream` в обоих направлениях, in-place
мутацией пакета, выбором пула через `plugin.route_pools` и host-managed
состоянием до 4 KiB на направление; **восемь showcase-плагинов** с готовыми
`.wasm` (`ipguard`, `http_guard`, `http_body_guard`, `path_router`, `sni_guard`,
`protocol_router`, `redis_guard`, `stream_guard`); **TLS passthrough** с
zero-alloc SNI peek и `sni_routes`; **TLS-терминация** на листенере с горячей
перезагрузкой сертификата; **health-checks** upstream (пассивные + активный TCP
probe). Метрики (Prometheus-текст, `/healthz`, `pprof`, `POST /reload`) — на
отдельном admin-листенере `admin_addr` (опционально); счётчики — атомики, ~60 нс
и 0 аллокаций на соединение. Покрыто тестами (`go test ./...`), включая chaos
reload под нагрузкой и e2e-прогоны против nginx/HAProxy на Linux.

**Следующие направления:** C# control plane (M6), upstream TLS/mTLS, zero-alloc
JSON DPI в ядре, eBPF-фильтрация и контейнерная упаковка. Куда встраивать —
§10 «Рецепты».

**Следующий шаг:** отдельная ветка `feat/dotnet-control-plane` — C#/.NET 8
control plane с gRPC/UDS, версиями конфигурации, публикацией и откатом.

**Milestones:** M0 ✅ · M1 ✅ · M2 ✅ · M3 ✅ · M4 ✅ · M5 ✅ · M6 ⏳ (см. `docs/05`).

---

## 0. Шпаргалка команд

| Задача               | Команда                                                                                                  |
| -------------------- | -------------------------------------------------------------------------------------------------------- |
| Собрать              | `go build -o bin/wasmgate.exe ./cmd/wasmgate`                                                            |
| Запустить без сборки | `go run ./cmd/wasmgate -config configs/wasmgate.yaml`                                                    |
| Все тесты            | `go test ./...`                                                                                          |
| Тесты без кэша       | `go test -count=1 ./...`                                                                                 |
| Покрытие             | `go test -cover ./...`                                                                                   |
| Тесты с гонками      | `go test -race ./...` *(нужен CGO + C-компилятор)*                                                       |
| Бенчмарки            | `go test -run '^$' -bench . -benchmem ./...`                                                             |
| Проверить конфиг     | `go run ./cmd/wasmgate -config configs/wasmgate.yaml -validate`                                          |
| Форматирование       | `gofmt -l .` (проверить) / `gofmt -w .` (исправить)                                                      |
| Статический анализ   | `go vet ./...`                                                                                           |
| Линтер               | `golangci-lint run` или `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0 run ./...` |
| Зависимости          | `go mod tidy`                                                                                            |
| Кросс-сборка (Linux) | `$env:CGO_ENABLED=0;$env:GOOS="linux";$env:GOARCH="amd64";go build -o bin/wasmgate-linux ./cmd/wasmgate` |

> В репозитории есть `Makefile` (`make build`, `make test`, `make bench`, …), но
> на Windows `make` обычно не установлен — используй команды `go` напрямую.

---

## 1. Требования

- **Go 1.25+** (проверено на 1.27). `go version`.
- **C-компилятор** — только для `-race` на Windows (CGO). Варианты:
  - mingw-w64 / MSYS2, либо запускать race-тесты в WSL/Linux;
  - либо пропускать `-race` локально (в CI гоняем на Linux).
- (опционально) **golangci-lint** для линта:
  `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest`
- (опционально) **benchstat** для сравнения бенчмарков:
  `go install golang.org/x/perf/cmd/benchstat@latest`
- (опционально) **nginx** (`ngx_stream_module`) и **HAProxy** для сравнительных
  e2e-бенчмарков на Linux (`BenchmarkProxyComparison`); без них эти подтесты
  пропускаются. `wrk2`/`k6` понадобятся только для будущих HTTP-сценариев.

---

## 2. Карта репозитория

```
WasmGate/
├── cmd/wasmgate/
│   ├── main.go              точка входа: флаги, сборка графа, сигналы
│   ├── signals_unix.go      SIGINT/SIGTERM + SIGHUP(reload)  [//go:build !windows]
│   └── signals_windows.go   только Ctrl+C                    [//go:build windows]
├── internal/
│   ├── config/
│   │   ├── config.go           корень Config, Load(), кросс-полевой Validate()
│   │   ├── listener_config.go  ListenerConfig + Validate: таймауты, rate_limit,
│   │   │                      plugin, sni_routes, tls
│   │   ├── pool_config.go      PoolConfig + Validate: balance, dial_retries,
│   │   │                      health_check, circuit_breaker
│   │   ├── config_test.go      тесты Load + бенчи Load/Validate
│   │   ├── listener_config_test.go  тесты listener'а и его дефолтов
│   │   └── pool_config_test.go      тесты пула
│   ├── proxy/               ← СЕРДЦЕ data plane
│   │   ├── doc.go           обзор пакета (архитектура, швы расширения)
│   │   ├── server.go        Server: листенеры, accept-loop, reload, shutdown
│   │   ├── proxy.go         Proxy.Handle: обработка одного соединения
│   │   ├── pool.go          Pool: upstream'ы + балансировщик
│   │   ├── upstream.go      upstream + интерфейс Balancer (RR, least-conn)
│   │   ├── state.go         registry: счётчики, живущие дольше snapshot
│   │   ├── copy.go          join(): двунаправленный copy (splice + idle/ctx)
│   │   ├── snapshot.go      Runtime/Snapshot: atomic-конфиг + индекс листенеров
│   │   ├── server_test.go   тесты + бенч server.go: lifecycle, ConnectionSetup
│   │   ├── proxy_test.go    тесты + бенч proxy.go: проброс, hot path
│   │   ├── copy_test.go     тесты + бенчи copy.go: idle_timeout, throughput
│   │   ├── snapshot_test.go тесты + бенчи snapshot.go: версии, Load/Reload
│   │   ├── state_test.go    тесты + бенч state.go: max_conns / limiter
│   │   ├── upstream_test.go тесты + бенч upstream.go: балансировщики
│   │   ├── pool_test.go     тесты pool.go: резервирование слотов
│   │   └── helpers_test.go  общие фикстуры: echo, сервер, dial, конфиг, sinks
│   ├── plugin/
│   │   ├── sandbox.go       песочница wazero: пул инстансов, CallWithStack,
│   │   │                    ABI v1/v2/v3 (on_accept/on_data/on_data_ex/
│   │   │                    on_data_state/on_close), ConnState, лимит памяти/таймаут
│   │   ├── sandbox_test.go  тесты + бенчи OnAccept / OnData / OnDataState
│   │   └── examples_test.go e2e-тесты showcase-плагинов + BenchmarkExampleHTTPBodyGuard
│   ├── ratelimit/
│   │   ├── window.go        скользящее окно по IP/подсети, IPv6-префиксы,
│   │   │                    allow-list, backoff-бан
│   │   ├── guard.go         лимит одновременных соединений на IP/подсеть
│   │   ├── window_test.go   тесты + бенчи Allow / AllowParallel
│   │   └── guard_test.go    тесты лимита одновременных соединений
│   ├── tlspeek/
│   │   ├── sni.go           zero-alloc разбор ClientHello + прозрачный replay
│   │   └── sni_test.go      тесты + бенч ParseClientHello
│   └── observe/
│       ├── log.go           ParseLevel + NewLogger (log/slog)
│       ├── limiter.go       ограничитель частоты логов (анти-«шторм»)
│       ├── metrics.go       счётчики соединений/байтов/reload (атомики) + рендер Prometheus
│       ├── admin.go         admin-эндпоинт: /metrics, /healthz, /reload, /debug/pprof
│       ├── limiter_test.go  тесты + бенчи лог-лимитера
│       ├── metrics_test.go  тесты + бенчи метрик (цена на соединение)
│       ├── admin_test.go    тесты эндпоинтов
│       └── log_test.go      тесты разбора уровня логирования
├── configs/
│   ├── wasmgate.yaml        основной пример
│   ├── showcase.yaml        все восемь плагинов + native SNI baseline
│   └── local-echo.yaml      конфиг для smoke-теста (см. §5)
├── benchmarks/
│   ├── README.md            методика, сценарии и таблица «что чем меряем»
│   └── linux-ubuntu-results.txt  дословный вывод Linux-прогона (nginx/HAProxy)
├── plugins/examples/        workspace восьми Rust no_std плагинов + common crate;
│                            готовые .wasm закоммичены, Rust для запуска не нужен
├── deployments/             production-сборка и systemd-юнит
├── docs/                    01–07: проектная документация (Obsidian)
├── go.mod / go.sum
├── Makefile / .golangci.yml / .gitignore
└── README.md / DEVELOPMENT.md
```

Правило: **всё внутреннее живёт в `internal/`** (нельзя импортировать извне
модуля). `cmd/` — только «сборка проводки», без бизнес-логики.

---

## 3. Быстрый цикл разработки

Типовой цикл после правки кода:

```powershell
gofmt -w .          # 1. формат
go vet ./...        # 2. статический анализ
go test ./...       # 3. тесты
go build ./...      # 4. компиляция всего
```

Перед коммитом дополнительно:
```powershell
golangci-lint run   # линт (если установлен)
go test -race ./... # гонки (если есть C-компилятор / в WSL)
```

---

## 4. Команды подробно

### 4.1. Сборка и запуск

```powershell
# Сборка одного бинаря в bin/
go build -o bin/wasmgate.exe ./cmd/wasmgate

# Запуск (см. флаги ниже)
.\bin\wasmgate.exe -config configs\wasmgate.yaml -log-level debug

# Либо сразу без сборки
go run ./cmd/wasmgate -config configs\wasmgate.yaml
```

Флаги `cmd/wasmgate`:

| Флаг | По умолчанию | Назначение |
|------|--------------|------------|
| `-config` | `configs/wasmgate.yaml` | путь к конфигу |
| `-log-level` | `info` | `debug`/`info`/`warn`/`error` |
| `-shutdown-grace` | `15s` | сколько ждать активные соединения при останове |
| `-validate` | `false` | проверить конфиг (включая компиляцию WASM и загрузку TLS-сертификатов) и выйти |

Остановка: на Unix — `Ctrl+C`, `SIGTERM` (graceful), `SIGHUP` (reload конфига
без рестарта). На Windows — `Ctrl+C`; reload доступен через `POST /reload` на
`admin_addr`.

### 4.2. Тесты

```powershell
go test ./...                                   # всё
go test -count=1 ./...                           # игнорировать кэш результатов
go test ./internal/proxy -v                      # один пакет, подробно
go test ./internal/proxy -run TestProxyForwardsBytes -v   # один тест
go test -cover ./...                             # покрытие
go test -coverprofile=cover.out ./... ; go tool cover -html=cover.out  # HTML-отчёт
go test -race ./...                              # детектор гонок (CGO!)
```

### 4.3. Бенчмарки

Бенчи — в `*_test.go` (в бинарь не попадают), в том же файле, что и тесты
исходника: у каждого `X.go` ровно один `X_test.go`, сначала тесты, ниже бенчи.
Подпапок `bench/` нет — так же устроена и стандартная библиотека Go
(`sync/mutex_test.go`, `net/http/serve_test.go` содержат и `TestXxx`, и
`BenchmarkXxx`).

Разложены по исходнику, который мерят:

- `copy_test.go` — релей и сравнение: `Throughput`, `ThroughputScaling`,
  `RelayLatency`, `ProxyComparison` (в т.ч. `nginx stream` и HAProxy TCP на Linux);
- `server_test.go` — установление соединений: `ConnectionSetup`;
- `snapshot_test.go` — шов hot reload: `RuntimeLoad`, `Reload`;
- `proxy_test.go`, `state_test.go`, `upstream_test.go` — микро: горячий путь,
  лимитер, балансировщик;
- `internal/config/config_test.go` — `Load` / `Validate` (цена SIGHUP);
- `internal/ratelimit/window_test.go` — `Allow` / `AllowParallel` (скользящее окно по IP);
- `internal/plugin/sandbox_test.go` — `OnAccept`, `OnAcceptParallel`, `OnData`,
  `OnDataState` (цена вызова WASM-хука);
- `internal/plugin/examples_test.go` — `ExampleHTTPBodyGuard` (полный stateful-плагин);
- `internal/tlspeek/sni_test.go` — `ParseClientHelloSNI`, `MatchRouteWildcard`;
- `internal/observe/limiter_test.go`, `metrics_test.go` — лог-лимитер и метрики.

В отдельный пакет (подпапку) бенчи не выносят: подпапка — это отдельный пакет, и
он видит только экспортированные символы. А мерят бенчи как раз неэкспортируемое —
`connLimiter`, `*upstream`, `resolvedListener`, `Snapshot.listeners` — и
`export_test.go` тут не помогает: файлы `_test.go` пакета попадают лишь в *его*
тестовый бинарь. Альтернатива — расширять публичный API ради бенчей, что хуже.

```powershell
# все бенчмарки с аллокациями
go test -run '^$' -bench . -benchmem ./...

# только e2e-пропускная способность прокси (подбенчи = варианты)
go test -run '^$' -bench 'BenchmarkThroughput' -benchmem ./internal/proxy

# масштабирование по числу соединений
go test -run '^$' -bench 'BenchmarkThroughputScaling' -benchmem ./internal/proxy

# микро-бенчи прокси + профили
go test -run '^$' -bench . -benchmem -cpuprofile cpu.out -memprofile mem.out ./internal/proxy

# сравнить «до/после»: N прогонов и benchstat
go test -run '^$' -bench . -benchmem -count=10 ./... > new.txt
benchstat old.txt new.txt
```

Ключевые метрики: `ns/op`, `MB/s` (где задан `b.SetBytes`), `allocs/op`, `B/op`.
Для fast path цель — **`0 allocs/op`** в steady state. Полная таблица «что чем
меряем» — `benchmarks/README.md`.

> **Платформа.** Zero-copy `splice(2)` есть только на Linux: на Windows/macOS оба
> варианта бенча идут через user-space copy, и только Linux-прогон что-то говорит
> о zero-copy (там разница кратная: 2.1–2.7× на 64 KiB). На fast path код не
> различает платформы — Linux просто доходит до `splice(2)` через `io.Copy`; на
> остальных `copyPlain` передаёт стандартному `io.CopyBuffer` буфер из пула
> (одна проверка `runtime.GOOS` в copy.go, без build-тегов). Подробно —
> `benchmarks/README.md`, §«Платформа».

### 4.4. Качество кода

```powershell
gofmt -l .          # список неотформатированных файлов (пусто = ок)
gofmt -w .          # переформатировать
go vet ./...        # встроенный анализатор
golangci-lint run   # конфиг в .golangci.yml
```

> Линтер — golangci-lint **v2** (конфиг в схеме v2). Локально ставится без
> install одной командой и работает с любым Go (включая 1.27):
>
> ```powershell
> go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0 run ./...
> ```

### 4.5. Зависимости

```powershell
go mod tidy         # привести go.mod/go.sum в порядок (после смены импортов)
go list -m all      # список модулей
```

### 4.6. Кросс-сборка (ядро — без CGO)

```powershell
$env:CGO_ENABLED="0"
$env:GOOS="linux";  $env:GOARCH="amd64"; go build -o bin/wasmgate-linux-amd64 ./cmd/wasmgate
$env:GOOS="linux";  $env:GOARCH="arm64"; go build -o bin/wasmgate-linux-arm64 ./cmd/wasmgate
$env:GOOS="windows";$env:GOARCH="amd64"; go build -o bin/wasmgate.exe ./cmd/wasmgate
```

### 4.7. Профилирование (pprof)

Живой эндпоинт: задай `admin_addr` и `admin_pprof: true` в конфиге — `/debug/pprof/`
появится вместе с `/metrics` и `/healthz` (см. §9). Пример:

```powershell
go tool pprof http://127.0.0.1:9099/debug/pprof/profile?seconds=10
```

Без запущенного процесса профилируем через тесты:

```powershell
go test -run '^$' -bench . -cpuprofile cpu.out ./internal/proxy
go tool pprof -http=:8080 cpu.out      # откроет браузер с флеймграфом
```

---

## 5. Ручной smoke-тест (без тестового кода)

Нужны два окна PowerShell: **эхо-сервер** и **клиент**. Прокси запускается между ними.

**Окно 1 — эхо-сервер на 9001:**
```powershell
$listener = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, 9001)
$listener.Start(); Write-Host "echo on 9001"
while ($true) {
    $client = $listener.AcceptTcpClient(); $stream = $client.GetStream()
    $buffer = New-Object byte[] 4096
    while (($n = $stream.Read($buffer, 0, $buffer.Length)) -gt 0) {
        $stream.Write($buffer, 0, $n); $stream.Flush()
    }
    $client.Close()
}
```

**Окно 2 — прокси** (конфиг `configs/local-echo.yaml`: `:8080 → :9001`):
```powershell
go run ./cmd/wasmgate -config configs\local-echo.yaml -log-level debug
```

**Окно 3 — клиент через прокси:**
```powershell
$client = [System.Net.Sockets.TcpClient]::new('127.0.0.1', 8080)
$stream = $client.GetStream()
$bytes  = [Text.Encoding]::ASCII.GetBytes('hello wasmgate')
$stream.Write($bytes, 0, $bytes.Length); $stream.Flush()
$buffer = New-Object byte[] 64; $n = $stream.Read($buffer, 0, $buffer.Length)
[Text.Encoding]::ASCII.GetString($buffer, 0, $n)   # ожидаем: hello wasmgate
$client.Close()
```

Если увидел эхо — проксирование работает end-to-end.

---

## 6. Как устроен код: путь одного соединения

```
main.run()
  ├─ config.Load(path)                     схема + валидация
  ├─ observe.NewMetrics()                  атомарные счётчики
  ├─ proxy.NewRuntime(cfg)                 Snapshot v1 + компиляция пулов/лимитеров
  ├─ proxy.NewServer(log, rt, metrics)
  ├─ srv.Start()                           bind листенеров + accept-loop на каждый
  └─ ждём сигналов: Ctrl+C/SIGTERM → Shutdown, SIGHUP → srv.Reload

Соединение:
  acceptLoop()          server.go   Accept() → горутина на соединение
    └─ Proxy.Handle()   proxy.go    rt.Load() → резолв listener+pool (1 lookup)
        ├─ max_conns    proxy.go    лимит одновременных соединений (limiter из registry)
        ├─ rate_limit   proxy.go    скользящее окно по IP/подсети (ratelimit.Limiter из registry)
        ├─ OnAccept     plugin      WASM-хук по IP/порту; ненулевой verdict = deny
        ├─ TLS          proxy.go    tls.Server (терминация) ИЛИ tlspeek.Peek + sni_routes
        ├─ OnData       proxy.go    once: первый пакет до dial + DrainTo;
        │                           stream: каждый чанк в join(), оба направления
        │                           (ABI v2/v3), legacy on_data — только client→server
        ├─ dialWithRetry proxy.go   Next() + DialContext; retry по пулу, circuit breaker
        └─ join()       copy.go     copy в обе стороны; splice при idle_timeout=0
                                    и отсутствии stream-плагина/TLS; teardown по ошибке
        ├─ OnClose      plugin      rx/tx байты после завершения сессии
        └─ Release/Metrics          active--, счётчики байтов/соединений
```

Ключевая идея: **конфиг читается один раз на соединение** из `Snapshot`, и это
чтение — один атомарный `Load()` без блокировок. Поэтому reload посреди
соединения его не ломает.

---

## 7. Разбор файлов и ключевых типов

### `internal/config` — чистая конфигурация

Пакет разбит по областям: `config.go` — корень и общие правила, `listener_config.go`
и `pool_config.go` — по объекту на файл.

- `Config` (`config.go`) — корень (`version`, `admin_addr`, `listeners`, `pools`);
  его `Validate()` добавляет только кросс-полевые правила: дубли имён/адресов и
  ссылки на пулы (взаимоисключения `tls`/`sni_routes` **нет** — при `tls`
  `sni_routes` просто игнорируется на горячем пути).
- `ListenerConfig` (`listener_config.go`) — фронтальный TCP-листенер, дефолты
  таймаутов, `rate_limit`, `plugin`, `sni_routes`, `tls`.
- `PoolConfig` (`pool_config.go`) — группа бэкендов (`balance`, `servers`,
  `dial_retries`, `health_check`, `circuit_breaker`).
- `Load(path)` — открыть → распарсить (`KnownFields(true)`) → `Validate()`.
- `Validate()` — применить дефолты и проверить конфиг (вызывается из `Load` и из
  `Server.Reload`; тот же вход для будущего control plane).
- Валидация отвергает: `version != 1`, пустой список листенеров, дубли имён и
  адресов, неизвестный `upstream_pool`/`route_pools`/`sni_routes`, некорректный
  адрес (`host:port`, порт — число), отрицательные таймауты/`max_conns`/
  `dial_retries`, пустой пул, дубли серверов, неизвестный `balance`,
  неполный `tls` (нужны `cert_file` + `key_file`, `min_version` 1.2/1.3),
  неверный `plugin.data_mode`. Сети не касается — адреса не резолвятся.

Строки `dial_timeout`/`idle_timeout` парсятся стандартным `time.Duration`
(`"3s"`, `"1500ms"`) — `gopkg.in/yaml.v3` понимает этот тип из коробки, отдельная
обёртка не нужна.

Никакого runtime-состояния здесь нет — только данные.

### `internal/proxy` — data plane

| Файл | Тип / функция | Роль |
|------|---------------|------|
| `snapshot.go` | `Snapshot`, `Runtime` | иммутабельный конфиг + `atomic.Pointer`; **шов hot reload** |
| `server.go` | `Server` | bind/`Reload` листенеров, `acceptLoop`, `Shutdown` (drain) |
| `proxy.go` | `Proxy.Handle` | обработка одного соединения: лимиты, хуки, TLS/SNI, dial, relay |
| `pool.go` | `pool` | `Next()` (reserve) / `Release()`, `dial_retries` |
| `upstream.go` | `upstream`, `Balancer` | эндпоинт + счётчик + circuit breaker; `round_robin`, `least_conn` |
| `state.go` | `registry`, `connLimiter` | счётчики, переживающие reload (лимит, active, rate-limit) |
| `copy.go` | `join`, `copyConn`, `activityReader` | двунаправленный pump; `io.Copy` → `splice(2)` на fast path, connection-wide idle watchdog с пулом буферов иначе; stream-плагин на каждом чанке |

`Runtime`:
```go
func (r *Runtime) Load() *Snapshot                 // lock-free чтение
func (r *Runtime) Reload(cfg *config.Config) uint64 // публикация новой версии
```

`Server`:
```go
func (s *Server) Start() error                       // bind листенеров + accept-loop
func (s *Server) Reload(ctx, cfg) (uint64, error)    // снимок + reconcile слушателей
func (s *Server) Shutdown(ctx) error                 // drain, при истечении grace — форс
func (s *Server) Addrs() []net.Addr
```

### `internal/ratelimit`

- `Limiter` (`window.go`) — скользящее окно из 10 слотов, 64 шарда по 16-байтному
  каноническому ключу IP (IPv4 и IPv4-mapped IPv6 отображаются в один ключ).
  Дополнительно: агрегация по `/24` (IPv4), для IPv6 — host-ключ `/64` и
  subnet-ключ `/56` (настраиваются), CIDR allow-list, экспоненциальный бан
  `1x..16x` базового `ban_duration`. Хранится в `registry`
  (`state.go`), поэтому reload не обнуляет счётчики.

### `internal/plugin` — WASM-песочница

- `Plugin` (`sandbox.go`) — wazero без WASI: компиляция один раз, пул
  реакторных инстансов (`sync.Pool`), вызовы через `CallWithStack` с
  числовыми аргументами, без аллокаций на горячем пути.
- Хуки: `OnAccept(ip, port)`, `OnData`/`OnDataConn` (ABI v1/v2), `OnDataState`
  (ABI v3), `OnClose`/`OnCloseEx`. `OnAccept`: любой ненулевой verdict = deny.
  `OnData`: `0` — allow/default pool, `1` — deny, `>= 2` — выбрать
  `route_pools[ret-2]` (только в режиме `once`).
- Для L7 байты кладутся в scratch-регион линейной памяти, адрес которого гость
  сообщает через обязательный `on_data_ptr()` (до 32 KiB); плагин может менять
  их на месте. ABI v3 дополнительно копирует до 4 KiB состояния на направление
  через `on_state_ptr()`. `max_memory_pages` (по умолчанию 16 страниц = 1 MiB) и
  опциональный `timeout` ограничивают ресурсы; при trap/timeout инстанс
  выбрасывается.

### `internal/tlspeek` — SNI без терминации

- `Peek(conn, timeout)` читает первый TLS-рекорд, zero-alloc парсит
  `ClientHello` и возвращает SNI; `Conn` прозрачно переигрывает прочитанные
  байты в апстрим.
- `PeekChunk(conn, timeout)` — тот же приём для первого пакета `on_data`.
- `DrainTo`/`Underlying` возвращают исходный `*net.TCPConn`, чтобы на Linux
  дальнейший relay снова шёл через `splice(2)`.
- `MatchRoute` поддерживает exact-host и `*.domain`.

### `internal/observe`

- `ParseLevel(s) (slog.Level, error)` — разбор текстового уровня (граница:
  флаг CLI/конфиг); внутри программы все ходят с `slog.Level`.
- `NewLogger(level)` — единая точка создания логгера (stdlib `log/slog`).
- `Metrics` — счётчики прокси (accepted/closed/active/rejected/dial-fail/bytes/
  reloads/config_version + per-upstream active/healthy). Методы nil-безопасны и
  стоят ~60 нс / 0 аллокаций на соединение; `WritePrometheus` — холодный путь.
- `Admin` — опциональный admin-листенер (`admin_addr`): `/metrics` (Prometheus
  text), `/healthz` (JSON), `POST /reload`, `/debug/pprof/*` (по флагу
  `admin_pprof`). Биндится синхронно при старте,
  `Serve()` — блокирующий, `Shutdown(ctx)` — graceful.

### `cmd/wasmgate`

`main.go` собирает граф, `signals_unix.go`/`signals_windows.go` дают набор
сигналов под платформу.

---

## 8. Модель конкурентности

- **1 горутина на листенер** (accept-loop).
- **1 горутина на соединение** (`Handle`).
- **+2 горутины** внутри `join` (по одной на направление copy).
- **+1 горутина на пул** с включённым `health_check` (активный TCP-probe);
  loops перезапускаются при reload и останавливаются при `Shutdown`.
- Два `sync.WaitGroup` в `Server`: `acceptWG` (accept-loop'ы) и `connWG`
  (обработчики). `Shutdown` сначала дожидается `acceptWG`, и только потом
  `connWG` — так ни один обработчик не может зарегистрироваться уже после начала
  ожидания (Add, гоняющийся с Wait, — неопределённое поведение).
- **Конфиг** — `atomic.Pointer`, на горячем пути **нет блокировок**; `Reload`
  сериализуется мьютексом (версия — это read-modify-write). Rate limiter держит
  таблицу в 64 шардах с короткой блокировкой на шард, поэтому разные IP обычно
  не конкурируют.
- **Состояние, зависящее от истории** (лимит соединений, счётчики active)
  вынесено в `registry` (`state.go`) и живёт дольше snapshot — reload его не
  сбрасывает.
- **Счётчики upstream** — типизированные атомики (`atomic.Int64`), безопасны на 32/64-бит.
- `sync.Mutex` только там, где он не создаёт общей точки: список листенеров,
  `Reload`, `registry` и отдельный шард rate limiter.
- **Два контекста на стороне соединений**: `ctx` из `Start`/`Reload` не трогает
  живые соединения; при `Shutdown` они сначала доигрывают, и лишь по истечении
  grace (или при форс-остановке) их контекст отменяется и `join` закрывает обе стороны.
- Паника в обработчике соединения перехватывается (`recover`) и логируется — она
  не роняет весь прокси.

---

## 9. Справка по конфигу

```yaml
version: 1

admin_addr: "127.0.0.1:9099" # опционально: /metrics, /healthz, /reload, /debug/pprof
                              # (пусто/отсутствует = выключен)

listeners:
  - name: edge              # уникальное имя (используется в логах/роутинге)
    addr: "127.0.0.1:8080"  # адрес прослушивания
    upstream_pool: web      # имя пула из pools
    max_conns: 10000        # лимит одновременных соединений (0 = без лимита)
    dial_timeout: 3s        # таймаут подключения к бэкенду (по умолчанию 5s)
    idle_timeout: 0s        # idle-таймаут соединения; 0 = выкл (по умолчанию)
    rate_limit:             # опционально: лимит новых соединений по IP и подсети (F2)
      max: 200              # соединений с одного IP за окно (0 = выкл)
      subnet_max: 1000      # соединений с подсети за окно (0 = выкл)
      max_concurrent: 0     # одновременных соединений с IP (0 = выкл)
      subnet_max_concurrent: 0  # одновременных с подсети (0 = выкл)
      ipv6_host_prefix: 64  # группировка IPv6-хоста (по умолчанию 64)
      ipv6_subnet_prefix: 56 # группировка IPv6-подсети (по умолчанию 56)
      window: 1s            # длина скользящего окна (по умолчанию 1s, мин. 10ms)
      ban_duration: 5s      # базовый TTL бана при превышении, растёт экспоненциально до 16x (0 = выкл)
      allow_cidrs:          # белые подсети, пропускаемые без подсчёта
        - "127.0.0.1/32"
    plugin:                 # опционально: WASM-плагин на этот листенер
      path: "plugins/examples/ipguard/ipguard.wasm"
      max_memory_pages: 16  # потолок линейной памяти на инстанс (0 = 16 страниц = 1 MiB)
      timeout: 0s           # таймаут на вызов хука (0 = без таймера, 0 allocs/op)
      data_mode: "once"     # ""/"once" (первый пакет) | "stream" (каждый чанк)
      route_pools: ["canary"]  # on_data >= 2 → route_pools[ret-2]
    sni_routes:             # TLS passthrough; игнорируется, если задан tls
      "api.example.com": web
      "*.example.com": web
    # tls:                  # опционально: TLS-терминация вместо passthrough
    #   cert_file: "certs/server.crt"
    #   key_file: "certs/server.key"
    #   min_version: "1.2"  # "1.2" | "1.3"
    #   handshake_timeout: 10s # таймаут рукопожатия (по умолчанию 10s)

pools:
  web:
    balance: round_robin    # round_robin (по умолчанию) | least_conn
    dial_retries: 1         # дополнительные попытки dial по серверам пула (0 = выкл)
    health_check:
      interval: 2s          # период активного TCP-probe (0 = выкл)
      timeout: 1s           # таймаут probe (по умолчанию 1s)
    circuit_breaker:
      error_threshold: 5    # подряд ошибок dial до Open (0 = Open после первой)
      open_duration: 10s    # Open → Half-Open (по умолчанию 10s при threshold > 0)
      half_open_requests: 2 # подряд успехов для Closed (по умолчанию 1)
    servers:
      - "127.0.0.1:9001"
      - "127.0.0.1:9002"

  canary:
    balance: least_conn
    servers:
      - "127.0.0.1:9003"
```

Правила: `version` строго `1`; `admin_addr` опционален (корректный `host:port`);
имена listener'ов уникальны; адреса слушателей не
дублируются (порт `0` исключён); `upstream_pool`, `plugin.route_pools` и
`sni_routes` ссылаются на существующие пулы; `addr`/`servers` — корректный
`host:port` (порт числовой); пул не пустой и без дублей серверов; `balance` —
`round_robin`/`least_conn`; таймауты, `max_conns`, `dial_retries` и поля
`circuit_breaker` неотрицательны; `plugin.data_mode` — `once`/`stream`;
неизвестные ключи = ошибка загрузки.

> **`idle_timeout` и zero-copy.** `idle_timeout: 0` (по умолчанию) сохраняет
> zero-copy `splice`. При `idle_timeout > 0` включается буферизованное копирование
> с общим watchdog'ом на всё соединение: дедлайн сдвигается при активности **любого**
> из направлений, поэтому долгая односторонняя передача (медленное скачивание, SSE)
> не рвётся, а «молчащие» полу-открытые соединения сбрасываются. Это выбор
> между защитой от полуоткрытых соединений и zero-copy: `idle_timeout > 0`
> отключает `splice`, поэтому включайте его осознанно.

> **Что отключает `splice`.** `idle_timeout > 0`, `plugin.data_mode: stream` и
> TLS-терминация (`listeners[].tls`) переводят соединение на буферизованный путь.
> `on_data` в режиме `once` и SNI peek сохраняют zero-copy: после первого пакета
> `DrainTo` возвращает исходный сокет.
>
> **`tls` vs `sni_routes`.** При включённой терминации `sni_routes` не
> применяется: TLS уже снят, и выбор пула по SNI в passthrough-режиме не нужен.
> Оба механизма можно держать в конфиге, но маршрутизировать по SNI будет только
> листенер без `tls`.
>
> **Circuit breaker.** `error_threshold: 0` означает «открыться после первой
> ошибки dial». При `threshold > 0` дефолты — `open_duration: 10s` и
> `half_open_requests: 1`. Явный `open_duration: 0` при `threshold > 0`
> валидация тоже заменяет на 10s: иначе локальная ошибка (`EMFILE`) выключила бы
> бэкенд навсегда.
>
> **`dial_retries`.** Это число *дополнительных* попыток (всего `1 + N`).
> Балансировщик может выбрать тот же сервер; гарантии «другой сервер» нет.

---

## 10. Рецепты: «как добавить X»

### 10.1. Новое поле конфига
1. Добавь поле с yaml-тегом в нужный объект: `listener_config.go` для
   `listeners[]`, `pool_config.go` для `pools`.
2. Задай дефолт/проверку в методе `Validate` своего типа: `ListenerConfig`
   для `listeners[]`, `PoolConfig` для `pools`. `Config.Validate` остаётся
   единственным входом и добавляет только кросс-полевые правила (уникальность
   имён/адресов, ссылки на пулы).
3. Используй в `proxy` (значение едет внутри `Snapshot`).
4. Обнови `configs/wasmgate.yaml` и `docs/`.

### 10.2. Новая стратегия балансировки
1. В `internal/proxy/upstream.go` реализуй `Balancer` (`Next`, `Name`).
2. Добавь ветку в `newBalancer`.
3. Разреши значение в валидации (`pool_config.go`, switch `Balance`).
4. Добавь тест в `proxy_test.go`.

### 10.3. Расширение rate-limit (`internal/ratelimit`)
- Лимитер живёт в `internal/ratelimit/window.go` и привязан к `registry`
  (`state.go`), вызов в `proxy.go` стоит до `DialContext`.
- Уже есть per-IP и per-subnet окна, CIDR allow-list и backoff-бан. Новые
  сигналы добавляй через `config.RateLimit` в `listener_config.go` и
  `ratelimit.Limiter`, сохраняя `0 allocs/op` в `BenchmarkAllow` /
  `BenchmarkAllowParallel`.

### 10.4. WASM-хуки и L7-логика
- Песочница и ABI — `internal/plugin/sandbox.go`; примеры —
  `plugins/examples/ipguard/` (Rust `no_std`, TinyGo) и
  `plugins/examples/README.md`.
- `on_accept` вызывается на уровне соединения; `on_data`/`on_data_ex`/
  `on_data_state` — либо один раз до `dial` (`once`), либо на каждый чанк в
  `join` (`stream`); `on_close`/`on_close_ex` — после завершения relay.
- Состояние между чанками — только через ABI v3 (`ConnState`); глобальная память
  инстанса не привязана к соединению, потому что инстансы берутся из пула.
- Не добавляй сетевой I/O или блокирующие вызовы в хуки: инстанс занят на время
  CPU-вызова, а `timeout`/trap приводят к его выбрасыванию.
- Для новой возможности сначала оцени, можно ли её сделать плагином: ядро
  остаётся L4, L7-инспекция включается только явными полями конфига.

### 10.5. TLS-терминация и SNI
- `tls.Config` собирается в `snapshot.go` (`buildTLSConfig`); сертификат
  перечитывается при reload.
- Passthrough — `internal/tlspeek`: `Peek`/`PeekChunk` + `DrainTo`.
- Помни про цену: терминация и stream-плагин отключают `splice`.

---

## 11. Как писать тесты

- Юнит-тесты — **table-driven**, там же, где код (`*_test.go`).
- **Имя тестового файла = имя исходника, который он покрывает**: `server.go` →
  `server_test.go`, `upstream.go` → `upstream_test.go`. Общие фикстуры, нужные
  больше чем одному файлу, — в `helpers_test.go`. Файл растёт вширь не строками,
  а новыми файлами: не сваливай тесты всех исходников в один `proxy_test.go`.
- Внешние тесты (через публичный API) — пакет `foo_test`.
- Интеграция: `startEcho(t)` поднимает TCP-эхо, дальше проверяем проксирование
  (см. `TestProxyForwardsBytes`).
- Для конфигов — `t.TempDir()` + временный файл (см. `config_test.go`).
- Всегда закрывай ресурсы через `t.Cleanup`.
- Бенчмарки — `BenchmarkXxx(b *testing.B)` в `*_test.go` (в бинарь не попадают),
  в том же `X_test.go`, что и тесты исходника: сначала тесты, ниже бенчи,
  отдельных `*_bench_test.go` и подпапок `bench/` не заводим. Полный список —
  `benchmarks/README.md`.
- Если бенчмарк меряет то, что читают все горутины (атомики, мьютекс, общий пул),
  нужен парный `BenchmarkXxxParallel` через `b.RunParallel`: одиночный прогон не
  создаёт конкуренции и цена общей кэш-линии в нём не видна.
- Полезно: `go test -run ... -count=1 -v`.

---

## 12. Соглашения

- **Коммиты:** Conventional Commits — `feat(proxy): …`, `fix(config): …`,
  `test(ratelimit): …`, `docs: …`, `chore: …`.
- **Ветки:** `feat/<краткое>`, `fix/<краткое>`.
- **Стиль:** Effective Go + Uber Go Style; ошибки оборачиваем через `%w`;
  `panic` в hot path запрещён; в hot path — ноль аллокаций.
- **Перед пушем:** `gofmt -w .`, `go vet ./...`, `go test ./...`, `golangci-lint run`.

---

## 13. Частые грабли

| Симптом | Причина / решение |
|---------|-------------------|
| `-race requires cgo` | Нет CGO/компилятора. `$env:CGO_ENABLED="1"` + gcc, либо гоняй `-race` в WSL/Linux |
| `read config: open ...: no such file` | Неверный `-config` (путь от рабочей директории) |
| `unknown field` при загрузке конфига | Опечатка в ключе YAML (`KnownFields(true)` специально падает) |
| `unknown upstream_pool` | Листенер ссылается на несуществующий пул |
| `invalid duration "60"` | Нужна единица: `60s`, `1500ms` |
| Импорты не находятся после смены пути модуля | `go mod tidy` |
| `SIGHUP` ничего не делает на Windows | Ожидаемо — reload только на Unix |
| Тест/соединение «висит» | Полу-открытое соединение без трафика. Задай `idle_timeout > 0` (буферизованный путь с общим watchdog'ом) |
| После reload «пропали» соединения на старом порту | Слушатель переименован или сменил адрес: `Reload` закрывает прежний сокет и открывает новый. Смена адреса без перезапуска поддержана. Смена `admin_addr` на лету не применяется (B4) — нужен рестарт |
| `sni_routes` не маршрутизирует | На этом листенере включён `tls` (терминация): SNI-routing работает только в passthrough-режиме без `tls` |
| Плагин «не видит» L7-трафик | Не экспортирован `on_data`/`on_data_ex`/`on_data_state` или выбран `stream`; в `once` инспектируется только первый пакет (до 16 389 байт) |
| `on_data` вернул `>= 2`, но пул не сменился | `route_pools` учитывается только в `data_mode: once`; в `stream` любой ненулевой вердикт закрывает соединение |
| После включения TLS/stream-плагина просел throughput | Ожидаемо: терминация и per-chunk инспекция отключают `splice(2)` и идут через буферизованный copy |
| После reload старый плагин продолжает жить | Runtime старого плагина закрывается через `runtime.AddCleanup` при сборке мусора, а не мгновенно при подмене; в долгоживущем процессе освобождение памяти может задерживаться (`docs/06`, §6) |
| В логе мало строк `connection limit reached` / `dial upstream failed` | Включён anti-storm лимитер (см. `observe/limiter.go`); рядом появляется `suppressed=N` |

---

## 14. Куда смотреть дальше

- Проектная логика и обоснование — [`docs/`](docs/), особенно `docs/03-mvp.md`
  (методика замеров) и `docs/02-architecture-tech.md` (стек, кодстайл, ADR).
- Незнакомый термин — [`docs/07-glossary.md`](docs/07-glossary.md).
- Пакет `internal/proxy` целиком описан в `internal/proxy/doc.go`.
- Точки расширения перечислены в §10 «Рецепты» и в `doc.go`.
