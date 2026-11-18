# Миграция плагинов на ABI v4

ABI v4 заменил поверхность хуков v1–v3. Рантаймы v1–v3 удалены: загрузчик
отклоняет legacy-экспорты, fallback нет. Гостя v1–v3 нужно пересобрать под
контракт v4, а не просто переименовать или перепаковать.

Загрузчик не определяет версию ABI по именам хуков. Модуль, который всё ещё
экспортирует `on_headers`, `on_data`, `on_data_ex` или `on_data_state`, не
загрузится. Модули v1/v2 также legacy. Таблица ниже покрывает имена, которые
отклоняет текущий загрузчик; перед началом проверьте фактические экспорты.

Формат сообщений описан в [контракте ABI v4](plugin-abi-v4.md). Это руководство
покрывает шаги миграции, нужные автору существующего плагина.

## Сопоставление хуков

| Legacy (v1–v3) | ABI v4 | Примечание |
|---|---|---|
| `on_accept(i64, i64, i32) -> i32` | та же сигнатура плюс `wg_abi`, `wg_caps` | `0` разрешает, любой ненулевой результат запрещает. L4-модуль объявляет `CapLifecycle`. |
| `on_close(i64, i64, i64, i64)` | та же сигнатура или `on_close_ex` | `on_close_ex` добавляет `conn_id`. После разрешающего `on_accept` close-хук вызывается ровно один раз, включая сбои TLS, заголовков, маршрутизации и dial. |
| `on_close_ex(i64, i64, i64, i64, i64)` | та же сигнатура | Предпочтителен, если гостю нужен `conn_id`. |
| `on_data(i32, i32) -> i32` | `on_event` с `PhaseOpaqueStream` (5) | Читайте IN envelope по переданному указателю. Opaque-события только для чтения. |
| `on_data_ex(i64, i32, i32, i32) -> i32` | `on_event` с `PhaseOpaqueStream` и флагом направления | `FlagClientToServer` — `8`, `FlagServerToClient` — `16`. Оба бита одновременно недопустимы. |
| `on_headers(i64, i32, i32) -> i32` | `on_event` с `PhaseRequestHeaders` (1) | IN envelope содержит blob, индекс заголовков и start line. Заголовки ответа — `PhaseResponseHeaders` (3), если их выбирает `body_policy`. |
| `on_data_state(...)` | прямого аналога нет | В v4 нет state для stream. State существует только для фаз заголовков/тела, выбранных `body_policy`, при `CapStateIn` и `CapStateOut`. |
| `on_data_ptr()`, `on_state_ptr()` | `wg_buf_ptr`, `wg_buf_len`, `wg_out_ptr`, `wg_out_len` | Host читает IN-окно; гость пишет эффекты в отдельное OUT-окно. Указателя на state нет. |

`on_event` имеет сигнатуру `(i64, i32, i32, i32, i32) -> i64`:
`conn_id, phase, flags, in_ptr, in_len`. Младшие 32 бита результата — вердикт:
`0` разрешает, `1` запрещает, `2 + index` выбирает `route_pools[index]`.
Отрицательные и выходящие за диапазон значения недопустимы и приводят к
fail closed. Маршрутизация доступна только для `once` и `headers`.

## Сопоставление конфигурации

| Legacy-настройка | ABI v4 |
|---|---|
| L4-модуль без `data_mode` | По-прежнему поддерживается. Модуль обязан экспортировать `wg_abi`, `wg_caps` и хотя бы один из `on_accept`, `on_close`, `on_close_ex`, а также объявить `CapLifecycle`. |
| `data_mode: once` | Тот же режим, но гость должен реализовать opaque-фазу v4 `on_event`. |
| `data_mode: headers` | Тот же режим, но `on_headers` отклоняется. `timeout` должен быть больше нуля. |
| `data_mode: stream` | Тот же режим, только чтение. `route_pools` запрещён. |
| `timeout: 0` | Выбирает дедлайн хука `1s`. Таймер нельзя отключить. |
| `max_memory_pages: 16` | По умолчанию `17` страниц (1,06 МиБ). Модуль может запрашивать меньше. |
| `max_instances` | По умолчанию `64`, жёсткий предел `1024`, на listener. |
| `route_pools` | Поддерживается для `once` и `headers`; для `stream` запрещён. |
| `body_policy` | `request`, `response` или `both`. Требует `data_mode: headers`. |
| `max_body_bytes` | По умолчанию `1 МиБ`; ограничивает заявленный `Content-Length` до чтения тела. |
| `mutation_policy` | По умолчанию `none`. `replace` требует `body_policy` и гостя с `CapMutation`. |
| `inspect_timeout` | По умолчанию `3s` на один header block. |
| `max_header_bytes` | По умолчанию `16 КиБ`, жёсткий предел `16389`; индекс принимает не более 512 полей. |

`headers` с `sni_routes` и без терминации TLS отклоняется: прокси не может
инспектировать открытый HTTP через зашифрованный passthrough-поток.

