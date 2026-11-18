# Control plane

Control plane — опциональный сервис на .NET 10, который превращает набор
WasmGate-узлов в управляемый парк. Он хранит желаемое состояние в PostgreSQL,
выпускает сертификаты агентам, подписывает релизы и присылает их по
двунаправленной gRPC-сессии. Он **не на пути трафика**: если control plane
недоступен, агенты продолжают работать с последней применённой конфигурацией.

```
оператор ──HTTP──► control plane :9097 (admin API + UI)
                          │
                          ├── PostgreSQL (состояние, аудит)
                          └── /data (CA, signing key, артефакты)
агент ─────gRPC :9096─────┘   Session / Enroll / Renew / Download
```

## Что где хранится

| Хранилище | Содержимое |
|---|---|
| PostgreSQL | агенты, сертификаты, токены enrollment, версии конфигурации, метаданные артефактов, релизы, деплойменты, волны, аудит |
| `/data/artifacts` | байты артефактов по SHA-256 |
| `/data/ca` | сертификат и ключ CA агентов |
| `/data/keys` | ключ подписи релизов (ECDSA P-256) и публичный ключ |
| Память процесса | живые сессии агентов; второй инстанс control plane их не увидит |

## Быстрый старт

С Docker (TLS + admin token + PostgreSQL; см. [развёртывание](deployment.md)):

```bash
./deployments/scripts/dev-certs.sh
cp deployments/scenarios/03-control-plane/.env.example \
   deployments/scenarios/03-control-plane/.env
# отредактируйте .env и задайте WASMGATE_CP_ADMIN_TOKEN
docker compose -f deployments/scenarios/03-control-plane/compose.yml up -d --build
```

Вручную:

```bash
# PostgreSQL (порт 6767 на хосте, чтобы не пересечься с локальным 5432)
docker run --rm -e POSTGRES_USER=wasmgate -e POSTGRES_PASSWORD=wasmgate \
  -e POSTGRES_DB=wasmgate -p 127.0.0.1:6767:5432 postgres:17
for f in control-plane/migrations/00*.sql; do
  psql "postgresql://wasmgate:wasmgate@127.0.0.1:6767/wasmgate" -f "$f"
done

cd control-plane
dotnet build WasmGate.Control.sln
dotnet run --project src/WasmGate.Control.Api
```

```bash
curl -fsS http://127.0.0.1:9097/healthz   # liveness
curl -fsS http://127.0.0.1:9097/readyz    # PostgreSQL + схема
```

## Админ-панель

Откройте `http://127.0.0.1:9097/` и вставьте admin-токен. Это статический SPA,
который отдаёт тот же процесс: агенты и их сертификаты, загрузка артефактов,
версии конфигурации, релизы, деплойменты с откатом и аудит. Токен хранится в
`sessionStorage`. Переключатель среды в верхней панели меняет данные на всех
экранах и позволяет создать новую среду.

## Среды

Среда — это жёсткая граница деплоя, а не метка. Каждый агент принадлежит ровно
одной среде, и деплоймент может попасть только в агентов своей среды; селекторы
волн лишь сужают этот набор. Конфигурации, релизы и деплойменты тоже разделены
по средам.

- `prod` и `staging` создаются миграцией. Новые среды добавляются через панель
  или `POST /api/v1/environments`; имя, впервые использованное конфигурацией,
  релизом или агентом, регистрируется автоматически.
- Среда агента выбирается при создании или при enroll по registration token.
  Оператор может перевести существующего агента в
  **Agents → Details → Change**; текущий релиз агент сохраняет до следующего
  деплоймента в новой среде.
- Одноразовый токен агента наследует его среду. У общего registration token
  своя среда; enroll с токеном из другой среды отклоняется. Registration token
  не может зарегистрировать существующего агента; для повторного enroll нужен
  персональный токен агента.
- Запускайте управляемых агентов с `-cp-environment <name>`: агент отклоняет
  подписанный релиз из другой среды до парсинга и применения.
