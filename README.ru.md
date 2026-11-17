# WasmGate

**WasmGate** — TCP/L4 reverse proxy на Go с поддержкой WebAssembly-плагинов и
опциональным control plane для парка edge-узлов. Это один статический бинарник
(без CGO): горячий путь не аллоцирует, а на Linux используется `splice(2)`, поэтому
обычный релей не копирует трафик через user space.

[English version](README.md) · [Документация](#документация)

## Зачем

- **Сырой TCP остаётся сырым.** На пути релея нет разбора HTTP, поэтому работают
  любые протоколы: PostgreSQL, Redis, SSH, gRPC, произвольный бинарный фрейминг.
- **Zero-copy на Linux.** Обычный релей доходит до `splice(2)` через стандартный
  `io.Copy`; буферы не выделяются на каждое соединение.
- **Программируемость при необходимости.** WASM-хуки на accept, на данные и на
  закрытие, в жёсткой песочнице: без WASI, с лимитом памяти и таймаутом.
- **Эксплуатация из коробки.** Hot reload по `SIGHUP`/`POST /reload`, метрики
  Prometheus, health-эндпоинт, rate limiting, health checks и circuit breaker.
- **Управление парком (опционально).** Control plane на .NET хранит версии
  конфигурации, подписывает релизы, выдаёт агентам сертификаты по mTLS и
  раскатывает волнами.

## Возможности

| Область | Что умеет |
|---|---|
| Листенеры | TCP, несколько листенеров, таймауты dial/idle и лимит соединений на листенер |
| Пулы | `round_robin` и `least_conn`, повторные попытки dial по здоровым узлам |
| Health | Активные TCP-пробы, пассивная отметка упавших upstream'ов |
| Устойчивость | Опциональный трёхсостоянийный circuit breaker (closed/open/half-open) |
| Rate limiting | Скользящее окно по IP и подсети, лимиты одновременных, баны, allow-list CIDR |
| TLS | Passthrough по SNI с выбором пула или терминация с hot reload сертификатов |
| WASM | L4 `on_accept`/`on_close`, ABI v4 `on_event` для `data_mode: headers`; legacy-хуки ABI v3 `once`/`stream` (`on_data`, `on_data_ex`, `on_data_state`) |
| Наблюдаемость | Admin endpoint (`/metrics`, `/healthz`, `/reload`), структурные логи, pprof по запросу |
| Control plane | Enrollment, сертификаты, неизменяемые конфиги, подписанные релизы, деплой волнами, rollback, аудит, admin UI |

## Производительность

Linux (WSL2, ядро 6.6.87, 4 vCPU, `-benchtime=1s -benchmem`), один и тот же
TCP echo-бэкенд для всех строк:

| Сценарий / метрика | direct | WasmGate L4 | nginx stream | HAProxy TCP |
|---|---:|---:|---:|---:|
| Throughput, 64 KiB | 644.6 MB/s | **588.8 MB/s** | 272.5 MB/s | 268.9 MB/s |
| Throughput, 1 MiB | 865.6 MB/s | **688.3 MB/s** | 230.9 MB/s | 248.3 MB/s |
| Задержка релея (RTT 1 байт) | 29.2 µs | **62.0 µs** | 394.0 µs | 439.2 µs |
| Установка соединения | 402.9 µs | **917.7 µs** | 1076.9 µs | 1163.8 µs |

WSL2 — это настоящее ядро Linux, поэтому `splice(2)` и относительные числа
корректны; абсолютные MB/s нельзя сравнивать с bare metal. Воспроизведение:
`make bench`; методика, сырой вывод и сравнение с Windows — в
[бенчмарках](docs/ru/benchmarks.md).

## Быстрый старт

```bash
# Статический бинарник
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o wasmgate ./cmd/wasmgate

# Проверить конфигурацию (заодно компилируются все указанные плагины)
./wasmgate -config configs/wasmgate.yaml -validate

# Запустить
./wasmgate -config configs/wasmgate.yaml

# Пример слушает 127.0.0.1:8080, метрики — на 127.0.0.1:9099
curl http://127.0.0.1:8080/
curl http://127.0.0.1:9099/metrics
```

Минимальная конфигурация:

```yaml
version: 1
admin_addr: "127.0.0.1:9099"

listeners:
  - name: edge
    addr: "0.0.0.0:443"
    upstream_pool: web
    max_conns: 10000

pools:
  web:
    balance: round_robin
    servers:
      - "10.0.0.10:8080"
      - "10.0.0.11:8080"
```

Полный справочник — [конфигурация](docs/ru/configuration.md); в
`configs/wasmgate.yaml` описан каждый параметр.

## Control plane

Опциональный control plane (`.NET 10`, PostgreSQL) превращает прокси в
управляемый парк: агенты enroll'ятся по одноразовому токену, получают mTLS-сертификат
и продлевают его автоматически; версии конфигурации становятся подписанными
релизами и раскатываются волнами. На admin-порту есть встроенная панель.

```bash
cd control-plane
dotnet build WasmGate.Control.sln
dotnet run --project src/WasmGate.Control.Api
# открыть http://127.0.0.1:9097/
```

Схема, API, сертификаты, протокол агента и модель безопасности —
[control plane](docs/ru/control-plane.md).

## Структура проекта

```
cmd/wasmgate/          точка входа и флаги
internal/config/       схема YAML, валидация, hot reload
internal/proxy/        листенеры, пулы, релей, snapshot
internal/plugin/       песочница wazero и ABI v3
internal/plugin/abi4/  ядро ABI v4: envelope, plan, engine (M0)
internal/tlspeek/      парсер SNI для passthrough
internal/ratelimit/    скользящее окно
internal/observe/      логи, метрики, admin endpoint
internal/controlplane/ клиент control plane (enroll, session, apply)
control-plane/         C# control plane и тесты
plugins/examples/      восемь эталонных плагинов (Rust, готовые .wasm)
configs/               пример и showcase-конфигурация
deployments/           сценарии развёртывания, Dockerfile'ы, systemd, dev-сертификаты
benchmarks/            сырой вывод бенчмарков
docs/en, docs/ru       документация на двух языках
```

## Документация

Русский:
[Архитектура](docs/ru/architecture.md) ·
[Конфигурация](docs/ru/configuration.md) ·
[Плагины](docs/ru/plugins.md) ·
[Бенчмарки](docs/ru/benchmarks.md) ·
[Развёртывание](docs/ru/deployment.md) ·
[Control plane](docs/ru/control-plane.md) ·
[Метрики](docs/ru/metrics.md) ·
[Разработка](docs/ru/development.md)

English:
[Architecture](docs/en/architecture.md) ·
[Configuration](docs/en/configuration.md) ·
[Plugins](docs/en/plugins.md) ·
[Benchmarks](docs/en/benchmarks.md) ·
[Deployment](docs/en/deployment.md) ·
[Control plane](docs/en/control-plane.md) ·
[Metrics](docs/en/metrics.md) ·
[Development](docs/en/development.md)

## Требования

- Go 1.26+ (в модуле закреплён toolchain 1.27).
- Для zero-copy `splice(2)` нужен Linux; на других платформах используется
  буферизованный релей с пулом буферов — работает корректно, но медленнее на
  больших потоках.
- Control plane требует .NET 10 и PostgreSQL 17.

## Лицензия

MIT. См. [LICENSE](LICENSE).
