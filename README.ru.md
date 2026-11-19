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
  закрытие, в жёсткой песочнице: без WASI, с лимитом памяти и таймаутом хука
  (по умолчанию 1 с).
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
| WASM | ABI v4 `on_event` для `once`, `headers` и `stream`, плюс ABI v4 lifecycle `on_accept`/`on_close` |
| Наблюдаемость | Admin endpoint (`/metrics`, `/healthz`, `/reload`), структурные логи, pprof по запросу |
| Control plane | Enrollment, сертификаты, неизменяемые конфиги, подписанные релизы, деплой волнами, rollback, аудит, admin UI |

## Производительность

Linux, контейнер на 4 vCPU в Docker Desktop/WSL2 (AMD Ryzen 5 3550H, Go 1.27.4),
один и тот же TCP echo-бэкенд для всех строк. Медиана трёх прогонов; full —
`max_conns` + `rate_limit` + ABI v4 lifecycle WASM hook:

| Сценарий / метрика | direct | WasmGate L4 | WasmGate full* | nginx stream | HAProxy TCP |
|---|---:|---:|---:|---:|---:|
| Throughput, 64 KiB | 580.6 MB/s | 452.3 MB/s | 419.5 MB/s | 277.6 MB/s | 233.0 MB/s |
| Throughput, 1 MiB | 793.5 MB/s | 589.3 MB/s | 549.8 MB/s | 269.4 MB/s | 230.4 MB/s |
| Задержка релея (RTT 1 байт) | 28.9 µs | 54.7 µs | 59.5 µs | 527.3 µs | 506.7 µs |

\* full = `max_conns` + `rate_limit` + ABI v4 lifecycle WASM; без TLS
termination, L7 body policy, circuit breaker и dial retries.

Это актуальные результаты v4 от 2026-11-19 (stable `2ff37ac`), снятые в
Linux-контейнере. WSL2 — настоящее ядро Linux, поэтому `splice(2)` и
относительные числа корректны; абсолютные MB/s нельзя сравнивать с bare metal.
Отдельный чистый reference на 2 vCPU в Yandex Cloud (`docs/ru/benchmarks.md`)
показал 1519 MB/s для L4 и 1572 MB/s для full на 64 KiB. В
[руководстве по бенчмаркам](docs/ru/benchmarks.md) есть таблица Windows,
пути 4 KiB / 1 MiB, режимы ABI v4 `once`/`headers`/`stream`, масштабирование и
микробенчмарки hot path. Сырой вывод:
[`benchmarks/linux-current-results.txt`](benchmarks/linux-current-results.txt),
[`benchmarks/yandex-current-results.txt`](benchmarks/yandex-current-results.txt)
и [`benchmarks/windows-current-results.txt`](benchmarks/windows-current-results.txt).

## Релизы

Релизы по тегам публикуются на странице **Releases** репозитория. В релизе:
готовые архивы data plane (Linux amd64/arm64, Windows amd64), self-contained
Native AOT архив control plane для Linux amd64, Docker-образы, `SHA256SUMS` и
готовые команды запуска. Начните с описания релиза; полное руководство — в
[docs/ru/deployment.md](docs/ru/deployment.md).

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

Опциональный control plane (`.NET 10`, SQLite) превращает прокси в
управляемый парк: агенты enroll'ятся по одноразовому токену, получают mTLS-сертификат
и продлевают его автоматически; версии конфигурации становятся подписанными
релизами и раскатываются волнами. На admin-порту есть встроенная панель.

На одной машине `WASMGATE_CP_LOCAL=true` и `wasmgate -cp-local` полностью
убирают TLS, сертификаты агентов и enrollment-токены: CP и DP общаются по
loopback. Этот режим доверяет любому локальному процессу, поэтому подходит
только для однопользовательской машины.

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
internal/proxy/        совместимый фасад и сквозные тесты
internal/proxy/runtime/ snapshot, registry плагинов и лимиты
internal/proxy/conn/    pipeline соединения и dial
internal/proxy/server/  жизненный цикл листенеров, reload и probes
internal/proxy/backend/ пулы, балансировка, health и breakers
internal/proxy/relay/  двунаправленный relay и idle/splice paths
internal/proxy/httpinspect/ HTTP/1 framing и body policy
internal/plugin/abi4/  рантайм ABI v4: envelope, plan, engine, lifecycle
internal/tlspeek/      парсер SNI для passthrough
internal/ratelimit/    скользящее окно
internal/observe/      логи, метрики, admin endpoint
internal/controlplane/ фасад клиента Agent
internal/controlplane/options/ общие options
internal/controlplane/identity/ enrollment и файлы сертификатов
internal/controlplane/release/ подписанные релизы и артефакты
internal/controlplane/session/ transport, heartbeat и dispatch
control-plane/         C# control plane и тесты
plugins/examples/      девять эталонных плагинов (Rust, готовые .wasm)
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
[ABI v4](docs/ru/plugin-abi-v4.md) ·
[Миграция на v4](docs/ru/migration-to-v4.md) ·
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
- Control plane требует .NET 10; SQLite-хранилище встроено.

## Лицензия

MIT. См. [LICENSE](LICENSE).