- При обновлении существующие агенты попадают в `prod`.
- A deployment whose selectors match no agent in its environment is recorded as
  `Failed`, with a message explaining which of the three cases happened: no
  agents in the environment, no selector match, or no live session.
- Среду можно удалить из переключателя. `prod` защищена. Если в среде есть
  агенты или запущенный деплоймент, удаление отклоняется; иначе вместе со средой
  удаляются её конфигурации, релизы, деплойменты и enrollment-токены.

Откат деплоит релиз, предшествующий текущему в той же среде; отозванные
релизы пропускаются. API возвращает `rollbackReleaseId` для каждого
деплоймента; панель делает кнопку неактивной, если он равен null.

## Детали деплоймента и логи

В списке деплойментов видна краткая причина падения. **Details** открывает
полное сообщение, состояния волн, цель отката и события аудита этого
деплоймента. Поле `message` заполняется, когда не удалось отправить команду,
когда агент сообщил об ошибке применения (включая ошибки листенеров) и когда
деплоймент закрыл reaper по таймауту.

Панель показывает действия и результаты, а не процессный лог. Логи движка —
в journald:

```bash
sudo journalctl -u wasmgate-control -n 200 --no-pager   # control plane
sudo journalctl -u wasmgate -n 200 --no-pager           # data plane
```

## Admin API

| Метод | Путь | Назначение |
|---|---|---|
| POST | `/api/v1/agents` | Создать агента и вернуть одноразовый токен |
| GET | `/api/v1/agents` | Список агентов: среда, статус, метки, последняя связь, активный релиз |
| POST | `/api/v1/agents/{id}/tokens` | Перевыпустить токен (потерян токен, повторный enroll) |
| POST | `/api/v1/agents/{id}/environment` | Перевести агента в другую среду |
| DELETE | `/api/v1/agents/{id}` | Удалить агента, его сертификаты и токены |
| GET | `/api/v1/agents/{id}/certificates` | Активные сертификаты (serial, отпечаток, срок) |
| POST | `/api/v1/agents/{id}/certificates/{serial}/revoke` | Отозвать сертификат |
| GET | `/api/v1/environments` | Список сред для переключателя |
| POST | `/api/v1/environments` | Создать среду (идемпотентно) |
| DELETE | `/api/v1/environments/{name}` | Удалить среду и её данные |
| POST | `/api/v1/registration-tokens` | Общий токен на раскатку (`namePrefix`, `environment`, `maxUses`, `ttlSeconds`) |
| POST | `/api/v1/artifacts` | Загрузить артефакт (base64, ≤64 MiB) |
| GET | `/api/v1/artifacts` | Список артефактов |
| POST | `/api/v1/configs` | Сохранить неизменяемую версию конфигурации |
| GET | `/api/v1/configs/{environment}` | Список версий |
| POST | `/api/v1/releases` | Проверить и подписать релиз |
| GET | `/api/v1/releases/{environment}` | Список релизов |
| POST | `/api/v1/releases/{id}/revoke` | Отозвать релиз (нельзя деплоить) |
| POST | `/api/v1/deployments` | Задеплоить релиз по селекторам волн |
| GET | `/api/v1/deployments/{environment}` | Список деплойментов с целью отката и причиной падения |
| POST | `/api/v1/deployments/{id}/rollback` | Задеплоить предыдущий релиз |
| GET | `/api/v1/audit` | Последние 100 событий аудита |
| GET | `/metrics` | Prometheus scrape (тот же токен, что и `/api/v1/*`) |
| GET | `/healthz`, `/readyz` | Пробы (без токена) |

Пользовательских аккаунтов нет. Bearer-токен из `WASMGATE_CP_ADMIN_TOKEN`
обязателен, когда admin endpoint слушает не loopback. Он же защищает
`/metrics`. См. [метрики](metrics.md) — справочник метрик и готовый стек
Prometheus + Grafana.

## Агенты и сертификаты

