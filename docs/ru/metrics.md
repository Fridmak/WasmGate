# Метрики и наблюдаемость

WasmGate отдаёт метрики в формате Prometheus text exposition (0.0.4) из обоих
процессов. Отдельный exporter и client library не нужны.

## Эндпоинты

| Процесс | Эндпоинт | Авторизация |
|---|---|---|
| Data plane | `GET http://<admin_addr>/metrics` | `WASMGATE_ADMIN_TOKEN`, если задан |
| Control plane | `GET http://<cp-http-addr>/metrics` | `WASMGATE_CP_ADMIN_TOKEN`, если задан |

Admin-эндпоинт DP задаётся `admin_addr` и выключен, если ключ пуст. Если он
задан, адрес должен быть loopback, пока не включён `admin_allow_remote: true`;
чтобы Prometheus из другого контейнера мог его достать, нужен non-loopback
`admin_addr`, `admin_allow_remote: true` и токен.

Эндпоинт CP защищён тем же bearer-токеном, что и `/api/v1/*`. Health-пробы
остаются открытыми.

Оба gauge `build_info` по умолчанию показывают development-версию (`dev` у data
plane, версия сборки у control plane). Чтобы DP сообщал реальный тег, собирайте
его с `make build VERSION=v1.2.3` или `--build-arg VERSION=v1.2.3`.

## Готовый Prometheus + Grafana

В репозитории есть провижининг-стек:

```text
deployments/observability/
```

Внутри:

- scrape-конфиг Prometheus для DP и CP;
- datasource Grafana;
- dashboard **WasmGate overview**;
- скрипт создания token-файлов;
- compose и инструкции для local demo, standalone, systemd и флота.

См. [`deployments/observability/README.md`](../../deployments/observability/README.md).

## Метрики data plane

| Метрика | Тип | Смысл |
|---|---|---|
| `wasmgate_build_info{version,go_version}` | gauge | версия сборки |
| `wasmgate_connections_accepted_total` | counter | принятые клиентские соединения |
| `wasmgate_connections_closed_total` | counter | завершённые обработчики |
| `wasmgate_connections_active` | gauge | активные соединения |
| `wasmgate_connections_limit_rejected_total` | counter | отказы по `max_conns` |
| `wasmgate_connections_rate_rejected_total` | counter | отказы по per-IP/concurrency лимитам |
| `wasmgate_connections_plugin_rejected_total` | counter | отказы WASM-плагином |
| `wasmgate_plugin_instances_discarded_total` | counter | уничтоженные WASM-инстансы после trap/timeout |
| `wasmgate_plugin_header_incomplete_total` | counter | headers-запросы, отклонённые до dial (неполные/слишком большие/HTTP-2) |
| `wasmgate_upstream_dial_failures_total` | counter | неудачные dial до upstream |
| `wasmgate_upstream_retries_total` | counter | прозрачные retry по серверам пула |
| `wasmgate_relayed_bytes_total` | counter | переданные байты в обе стороны |
| `wasmgate_reloads_total` | counter | успешные reload конфигурации |
| `wasmgate_config_version` | gauge | текущая версия конфигурации |
| `wasmgate_uptime_seconds` | gauge | uptime процесса |
| `wasmgate_upstream_active{server}` | gauge | активные соединения к бэкенду |
| `wasmgate_upstream_healthy{server}` | gauge | 1 healthy, 0 unhealthy/circuit-open |
| `go_goroutines` | gauge | число горутин |
| `go_mem_heap_alloc_bytes` | gauge | heap Go |

## Метрики control plane

| Метрика | Тип | Смысл |
|---|---|---|
| `wasmgate_control_build_info{version,dotnet_version}` | gauge | версия сборки |
| `wasmgate_control_http_requests_total{method,status}` | counter | HTTP-запросы admin, API и scrape |
| `wasmgate_control_agents{environment,status}` | gauge | агенты по среде и статусу |
| `wasmgate_control_grpc_sessions_active` | gauge | живые gRPC-сессии агентов |
| `wasmgate_control_agent_query_failures_total` | counter | scrape, не сумевшие прочитать агентов из SQLite |
| `wasmgate_control_uptime_seconds` | gauge | uptime процесса |
| `wasmgate_control_working_set_bytes` | gauge | resident working set |
| `wasmgate_control_gc_heap_bytes` | gauge | managed heap |
| `wasmgate_control_thread_pool_threads` | gauge | потоки thread pool |
| `wasmgate_control_gen2_collections_total` | counter | сборки Gen2 GC |

## Полезные запросы

```promql
rate(wasmgate_connections_accepted_total[5m])
rate(wasmgate_relayed_bytes_total[5m])
sum(rate(wasmgate_connections_limit_rejected_total[5m]))
sum(rate(wasmgate_connections_rate_rejected_total[5m]))
sum(rate(wasmgate_connections_plugin_rejected_total[5m]))
wasmgate_upstream_healthy == 0
wasmgate_control_agents{status="Online"}
increase(wasmgate_control_agent_query_failures_total[5m]) > 0
```

## Scrape-конфиг

Минимальный job:

```yaml
scrape_configs:
  - job_name: wasmgate-data-plane
    metrics_path: /metrics
    authorization:
      type: Bearer
      credentials_file: /etc/prometheus/wasmgate-admin-token
    static_configs:
      - targets: ["data-plane:9099"]

  - job_name: wasmgate-control-plane
    metrics_path: /metrics
    authorization:
      type: Bearer
      credentials_file: /etc/prometheus/cp-admin-token
    static_configs:
      - targets: ["control-plane:9097"]
```

Для флота добавьте по target на каждый агент или используйте
`file_sd_configs` / Kubernetes service discovery. Admin-порт каждого DP должен
быть доступен из сети Prometheus.

## Безопасность

- Два разных токена: data-plane `WASMGATE_ADMIN_TOKEN` и control-plane
  `WASMGATE_CP_ADMIN_TOKEN`.
- Не публикуйте admin-порт DP в интернет. В observability-compose он доступен
  только внутри сети.
- Grafana публикуется на loopback. Для удалённого доступа — SSH-туннель или
  TLS reverse proxy.
- Token-файлы в gitignore. `setup.sh` пишет их с правами `0644`, чтобы
  непривилегированный пользователь Prometheus мог прочитать bind mount; на
  общем хосте ограничьте каталог или используйте Docker secrets.
