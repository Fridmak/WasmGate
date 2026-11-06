# 06. Журнал разработки: шаги, задачи, проблемы

> Рабочий журнал для текста ВКР (глава «Реализация» и «Эксперименты»). Фиксирует
> **что делали, зачем, с какими проблемами столкнулись и как решали**. Источник
> фактов — история git (коммиты указаны), `benchmarks/README.md` и ADR в
> `02-architecture-tech.md`. Открытые баги вынесены отдельно, в §6.

---

## 1. Хронология шагов

| Дата | Коммит | Шаг | Итог |
|------|--------|-----|------|
| 15.10 | `845e435`, `1882f4c` | Начальный коммит: базовая логика прокси и тесты | Работающий TCP-прокси (прототип) |
| 15.10 | `449c9cb` | Сохранение рабочего состояния | — |
| 15.10 | `a78ef26` | Сериализация reload, соблюдение `idle`/`max_conns`, защита `release` | Устранены гонки при reload |
| 15.10 | `609c11c` | Перестройка README, раздел про L4 | Документация в формальной структуре |
| 17.10 | `96953a8` | Рефакторинг конфига: валидация по объектам, `time.Duration` вместо своего типа | Тесты рядом с кодом |
| 17.10 | `393513e` | Исправление idle-таймаута и буферов копирования | См. проблемы P3, P4 |
| 17.10 | `4f5b03e` | Graceful lifecycle, состояние между reload, покрытие, бенчмарки | Shutdown/drain, registry, лог-лимитер |
| 17.10 | `19f3baa`, `89aeee7` | Методика бенчмарков, ADR-7 про стратегию копирования | Замеры Windows и Linux, обоснование решения |
| 24.10 | `df01ed2` | Уровень логирования как `slog.Level`, разбор один раз на границе CLI | Опечатки в `-log-level` больше не игнорируются |
| 24.10 | `703b093` | Метрики, admin-эндпоинт (`/metrics`, `/healthz`, pprof) | Наблюдаемость без нагрузки на data path |
| 25.10 | `acde0d7` | CI: fmt, vet, lint, `-race` на Linux, ручной бенчмарк-workflow | Проверки на каждый коммит |
| 25.10 | `417f5a0` | Исправления по результатам линтера CI | Чистый линт |
| 25.10 | `c4cebac` | Миграция golangci-lint на схему v2 | Актуальный линтер в CI |
| 05.11 | `df081ba`, `6f2f05f`, `f073f1d` | Исправление экранирования меток Prometheus (B2), переименование листенера при reload (B1), журнал разработки | Метрики и reconcile исправлены |
| 05.11 | `6ed2994` | Актуализация всей документации и удаление устаревших меток `FUTURE` | Документация синхронизирована с кодом |
| 05.11 | `7298e76` | **F2 (полный набор)**: `allow_cidrs` (16-байтные префиксы без аллокаций), агрегация `/24` и `/64` (`subnet_max`), экспоненциальный backoff-бан (`ban_duration`, 1x..16x) | `BenchmarkAllow` ~65 ns/op, `BenchmarkAllowParallel` ~20 ns/op, 0 allocs/op |
| 05.11 | `c98056e`, `ac8fdd3` | **M5 WASM Sandbox (`internal/plugin`)** + эталонный плагин `ipguard`: `wazero` без CGO и WASI, Reactor-пул инстансов, `CallWithStack` без аллокаций, fail-fast при старте/reload | `BenchmarkOnAccept` ~81 ns/op, `BenchmarkOnAcceptParallel` ~25 ns/op, **0 B/op, 0 allocs/op** |
| 05.11 | `fb9a00f` | **Health-checks апстримов**: `pools.*.health_check`, пассивное исключение при ошибке `dial` + активный TCP-пробинг, failover в `round_robin`/`least_conn` | Мёртвые бэкенды исключаются и возвращаются при выздоровлении |
| 05.11 | `465f7dc` | **`-validate` и `POST /reload` на `admin_addr`**: проверка конфига/WASM из CLI и HTTP-триггер hot reload для Windows (без `SIGHUP`) | Hot reload доступен на Windows и в скриптах |
| 05.11 | `18d4249`, `8a07de5`, `faa807c` | Автотесты и бенчмарки на Ubuntu 24.04 Linux против `nginx stream` 1.24.0 и `HAProxy` 2.8.30 + e2e-тест `ipguard.wasm` по реальным IP `127.0.0.1` и `127.0.0.99` | Сняты и сохранены таблицы S0–S5 в `benchmarks/linux-ubuntu-results.txt` |
| 06.11 | `0fe01ab` | **Circuit Breaker (`Closed/Open/Half-Open`) + `dial_retries`**, **WASM L7 роутинг (`route_pools`) и in-place мутация пакета**, **опциональная TLS-терминация (`listeners[].tls`)**, расширенные метрики Prometheus | Полный продакшн-контур ядра с `0 allocs/op` на fast path |