## State и замены

Эффекты v4 передаются в OUT envelope. Гость обязан экспортировать
`wg_out_ptr` и `wg_out_len`, если объявляет `CapStateOut` или `CapMutation`.

- State необязателен. Прокси хранит его только для гостей, объявивших обе
  state-возможности, и только для фаз заголовков/тела, выбранных `body_policy`.
  Опущенный state оставляет прежнее значение; явно пустой очищает байты, но
  остаётся присутствующим.
- Замены ограничены проверяемыми блоками тела и требуют одновременно
  `mutation_policy: replace` и `CapMutation`. Длина замены должна точно
  совпадать с длиной входного блока. Мутация заголовков не поддерживается.
- Прокси использует блоки замены до 2 КиБ. При включённой мутации слот state
  допускает 1 984 байта, чтобы оба эффекта помещались в стандартное 4-КиБ
  OUT-окно.
- Опущенный или truncated-результат не фиксирует state и не пересылает байты
  замены. Отказ или некорректный эффект закрывает соединение.

Стандартное IN-окно использует первые 40 КиБ буфера гостя; гость должен
объявить не менее 44 КиБ по `wg_buf_ptr` и 4 КиБ по `wg_out_ptr`, причём
активная IN-область и OUT не должны пересекаться.

## Возможности, которых намеренно нет

Legacy-развёртыванию, которое зависит от этих поведений, нужен редизайн гостя
или конфигурации:

- мутация непрозрачных чанков `stream`;
- постоянное состояние между произвольными чанками потока или между
  keep-alive HTTP-обменами;
- мутация заголовков;
- fallback с `on_headers` на `on_data`;
- HTTP/1 upgrades, `CONNECT`, `Expect`, промежуточные ответы,
  `Transfer-Encoding` (включая chunked), тело до EOF и неоднозначные длины при
  включённом `body_policy`;
- инспекция HTTP/2.

`body_policy` также ограничивает соединение одним HTTP/1-обменом. Следующие
keep-alive сообщения не пересылаются.

## Минимальный Rust-гость

Референсный SDK — `plugins/examples/common`. Event-гость экспортирует
поверхность v4 через `wg_v4_plugin!`:

```rust
#![no_std]

use wg_common::{self as wg, v4};

wg::wg_v4_plugin!(v4::CAP_HTTP_HEADERS, inspect);

fn inspect(call: v4::Call, event: v4::Event<'_>, _out: &mut v4::Output<'_>) -> i32 {
    if call.phase != v4::Phase::RequestHeaders {
        return v4::DENY;
    }
    // Проверьте event.request_line() и event.headers(); верните ALLOW или DENY.
    v4::ALLOW
}
```

Lifecycle-only гость использует `wg_v4_metadata!(v4::CAP_LIFECYCLE)` и напрямую
экспортирует `on_accept`/`on_close` или `on_close_ex`, как в
[`ipguard`](../../plugins/examples/ipguard/src/lib.rs). Макрос добавляет
`CapBlob`; lifecycle-метаданные — нет.

## Сборка и проверка

Запускайте Cargo из `plugins/examples`: Cargo читает `.cargo/config.toml`
только из рабочего каталога, а этот файл задаёт 16-КиБ wasm-стек, с которым
собраны закоммиченные гости.

```bash
cd plugins/examples
cargo test --locked -p wg-common
cargo build --locked --workspace --release --target wasm32-unknown-unknown
```

Проверьте пересобранный модуль в реальной конфигурации:

```bash
wasmgate -config configs/showcase.yaml -validate
```

`-validate` компилирует каждый указанный модуль через загрузчик ABI v4 и
отклоняет legacy-экспорты, неверные сигнатуры, отсутствующие capabilities и
некорректные окна. Прошедший валидацию модуль всё равно нужно проверить на
запросах: загрузчик не проверяет логику политики.

## Чек-лист выката

1. Составьте список всех listener'ов с плагинами и их `data_mode`.
2. Пересоберите каждого гостя v1–v3 под SDK v4. Не оставляйте legacy-экспорты
   как fallback: загрузчик их отклоняет.
3. Перенесите stream-мутацию или state в `headers` + `body_policy` либо
   уберите поведение, если протокол нельзя разметить через `Content-Length`.
4. Явно задайте `timeout` для `headers`-listener'ов и проверьте
   `max_body_bytes`, `max_header_bytes` и `mutation_policy`.
5. Прогоните `-validate` на целевой конфигурации.
6. Выкатывайте по одному listener'у или окружению и следите за
   `wasmgate_plugin_header_incomplete_total` и
   `wasmgate_plugin_instances_discarded_total`.
7. Держите предыдущий релиз доступным для отката.

Режимы инспекции и лимиты — в [руководстве по плагинам](plugins.md), формат
сообщений — в [контракте ABI v4](plugin-abi-v4.md).