1. **Создание** агента (`Pending`) в среде или общего registration token,
   привязанного к среде.
2. **Enroll**: агент генерирует RSA-ключ и CSR с `CN=wasmgate-agent:<guid>` и
   вызывает `AgentEnrollment.Enroll` с токеном. Токен атомарно списывается до
   выпуска сертификата.
3. **Session**: агент открывает `AgentControl.Session`; CN должен совпадать с
   `Hello.agent_id`, а отпечаток сертификата должен быть активен.
4. **Renew**: до истечения агент вызывает `AgentEnrollment.Renew` по mTLS; новый
   ключ и сертификат заменяют старые. Токен не нужен, отозванный сертификат
   продлить нельзя.
5. **Отзыв** сертификата: следующая сессия будет отклонена.

Срок жизни сертификата по умолчанию 30 дней (`WASMGATE_CP_AGENT_CERT_TTL`),
продление — за 7 дней до истечения (`-cp-renew-before` на агенте). Файлы CA
создаются при первом старте; их потеря инвалидирует все сертификаты.

Операторские метки главнее: метки из `Hello` только дополняют их, поэтому агент
не может сам перейти в другую волну.

Чтобы удалить агента, используйте **Agents → Details → Delete agent** (или
`DELETE /api/v1/agents/{id}`). Удаляются запись, сертификаты и enrollment-токены,
а живая сессия закрывается. Процесс data plane при этом не останавливается —
остановите сервис на машине отдельно. После удаления агент не сможет
переподключиться, потому что его сертификата больше нет; для повторного enroll
нужно создать новую запись агента.

## Релизы и деплойменты

1. **Версия конфигурации** — неизменяемый YAML; SHA-256 уникален в пределах
   environment, одинаковые документы хранятся один раз.
2. **Артефакт** — по SHA-256; одинаковые байты хранятся один раз.
3. **Релиз** — конфигурация плюс артефакты с путями установки. Control plane
   проверяет его через `wasmgate -validate`, подписывает канонический манифест
   ECDSA P-256 и хранит подпись.
4. **Деплоймент** — селекторы волн (JSON-объекты меток или `*`) применяются к
   агентам среды релиза. Подпись релиза проверяется перед отправкой, затем
   каждый подходящий агент с живой сессией получает `ApplyRelease` с манифестом
   и подписью.

Агент проверяет подпись публичным ключом и сверяет манифест (`release_id`,
`config_sha256`, id артефактов), прежде чем что-то применить.

Эксплуатация:

- Артефакты скачиваются через `AgentArtifacts.Download` и проверяются по SHA-256
  и размеру; отклонённый reload возвращает и конфигурацию, **и** артефакты.
- Деплоймент терминален по первому результату агента; волны рассылаются по
  порядку, но не гейтят друг друга.
- Reaper закрывает застрявшие в `Running` деплойменты старше
  `WASMGATE_CP_DEPLOYMENT_TIMEOUT` (15 минут).
- Откат деплоит релиз, предшествующий текущему в той же среде, пропуская
  отозванные, с теми же селекторами.
- Лимиты: 64 MiB на артефакт, 1 MiB на конфигурацию, 64 артефакта на релиз.

## Протокол агента (кратко)

| RPC | Направление | Авторизация |
|---|---|---|
| `AgentEnrollment.Enroll` | unary | одноразовый токен enrollment |
| `AgentEnrollment.Renew` | unary | существующий клиентский сертификат |
| `AgentControl.Session` | двунаправленный поток | клиентский сертификат |
| `AgentArtifacts.Download` | server stream | клиентский сертификат |

Сессия начинается с `Hello` (id, имя, версия, ABI, метки, активный релиз); CP
обрабатывает `Heartbeat`/`Health`, отправляет `ApplyRelease`, `Drain`,
`SetLogLevel`, `Ping`, `Rollback`. Сейчас отправляется только `ApplyRelease`,
остальные команды агент логирует. Агент шлёт `Heartbeat` каждые 15 с и
переподключается с экспоненциальным backoff (1→30 с).

