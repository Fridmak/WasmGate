# Архитектура

WasmGate разделён на **data plane** (сам прокси) и опциональный **control plane**,
который хранит желаемое состояние и присылает релизы. На пути запроса прокси
никогда не обращается к control plane.

```
клиент ── TCP ──► листенер ──► rate limit / max_conns ──► выбор пула
                                    │                         │
                                    ▼                         ▼
                              WASM on_accept              dial к upstream
                                    │                         │
                                    └──────── релей ──────────┘
                                       io.Copy / splice(2)
```

## Жизненный цикл соединения

1. **Accept.** Листенер проверяет `max_conns`, затем скользящее окно по
   IP/подсети (`rate_limit`). Отказы считаются в метриках и не доходят ни до
   плагина, ни до бэкенда.
2. **Плагины (L4).** `on_accept` получает канонический IP клиента и порт и может
   отклонить соединение до dial к upstream.
3. **Выбор пула.** `round_robin` или `least_conn` выбирает здоровый узел. При
   ошибке dial выполняются повторные попытки по другим узлам (`dial_retries`).
4. **TLS.** С `tls` прокси терминирует TLS и релей видит открытый текст; с
   `sni_routes` читается только ClientHello, выбирается пул и байты
   переигрываются, поэтому сквозной TLS и `splice(2)` сохраняются.
5. **Релей.** На Linux обычный путь использует `io.Copy`, который доходит до
   `TCPConn.ReadFrom` → `splice(2)`: payload не попадает в user space. На других
   платформах тот же цикл получает по буферу на направление из пула.
6. **Close.** Считаются отправленные и полученные байты, вызывается `on_close`,
   обновляются счётчики без гонок.

## Hot reload

Работающая конфигурация — неизменяемый **snapshot**: листенеры, пулы, лимитеры,
плагины. `SIGHUP` или `POST /reload` разбирает и валидирует новый документ в
новый snapshot, затем атомарно подменяет его; текущие соединения дорабатывают на
старом. Документ, не прошедший валидацию, отклоняется до подмены, поэтому опечатка
не роняет процесс. Reload под нагрузкой покрыт тестом с нулём потерянных
соединений.

## Пакеты

| Пакет | Ответственность |
|---|---|
| `internal/config` | схема YAML (`KnownFields`), дефолты, валидация, `Resolve` для относительных путей |
| `internal/proxy` | совместимый фасад API data plane |
| `internal/proxy/runtime` | неизменяемые snapshot, публикация reload, registry плагинов и лимиты |
| `internal/proxy/conn` | лимиты соединения, pipeline плагина, dial и relay |
| `internal/proxy/server` | bind листенеров, accept, reload, probes и shutdown |
| `internal/proxy/backend` | пулы, балансировка, health и circuit breaker |
| `internal/proxy/relay` | двунаправленный relay, half-close, idle watchdog и splice |
| `internal/proxy/httpinspect` | HTTP/1 headers, framing, body policy и mutation |
| `internal/plugin/abi4/engine` | рантайм ABI v4, lifecycle инстансов, dispatch и hooks |
| `internal/plugin/abi4/envelope` | IN envelope ABI v4 и HTTP header indexing |
| `internal/plugin/abi4/output` | OUT envelope ABI v4 |
| `internal/plugin/abi4/plan` | phases, flags, plans, capabilities и verdicts |
| `internal/plugin/abi4/state` | ограниченные host-owned state slots |
| `internal/plugin/abi4/wire` | общая проверка и кодирование section table |
| `internal/tlspeek` | парсер ClientHello для SNI-маршрутизации |
| `internal/ratelimit` | скользящее окно и guard одновременных соединений |
| `internal/observe` | структурные логи, метрики Prometheus, admin HTTP endpoint |
| `internal/controlplane` | публичный фасад Agent и совместимые алиасы |
| `internal/controlplane/options` | общие options и Normalize |
| `internal/controlplane/identity` | файлы identity, enrollment и продление сертификата |
| `internal/controlplane/release` | проверка подписанных релизов, артефакты и apply |
| `internal/controlplane/session` | transport, stream loop, heartbeat и dispatch |

## Ограничения дизайна

- **Без CGO.** Бинарник статический и кросс-компилируется; песочница — чистый Go
  (`wazero`), без WASI и системных вызовов для гостя.
- **Горячий путь без аллокаций.** Балансировщик, лимитер, чтение snapshot и хуки
  плагинов не аллоцируют на соединение; буферы переиспользуются.
- **Fail fast.** Неверный конфиг, сигнатуры плагинов, лимиты памяти или TLS
  прерывают валидацию до изменения листенеров.
- **Ограниченные ресурсы.** `max_conns` на листенер, страницы памяти WASM,
  таймаут плагина, лимиты частоты и idle timeout задаются явно.
- **Control plane — вне трафика.** Он хранит конфигурацию и артефакты, подписывает
  релизы и присылает команды по gRPC-сессии; если он недоступен, data plane
  продолжает работать с последней применённой конфигурацией.

Управляемый режим — в [control plane](control-plane.md), все параметры — в
[конфигурации](configuration.md).
