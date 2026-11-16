# Развёртывание

Data plane WasmGate — статический бинарник без CGO. Для production-узла нужны
бинарник, один YAML-файл, `.wasm`-плагины и TLS-материалы. Control plane
опционален и работает отдельным сервисом с PostgreSQL.

## Сборка

```bash
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o wasmgate ./cmd/wasmgate

# Linux с любой платформы
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags="-s -w" -o wasmgate-linux-amd64 ./cmd/wasmgate
```

Перед выкладкой всегда проверяйте:

```bash
./wasmgate -config /etc/wasmgate/wasmgate.yaml -validate
```

`-validate` разбирает YAML, компилирует все указанные `.wasm`, проверяет TLS и
завершается с ошибкой при любой проблеме. Runtime-секреты вроде
`WASMGATE_ADMIN_TOKEN` для валидации не нужны.

## Раскладка файлов

```
/usr/local/bin/wasmgate
/etc/wasmgate/wasmgate.yaml
/etc/wasmgate/plugins/...        # .wasm-файлы
/etc/wasmgate/certs/...          # сертификаты и ключи
/etc/wasmgate/admin.token        # опционально, 0640 root:wasmgate (или WASMGATE_ADMIN_TOKEN)
```

Относительные пути плагинов и TLS резолвятся от каталога конфигурации, поэтому
data plane можно запускать из любого рабочего каталога.

## systemd

Юнит в
[`deployments/scenarios/02-standalone-data-plane/wasmgate.service`](../../deployments/scenarios/02-standalone-data-plane/wasmgate.service)
работает от непривилегированного пользователя, получает низкие порты через
`CAP_NET_BIND_SERVICE` и использует `ProtectSystem=strict`. Для управляемого
агента используйте writable-юнит из
[`04-single-node-systemd`](../../deployments/scenarios/04-single-node-systemd/).

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin wasmgate
sudo install -m 0755 wasmgate /usr/local/bin/wasmgate
sudo install -d -m 0750 -o wasmgate -g wasmgate /etc/wasmgate
sudo install -m 0640 -o wasmgate -g wasmgate wasmgate.yaml /etc/wasmgate/wasmgate.yaml
sudo cp deployments/scenarios/02-standalone-data-plane/wasmgate.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now wasmgate
```

`ExecReload=/bin/kill -HUP $MAINPID` даёт reload без простоя: текущие соединения
дорабатывают на старом snapshot.

Если узлом управляет control plane, он перезаписывает файл, переданный в
`-config`. Этот каталог должен быть доступен сервису на запись: используйте
systemd drop-in с `ReadWritePaths=` и указывайте `-config`/`-cp-state` в
записываемых путях.

## Docker (data plane)

```bash
docker build -f deployments/docker/data-plane.Dockerfile -t wasmgate-dp .

docker run -d --name wasmgate --restart unless-stopped --network host \
  -v /etc/wasmgate:/etc/wasmgate:ro \
  -v /var/lib/wasmgate:/var/lib/wasmgate \
  wasmgate-dp -config /etc/wasmgate/wasmgate.yaml
```

`--network host` сохраняет `splice(2)` и реальные IP клиентов; user-space NAT
перед контейнером скрывает часть выигрыша zero-copy.

## Control plane + PostgreSQL

[`deployments/scenarios/03-control-plane/compose.yml`](../../deployments/scenarios/03-control-plane/compose.yml)
поднимает PostgreSQL и control plane, публикует оба порта только на loopback,
требует `WASMGATE_CP_ADMIN_TOKEN` и отдаёт gRPC агентов по TLS.

```bash
./deployments/scripts/dev-certs.sh         # один раз, локальный самоподписанный сертификат
cp deployments/scenarios/03-control-plane/.env.example \
   deployments/scenarios/03-control-plane/.env
# отредактируйте .env и задайте WASMGATE_CP_ADMIN_TOKEN
docker compose -f deployments/scenarios/03-control-plane/compose.yml up -d --build

curl -fsS http://127.0.0.1:9097/readyz
# админ-панель: http://127.0.0.1:9097/  (вставьте токен)
```

Полный локальный стенд (control plane + один data plane + nginx-бэкенд) —
в [`01-local-demo`](../../deployments/scenarios/01-local-demo/).

Миграции применяются init-каталогом PostgreSQL при первом старте. Для уже
существующей базы выполните их по порядку:

```bash
for f in control-plane/migrations/00*.sql; do
  psql "$WASMGATE_CP_POSTGRES" -v ON_ERROR_STOP=1 -f "$f"
done
```

Без Docker — см. [control plane](control-plane.md): переменные окружения и ручной
запуск.

## Сценарии развёртывания

Готовые заготовки для пяти типовых схем:

| Сценарий | Каталог |
|---|---|
| Локальный демо-стенд (CP + один DP + backend) | [`deployments/scenarios/01-local-demo`](../../deployments/scenarios/01-local-demo/) |
| Standalone data plane | [`deployments/scenarios/02-standalone-data-plane`](../../deployments/scenarios/02-standalone-data-plane/) |
| Только control plane | [`deployments/scenarios/03-control-plane`](../../deployments/scenarios/03-control-plane/) |
| Single-node systemd (CP + DP) | [`deployments/scenarios/04-single-node-systemd`](../../deployments/scenarios/04-single-node-systemd/) |
| Флот data plane | [`deployments/scenarios/05-dp-fleet`](../../deployments/scenarios/05-dp-fleet/) |

Индекс и общие файлы: [`deployments/README.md`](../../deployments/README.md).

## Метрики

Оба процесса отдают метрики Prometheus: `<admin_addr>/metrics` у data plane и
`/metrics` на HTTP-адресе control plane. Готовый стек Prometheus + Grafana лежит
в [`deployments/observability`](../../deployments/observability/). Полный список
метрик, токены и примеры scrape — в [метриках](metrics.md).

## Обновления

1. Соберите новый бинарник/конфиг и прогоните `-validate`.
2. Замените бинарник и выполните `systemctl restart wasmgate` (или обновите образ).
3. Только конфигурация: SIGHUP/reload, без рестарта.
4. С control plane опубликуйте новый релиз и задеплойте; откат — обычный деплой
   предыдущего релиза.

## Облако

- **Только L4.** HTTP/L7-балансировщик терминирует TCP и ломает сырые потоки,
  SNI passthrough и `splice`. Нужен NLB/TCP-балансировщик или прямой публичный IP.
- **Пробы.** Data plane: `GET /healthz` на admin-адресе (держите loopback и
  скрейпьте локально). Control plane: `/healthz` (liveness) и `/readyz`
  (PostgreSQL + схема).
- **Секреты.** `WASMGATE_CP_ADMIN_TOKEN`, пароль БД и ключ подписи релизов — в
  секретах оркестратора, не в образе.
- **Состояние.** Бэкапьте PostgreSQL и `/data` (CA агентов, signing key,
  артефакты). Потеря CA инвалидирует все сертификаты агентов.
- **Адреса.** Динамический публичный IP меняется после stop/start; для постоянной
  точки входа зарезервируйте статический. Прерываемые VM годятся для стенда, но
  не для production-edge.
- **Порты.** Листенеры data plane должны быть доступны клиентам; admin-порт —
  нет. gRPC control plane (`9096`) — только агентам, admin API (`9097`) — только
  операторам.
