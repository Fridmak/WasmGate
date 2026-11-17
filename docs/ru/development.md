# Разработка

## Требования

- Go 1.26+ (в `go.mod` закреплён toolchain 1.27).
- `make` опционален: каждая цель — одна команда `go`.
- .NET 10 SDK для control plane; `clang` + `zlib` для его Native AOT publish.
- `protoc`, `protoc-gen-go`, `protoc-gen-go-grpc` — только при изменении
  gRPC-контракта.

## Структура репозитория

```
cmd/wasmgate/          флаги и запуск процесса
internal/config/       схема YAML, дефолты, валидация, reload
internal/proxy/        листенеры, пулы, релей, snapshot
internal/plugin/       песочница WASM и ABI v3
internal/plugin/abi4/  ядро ABI v4 (envelope, plan, engine)
internal/tlspeek/      парсер ClientHello/SNI
internal/ratelimit/    скользящее окно и guard конкурентности
internal/observe/      логи, метрики, admin endpoint
internal/controlplane/ клиент control plane
plugins/examples/      Rust workspace с эталонными плагинами
control-plane/         C# solution и xunit-тесты
configs/               пример и showcase-конфигурация
deployments/           сценарии развёртывания, Dockerfile'ы, systemd, dev-сертификаты
benchmarks/            сырой вывод бенчмарков
docs/en, docs/ru       документация на двух языках
```

## Сборка и запуск

```bash
make build                 # bin/wasmgate
go build ./...             # собрать всё
go run ./cmd/wasmgate -config configs/wasmgate.yaml -validate
go run ./cmd/wasmgate -config configs/wasmgate.yaml
```

## Тесты

```bash
go test ./...              # unit + e2e через loopback
make race                  # go test -race ./...
go test ./internal/proxy   # один пакет
go test -run TestReload ./internal/proxy
```

Тесты лежат рядом с исходником (`X.go` → `X_test.go`): сначала тесты, затем
бенчмарки этого файла. E2E-тесты поднимают реальный сервер на loopback и, если
установлены `nginx`/`haproxy`, сравниваются с ними (иначе пропускаются).

## Форматирование и линт

```bash
gofmt -l .                 # должно быть пусто
go vet ./...
golangci-lint run          # v2.15.0, конфиг .golangci.yml
```

CI запускает те же команды на Ubuntu и Windows плюс тесты с `-race` на Linux.

## Бенчмарки

```bash
make bench
go test -run '^$' -bench BenchmarkThroughput -benchmem ./internal/proxy
benchstat old.txt new.txt
```

Методика, интерпретация и результаты — в [бенчмарках](benchmarks.md); сырой
вывод лежит в `benchmarks/`.

## Профилирование

Включите `admin_pprof: true` и держите admin endpoint на loopback, затем
`go tool pprof http://127.0.0.1:9099/debug/pprof/profile` или `/heap`. CPU-профиль
релея показывает `splice`/`copy` на горячем пути; мьютекс или семафор там — баг,
а не повод для тюнинга.

## Control plane

```bash
cd control-plane
dotnet build WasmGate.Control.sln -c Release
dotnet test  WasmGate.Control.sln -c Release
dotnet format WasmGate.Control.sln --verify-no-changes
```

API-проект рассчитан на Native AOT; для локальной работы достаточно
framework-dependent publish:

```bash
dotnet publish src/WasmGate.Control.Api -c Release -p:PublishAot=false -o out
```

Для Native AOT нужен C-тулчейн (MSVC на Windows, `clang` + `zlib` на Linux).

Общий gRPC-контракт — `control-plane/src/WasmGate.Control.Api/Protos/agent_control.proto`.
После правки:

```bash
make proto    # перегенерировать internal/controlplane/wasmgatepb
```

C#-биндинги генерирует сборка; Go-биндинги коммитятся.

## Docker

```bash
docker build -f deployments/docker/data-plane.Dockerfile -t wasmgate-dp .
docker build -f deployments/docker/control-plane.Dockerfile -t wasmgate-cp .
docker compose -f deployments/scenarios/03-control-plane/compose.yml up --build
```

## Правило документации

Каждый отслеживаемый документ существует на двух языках: `README.md` /
`README.ru.md` и `docs/en/...` / `docs/ru/...`. Меняйте обе версии в одном
коммите. Внутренние материалы для нетабличных (учебных) задач лежат в
игнорируемом каталоге `thesis/` и не должны ссылаться из продуктовой
документации или кода.
