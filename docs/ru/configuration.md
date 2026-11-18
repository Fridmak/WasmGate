# Конфигурация

WasmGate читает один YAML-документ. Его же используют `-validate`, первый запуск
и каждый hot reload, поэтому конфигурация, прошедшая валидацию, загрузится
одинаково и в рантайме.

```bash
wasmgate -config /etc/wasmgate/wasmgate.yaml -validate
wasmgate -config /etc/wasmgate/wasmgate.yaml
kill -HUP $(pidof wasmgate)          # или: curl -X POST http://127.0.0.1:9099/reload
```

Пример с комментариями: [`configs/wasmgate.yaml`](../../configs/wasmgate.yaml).
Showcase со всеми плагинами: [`configs/showcase.yaml`](../../configs/showcase.yaml).

## Верхний уровень

| Ключ | Тип | По умолчанию | Значение |
|---|---|---|---|
| `version` | int | — | Версия схемы; должна быть `1` |
| `admin_addr` | string | пусто (выключен) | `host:port` для `/metrics`, `/healthz`, `/reload`, опционально pprof |
| `admin_token_file` | string | пусто | Файл с bearer-токеном для admin endpoint (или `WASMGATE_ADMIN_TOKEN`) |
| `admin_pprof` | bool | `false` | Включить `/debug/pprof/` (только для локальной отладки) |
| `admin_allow_remote` | bool | `false` | Разрешить не-loopback `admin_addr` |
| `listeners` | list | — | Входные TCP-листенеры, см. ниже |
| `pools` | map | — | Группы бэкендов по имени |

Неизвестные ключи отклоняются (`KnownFields(true)`); дубли имён листенеров,
адресов и ссылки на отсутствующий пул ломают валидацию.

## Листенеры

| Ключ | Тип | По умолчанию | Значение |
|---|---|---|---|
| `name` | string | — | Уникальное имя для логов, метрик и ошибок |
| `addr` | string | — | Адрес, например `0.0.0.0:443` |
| `upstream_pool` | string | — | Пул, получающий соединения |
| `max_conns` | int | `0` (без лимита) | Лимит одновременных соединений; лишние отклоняются и считаются |
| `dial_timeout` | duration | `5s` | Таймаут dial к upstream |
| `idle_timeout` | duration | `0` (выкл) | Idle-дедлайн, обновляемый активностью в любую сторону; `0` сохраняет `splice(2)` |
| `rate_limit` | object | выкл | Скользящее окно по IP/подсети, см. ниже |
| `plugin` | object | выкл | WASM-плагин для листенера |
| `sni_routes` | map | пусто | `host` или `*.domain` → пул, для TLS passthrough |
| `tls` | object | выкл | Терминация TLS (отключает `splice` и `sni_routes` на листенере) |

### `rate_limit`

| Ключ | По умолчанию | Значение |
|---|---:|---|
| `max` | `0` (выкл) | Новых соединений с IP за окно |
| `subnet_max` | `0` (выкл) | Новых соединений с подсети за окно |
| `max_concurrent` | `0` (выкл) | Одновременных соединений с IP |
| `subnet_max_concurrent` | `0` (выкл) | Одновременных соединений с подсети |
| `window` | `1s` | Длина окна (минимум 10 мс) |
| `ban_duration` | `0` (выкл) | Базовый бан за нарушение; удваивается до 16× |
| `allow_cidrs` | пусто | CIDR, которые не считаются |
| `ipv6_host_prefix` | `64` | Группировка IPv6 для лимита на IP |
| `ipv6_subnet_prefix` | `56` | Группировка IPv6 для лимита на подсеть |

### `plugin`

