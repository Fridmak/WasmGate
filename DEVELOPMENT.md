# DEVELOPMENT.md — гайд по репозиторию WasmGate

Практическое руководство: команды, структура кода, как ориентироваться и куда
вставлять новые фичи. Дополняет [`README.md`](README.md) и проектную
документацию в [`docs/`](docs/).

---

## Текущий статус и следующий шаг

**Реализовано (M0–M2):** L4 TCP reverse proxy на `net` — приём соединений,
балансировка `round_robin`/`least_conn`, проксирование (`io.Copy` → `splice` на
Linux; fast path при `idle_timeout: 0`), таймауты (`dial_timeout`,
`idle_timeout` — общий для соединения, а не по направлениям), лимит соединений
`max_conns`, half-close и корректный teardown при ошибке/принудительном закрытии,
graceful shutdown (сначала перестаём принимать, затем drain до grace, и только
потом — принудительное закрытие), reload конфига по SIGHUP через атомарный
snapshot, **reconcile слушателей** (добавление/удаление адресов без перезапуска),
сохранение счётчиков лимитов и балансировщика между reload, ограничение частоты
«шумных» логов, строгая валидация конфига, структурные логи, сборка без CGO.
Метрики (Prometheus-текст, `/healthz`, `pprof`) — на отдельном admin-листенере
`admin_addr` (опционально); счётчики — атомики, ~60 нс и 0 аллокаций на
соединение. Покрыто тестами (`go test ./...`).

**Ещё не реализовано:** защита от connection flood (rate-limit), WASM-плагины,
SNI/TLS-peek, health-checks апстримов, CLI, нагрузочные бенчмарки. Куда
встраивать — §10 «Рецепты».

**Следующий шаг:** пакет `internal/ratelimit` (скользящее окно — первая фича
ядра). Чек-лист и точка вставки — §10.3; в коде это метка `FUTURE` в
`internal/proxy/proxy.go`.

**Milestones:** M0 ✅ · M1 ✅ · M2 ✅ · M3 ⏳ · M4–M6 ⏳ (см. `docs/05`).

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
| Форматирование       | `gofmt -l .` (проверить) / `gofmt -w .` (исправить)                                                      |
| Статический анализ   | `go vet ./...`                                                                                           |
| Линтер               | `golangci-lint run`                                                                                      |
| Зависимости          | `go mod tidy`                                                                                            |
| Кросс-сборка (Linux) | `$env:CGO_ENABLED=0;$env:GOOS="linux";$env:GOARCH="amd64";go build -o bin/wasmgate-linux ./cmd/wasmgate` |

> В репозитории есть `Makefile` (`make build`, `make test`, `make bench`, …), но
> на Windows `make` обычно не установлен — используй команды `go` напрямую.

---

## 1. Требования

- **Go 1.23+** (проверено на 1.27). `go version`.
- **C-компилятор** — только для `-race` на Windows (CGO). Варианты:
  - mingw-w64 / MSYS2, либо запускать race-тесты в WSL/Linux;
  - либо пропускать `-race` локально (в CI гоняем на Linux).
- (опционально) **golangci-lint** для линта:
  `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest`
- (опционально) **benchstat** для сравнения бенчмарков:
  `go install golang.org/x/perf/cmd/benchstat@latest`
- (опционально) **wrk2 / k6 / iperf3** для нагрузочных тестов (см. `benchmarks/`).

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
│   │   ├── listener_config.go  ListenerConfig + Validate + дефолты таймаутов
│   │   ├── pool_config.go      PoolConfig + Validate
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
│   └── observe/
│       ├── log.go           ParseLevel + NewLogger (log/slog)
│       ├── limiter.go       ограничитель частоты логов (анти-«шторм»)
│       ├── metrics.go       счётчики соединений/байтов/reload (атомики) + рендер Prometheus
│       ├── admin.go         admin-эндпоинт: /metrics, /healthz, /debug/pprof
│       ├── limiter_test.go  тесты + бенчи лог-лимитера
│       ├── metrics_test.go  тесты + бенчи метрик (цена на соединение)
│       ├── admin_test.go    тесты эндпоинтов
│       └── log_test.go      тесты разбора уровня логирования
├── configs/
│   ├── wasmgate.yaml        основной пример
│   └── local-echo.yaml      конфиг для smoke-теста (см. §5)
├── benchmarks/              методика измерений Go-бенчей (см. benchmarks/README)
├── plugins/ deployments/    заглушки под будущие milestone'ы
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

Остановка: на Unix — `Ctrl+C`, `SIGTERM` (graceful), `SIGHUP` (reload конфига
без рестарта). На Windows — `Ctrl+C`.

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

- `copy_test.go` — релей: `Throughput`, `ThroughputScaling`, `RelayLatency`;
- `server_test.go` — установление соединений: `ConnectionSetup`;
- `snapshot_test.go` — шов hot reload: `RuntimeLoad`, `Reload`;
- `proxy_test.go`, `state_test.go`, `upstream_test.go` — микро: горячий путь,
  лимитер, балансировщик;