## Конфигурация

| Переменная | По умолчанию |
|---|---|
| `WASMGATE_CP_POSTGRES` | `Host=127.0.0.1;Port=6767;Database=wasmgate;Username=wasmgate;Password=wasmgate` |
| `WASMGATE_CP_GRPC_LISTEN` | `http://127.0.0.1:9096` |
| `WASMGATE_CP_HTTP_LISTEN` | `http://127.0.0.1:9097` |
| `WASMGATE_CP_ADMIN_TOKEN` | не задан; обязателен, если admin endpoint не loopback |
| `WASMGATE_CP_ARTIFACT_ROOT` | `data/artifacts` |
| `WASMGATE_CP_AGENT_CA_CERT` / `_KEY` | `data/ca/agents-ca.crt` / `.key` |
| `WASMGATE_CP_SIGNING_KEY` | `data/keys/release-signing.key` |
| `WASMGATE_CP_WASMGATE_BIN` | `wasmgate` из `PATH` |
| `WASMGATE_CP_VALIDATE_TIMEOUT` | `00:01:00` |
| `WASMGATE_CP_AGENT_CERT_TTL` | `30.00:00:00` |
| `WASMGATE_CP_APPLY_TIMEOUT` | `00:05:00` |
| `WASMGATE_CP_DEPLOYMENT_TIMEOUT` | `00:15:00` |
| `WASMGATE_CP_MAX_ARTIFACT_BYTES` | 64 MiB |
| `WASMGATE_CP_MAX_CONFIG_BYTES` | 1 MiB |
| `WASMGATE_CP_GRPC_TLS_CERT` / `_KEY` | не заданы; включают `https://` |
| `WASMGATE_CP_GRPC_CLIENT_CA` | `data/ca/agents-ca.crt` |
| `WASMGATE_CP_GRPC_REQUIRE_CLIENT_CERT` | `true` |

Длительности — в инвариантном формате `TimeSpan`.

## Модель безопасности

- Plaintext gRPC запрещён на не-loopback адресе: без TLS нет identity, и любой
  мог бы открыть сессию как агент.
- Admin API вне loopback требует bearer-токен. Панель статическая и не содержит
  данных; API защищён.
- Сертификаты выпускаются из CSR; приватные ключи не покидают узел. Отзыв
  проверяется при старте сессии, при продлении и перед каждым скачиванием
  артефакта.
- ApplyResult принимается только от агента из environment деплоймента, чьи
  labels совпадают с wave selector; если release id передан, он должен совпадать
  с деплойментом.
- Подпись релиза проверяет control plane перед отправкой и агент перед
  применением, поэтому переписанная строка в БД отклоняется.

## Известные ограничения

- Один процесс control plane: реестр сессий в памяти.
- Команды релиза можно воспроизвести. `ApplyRelease` для старого подписанного
  релиза неотличим от отката оператора; исправление требует deployment-level
  подписанного envelope с монотонной последовательностью и сроком действия
  (DPCP-1), а не проверки на уровне релиза.
- Волны без health-gate; первый `ApplyResult` закрывает деплоймент.
- Нет heartbeat-reaper: полуоткрытое соединение держит агента `Online`, пока
  транспорт не отвалится. Сами деплойменты закрываются reaper'ом.
- Нет OIDC/RBAC: один статический admin-токен.
- Нет ретенции старых артефактов, конфигураций и аудита.
- `Drain` и `SetLogLevel` принимаются, но data plane их только логирует.

## Обновление парка

1. Обновите control plane (он начинает отправлять подписанный манифест).
2. Скопируйте `release-signing.key.pub` на каждый узел агента.
3. Обновите агентов и запустите с `-cp-release-key`; без ключа они не стартуют.
4. Новый control plane работает со старыми агентами до следующего релиза, но те
   не проверяют подписи. Не оставляйте такую смесь надолго.
