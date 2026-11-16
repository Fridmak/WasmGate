# Плагины (WASM ABI v3)

Плагины — WebAssembly-модули, которые Go-хост исполняет через `wazero`. Плагин
работает в чистой песочнице: без WASI, файловой системы, сокетов, часов и
генератора случайных чисел. Это детерминированная функция от метаданных
соединения.

Восемь эталонных плагинов лежат в [`plugins/examples/`](../../plugins/examples/);
их `.wasm` уже закоммичены, Rust для запуска не нужен.

| Плагин | Что демонстрирует | Режим | Вердикт |
|---|---|---|---|
| `ipguard` | `on_accept`: L4 allow/deny до `dial` | L4 | `0` allow, `≠0` deny |
| `http_guard` | `on_data_ex`: методы/пути, Basic auth, CL+TE-smuggling, маскировка заголовков | `stream` | `0` / `1` |
| `http_body_guard` | `on_data_state` (ABI v3): разорванные заголовки, `Content-Length`, сигнатуры в теле, keep-alive счётчики | `stream` | `0` / `1` |
| `path_router` | `route_pools` по `Host` и пути | `once` | `2..4` → пулы |
| `sni_guard` | SNI из ClientHello без терминации TLS | `once` | `2..4` → пулы |
| `protocol_router` | Определение TLS/HTTP/SSH/PostgreSQL/Redis по первым байтам | `once` | `2..6` → пулы |
| `redis_guard` | Разбор RESP и inline-команд, ACL для `CONFIG`/`FLUSHALL`/`EVAL` | `stream` | `0` / `1` |
| `stream_guard` | Сигнатуры атак в теле запроса, in-place маскировка JSON-секретов | `stream` | `0` / `1` |

Полный showcase: [`configs/showcase.yaml`](../../configs/showcase.yaml).

## Минимальная конфигурация

```yaml
listeners:
  - name: edge
    addr: "127.0.0.1:8080"
    upstream_pool: web
    plugin:
      path: "plugins/examples/ipguard/ipguard.wasm"
      max_memory_pages: 16    # 16 * 64 KiB = 1 MiB на инстанс
      timeout: 0s             # 0 = без таймера (только доверенные плагины)
      data_mode: "once"       # "once" (по умолчанию) или "stream"
      route_pools: ["canary"] # on_data >= 2 -> route_pools[ret-2]
```

## Контракт хуков

Хост передаёт метаданные через стек `uint64` (`wazero` `CallWithStack`), без
выделения строк и копирования в линейную память.

| Экспорт | Сигнатура | Аргументы | Результат |
|---|---|---|---|
| `on_accept` | `(i64, i64, i32) -> i32` | `ip_hi`, `ip_lo` (16-байтный канонический IP, big-endian; IPv4 — `::ffff:a.b.c.d`), порт клиента | `0` allow, любой ненулевой запрещает до `dial` |
| `on_data` | `(i32, i32) -> i32` | `ptr`, `len` в линейной памяти (**обязателен `on_data_ptr() -> i32`**); до 32 KiB, можно менять на месте | `0` allow, `1` deny, `>= 2` → `route_pools[ret-2]` (только `once`) |
| `on_data_ex` | `(i64, i32, i32, i32) -> i32` | `conn_id`, `dir` (0 client→server, 1 server→client), `ptr`, `len` | Как `on_data` |
| `on_data_state` | `(i64, i32, i32, i32, i32, i32, i32) -> i64` | `conn_id`, `dir`, `state_ptr`, `state_len`, `state_cap` (4 KiB), `data_ptr`, `data_len`; нужны `on_data_ptr()` и `on_state_ptr()` | Младшие 32 бита — вердикт; старшие — новая длина состояния (`0..4096`) |
| `on_close` | `(i64, i64, i64, i64) -> ()` | `ip_hi`, `ip_lo`, `rx_bytes`, `tx_bytes` | — |
| `on_close_ex` | `(i64, i64, i64, i64, i64) -> ()` | `conn_id`, `ip_hi`, `ip_lo`, `rx_bytes`, `tx_bytes` | — |

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
- **Таймаут.** При `timeout > 0` зависший хук прерывается, соединение отклоняется,
  инстанс уничтожается. При `timeout: 0` таймер не создаётся (0 аллокаций на
  вызов) — только для доверенных плагинов.

## Режимы данных и TLS

- `data_mode: once` (по умолчанию) проверяет первое чтение клиента (до 16 389
  байт) до `dial`, затем возвращает `splice(2)`. Это подсказка для маршрутизации
  и мутации первого пакета, а не полноценный фильтр: для фильтрации нужен
  `stream`.
- `data_mode: stream` вызывает хук на каждый чанк в обе стороны (через
  `on_data_ex`), отключает `splice` и не применяет `route_pools`.
- Терминация TLS расшифровывает поток до `on_data`; passthrough оставляет его
  зашифрованным, и плагин видит `ClientHello`/record'ы. На листенере с
  терминацией `sni_routes` игнорируется.

## Состояние ABI v3

`on_data_state` даёт каждому направлению соединения ограниченный буфер состояния
4 KiB, которым управляет хост и который копируется внутрь/наружу вокруг вызова.
Он нужен для разорванных заголовков, keep-alive-фрейминга и инспекции тела, где
stateless-хуков недостаточно. Эталон — [`http_body_guard`](../../plugins/examples/http_body_guard).

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
