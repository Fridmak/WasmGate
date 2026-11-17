# Плагины (ABI v4)

Плагины — WebAssembly-модули, которые Go-хост исполняет через `wazero`. Плагин
работает в чистой песочнице: без WASI, файловой системы, сокетов, часов и
генератора случайных чисел. Это детерминированная функция от метаданных
соединения.

> **Статус ABI:** proxy теперь использует ABI v4 и для L4 lifecycle, и для всех
> L7 `data_mode`. Legacy-модули ABI v3 отвергаются при загрузке snapshot.
> Закоммиченные v3-примеры оставлены как история и должны быть портированы на v4.

Девять эталонных плагинов лежат в [`plugins/examples/`](../../plugins/examples/);
это legacy ABI v3 исходники/бинарники, кроме v4-фикстур тестов.

| Плагин | Что демонстрирует | Режим | Вердикт |
|---|---|---|---|
| `ipguard` | legacy ABI v3 `on_accept`; не запускается до порта на v4 | legacy | `0` allow, `≠0` deny |
| `http_guard` | legacy ABI v3 `on_data_ex`; не запускается до порта на v4 | legacy | `0` / `1` |
| `http_headers_guard` | legacy ABI v3 `on_headers`; не принимается в `headers` до порта на v4 | legacy | `0` / `1` |
| `http_body_guard` | `on_data_state` (ABI v3): разорванные заголовки, `Content-Length`, сигнатуры в теле, keep-alive счётчики | `stream` | `0` / `1` |
| `path_router` | `route_pools` по `Host` и пути | `once` | `2..4` → пулы |
| `sni_guard` | SNI из ClientHello без терминации TLS | `once` | `2..4` → пулы |
| `protocol_router` | Определение TLS/HTTP/SSH/PostgreSQL/Redis по первым байтам | `once` | `2..6` → пулы |
| `redis_guard` | Разбор RESP и inline-команд, ACL для `CONFIG`/`FLUSHALL`/`EVAL` | `stream` | `0` / `1` |
| `stream_guard` | Сигнатуры атак в теле запроса, in-place маскировка JSON-секретов | `stream` | `0` / `1` |

Все закоммиченные примеры, включая `ipguard`, — legacy ABI v3 и отвергаются
текущим ABI v4-only loader до порта. Тесты используют собранные вручную
v4-фикстуры.

Полный showcase: [`configs/showcase.yaml`](../../configs/showcase.yaml).

## Минимальная конфигурация

```yaml
listeners:
  - name: edge
    addr: "127.0.0.1:8080"
    upstream_pool: web
    plugin:
      path: "plugins/examples/<v4-plugin>/<v4-plugin>.wasm"
      max_memory_pages: 16    # 16 * 64 KiB = 1 MiB на инстанс
      timeout: 1s             # таймаут хука; 0 выбирает безопасный дефолт 1s
      # L4-only модули не задают data_mode. ABI v4 L7-модули используют:
      # data_mode: "once" | "headers" | "stream"
      # max_header_bytes: 16384
      # inspect_timeout: 3s
      # route_pools: ["canary"] # once/headers only; stream cannot route
```

## Контракт хуков

Хост передаёт метаданные через стек `uint64` (`wazero` `CallWithStack`), без
выделения строк и копирования в линейную память.

| Экспорт | Сигнатура | Аргументы | Результат |
|---|---|---|---|
| `on_event` | `(i64, i32, i32, i32, i32) -> i64` | ABI v4: `conn_id`, `phase`, `flags`, `ptr`, `len` на envelope | Младшие 32 бита: `0` allow, `1` deny, `>= 2` route; старшие зарезервированы |
| `on_accept` | `(i64, i64, i32) -> i32` | `ip_hi`, `ip_lo` (16-байтный канонический IP, big-endian; IPv4 — `::ffff:a.b.c.d`), порт клиента | `0` allow, любой ненулевой запрещает до `dial` |
| `on_headers` | `(i64, i32, i32) -> i32` | только legacy ABI v3; текущий loader его отвергает | `0` / `1` |
| `on_data` | `(i32, i32) -> i32` | `ptr`, `len` в линейной памяти (**обязателен `on_data_ptr() -> i32`**); до 32 KiB, можно менять на месте | `0` allow, `1` deny, `>= 2` → `route_pools[ret-2]` (только `once`) |
| `on_data_ex` | `(i64, i32, i32, i32) -> i32` | `conn_id`, `dir` (0 client→server, 1 server→client), `ptr`, `len` | Как `on_data` |
| `on_data_state` | `(i64, i32, i32, i32, i32, i32, i32) -> i64` | `conn_id`, `dir`, `state_ptr`, `state_len`, `state_cap` (4 KiB), `data_ptr`, `data_len`; нужны `on_data_ptr()` и `on_state_ptr()` | Младшие 32 бита — вердикт; старшие — новая длина состояния (`0..4096`) |
| `on_close` | `(i64, i64, i64, i64) -> ()` | `ip_hi`, `ip_lo`, `rx_bytes`, `tx_bytes` | — |
| `on_close_ex` | `(i64, i64, i64, i64, i64) -> ()` | `conn_id`, `ip_hi`, `ip_lo`, `rx_bytes`, `tx_bytes` | — |