| Ключ | По умолчанию | Значение |
|---|---|---|
| `path` | — | Файл `.wasm`; пусто отключает плагин |
| `max_memory_pages` | `17` | Лимит линейной памяти на инстанс (17 × 64 KiB = 1,06 МиБ; потолок, а не начальный размер) |
| `max_instances` | `64` | Максимум одновременных инстансов гостя на listener (жёсткий предел `1024`); запросы сверх лимита ждут слот или завершаются по своему дедлайну |
| `timeout` | `1s` | Таймаут хука; отсутствие или `0` выбирает безопасный дефолт `1s`. Для `data_mode: headers` обязателен `> 0` |
| `data_mode` | обязательно | `once` проверяет первый пакет клиента и сохраняет `splice` при `idle_timeout: 0`; `headers` собирает полный HTTP/1 request-header block и сохраняет `splice` для тела только без `body_policy` (и при `idle_timeout: 0`); `stream` проверяет каждый чанк. Все три — пути ABI v4 `on_event`. Обязателен, если модуль экспортирует L7-хук |
| `max_header_bytes` | `16384` | Режим headers: максимум заголовков; жёсткий предел `16389`. v4-индекс принимает не более 512 заголовков; больше — fail closed |
| `inspect_timeout` | `3s` | Режим headers: общий дедлайн на один header block |
| `route_pools` | пусто | Пулы, выбираемые возвратом v4 `on_event` `2..N`; поддерживаются для `once`/`headers`, запрещены для `stream` |
| `body_policy` | пусто | `request`, `response` или `both`; ограничивает `headers` одним HTTP/1-обменом ([правила](plugins.md#политика-http-тела)) |
| `max_body_bytes` | `1048576` | Максимальный заявленный `Content-Length` в любом направлении при `body_policy`; сообщения больше лимита отвергаются до чтения тела |
| `mutation_policy` | `none` | `replace` разрешает замену блоков тела без изменения длины; требуются `body_policy` и `CapMutation` у гостя |

Для любого `data_mode` модуль обязан экспортировать ABI v4-контракт
(`on_event`, `wg_abi`, `wg_caps`, `wg_buf_ptr`, `wg_buf_len`). L4-only модули
(без `data_mode`) обязаны экспортировать `wg_abi`, `wg_caps` и хотя бы один из
`on_accept`/`on_close`/`on_close_ex`. Legacy-модули ABI v3 отвергаются при
загрузке snapshot.

Прокси поддерживает только однозначный framing через `Content-Length`.
`Transfer-Encoding`, дубли и некорректный `Content-Length` отвергаются до
использования upstream. Заявленная длина больше `max_body_bytes` отвергается
до чтения тела.

ABI и ограничения песочницы — в [плагинах](plugins.md).

### `tls`

| Ключ | По умолчанию | Значение |
|---|---|---|
| `cert_file` | — | PEM-цепочка сертификатов |
| `key_file` | — | PEM-ключ |
| `min_version` | `1.2` | `1.2` или `1.3` |
| `handshake_timeout` | `10s` | Дедлайн handshake |

Сертификаты перезагружаются вместе с остальным документом.

## Пулы

| Ключ | По умолчанию | Значение |
|---|---|---|
| `balance` | `round_robin` | `round_robin` или `least_conn` |
| `servers` | — | Список `host:port` |
| `dial_retries` | `0` | Дополнительные попытки по другим здоровым узлам |
| `health_check` | выкл | Активные TCP-пробы |
| `circuit_breaker` | выкл | Трёхсостоянийный пассивный breaker |

`health_check`:

| Ключ | По умолчанию | Значение |
|---|---|---|
| `interval` | `0` (выкл) | Период проб |
| `timeout` | `1s` | Таймаут dial на пробу |

`circuit_breaker`:

| Ключ | По умолчанию | Значение |
|---|---|---|
| `error_threshold` | `0` | Подряд ошибок для открытия; `0` — после первой |
| `open_duration` | `10s` | Время в Open до half-open пробы |
| `half_open_requests` | `1` | Подряд успехов для закрытия |

## Семантика

- **Относительные пути.** Пути плагинов и TLS резолвятся от каталога файла
  конфигурации, а не от рабочего каталога процесса. Поэтому конфигурация,
  применённая control plane, работает с артефактами рядом с собой.
- **Атомарный reload.** Новый документ валидируется целиком, затем подменяет
  snapshot; текущие соединения дорабатывают на старом.
- **Fail fast.** Неверный YAML, неизвестные поля, отсутствующий пул, битый TLS или
  плагин, нарушающий ABI/лимиты, отменяют reload — старая конфигурация продолжает
  работать.
- **Admin endpoint.** Держите `admin_addr` на loopback. Не-loopback адрес требует
  `admin_allow_remote: true` **и** токен, иначе процесс не стартует.