- `internal/config/config_test.go` — `Load` / `Validate` (цена SIGHUP);
- `internal/observe/limiter_test.go` — лог-лимитер.

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

Живой эндпоинт: задай `admin_addr` в конфиге — `/debug/pprof/` появится вместе
с `/metrics` и `/healthz` (см. §9). Пример:

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
  ├─ proxy.NewRuntime(cfg)                 Snapshot v1 + компиляция пулов
  ├─ proxy.NewServer(log, rt)
  ├─ srv.Start()                           bind листенеров + accept-loop на каждый
  └─ ждём сигналов: Ctrl+C/SIGTERM → Shutdown, SIGHUP → srv.Reload

Соединение:
  acceptLoop()          server.go   Accept() → горутина на соединение
    └─ Proxy.Handle()   proxy.go    rt.Load() → резолв listener+pool (1 lookup)
        ├─ max_conns    proxy.go    лимит соединений на listener (limiter из registry)
        ├─ (FUTURE) rate-limit соединений
        ├─ pool.Next()  pool.go     балансировщик выбирает upstream (active++)
        ├─ DialContext  proxy.go    подключение к upstream (с dial_timeout)
        └─ join()       copy.go     copy в обе стороны; splice при idle_timeout=0,
                                    иначе общий idle-watchdog; teardown по ошибке
        └─ pool.Release(up)         active-- (защищено от двойного release)
