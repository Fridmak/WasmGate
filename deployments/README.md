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

## 5. Контейнеры

Готового Dockerfile в репозитории нет. Для контейнера достаточно
multi-stage-сборки: `golang:1.25` → `CGO_ENABLED=0 go build`, затем
`scratch`/`distroless` с бинарником, конфигом и каталогами `plugins/`, `certs/`.
Помните, что `splice(2)` работает только на Linux-хосте с обычными TCP-сокетами;
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
