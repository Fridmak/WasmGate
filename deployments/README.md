# Развёртывание WasmGate

WasmGate — статический Go-бинарник без CGO и без внешних зависимостей времени
выполнения. Для production достаточно бинарника, YAML-конфига, `.wasm`-плагинов и
TLS-сертификатов.

## 1. Сборка

```bash
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o wasmgate ./cmd/wasmgate
```

Кросс-сборка под Linux:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags="-s -w" -o wasmgate-linux-amd64 ./cmd/wasmgate
```

Проверьте артефакт до запуска:

```bash
./wasmgate -config /etc/wasmgate/wasmgate.yaml -validate
```

`-validate` компилирует все `.wasm`, проверяет TLS-сертификаты и валидирует YAML.
Это тот же путь, который выполняется при `SIGHUP` / `POST /reload`.

## 2. Раскладка файлов

```
/usr/local/bin/wasmgate
/etc/wasmgate/wasmgate.yaml
/etc/wasmgate/plugins/...        # .wasm-плагины
/etc/wasmgate/certs/...          # сертификаты и ключи
/etc/wasmgate/admin.token        # опционально, 0640 root:wasmgate (или 0600 wasmgate)
```

Пути в конфиге можно задавать абсолютными или относительно `WorkingDirectory`.
Для systemd-юнита ниже `WorkingDirectory=/etc/wasmgate`.

## 3. systemd

Готовый юнит: [`wasmgate.service`](wasmgate.service).

```bash
# один раз: системный пользователь без домашнего каталога
sudo useradd --system --no-create-home --shell /usr/sbin/nologin wasmgate

sudo install -m 0755 wasmgate /usr/local/bin/wasmgate
sudo install -d -m 0750 -g wasmgate /etc/wasmgate
sudo install -m 0640 -g wasmgate wasmgate.yaml /etc/wasmgate/wasmgate.yaml
sudo install -m 0644 wasmgate.service /etc/systemd/system/wasmgate.service
sudo systemctl daemon-reload
sudo systemctl enable --now wasmgate
```

Каталоги `plugins/` и `certs/` внутри `/etc/wasmgate` должны быть читаемы
пользователю `wasmgate` (например, `install -d -m 0750 -g wasmgate ...` и файлы
`0640 root:wasmgate`); иначе reload не сможет перечитать новый `.wasm` или
сертификат.

Проверка:

```bash
systemctl status wasmgate
journalctl -u wasmgate -f
curl -s http://127.0.0.1:9099/healthz
curl -s http://127.0.0.1:9099/metrics
```

Reload без разрыва соединений:

```bash
sudo systemctl reload wasmgate      # ExecReload = kill -HUP
# или
curl -X POST http://127.0.0.1:9099/reload
```

## 4. Рекомендации для production

- **Не запускайте от root.** Юнит использует отдельного пользователя `wasmgate`;
  для портов < 1024 добавлена capability `CAP_NET_BIND_SERVICE`, а не полный root.
- **Ограничьте admin-листенер.** Оставьте `admin_addr` на loopback. Если нужен
  удалённый доступ — только `admin_allow_remote: true` + `admin_token_file` (или
  `WASMGATE_ADMIN_TOKEN`) и сетевой ACL.
- **Не включайте pprof без необходимости.** `admin_pprof: true` — только на время
  отладки: профилировщик может влиять на память и STW.
- **Задайте `max_conns` и `max_concurrent`.** `0` означает «без лимита»; при
  старте ядро пишет WARN. Для периметра лимиты обязательны.
- **Для недоверенных плагинов задайте `plugin.timeout`.** `0` — самый быстрый
  режим, но без таймера; зависший цикл может остановить процесс.
- **`idle_timeout`.** `0` сохраняет zero-copy `splice`, но не закрывает молчащие
  полуоткрытые соединения. Для интернет-периметра обычно разумно `30–120s` ценой
  перехода на буферизованный путь.
- **TLS-терминация только клиентская.** Upstream идёт обычным TCP; для upstream
  TLS/mTLS используйте отдельный туннель/терминатор.
- **Логи.** `log/slog` пишет структурный текст; частота «шумных» событий
  ограничена. Собирайте journald/Filebeat и не парсите свободный текст.
- **Мониторинг.** Скрейпьте `/metrics`, проверяйте `/healthz`, следите за
  `wasmgate_connections_*_rejected_total`, `wasmgate_upstream_healthy` и
  `wasmgate_plugin_instances_discarded_total`.
- **Обновление.** Соберите новый бинарник, прогоните `-validate` на текущем
  конфиге, замените файл и сделайте `systemctl restart` либо `reload`, если
  меняется только конфиг.

## 5. Контейнеры и облако

Образы собираются из корня репозитория:

```bash
docker build -f deployments/docker/data-plane.Dockerfile -t wasmgate-dp .
docker build -f deployments/docker/control-plane.Dockerfile -t wasmgate-cp .
```

Control-plane образ — Native AOT (`clang` + `zlib` на этапе сборки), внутрь
кладётся Go-бинарник `wasmgate` для `-validate`. Data-plane образ — статический
CGO-free бинарник на alpine с CA-сертификатами для mTLS.

Локальный стенд (PostgreSQL + control plane; миграции применяются при создании
volume). Control plane работает с TLS и требует admin-токен, поэтому сначала
создаются сертификаты и секрет:

```bash
./deployments/dev-certs.sh
export WASMGATE_CP_ADMIN_TOKEN=$(openssl rand -hex 24)
docker compose -f deployments/docker-compose.yml up --build
curl -fsS http://127.0.0.1:9097/healthz
curl -fsS http://127.0.0.1:9097/readyz
# admin API: токен обязателен
curl -fsS -H "Authorization: Bearer $WASMGATE_CP_ADMIN_TOKEN" \
  http://127.0.0.1:9097/api/v1/agents