```

Ключевая идея: **конфиг читается один раз на соединение** из `Snapshot`, и это
чтение — один атомарный `Load()` без блокировок. Поэтому reload посреди
соединения его не ломает.

---

## 7. Разбор файлов и ключевых типов

### `internal/config` — чистая конфигурация

Пакет разбит по областям: `config.go` — корень и общие правила, `listener_config.go`
и `pool_config.go` — по объекту на файл.

- `Config` (`config.go`) — корень (`version`, `listeners`, `pools`); его `Validate()`
  добавляет только кросс-полевые правила: дубли имён/адресов, ссылки на пулы.
- `ListenerConfig` (`listener_config.go`) — фронтальный TCP-листенер и дефолты
  таймаутов.
- `PoolConfig` (`pool_config.go`) — группа бэкендов (`balance`, `servers`).
- `Load(path)` — открыть → распарсить (`KnownFields(true)`) → `Validate()`.
- `Validate()` — применить дефолты и проверить конфиг (вызывается из `Load` и из
  `Server.Reload`; тот же вход для будущего control plane).
- Валидация отвергает: `version != 1`, пустой список листенеров, дубли имён и
  адресов, неизвестный `upstream_pool`, некорректный адрес (`host:port`, порт —
  число), отрицательные таймауты/`max_conns`, пустой пул, дубли серверов и
  неизвестный `balance`. Сети не касается — адреса не резолвятся.

Строки `dial_timeout`/`idle_timeout` парсятся стандартным `time.Duration`
(`"3s"`, `"1500ms"`) — `gopkg.in/yaml.v3` понимает этот тип из коробки, отдельная
обёртка не нужна.

Никакого runtime-состояния здесь нет — только данные.

### `internal/proxy` — data plane

| Файл | Тип / функция | Роль |
|------|---------------|------|
| `snapshot.go` | `Snapshot`, `Runtime` | иммутабельный конфиг + `atomic.Pointer`; **шов hot reload** |
| `server.go` | `Server` | bind/`Reload` листенеров, `acceptLoop`, `Shutdown` (drain) |
| `proxy.go` | `Proxy.Handle` | обработка одного соединения |
| `pool.go` | `pool` | `Next()` (reserve) / `Release()` |
| `upstream.go` | `upstream`, `Balancer` | эндпоинт + счётчик; `round_robin`, `least_conn` |
| `state.go` | `registry`, `connLimiter` | счётчики, переживающие reload (лимит, active) |
| `copy.go` | `join`, `copyConn`, `activityReader` | двунаправленный pump; `io.Copy` → `splice(2)` на fast path, connection-wide idle watchdog с пулом буферов иначе |

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

### `internal/observe`

- `ParseLevel(s) (slog.Level, error)` — разбор текстового уровня (граница:
  флаг CLI/конфиг); внутри программы все ходят с `slog.Level`.
- `NewLogger(level)` — единая точка создания логгера (stdlib `log/slog`).
- `Metrics` — счётчики прокси (accepted/closed/active/rejected/dial-fail/bytes/
  reloads/config_version + per-upstream active). Методы nil-безопасны и стоят
  ~60 нс / 0 аллокаций на соединение; `WritePrometheus` — холодный путь.
- `Admin` — опциональный admin-листенер (`admin_addr`): `/metrics` (Prometheus
  text), `/healthz` (JSON), `/debug/pprof/*`. Биндится синхронно при старте,
  `Serve()` — блокирующий, `Shutdown(ctx)` — graceful.

### `cmd/wasmgate`

`main.go` собирает граф, `signals_unix.go`/`signals_windows.go` дают набор
сигналов под платформу.

---

## 8. Модель конкурентности

- **1 горутина на листенер** (accept-loop).
- **1 горутина на соединение** (`Handle`).
- **+2 горутины** внутри `join` (по одной на направление copy).
- Два `sync.WaitGroup` в `Server`: `acceptWG` (accept-loop'ы) и `connWG`
  (обработчики). `Shutdown` сначала дожидается `acceptWG`, и только потом
  `connWG` — так ни один обработчик не может зарегистрироваться уже после начала
  ожидания (Add, гоняющийся с Wait, — неопределённое поведение).
- **Конфиг** — `atomic.Pointer`, на горячем пути **нет блокировок**; `Reload`
  сериализуется мьютексом (версия — это read-modify-write).
- **Состояние, зависящее от истории** (лимит соединений, счётчики active)
  вынесено в `registry` (`state.go`) и живёт дольше snapshot — reload его не
  сбрасывает.
- **Счётчики upstream** — типизированные атомики (`atomic.Int64`), безопасны на 32/64-бит.
- `sync.Mutex` только там, где не горячо: список листенеров, `Reload`, `registry`.
- **Два контекста на стороне соединений**: `ctx` из `Start`/`Reload` не трогает
  живые соединения; при `Shutdown` они сначала доигрывают, и лишь по истечении
  grace (или при форс-остановке) их контекст отменяется и `join` закрывает обе стороны.
- Паника в обработчике соединения перехватывается (`recover`) и логируется — она
  не роняет весь прокси.

---

## 9. Справка по конфигу

```yaml
version: 1

admin_addr: "127.0.0.1:9099" # опционально: /metrics, /healthz, /debug/pprof
                              # (пусто/отсутствует = выключен)

listeners:
  - name: edge              # уникальное имя (используется в логах/роутинге)
    addr: "127.0.0.1:8080"  # адрес прослушивания
    upstream_pool: web      # имя пула из pools
    max_conns: 10000        # лимит одновременных соединений (0 = без лимита)
    dial_timeout: 3s        # таймаут подключения к бэкенду (по умолчанию 5s)
    idle_timeout: 0s        # idle-таймаут соединения; 0 = выкл (по умолчанию)

pools:
  web:
    balance: round_robin    # round_robin (по умолчанию) | least_conn
    servers:
      - "127.0.0.1:9001"
      - "127.0.0.1:9002"
```

Правила: `version` строго `1`; `admin_addr` опционален (корректный `host:port`);
имена listener'ов уникальны; адреса слушателей не
дублируются (порт `0` исключён); `upstream_pool` обязан существовать;
`addr`/`servers` — корректный `host:port` (порт числовой); пул не пустой и без
дублей серверов; `balance` — `round_robin`/`least_conn`; таймауты и `max_conns`
неотрицательны; неизвестные ключи = ошибка загрузки.

> **`idle_timeout` и zero-copy.** `idle_timeout: 0` (по умолчанию) сохраняет
> zero-copy `splice`. При `idle_timeout > 0` включается буферизованное копирование
> с общим watchdog'ом на всё соединение: дедлайн сдвигается при активности **любого**
> из направлений, поэтому долгая односторонняя передача (медленное скачивание, SSE)
> не рвётся, а «молчащие» полу-открытые соединения сбрасываются. В проде
> `idle_timeout` лучше задать.

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

### 10.3. Rate-limit на скользящем окне (первая фича ядра)
- Создай `internal/ratelimit` (ring-бакеты + шарды, без аллокаций). Окна/счётчики
  положи в `registry` (`state.go`), а не в snapshot, — иначе reload будет их сбрасывать.
- В `proxy.go` на метке `// --- FUTURE: connection-flood ... ---` вызови проверку **до** dial.
- Конфиг: `listeners[].rate_limit` (порог, окно) → поле в `ListenerConfig`.
- Отказы логируй через `observe.NewLimiter(...)`, чтобы не получить «шторм» логов.
- Бенчмарк: `go test -bench -benchmem ./internal/ratelimit` → цель `0 allocs/op`.

### 10.4. WASM-хуки плагинов (киллер-фича)
- Определи интерфейс хуков и хост на `wazero` в `internal/plugin`.
- Оборачивай `join` в `Handle`: `on_accept` → data loop → `on_close`.
- Плагин вызывай на **уровне соединения**, не на каждый чанк (см. `docs/02`, §6).

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
| После reload «пропали» соединения на старом порту | Слушатель переименован или сменил адрес: `Reload` закрывает прежний сокет и открывает новый. Смена адреса без перезапуска поддержана |
| В логе мало строк `connection limit reached` / `dial upstream failed` | Включён anti-storm лимитер (см. `observe/limiter.go`); рядом появляется `suppressed=N` |

---

## 14. Куда смотреть дальше

- Проектная логика и обоснование — [`docs/`](docs/), особенно `docs/03-mvp.md`
  (методика замеров) и `docs/02-architecture-tech.md` (стек, кодстайл, ADR).
- Незнакомый термин — [`docs/07-glossary.md`](docs/07-glossary.md).
- Пакет `internal/proxy` целиком описан в `internal/proxy/doc.go`.
- Точки расширения помечены в коде комментариями `FUTURE` / `TODO`.
