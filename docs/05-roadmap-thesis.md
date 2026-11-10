# 05. Дорожная карта и структура ВКР

> Документ для **планирования** и **написания диплома**: этапы работ, milestones,
> регистр рисков, структура текста ВКР и артефакты защиты.

---

## 1. Дорожная карта (по спринтам)

Оценка дана в «условных неделях» (2 недели = 1 спринт). Порядок приоритетов:
сначала **baseline ядра**, затем **ядровые фичи** (F2 → F1), затем расширения.

| Спринт | Тема | Задачи | Deliverable |
|--------|------|--------|-------------|
| **S1** | Skeleton ✅ | accept-loop (`net`), логирование, zero-alloc каркас, CI + линт | Приём соединений, `-race` зелёный |
| **S2** | Proxy core ✅ | upstream pool, балансировка, copy/splice, таймауты | TCP-прокси end-to-end |
| **S3** | Config + reload + drain ✅ | YAML, `Snapshot`, `atomic.Pointer`, SIGHUP, reconcile слушателей, graceful drain | Reload без разрыва + chaos-тест |
| **S4** | Observe + hardening ✅ | метрики, логи, pprof, health-checks, CLI, `-validate`, `POST /reload`, SNI peek | Метрики/admin/pprof + baseline S0–S5 |
| **S5** | **F2 rate limiting** ✅ | sliding window, 64 шарда, per-IP/subnet, allow-list, backoff-бан | Фильтр в fast path + замеры |
| **S6** | **WASM host + L7-плагины** ✅ | wazero без WASI, ABI v1/v2/v3, `on_data`/`on_data_ex`/`on_data_state` once/stream, mutation, `route_pools`, ConnState, sandbox-лимиты, 8 плагинов | Плагин на Rust/TinyGo работает без рестарта |
| **S7** | **TLS + отказоустойчивость** ✅ | TLS passthrough/termination, SNI-routes, circuit breaker, `dial_retries` | TLS-листенеры и failover |
| **S8** | **F1 Control Plane** ⏳ | C#/.NET + gRPC/UDS, версии, валидация, rollback, аудит | Hot-reload из CP без простоя |
| **S9** | Эксперименты ✅ | прогоны S0–S5, `benchstat`, сравнение с nginx/HAProxy | Глава «Эксперименты» |
| **S10** | F3 (опц.) ⏳ | zero-alloc JSON DPI в ядре | Дифференциатор №1 (опц.) |
| **S11** | Текст + защита ⏳ | написание ВКР, слайды, демо-стенд | Документ + защита |

> **Критический путь:** S1 → S2 → S3 → S4 (baseline) → S5 (F2) → S6/S7
> (WASM/TLS) → S8 (CP). S10 (F3) и eBPF (F4) — бонус/будущие работы.

---

## 2. Milestones → критерии перехода

| Milestone | Критерий перехода (gate) |
|-----------|--------------------------|
| **M0** Skeleton | приём соединений, 0 alloc в idle, `-race` зелёный — ✅ |
| **M1** Proxy core | TCP end-to-end, нет утечек горутин — ✅ |
| **M2** Reload | 0 dropped соединений под reload (chaos-тест: 12 reload, 0 dropped) — ✅ |
| **M3** Baseline | метрики/pprof/health-checks/CLI + числа S0–S5 против nginx/HAProxy — ✅ |
| **M4** Rate limiting (F2) | купирование flood'а, 0 allocs/op в fast path (≈65 нс) — ✅ |
| **M5** WASM + L7 + TLS | плагин на Rust/TinyGo, sandbox-лимиты, `on_data`/`on_data_ex`/`on_data_state`, `route_pools`, TLS/SNI, ABI v3 — ✅ |
| **M6** Hot Reload CP (F1) | apply/rollback из C# без разрыва, < 100 мс — ⏳ plan |

---

## 3. Регистр рисков (проектный)

| Риск | P | I | Митигация |
|------|---|---|-----------|
| Не хватает времени на F3/F4 | Высокая | Среднее | Заранее вынести в «future work» |
| Движок event-loop (`gnet`) преждевременно усложняет ядро | Средняя | Среднее | Старт на `net`+`splice`; `gnet` — только по результатам замеров |
| Оверхед WASM выше ожиданий | Средняя | Высокое | Пул инстансов, `CallWithStack`, компиляция один раз, budget; измерено ≈76–81 нс на вызов |
| Reload ловит гонки на drain | Средняя | Высокое | Snapshot-per-conn, race, chaos |
| TLS-терминация/stream-плагин ломают zero-copy | Высокая | Среднее | `on_data: once` сохраняет `splice`; TLS/stream — только где нужна инспекция |
| Старый WASM-runtime не закрывается мгновенно при reload | Средняя | Низкое | `runtime.AddCleanup` закрывает runtime при сборке мусора; инстансы доживают на старом snapshot |
| Замеры «шумят» | Средняя | Среднее | pinning, netns, ≥5 прогонов, benchstat |
| Разрастание скоупа | Высокая | Среднее | Жёсткий scope + milestones |

---

## 4. Структура ВКР (рекомендуемая)