Строки `on_data`/`on_data_ex`/`on_data_state` описывают legacy-поверхность ABI
v3; текущий proxy отвергает их для любого `data_mode`. `on_accept` и `on_close`
активны на ABI v4 L4 lifecycle-пути.

Отрицательные вердикты и `INT32_MIN` означают deny; allow — только `0`. Индекс
маршрута вне `route_pools` — deny, а не fallback. Модуль без хуков или с
несовпадающей сигнатурой не проходит валидацию до подмены snapshot.

## Ограничения песочницы

- **Нет WASI и системных вызовов.** Файловая система, сокеты, окружение, часы и
  случайность недоступны.
- **Лимит памяти.** По умолчанию 16 страниц (1 MiB) на инстанс. Модуль, требующий
  больше `min`, отвергается при загрузке; `memory.grow` сверх лимита возвращает
  `-1`.
- **Reactor-модель.** Инстансы живут в `sync.Pool` и переиспользуются: глобальное
  состояние WASM не сбрасывается на соединение и не разделяется между инстансами.
- **Таймаут.** У каждого вызова хука есть дедлайн (`plugin.timeout`, по
  умолчанию `1s`, если не задан); зависший хук прерывается, соединение
  закрывается, инстанс уничтожается. Загрузка и инстанцирование плагина
  ограничены отдельным дедлайном 5s.

## Режимы данных и TLS

- `data_mode` **обязателен** для модулей с L7-событиями. `once` проверяет первое
  чтение клиента (до 16 389 байт) до `dial`, затем возвращает `splice(2)`. Это
  подсказка для маршрутизации и allow/deny, а не полноценный фильтр; envelope
  ABI v4 в M2 read-only.
- `data_mode: headers` собирает один полный HTTP/1.x request-header block
  (до `\r\n\r\n`) до `dial`, вызывает плагин один раз и возвращает `splice(2)`
  для тела. Модуль обязан экспортировать `on_event`, `wg_abi`, `wg_caps`,
  `wg_buf_ptr` и `wg_buf_len`; legacy-модули ABI v3 с `on_headers` отвергаются.
  Инспектируется только первый запрос keep-alive соединения. Индекс заголовков
  ограничен 512 записями; при большем количестве запрос отклоняется. Неполный,
  превышающий лимит, просроченный, malformed-блок и HTTP/2-preface отклоняются
  до `dial`. `plugin.timeout` должен быть больше нуля; `sni_routes` без
  терминации TLS запрещены.
- `data_mode: stream` вызывает `on_event` на каждый чанк в обе стороны,
  отключает `splice` и не умеет маршрутизировать: `route_pools` запрещены.
  В M2 поддерживается только allow/deny; in-place mutation и stateful body
  политики отвергаются до M4.
- Терминация TLS расшифровывает поток до `on_event`; passthrough оставляет его
  зашифрованным, и плагин видит `ClientHello`/record'ы. На листенере с
  терминацией `sni_routes` игнорируется.

## Legacy-состояние ABI v3

`on_data_state` относится к legacy-рантайму ABI v3. Текущий ABI v4-only L7-путь
его отвергает; state slots запланированы на M4. Эталонная v3-реализация
[`http_body_guard`](../../plugins/examples/http_body_guard) сохранена как
историческая.

## Сборка плагина

Rust (`#![no_std]`, рекомендуется):

```bash
cd plugins/examples
rustup target add wasm32-unknown-unknown
cargo build --release --target wasm32-unknown-unknown
cp target/wasm32-unknown-unknown/release/ipguard.wasm ipguard/ipguard.wasm
```

TinyGo:

```bash
cd plugins/examples/ipguard/tinygo
tinygo build -o ../ipguard.wasm -target=wasm-unknown -no-debug .
```

Проверка вместе с конфигурацией:

```bash
wasmgate -config configs/showcase.yaml -validate
```