```

Порты опубликованы только на `127.0.0.1`: gRPC снаружи контейнера доступен по
TLS, admin API — только с хоста. `deployments/certs` — локальная разработка, в
репозиторий не попадает.

Data plane в compose намеренно не стартует: ему нужен одноразовый enrollment
token, agent id и публичный ключ подписи релизов. Команда `docker run` — в
комментарии к `deployments/docker-compose.yml`, порядок onboarding — в
[`control-plane/AGENT-PROTOCOL.md`](../control-plane/AGENT-PROTOCOL.md).

Для облака:

- **Пробы.** Liveness — `/healthz` (процесс отвечает), readiness — `/readyz`
  (проверяет PostgreSQL и наличие схемы, отдаёт 503, пока база недоступна).
- **Конфигурация.** Только env: `WASMGATE_CP_*`. Данные, ключи CA, подпись
  релизов и артефакты — в `/data` (volume).
- **Admin API.** Вне loopback обязателен `WASMGATE_CP_ADMIN_TOKEN`; без него
  процесс не стартует. Токен передавайте секретом оркестратора, не в образе.
- **Plaintext gRPC запрещён** на не-loopback адресе: либо TLS, либо loopback/unix.
- **Миграции** — отдельный шаг деплоя: `psql ... -f control-plane/migrations/00N_*.sql`
  (файлы идемпотентны), либо init-каталог PostgreSQL, как в compose.
- **Порты.** `9096/tcp` — gRPC (HTTP/2) для агентов, `9097/tcp` — admin HTTP
  (только loopback или приватная сеть).
- **TLS.** Смонтируйте PEM-сертификат и ключ, задайте
  `WASMGATE_CP_GRPC_TLS_CERT/KEY` и `WASMGATE_CP_GRPC_LISTEN=https://...`;
  клиентский CA — `WASMGATE_CP_GRPC_CLIENT_CA`.
- **Секреты.** Пароль БД и signing key не встраивайте в образ: передавайте через
  секреты оркестратора (`WASMGATE_CP_POSTGRES`, `WASMGATE_CP_SIGNING_KEY`).
  Вне compose обязательно задайте `WASMGATE_CP_AGENT_CA_KEY` и
  `WASMGATE_CP_SIGNING_KEY`: по умолчанию это относительные пути, а рабочий
  каталог образа (`/app`) доступен только на чтение пользователю процесса.

Ограничение: `splice(2)` работает только на Linux-хосте с обычными TCP-сокетами;
проброс портов через user-space NAT может скрыть часть выигрыша.

## 6. Проверка перед выкладкой

```bash
# конфиг, WASM и сертификаты
./wasmgate -config /etc/wasmgate/wasmgate.yaml -validate

# smoke: health и метрики
curl -fsS http://127.0.0.1:9099/healthz
curl -fsS http://127.0.0.1:9099/metrics | head

# reload под нагрузкой: активные соединения продолжают обслуживаться,
# accepted/closed растут, а не обрываются скачком
curl -X POST http://127.0.0.1:9099/reload
```