```
ВВЕДЕНИЕ
  • Актуальность, проблема (см. 01), объект/предмет, цель, задачи, новизна.

ГЛАВА 1. АНАЛИЗ ПРЕДМЕТНОЙ ОБЛАСТИ
  1.1. Прокси-шлюзы и API-gateways: назначение и эволюция.
  1.2. Обзор и сравнительный анализ аналогов (nginx, Envoy, HAProxy, Kong, Traefik).
  1.3. Технология WebAssembly как платформо-независимый рантайм расширений.
  1.4. Постановка задачи и формулировка гипотез (H1–H5), критерии успеха.

ГЛАВА 2. ПРОЕКТИРОВАНИЕ
  2.1. Архитектура решения (Data/Control Plane, plugin host).
  2.2. Выбор технологий и библиотек (Go, `net`, wazero, .NET, gRPC).   ← см. 02
  2.3. Модель конфигурации и механизм Zero-Downtime Hot Reload (RCU/Snapshot).
  2.4. Модель rate limiting соединений на скользящем окне.
  2.5. Подсистема WASM-плагинов: ABI без WASI, sandbox, контракты, L7-хуки,
       host-managed состояние ABI v3.
  2.6. TLS: passthrough (SNI peek) и терминация; взаимодействие с zero-copy.
  2.7. Соглашения разработки, кодстайл, стратегия тестирования.

ГЛАВА 3. РЕАЛИЗАЦИЯ
  3.1. MVP: L4-TCP-прокси (ядро, проксирование, балансировка, drain).   ← см. 03
  3.2. Реализация Hot Reload; C# Control Plane — план (M6).
  3.3. Реализация rate limiting соединений.
  3.4. Реализация WASM-рантайма и ABI (без host functions).
  3.5. Реализация TLS/SNI, health-checks и circuit breaker.
  3.6. Инструментарий наблюдаемости.

ГЛАВА 4. ЭКСПЕРИМЕНТАЛЬНАЯ ОЦЕНКА
  4.1. Методика замеров (стенд, инструменты, протокол, benchstat).
  4.2. Baseline-производительность (S0–S5), сравнение с nginx/HAProxy.
  4.3. Оверхед фич: WASM-плагин, rate limiting, hot reload, TLS.
  4.4. Проверка гипотез H1–H5.
  4.5. Обсуждение ограничений и воспроизводимость.

ЗАКЛЮЧЕНИЕ
  • Итоги, вклад, соответствие цели/задачам, направления развития (F3, F4).

СПИСОК ИСПОЛЬЗОВАННЫХ ИСТОЧНИКОВ

ПРИЛОЖЕНИЯ
  • A. Схемы архитектуры и ABI.
  • B. Листинги ключевого кода.
  • C. Сценарии нагрузки и результаты замеров.
  • D. Руководство по запуску и демо.
```

---

## 5. Артефакты для защиты

| Артефакт | Что показывает |
|----------|----------------|
| **Живое демо** | nginx-стиль reload vs **0 dropped** в WasmGate (график) |
| **Демо плагина** | `ipguard.wasm` (Rust/TinyGo) фильтрует по IP и переживает reload; L7 mutation/route |
| **Демо rate limiting** | Синтетическая вспышка → фильтр купирует, легитимный трафик жив |
| **Графики** | throughput, p99, allocs/conn, RSS (S0–S5) |
| **Таблица сравнения** | WasmGate vs nginx/HAProxy (Linux, дословный вывод в `benchmarks/`) |
| **Слайды** | 10–12: проблема → идея → архитектура → демо → числа → выводы |

---

## 6. Вклад и новизна (формулировка для защиты)

1. Показано, что **WASM-песочница совместима с high-performance data plane**:
   расширяемость без WASI при 0 allocs/op и ≈76–81 нс на вызов хука.
2. Предложен механизм **Zero-Downtime Hot Reload** на immutable-snapshot/RCU,
   дающий **0 разорванных соединений** при смене логики под нагрузкой.
3. Реализован **rate limiting соединений** на скользящем окне (per-IP/subnet,
   allow-list, backoff) **в fast path**: 0 аллокаций, короткая критическая
   секция шарда, ≈65 нс.
4. Реализована **L7-инспекция** через WASM `on_data`/`on_data_ex`/
   `on_data_state`: inspect/mutate/route первого пакета с сохранением `splice`,
   состояние между чанками и keep-alive, плюс TLS passthrough/termination.
5. Собран **open baseline** и методика воспроизводимых замеров, позволившая
   сравнить решение с индустриальными аналогами.

---

## 7. Быстрый чек-лист автора

- [x] Проблема и гипотезы — `01-problem.md`.
- [x] Архитектура/стек/кодстайл/ADR — `02-architecture-tech.md`.
- [x] MVP + методика замеров — `03-mvp.md`.
- [x] Выбор ядровых фич + обоснование — `04-diploma-features.md`.
- [x] План/структура ВКР — `05-roadmap-thesis.md` (этот файл).
- [x] Журнал разработки (шаги, задачи, проблемы, решения) — `06-dev-journal.md`.
- [x] Рабочий код: M0–M5 + ABI v3 (L4-прокси, config, reload, drain,
  метрики/admin/pprof, rate-limit, WASM, L7, TLS/SNI, health-checks, circuit
  breaker, 8 showcase-плагинов) — см. [`DEVELOPMENT.md`](../DEVELOPMENT.md).
- [x] Внешний baseline S0–S5 снят и задокументирован (M3).
- [x] Демо reload / plugin / rate limiting готовы (M5).
- [ ] C# Control Plane (M6) и защита ВКР.
