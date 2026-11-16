# Протокол CP ↔ DP

Контракт описан в `control-plane/src/WasmGate.Control.Api/Protos/agent_control.proto`.
Go-биндинги генерируются из него командой `make proto`.

## Схема

Data plane всегда сам инициирует соединение, control plane только отвечает и
пушит команды в тот же поток. Поэтому CP не нужен прямой доступ к сети агентов.

```
Enroll (unary, токен)  →  сертификат агента
Session (bidirectional stream)  →  Hello / Heartbeat / ApplyResult  ⇄  ApplyRelease / Ping
AgentArtifacts.Download (server stream)  →  байты артефакта
```

## 1. Enrollment

1. Оператор создаёт агента: `POST /api/v1/agents` → `agentId` + одноразовый
   `enrollToken`. Для массового онбординга вместо этого выдаётся общий
   registration token с префиксом имени.
2. DP генерирует RSA-ключ и CSR с `CN=wasmgate-agent:<agentId>`.
3. DP вызывает `AgentEnrollment.Enroll` (`agent_id`, `name`, `token`, `csr_pem`).
4. CP проверяет токен, атомарно списывает использование, подписывает CSR
   встроенным CA и возвращает `certificate_pem`, `ca_pem`, `expires_at_unix_ms`.
5. DP сохраняет `agent.id`, `agent.key` (0600), `agent.crt`, `agents-ca.crt` в
   `-cp-state`. Пока эти файлы на месте, повторный enrollment не выполняется.

Правила имени: 1–`AgentNaming.MaxLength` ASCII-символов, начинается и
заканчивается буквой или цифрой. Для per-agent токена имя должно совпадать с
зарегистрированным; для общего токена — начинаться с `namePrefix`.

### Продление

Сертификат живёт `WASMGATE_CP_AGENT_CERT_TTL` (по умолчанию 30 дней). DP сам
вызывает `AgentEnrollment.Renew` за 7 дней до истечения (`-cp-renew-before`):

- авторизация — существующим клиентским сертификатом, токен не нужен;
- DP генерирует новый ключ и CSR, CP проверяет, что CN сертификата совпадает с
  `agent_id`, и выпускает новый сертификат;
- старый сертификат остаётся в истории и может быть отозван.

Если сертификат уже истёк, mTLS невозможен: DP сообщает об этом и требует новый
enrollment token. Продление поэтому делается заранее, а не по факту отказа TLS.

## 2. Session

Первое сообщение обязано быть `Hello`:

- `agent_id` — GUID, должен совпадать с CN клиентского сертификата (при mTLS);
- `name`, `version`, `abi_versions`, `labels` — инвентаризация;
- `active_release_id`, `active_config_sha256` — что DP реально применяет.

CP:

- проверяет `Hello`, при mTLS сверяет CN, пишет агента в PostgreSQL;
- регистрирует сессию; если у агента уже была сессия, старую закрывает
  (takeover), новая остаётся;
- на `Heartbeat` обновляет `last_seen_at` и `status = Online`;
- на `ApplyResult` переводит деплой в `Succeeded`/`Failed`;
- при разрыве убирает сессию и помечает агента `Offline`, если новых сессий нет.

## 3. Команды CP → DP

| Сообщение | Поведение DP |
|---|---|
| `ApplyRelease` | скачать артефакты, проверить, применить конфиг, вернуть `ApplyResult` |
| `Ping` | пишется в debug-лог; ответ не требуется |
| `Drain` | логируется как предупреждение, соединения продолжают обслуживаться |
| `SetLogLevel` | не поддерживается на лету; нужен рестарт с `-log-level` |
| `Rollback` | CP откатывает через обычный `ApplyRelease`, команда игнорируется |

`ApplyRelease` содержит `release_id`, `config_sha256`, `config_yaml`,
`deployment_id`, `timeout_ms`, `manifest`, `manifest_signature` и список
артефактов `{id, name, kind, size, signature}`.

## 4. Артефакты

Байты не передаются в `ApplyRelease`: DP скачивает их отдельным RPC
`AgentArtifacts.Download` по SHA-256 id.

