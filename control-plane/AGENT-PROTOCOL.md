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
`deployment_id`, `timeout_ms` и список артефактов `{id, name, kind, size, signature}`.

## 4. Артефакты

Байты не передаются в `ApplyRelease`: DP скачивает их отдельным RPC
`AgentArtifacts.Download` по SHA-256 id.

- `id` — 64 hex-символа; CP отвечает `InvalidArgument`/`NotFound`.
- DP сверяет `sha256` и размер, ограничивает размер 256 МиБ.
- Файл кладётся по пути `name` **относительно каталога `-config`**; абсолютные
  пути и `..` отклоняются.
- Запись идёт через временный файл с последующим rename, чтобы не оставить
  половину файла.

## 5. Применение релиза на DP

1. `config_yaml` проверяется по `config_sha256` и разбирается `config.Parse`
   (та же схема, что и при локальной загрузке).
2. Артефакты скачиваются и проверяются.
3. Новый YAML атомарно записывается в файл `-config`. Файл после этого
   управляется CP.
4. `proxy.Server.Reload` валидирует плагины/TLS и публикует новый snapshot без
   разрыва существующих соединений.
5. Если reload отклонён, DP возвращает старый файл конфигурации и отправляет
   `ApplyResult{ok:false}` с текстом ошибки.

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
  -cp-token $agent.enrollToken -cp-agent-id $agent.agentId `
  -cp-name dp-01 -cp-labels region=eu,role=edge `
  -cp-state state\agent
```

Для общего registration token `-cp-agent-id` не нужен: DP сгенерирует GUID сам.

## 7. mTLS в продакшене

- CP: `WASMGATE_CP_GRPC_TLS_CERT`, `WASMGATE_CP_GRPC_TLS_KEY`, клиентский CA —
  `WASMGATE_CP_GRPC_CLIENT_CA` (по умолчанию `data/ca/agents-ca.crt`).
- DP: `-cp https://cp.example:9096`, при необходимости `-cp-server-ca` с CA,
  подписавшим серверный сертификат CP.

При включённом TLS `Session` требует клиентский сертификат и сравнивает CN с
`Hello.agent_id`; `AgentArtifacts.Download` требует такой же сертификат.

## 8. Ограничения текущей версии

- Heartbeat-таймаута/reaper нет: агент становится `Offline` при разрыве
  сессии, но зависший без разрыва остаётся `Online`.
- Health-gate волн и approvals не реализованы: первый `ApplyResult` закрывает
  деплой.
- Admin API без аутентификации (OIDC/RBAC — следующий шаг).
- Один DP обслуживает одного агента; горизонтального шаринга состояния нет.