**Текущий статус (на 06.11):** M0 ✅, M1 ✅, M2 ✅, **M3 ✅**, **M4 (F2 полный) ✅**, **M5 (WASM + L7-lite + TLS/SNI + circuit breaker) ✅**; M6 (C# Control Plane) — следующий шаг.
Все пакеты (`config`, `observe`, `plugin`, `proxy`, `ratelimit`, `tlspeek`) проходят
`go vet`, `go test ./...`; на Ubuntu — `go test -race ./...`; `golangci-lint v2.15.0` — 0 issues.

---

## 2. Постановка задач (по этапам)

Порядок из `05-roadmap-thesis.md`: S1 skeleton → S2 proxy core → S3 config/reload/drain
→ S4 observe + hardening → **S5 F2 (rate limiting соединений)** → S6 WASM → S7 Control Plane.

Задачи, которые решались по ходу:

1. **Принять соединения и проксировать TCP** (M0–M1): accept-loop на `net`, горутина на соединение.
2. **Балансировка** round-robin и least-conn с учётом активных соединений.
3. **Конфиг и reload без разрыва** (M2): YAML с проверкой, атомарный snapshot, reconcile слушателей, drain.
4. **Ограничение ресурсов**: `max_conns`, `dial_timeout`, `idle_timeout`.
5. **Наблюдаемость** (M3): метрики, логи с ограничением частоты, pprof.
6. **Качество**: race-тесты, линтер, CI, бенчмарки с `benchstat`-совместимым выводом.
7. Для диплома: зафиксировать **baseline** (M3) до внедрения фич — ✅ снят на Linux (S0–S5, `benchmarks/linux-ubuntu-results.txt`).

---

## 3. Проблемы и решения

### P1. Нет `splice(2)` на Windows: копирование дорогое по аллокациям
- **Что:** на Linux `io.Copy(dst, src)` через `TCPConn.ReadFrom` уходит в `splice(2)`:
  ядро переносит байты между сокетами, буферов в user-space нет.
  На Windows/macOS `ReadFrom` → `genericReadFrom` → `io.copyBuffer`, и на **каждое
  направление каждого соединения** выделяется новый буфер 32 KiB (~64 KiB мусора на соединение).
- **Замер:** `ConnectionSetup/through_proxy` на Windows — **102.9 KB/op** при голом `io.Copy`.
- **Решение (ADR-7):** на не-Linux plain-путь отдаёт стандартному `io.CopyBuffer` один буфер
  из `sync.Pool` (`copyPlain`). Выбор платформы — одна проверка `runtime.GOOS` без build-тегов.
  Итог: **38.5 KB/op** на Windows, на Linux без изменений (0 буферов копирования).
- **Отвергнутые варианты** (для главы «Проектирование»):
  - build-теги `copy_linux.go`/`copy_other.go` — два файла ради одной ветки;
  - свой цикл копирования вместо `io.CopyBuffer` — дубликат семантики stdlib (EOF, `ErrShortWrite`), ради ~100 Б на соединение;
  - голый `io.Copy` везде — платит каждый, кто запускает на ноутбуке.
- **Важно для защиты:** нельзя спрятать `TCPConn.ReadFrom` обёрткой на Linux — тогда пропадёт путь к `splice`. Поэтому проверка платформы, а не обёртка.

### P2. Zero-copy нельзя измерить на Windows
- **Что:** на Windows варианты `zero_copy` и `idle_watchdog` дают почти одинаковые числа
  (оба копируют в user-space). Значит, Windows-замеры **не показывают** эффект zero-copy.
- **Замер на WSL2** (ядро 6.6.87, 4 vCPU): `Throughput/zero_copy/64KiB` **557–725 MB/s**
  против `idle_watchdog` 257–286 MB/s (в 2.1–2.7 раза); на 1 MiB — 3.3–3.7 раза.
- **Ограничение:** WSL2 — виртуальная машина, NIC и ФС виртуальные. Относительные числа
  валидны, абсолютные нет. Для цифр в диплом нужен bare metal, VM или CI (`ubuntu-latest`).
- **Решение:** в тексте сравнивать **отношения** (zero-copy / buffered), а не абсолюты;
  в диплом выносить Linux-прогон, Windows — как «замер окружения разработки».

### P3. Idle-таймаут резал односторонний поток (баг, исправлен в `393513e`)
- **Что:** таймаут обновлялся только для направления, из которого идёт чтение. Клиент молчит,
  пока сервер качает данные (медленное скачивание, SSE) — направление «клиент→сервер» не
  обновлялось и соединение обрывалось.
- **Воспроизведение:** тест `TestIdleTimeoutKeepsOneWayStreamAlive`: на старом коде поток
  обрывался после **6 из 20** байт.
- **Решение:** `activityReader` обновляет дедлайн на **обоих** концах перед каждым чтением.
  Соединение умирает только если **оба** направления молчат весь период.
- **Цена:** при `idle_timeout > 0` включается буферизованный путь без `splice`. Поэтому
  по умолчанию `idle_timeout: 0`, и бенчмарки снимаются при 0.

### P4. Аллокации буферов на буферизованном пути
- **Что:** `io.Copy` выделял буфер на каждое направление. Решено пулом (`bufferPool`, 32 KiB,
  совпадает с размером `io.Copy`).

### P5. Гонки при reload и потеря счётчиков (исправлено в `a78ef26`, `4f5b03e`)
- **Гонка версий:** инкремент версии при двух одновременных reload дал бы одинаковую версию.
  Решено мьютексом `Runtime.mu`, который берётся только на reload (не на data path).
- **Потеря состояния:** если хранить `max_conns` и счётчики `active` внутри snapshot,
  каждый reload обнулял бы их. N reload пропускали бы N лимитов. Решено выносом состояния
  в `registry` (`state.go`), которое переживает snapshot. Атомики с CAS, без мьютекса на горячем пути.
- **Ошибка `WaitGroup`:** `Shutdown` сначала ждёт accept-loop'ы и только потом `connWG`,
  иначе `Add` мог бы гоняться с `Wait` (неопределённое поведение).

### P6. Откат reload при занятом порту
- **Что:** новые сокеты биндятся **до** публикации snapshot. Если bind падает, уже открытые
  сокеты закрываются, версия не меняется, прежняя конфигурация остаётся. Тест покрывает отказ.
- **Ограничение:** переименование листенера без смены адреса сначала не укладывалось в этот механизм (B1). Исправлено: старый сокет освобождается до bind, при откате восстанавливается. Порт `0` в этой логике не конфликтует (см. §6).

### P7. Лог-шторм при флуде
- **Что:** каждое отклонённое соединение и каждый failed dial писали бы строку лога.
- **Решение:** `observe.Limiter`, ключ = листенер или адрес, окно 1 с, до 2 строк. В строке
  пишется `suppressed=N`.
- **Замер:** `LimiterAllowParallel` ~116 ns независимо от числа ключей. Узкое место — один
  общий `sync.Mutex`. Принято сознательно: под флудом это дешевле записи в лог.

### P8. Бенчмарки давали контринтуитивные числа
Разобраны в `benchmarks/README.md`, раздел «Почему цифры такие». Для диплома важны:
- `ConnLimiter/at_limit` (1.2 ns) дешевле `below_limit` (15 ns): отказ — это один `Load`, приём — CAS.
- `least_conn` O(n) на 64 серверах — 73 ns, но только чтение, масштабируется. На 1000 серверов ~1 µs (heap — future work).
- `ThroughputScaling/conns=64` шумит ±20 %. Профиль показал 94 % в `runtime.cgocall` (сетевые syscalls на Windows), а не мьютекс.
- `Load` конфига 223 µs — YAML с `KnownFields`. Допустимо, потому что вызывается на SIGHUP, а не на запрос.

**Урок:** парный `BenchmarkXxxParallel` обязателен для всего, что шарят горутины. Одиночный бенч не создаёт конкуренции.

### P9. Race-тесты на Windows требуют CGO
- **Что:** `go test -race` требует CGO и C-компилятор, на Windows по умолчанию их нет.
- **Решение:** CI гоняет `-race` на Linux (CGO бесплатно), на Windows — обычный набор тестов.
  Это совпадает с целевой платформой.

### P10. Линтер: миграция v1 → v2
- **Что:** action `golangci-lint-action@v7` требует линтер v2, а конфиг был v1.
- **Решение:** `417f5a0` (исправления по замечаниям CI), `c4cebac` (схема v2). `noctx` исключён
  только в тестах: loopback-фикстуры используют `net.Listen` идиоматично, в коде — `ListenConfig`.

### P11. SIGHUP отсутствует на Windows
- **Что:** на Windows нет SIGHUP, reload не привязан к сигналу. Документировано в `DEVELOPMENT.md` §13.
- **Решение:** принято как ограничение платформы; для продакшена reload должен идти через CP (S7).

### P12. Как сделать WASM-песочницу без просадки по памяти и скорости
- **Проблема наивной реализации:**
  1. Инстанциация модуля (`InstantiateModule`) на каждое соединение выделяет новую линейную память WASM (минимум 1 страница = **64 KiB** на соединение).
  2. Вызов `api.Function.Call(ctx, params...)` в `wazero` выделяет variadic-слайс аргументов и слайс результатов в heap на каждый хук.
  3. Передача IP строкой в линейную память WASM требует `malloc`/`free` внутри гостя и копирования байт.
- **Решение (`internal/plugin/sandbox.go`):**
  1. **Reactor-пул инстансов (`sync.Pool`):** модуль компилируется один раз при загрузке/reload (в AOT машинный код на `amd64`/`arm64`), инстансы переиспользуются между соединениями и кэшируются в `registry`, если конфиг плагина при reload не менялся.
  2. **`CallWithStack` по преаллоцированному буферу `[4]uint64` внутри инстанса:** канонический 16-байтный IP (IPv4-mapped IPv6 или IPv6) передаётся как два `uint64` (`ip_hi`, `ip_lo`) плюс порт в регистрах стека вызова — **0 B/op, 0 allocs/op**.
  3. **Изоляция и лимиты:** гостю не выдаются WASI-права (нет ФС, сокетов, env, системных часов); память жёстко ограничена `WithMemoryLimitPages` (по умолчанию 16 страниц = 1 MiB); при заданном `timeout` включается `WithCloseOnContextDone(true)`, а упавший по trap/таймауту инстанс закрывается и не возвращается в пул.
- **Замер:** `BenchmarkOnAccept` = **81.3 ns/op, 0 B/op, 0 allocs/op**; `BenchmarkOnAcceptParallel` = **24.7 ns/op, 0 B/op, 0 allocs/op** (Linux, Ryzen 5 3550H).

### P13. Как сделать TLS SNI-peek, не сломав zero-copy `splice(2)` на Linux
- **Проблема:** чтобы прочитать `ServerName` из TLS `ClientHello`, нужно вычитать первые ~300 байт из клиентского сокета до выбора апстрима. Если обернуть сокет в обычный `bufio.Reader` или `io.MultiReader`, он перестанет быть `*net.TCPConn`, и `io.Copy` на Linux навсегда потеряет `splice(2)` для всего остального TLS-соединения.
- **Решение (`internal/tlspeek/sni.go` + `internal/proxy/copy.go`):**
  1. `tlspeek.Peek` читает первую TLS-запись в буфер из `sync.Pool`, разбирает `ClientHello` без аллокаций (`ParseClientHelloSNI`: **26.8 ns/op, 0 B/op, 0 allocs/op**) и возвращает `*tlspeek.Conn`.
  2. В `copyPlain` перед запуском `io.Copy` вызывается `pc.DrainTo(dst)`: вычитанный префикс `ClientHello` сбрасывается в апстрим, буфер сразу возвращается в `sync.Pool`, а наружу отдаётся исходный `*net.TCPConn` — дальше на Linux снова работает ядерный `splice(2)`.

### P14. Как дать WASM-плагину свободный доступ к L7-пакету (`on_data`), не убив 0-alloc и `splice(2)`
- **Проблема:**
  1. Если вызывать WASM на каждый пакет соединения, на Linux пропадает `splice(2)` для всего потока, хотя в 90 % задач (HTTP-метод/URL, заголовки, JWT, JSON RPC) нужно проверить только первый запрос клиента.
  2. Если удерживать WASM-инстанс из `sync.Pool` на время `conn.Read()`, медленные клиенты (Slowloris) вычерпают все инстансы.
- **Решение (`internal/plugin/sandbox.go`, `internal/proxy/proxy.go`, `internal/proxy/copy.go`):**
  1. В линейной памяти каждого инстанса резервируется scratch-буфер 32 KiB (по смещению `32768` либо `on_data_ptr()`). Вызов `OnData` копирует прочитанный чанк в память инстанса и вызывает `on_data(ptr, len) -> verdict` через `CallWithStack` за **76 ns/op, 0 B/op, 0 allocs/op**, сразу возвращая инстанс в пул. Максимум инспекции/мутации — 32 KiB; in-place изменения копируются обратно в payload.
  2. По умолчанию (`data_mode: "once"`) проверяется первый пакет клиента **до** `dial` к апстриму (через `tlspeek.PeekChunk`, разделяющий буфер с `sni_routes`), а после `VerdictAllow` `DrainTo` сбрасывает префикс и возвращает сокет на ядерный `splice(2)`.
  3. Вердикт `>= 2` выбирает пул из `plugin.route_pools` (`ret-2`) — L7-роутинг/canary. Это работает только в `once`: в `stream` пул уже выбран до `dial`, поэтому `>= 2` трактуется как allow.
  4. Для протоколов с проверкой каждого фрейма предусмотрен `data_mode: "stream"` (`copyInspected` в `copy.go`), также работающий с `0 allocs/op` поверх `bufferPool`.

### P15. Circuit breaker и `dial_retries`: отказоустойчивость без блокировок
- **Проблема:** при падении одного бэкенда `round_robin` продолжает отправлять на него долю соединений; пассивной отметки `unhealthy` не хватает, если сервер «мигает». Нужен автоматический размыкатель и повторный `dial` на другой сервер.
- **Решение (`internal/proxy/upstream.go`, `pool.go`, `proxy.go`):**
  1. На каждый upstream — атомарные `consecFails`, `openUntilNano`, `halfOpenLeft`; состояния `Closed → Open → Half-Open` без мьютексов.
  2. `error_threshold` задаёт число подряд ошибок `dial` до `Open`; `open_duration` — время до пробного запроса; `half_open_requests` — число успехов для возврата в `Closed`.
  3. `dial_retries` — дополнительные попытки `dial` по пулу (всего `1 + N`); неудачная попытка пассивно помечает сервер и освобождает слот.
- **Ограничение:** балансировщик может выбрать тот же сервер повторно; гарантии «другой сервер» нет. При `error_threshold: 0` размыкание происходит после первой ошибки.
- **Замер:** на горячем пути — только атомарные чтения/записи; отдельных аллокаций нет.

### P16. TLS-терминация и её цена для zero-copy
- **Что:** `listeners[].tls` оборачивает клиентский сокет в `*tls.Conn`, поэтому `io.Copy` больше не видит `*net.TCPConn` и Linux `splice(2)` отключается для всего соединения. Зато WASM `on_data` получает открытый текст.
- **Решение:** терминация — opt-in; по умолчанию работает passthrough через `tlspeek` (SNI peek + replay), который возвращает исходный сокет после первого пакета. `tls.Config` пересобирается при reload, сертификат перечитывается; невалидная пара не публикуется.
- **Ограничение:** `sni_routes` на листенере с `tls` игнорируется — SNI уже не нужен для passthrough-маршрутизации.

### P17. L7-роутинг и mutation: `route_pools` только в `once`
- **Что:** `on_data` возвращает `0` (allow), `1` (deny) или `>= 2` (пул `route_pools[ret-2]`). В режиме `stream` пул уже выбран до `dial`, поэтому вердикт `>= 2` не может сменить бэкенд и трактуется как allow. В `once` первый пакет читается до `dial`, поэтому выбор пула корректен.
- **Mutation:** плагин меняет байты в `[ptr, ptr+len)`; хост копирует изменённый префикс обратно в payload до `DrainTo`/отправки. Ограничение — 32 KiB scratch.
- **Решение:** документировать это как явное ограничение, а не молчаливое поведение; в `stream` использовать `on_data` только для allow/deny.

---

## 4. Ключевые архитектурные решения (для главы «Проектирование»)

| Решение | Альтернатива | Почему |
|---------|--------------|--------|
| `net` + горутина на соединение (ADR-1) | `gnet`, event-loop | `splice` и backpressure бесплатно; `gnet` — только по замерам |
| Snapshot + `atomic.Pointer` (ADR-3) | двойной буфер с мьютексом | lock-free чтение на каждом соединении |
| Состояние в `registry`, а не в snapshot | счётчики внутри snapshot | reload не сбрасывает лимиты и `active` |
| Копирование: `splice` на Linux, пул буферов иначе (ADR-7) | build-теги, свой цикл, голый `io.Copy` | см. P1 |
| Метрики — атомики на соединение, рендер на cold path | метрики на каждый чанк | ~60 ns и 0 аллокаций на соединение |
| WASM ABI без WASI: `CallWithStack` + scratch (ADR-8) | WASI + host functions | 0 allocs/op и узкая поверхность атаки; см. P12/P14 |
| TLS passthrough по умолчанию, терминация opt-in (ADR-9) | всегда терминация | сохраняет `splice` и end-to-end TLS; см. P16 |
| Circuit breaker на атомиках в `upstream` | мьютекс/внешний балансировщик | 0 блокировок на hot path, состояние переживает reload; см. P15 |

---

## 5. Результаты для главы «Эксперименты» (сняты на Ubuntu 24.04 Linux)

Все гипотезы `H1–H4` из `01-problem.md` проверены замерами на Linux (`go1.27.2 linux/amd64`, ядро `6.6.87`, `nginx 1.24.0`, `HAProxy 2.8.30`):

- **H1 (оверхед WASM-песочницы):** `BenchmarkOnAccept` = **81.3 ns/op** (`24.7 ns/op` параллельно), `BenchmarkOnData` (L7) = **76.0 ns/op**, **0 B/op, 0 allocs/op**. На тёплом соединении (`RelayLatency`) `wasmgate_full` (61.1 µs) неотличим от `wasmgate_l4` (62.0 µs); при открытии соединения количество аллокаций не увеличивается вовсе (`65 allocs/op` против `65 allocs/op`).
- **H2 (Zero-Downtime Hot Reload):** `TestReloadUnderLoadZeroDroppedConnections` выполняет 12 переключений конфигурации и WASM-плагинов под непрерывным потоком долгоживущих и коротких TCP-соединений — **0 разорванных соединений**.
- **H3 (скользящее окно F2 в fast path):** `BenchmarkAllow` = **65.4 ns/op** (`20.4 ns/op` параллельно), **0 B/op, 0 allocs/op**.
- **H4 (сравнение с `nginx stream` и `HAProxy TCP`):**
  - `Throughput/64KiB`: `wasmgate_l4` **588.8 MB/s** / `wasmgate_full` **734.1 MB/s** против `nginx_stream` **272.5 MB/s** и `haproxy_tcp` **268.9 MB/s**;
  - `Throughput/1MiB`: `wasmgate_l4` **688.3 MB/s** / `wasmgate_full` **794.9 MB/s** против `nginx_stream` **230.9 MB/s** и `haproxy_tcp` **248.3 MB/s** (эффект ядерного `splice(2)` против user-space буферов);
  - `ConnectionSetup`: `wasmgate_l4` **917.7 µs** быстрее `nginx_stream` (**1 076.9 µs**) и `haproxy_tcp` (**1 163.8 µs**).

> Числа сняты на коммите `18d4249`, до батча `0fe01ab` (circuit breaker/TLS).
> `wasmgate_full` включает `max_conns` + `rate_limit` + `ipguard.wasm`, но **не**
> TLS/circuit breaker. Это важно оговаривать в тексте ВКР.

### 5.1. Дословный вывод консоли Ubuntu 24.04 (для вставки в Приложение ВКР)

Сырой лог также сохранён в [`benchmarks/linux-ubuntu-results.txt`](../benchmarks/linux-ubuntu-results.txt):

```text
$ uname -a
Linux FridTop 6.6.87.2-microsoft-standard-WSL2 #1 SMP PREEMPT_DYNAMIC Thu Jun  5 18:30:46 UTC 2025 x86_64 x86_64 x86_64 GNU/Linux
$ go version
go version go1.27.2 linux/amd64
$ nginx -v
nginx version: nginx/1.24.0 (Ubuntu)
$ haproxy -v
HAProxy version 2.8.30-0ubuntu0.24.04.2 2026/10/27 - https://haproxy.org/

$ go test -race -count=1 ./...
?   	wasmgate/cmd/wasmgate	[no test files]
ok  	wasmgate/internal/config	1.109s
ok  	wasmgate/internal/observe	1.050s
ok  	wasmgate/internal/plugin	1.069s
ok  	wasmgate/internal/proxy	2.939s
ok  	wasmgate/internal/ratelimit	3.387s
ok  	wasmgate/internal/tlspeek	1.033s

$ go test -race -count=1 -v ./internal/proxy -run TestRealProxiesComparison
=== RUN   TestRealProxiesComparison
=== RUN   TestRealProxiesComparison/wasmgate_l4
=== RUN   TestRealProxiesComparison/wasmgate_full
=== RUN   TestRealProxiesComparison/nginx_stream
=== RUN   TestRealProxiesComparison/haproxy_tcp
--- PASS: TestRealProxiesComparison (0.13s)
    --- PASS: TestRealProxiesComparison/wasmgate_l4 (0.00s)
    --- PASS: TestRealProxiesComparison/wasmgate_full (0.01s)
    --- PASS: TestRealProxiesComparison/nginx_stream (0.02s)
    --- PASS: TestRealProxiesComparison/haproxy_tcp (0.10s)
PASS
ok  	wasmgate/internal/proxy	1.161s

$ go test -race -count=1 -v ./internal/plugin
=== RUN   TestPluginAllowAndDenyByIP
--- PASS: TestPluginAllowAndDenyByIP (0.00s)
=== RUN   TestPluginMemoryLimitRejectsOversizedModule
--- PASS: TestPluginMemoryLimitRejectsOversizedModule (0.00s)
=== RUN   TestPluginTimeoutStopsInfiniteLoop
--- PASS: TestPluginTimeoutStopsInfiniteLoop (0.02s)
=== RUN   TestPluginRejectsBadSignature
--- PASS: TestPluginRejectsBadSignature (0.00s)
=== RUN   TestLoadExampleIPGuardPlugin
--- PASS: TestLoadExampleIPGuardPlugin (0.00s)
=== RUN   TestPluginOnDataInspectsPacketBytes
--- PASS: TestPluginOnDataInspectsPacketBytes (0.00s)
PASS
ok  	wasmgate/internal/plugin	1.066s

$ go test -race -count=1 -v ./internal/proxy -run TestProxy
=== RUN   TestProxyForwardsBytes
--- PASS: TestProxyForwardsBytes (0.00s)
=== RUN   TestProxyClosesConnectionWhenUpstreamIsDown
--- PASS: TestProxyClosesConnectionWhenUpstreamIsDown (0.00s)
=== RUN   TestProxyMetricsCountRejections
--- PASS: TestProxyMetricsCountRejections (0.00s)
=== RUN   TestProxyMetricsCountDialFailures
--- PASS: TestProxyMetricsCountDialFailures (0.00s)
=== RUN   TestProxyRateLimitsConnections
--- PASS: TestProxyRateLimitsConnections (0.01s)
=== RUN   TestProxyWasmPluginRejectsAndAllows
--- PASS: TestProxyWasmPluginRejectsAndAllows (0.01s)
=== RUN   TestProxySNIRoutesToMatchingPoolAndReplaysClientHello
--- PASS: TestProxySNIRoutesToMatchingPoolAndReplaysClientHello (0.01s)
=== RUN   TestProxyWasmOnDataInspectsL7OnceAndStream
--- PASS: TestProxyWasmOnDataInspectsL7OnceAndStream (0.01s)
=== RUN   TestProxyExampleIPGuardWasmBlocks127_0_0_99
--- PASS: TestProxyExampleIPGuardWasmBlocks127_0_0_99 (0.01s)
PASS
ok  	wasmgate/internal/proxy	1.085s

$ go test -run "^$" -bench BenchmarkProxyComparison -benchmem ./internal/proxy
goos: linux
goarch: amd64
pkg: wasmgate/internal/proxy
cpu: AMD Ryzen 5 3550H with Radeon Vega Mobile Gfx  
BenchmarkProxyComparison/Throughput/64KiB/direct-4         	   12596	    101664 ns/op	 644.64 MB/s	       2 B/op	       0 allocs/op
BenchmarkProxyComparison/Throughput/64KiB/wasmgate_l4-4    	   10000	    111306 ns/op	 588.79 MB/s	      10 B/op	       0 allocs/op
BenchmarkProxyComparison/Throughput/64KiB/wasmgate_full-4  	   12163	     89272 ns/op	 734.12 MB/s	      11 B/op	       0 allocs/op
BenchmarkProxyComparison/Throughput/64KiB/nginx_stream-4   	    4760	    240516 ns/op	 272.48 MB/s	      13 B/op	       0 allocs/op
BenchmarkProxyComparison/Throughput/64KiB/haproxy_tcp-4    	    5503	    243710 ns/op	 268.91 MB/s	      23 B/op	       0 allocs/op
BenchmarkProxyComparison/Throughput/1MiB/direct-4          	    1063	   1211439 ns/op	 865.56 MB/s	       0 B/op	       0 allocs/op
BenchmarkProxyComparison/Throughput/1MiB/wasmgate_l4-4     	     936	   1523550 ns/op	 688.25 MB/s	      43 B/op	       0 allocs/op
BenchmarkProxyComparison/Throughput/1MiB/wasmgate_full-4   	     879	   1319181 ns/op	 794.87 MB/s	      40 B/op	       0 allocs/op
BenchmarkProxyComparison/Throughput/1MiB/nginx_stream-4    	     386	   4541468 ns/op	 230.89 MB/s	       0 B/op	       0 allocs/op
BenchmarkProxyComparison/Throughput/1MiB/haproxy_tcp-4     	     325	   4223477 ns/op	 248.27 MB/s	       0 B/op	       0 allocs/op
BenchmarkProxyComparison/RelayLatency/direct-4             	   41700	     29223 ns/op	       0 B/op	       0 allocs/op
BenchmarkProxyComparison/RelayLatency/wasmgate_l4-4        	   18120	     61983 ns/op	       2 B/op	       0 allocs/op
BenchmarkProxyComparison/RelayLatency/wasmgate_full-4      	   19306	     61120 ns/op	       3 B/op	       0 allocs/op
BenchmarkProxyComparison/RelayLatency/nginx_stream-4       	    3142	    393999 ns/op	      20 B/op	       0 allocs/op
BenchmarkProxyComparison/RelayLatency/haproxy_tcp-4        	    2796	    439174 ns/op	      23 B/op	       0 allocs/op
PASS
ok  	wasmgate/internal/proxy	25.633s

$ go test -run "^$" -bench BenchmarkConnectionSetup -benchmem ./internal/proxy
goos: linux
goarch: amd64
pkg: wasmgate/internal/proxy
cpu: AMD Ryzen 5 3550H with Radeon Vega Mobile Gfx  
BenchmarkConnectionSetup/direct-4         	    3027	    402884 ns/op	   33792 B/op	      23 allocs/op
BenchmarkConnectionSetup/through_proxy-4  	    1380	    917683 ns/op	   36078 B/op	      65 allocs/op
BenchmarkConnectionSetup/wasmgate_full-4  	    1219	   1420898 ns/op	   36180 B/op	      65 allocs/op
BenchmarkConnectionSetup/nginx_stream-4   	    1290	   1076898 ns/op	   33805 B/op	      23 allocs/op
BenchmarkConnectionSetup/haproxy_tcp-4    	    1390	   1163823 ns/op	   33804 B/op	      23 allocs/op
PASS
ok  	wasmgate/internal/proxy	11.164s

$ go test -run "^$" -bench . -benchmem ./internal/ratelimit ./internal/plugin ./internal/tlspeek
goos: linux
goarch: amd64
pkg: wasmgate/internal/ratelimit
cpu: AMD Ryzen 5 3550H with Radeon Vega Mobile Gfx  
BenchmarkAllow-4           	19714239	        65.43 ns/op	       0 B/op	       0 allocs/op
BenchmarkAllowParallel-4   	65511637	        20.39 ns/op	       0 B/op	       0 allocs/op
PASS
ok  	wasmgate/internal/ratelimit	2.726s
goos: linux
goarch: amd64
pkg: wasmgate/internal/plugin
cpu: AMD Ryzen 5 3550H with Radeon Vega Mobile Gfx  
BenchmarkOnData-4             	13660726	        76.01 ns/op	       0 B/op	       0 allocs/op
BenchmarkOnAccept-4           	13226593	        81.34 ns/op	       0 B/op	       0 allocs/op
BenchmarkOnAcceptParallel-4   	48213128	        24.72 ns/op	       0 B/op	       0 allocs/op
PASS
ok  	wasmgate/internal/plugin	3.565s
goos: linux
goarch: amd64
pkg: wasmgate/internal/tlspeek
cpu: AMD Ryzen 5 3550H with Radeon Vega Mobile Gfx  
BenchmarkParseClientHelloSNI-4   	42375142	        26.02 ns/op	       0 B/op	       0 allocs/op
BenchmarkMatchRouteWildcard-4    	13836912	        93.29 ns/op	       0 B/op	       0 allocs/op
PASS
ok  	wasmgate/internal/tlspeek	2.530s
```

---

## 6. Открытые проблемы (найдены при ревью, на 06.11)

| # | Проблема | Статус | Где |
|---|----------|--------|-----|
| B1 | Переименование листенера без смены адреса: reload падал `address already in use`, хотя DEVELOPMENT.md обещает «переименован — открывает новый» | **Исправлено**: старый сокет освобождается до bind и восстанавливается при откате (`restoreReleased`), тесты `TestReloadRenameKeepsAddress`, `TestReloadRenameRollbackRestoresListener` | `server.go`, `Reload` |
| B2 | Экранирование меток Prometheus: `escapeLabel` + `%q` дают двойное экранирование (`a"b` → `a\\\"b`) | **Исправлено** 05.11, тест `TestWritePrometheusEscapesLabel` | `metrics.go` |
| B3 | `least_conn`: выбор и резервирование (`Next` + `active.Add`) не один атомарный шаг | Теоретическое окно, **не воспроизведено** (200 параллельных выборов → 100/100). Счётчики сами по себе атомарны | `pool.go`, `upstream.go` |
| B4 | `admin_addr` при reload игнорируется молча | Открыто (нужен рестарт) | `server.go`, `main.go` |
| B5 | `Start` при ошибке bind одного из листенеров оставлял уже запущенные accept-loop'ы; `started` оставался `true` | **Исправлено**: `Start` сначала биндит все сокеты и запускает accept-loop'ы только после успеха всех `Listen` | `server.go`, `Start` |
| B6 | Runtime старого `plugin.Plugin` не вызывается `Close` при замене/удалении плагина: wazero-рантайм и пул инстансов живут до сборки мусора | Открыто; нужно закрывать runtime при вытеснении из `registry` | `state.go`, `plugin.go` |
| B7 | `/metrics` вызывает `u.Healthy()`, который может перевести `Open → Half-Open`; скрейп меняет состояние breaker'а | Открыто; для метрик нужен не мутирующий снимок | `upstream.go`, `snapshot.go` |
| B8 | TLS-терминация отключает `splice` для соединения (клиентский conn становится `*tls.Conn`) | Осознанное ограничение, задокументировано; альтернатива — passthrough | `proxy.go`, `copy.go` |
| B9 | `route_pools` учитывается только в `data_mode: once`; в `stream` вердикт `>= 2` = allow | Осознанное ограничение, задокументировано | `proxy.go`, `copy.go` |
| B10 | `dial_retries` может повторно выбрать тот же сервер: гарантии «другой сервер» нет | Осознанное ограничение; при необходимости — исключать уже опробованные | `proxy.go`, `pool.go` |
| M1 | `wasmgate_relayed_bytes_total` добавляется только по завершении соединения: на долгих потоках метрика стоит на нуле, графики скорости не видны | Открыто (архитектурное ограничение, решать через периодическое добавление) | `proxy.go`, `copy.go` |
| M2 | `/metrics` выводит `wasmgate_upstream_active` для всех upstream из registry, включая удалённые из конфига, пока у них есть активные соединения | Открыто, поведение ожидаемое, но не задокументировано | `snapshot.go`, `UpstreamStats` |
| M3 | `/healthz` всегда отвечает `ok`, не проверяя состояние (health-checks upstream уже есть, но endpoint их не агрегирует) | Открыто | `admin.go` |
| M4 | Round-robin сбрасывается при каждом reload (счётчик внутри pool, а не в registry) | Открыто, по смыслу допустимо | `upstream.go` |
| M5 | `server_test` покрывает health-check/circuit breaker, но валидация новых полей TLS/circuit-breaker/`route_pools` в `config_test` неполная | Открыто (coverage gap) | `config_test.go` |

---

## 7. Заметки для текста ВКР (черновые формулировки)

- **Про Windows-копирование:** «Стандартный `io.Copy` на не-Linux платформах выделяет по 32 KiB на каждое направление каждого соединения (≈102.9 KB на соединение по замеру ConnectionSetup). Пул буферов, переданный стандартному `io.CopyBuffer`, снизил эту величину до 38.5 KB без изменения поведения и без build-тегов».
- **Про zero-copy:** «Эффект `splice(2)` подтверждён только на Linux (2.1–3.7× относительно буферизованного пути). На Windows варианты неразличимы, поэтому абсолютные значения на Windows в главе экспериментов не используются».
- **Про idle-таймаут:** «Исходная реализация таймаута обрывала односторонние потоки (6 из 20 байт в тесте). Исправлено переходом к таймауту на уровне соединения».
- **Про отличие от аналогов:** не утверждать «такого нигде нет». Формулировать как сборку механизмов в одном компактном ядре (см. `04-diploma-features.md`).