- `id` — 64 hex-символа; CP отвечает `InvalidArgument`/`NotFound`.
- DP сверяет `sha256` и размер, ограничивает размер 256 МиБ.
- Файл кладётся по пути `name` **относительно каталога `-config`**; имена — это
  переносимые POSIX-пути: абсолютные пути, `..`, `\`, `:` и управляющие символы
  отклоняются на обеих сторонах.
- Запись идёт через временный файл с последующим rename, чтобы не оставить
  половину файла. Перезаписанные файлы запоминаются и восстанавливаются, если
  reload нового конфига не удался.

## 5. Применение релиза на DP

1. `config_yaml` проверяется по `config_sha256` и разбирается `config.Parse`
   (та же схема, что и при локальной загрузке).
2. Проверяется подпись: `manifest_signature` (ECDSA P-256, P1363) над
   `manifest`; затем манифест сверяется с командой — `release_id`,
   `config_sha256` и набор `name → artifact_id` должны совпадать. Это защищает от
   правки строки релиза в БД CP: без приватного ключа подпись не пересчитать.
3. Артефакты скачиваются и проверяются.
4. Новый YAML атомарно записывается в файл `-config`. Файл после этого
   управляется CP.
5. `proxy.Server.Reload` валидирует плагины/TLS и публикует новый snapshot без
   разрыва существующих соединений.
6. Если reload отклонён, DP возвращает старый YAML **и старые артефакты** и
   отправляет `ApplyResult{ok:false}` с текстом ошибки.

Состояние (`release_id`, `config_sha256`, номер версии конфига) хранится в
`<state>/applied.json` и переживает рестарт: `Hello` и `Heartbeat` сразу
сообщают, что применено.

Относительные пути в конфиге (плагины, TLS-сертификаты) резолвятся относительно
каталога файла конфигурации, а не рабочего каталога процесса.

## 6. Запуск пары локально

```powershell
# PostgreSQL и миграции — см. README.
docker start wasmgate-pg

# CP
cd control-plane
$env:WASMGATE_CP_WASMGATE_BIN = "..\bin\wasmgate.exe"
& "$env:LOCALAPPDATA\Microsoft\dotnet10\dotnet.exe" run --project src/WasmGate.Control.Api

# агент
$agent = Invoke-RestMethod -Method Post -Uri http://127.0.0.1:9097/api/v1/agents `
  -ContentType application/json -Body '{"name":"dp-01"}'

# DP (в другом окне; bin\wasmgate.exe собран make build)
.\bin\wasmgate.exe `
  -config state\dp.yaml `
  -cp http://127.0.0.1:9096 -cp-insecure `
  -cp-release-key ..\control-plane\data\keys\release-signing.key.pub `
  -cp-token $agent.enrollToken -cp-agent-id $agent.agentId `
  -cp-name dp-01 -cp-labels region=eu,role=edge `
  -cp-state state\agent
```

Для общего registration token `-cp-agent-id` не нужен: DP сгенерирует GUID сам.
`-cp-release-key` обязателен: без публичного ключа DP не может отличить настоящий
релиз от подменённой строки в БД. Публичный ключ создаётся CP при первом релизе
рядом с `release-signing.key` (`<path>.pub`).

## 7. mTLS в продакшене

- CP: `WASMGATE_CP_GRPC_TLS_CERT`, `WASMGATE_CP_GRPC_TLS_KEY`, клиентский CA —
  `WASMGATE_CP_GRPC_CLIENT_CA` (по умолчанию `data/ca/agents-ca.crt`).
- DP: `-cp https://cp.example:9096`, при необходимости `-cp-server-ca` с CA,
  подписавшим серверный сертификат CP.

При включённом TLS `Session` требует клиентский сертификат и сравнивает CN с
`Hello.agent_id`; `AgentArtifacts.Download` требует такой же сертификат.
Дополнительно CP проверяет, что отпечаток сертификата не отозван.

CP не стартует с plaintext-gRPC на не-loopback адресе: в plaintext нет identity, и
любой, кто знает GUID, мог бы открыть сессию как этот агент.

### Порядок обновления

1. Обновите CP: он начинает подписывать `ApplyRelease` манифестом.
2. Скопируйте `release-signing.key.pub` на каждый DP.
3. Обновите DP и запустите его с `-cp-release-key` и `-cp-renew-before`
   (по умолчанию 7 дней). Без ключа DP не стартует.
4. Старый DP с новым CP продолжит работать до первого релиза: поле `manifest`
   он игнорирует, но подпись не проверяет. Не оставляйте такую смесь надолго.

## 8. Ограничения текущей версии

- Heartbeat-таймаута/reaper нет: агент становится `Offline` при разрыве
  сессии, но зависший без разрыва остаётся `Online`. Деплойменты при этом
  закрывает отдельный reaper по таймауту.
- Health-gate волн и approvals не реализованы: первый `ApplyResult` закрывает
  деплой.
- Admin API защищён статическим bearer-токеном; OIDC/RBAC — следующий шаг.
- Один DP обслуживает одного агента; горизонтального шаринга состояния нет.
- Нет ретенции старых артефактов, конфигов и аудита.
