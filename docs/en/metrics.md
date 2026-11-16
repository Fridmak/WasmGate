# Metrics and observability

WasmGate exposes Prometheus text exposition (version 0.0.4) from both processes.
There is no separate exporter and no client library dependency.

## Endpoints

| Process | Endpoint | Authentication |
|---|---|---|
| Data plane | `GET http://<admin_addr>/metrics` | `WASMGATE_ADMIN_TOKEN` when configured |
| Control plane | `GET http://<cp-http-addr>/metrics` | `WASMGATE_CP_ADMIN_TOKEN` when configured |

The data-plane admin endpoint is configured by `admin_addr`. It is loopback-only
unless `admin_allow_remote: true`; Prometheus in another container therefore
needs a non-loopback admin address plus the admin token.

The control-plane endpoint is protected by the same bearer token as `/api/v1/*`.
Health probes stay open.

## Ready-made Prometheus + Grafana

The repository contains a provisioned stack:

```text
deployments/observability/
```

It includes:

- Prometheus scrape config for both processes;
- Grafana datasource provisioning;
- the **WasmGate overview** dashboard;
- token setup helper;
- Compose files and notes for local demo, standalone, systemd and fleet layouts.

See [`deployments/observability/README.md`](../../deployments/observability/README.md).

## Data-plane metrics

| Metric | Type | Meaning |
|---|---|---|
| `wasmgate_build_info{version,go_version}` | gauge | build information |
| `wasmgate_connections_accepted_total` | counter | accepted client connections |
| `wasmgate_connections_closed_total` | counter | finished connection handlers |
| `wasmgate_connections_active` | gauge | currently proxied connections |
| `wasmgate_connections_limit_rejected_total` | counter | refused by `max_conns` |
| `wasmgate_connections_rate_rejected_total` | counter | refused by per-IP/concurrency limits |
| `wasmgate_connections_plugin_rejected_total` | counter | refused by a WASM hook |
| `wasmgate_plugin_instances_discarded_total` | counter | WASM instances destroyed after trap/timeout |
| `wasmgate_upstream_dial_failures_total` | counter | failed upstream dials |
| `wasmgate_upstream_retries_total` | counter | transparent retries across pool servers |
| `wasmgate_relayed_bytes_total` | counter | bytes relayed in both directions |
| `wasmgate_reloads_total` | counter | successful configuration reloads |
| `wasmgate_config_version` | gauge | current configuration version |
| `wasmgate_uptime_seconds` | gauge | process uptime |
| `wasmgate_upstream_active{server}` | gauge | in-flight connections per backend |
| `wasmgate_upstream_healthy{server}` | gauge | 1 healthy, 0 unhealthy/circuit-open |
| `go_goroutines` | gauge | goroutine count |
| `go_mem_heap_alloc_bytes` | gauge | Go heap allocation |

## Control-plane metrics

| Metric | Type | Meaning |
|---|---|---|
| `wasmgate_control_build_info{version,dotnet_version}` | gauge | build information |
| `wasmgate_control_http_requests_total{method,status}` | counter | admin/API HTTP requests |
| `wasmgate_control_agents{environment,status}` | gauge | registered agents grouped by environment and status |
| `wasmgate_control_grpc_sessions_active` | gauge | live agent control sessions |
| `wasmgate_control_uptime_seconds` | gauge | process uptime |
| `wasmgate_control_working_set_bytes` | gauge | resident working set |
| `wasmgate_control_gc_heap_bytes` | gauge | managed heap size |
| `wasmgate_control_thread_pool_threads` | gauge | thread-pool thread count |
| `wasmgate_control_gen2_collections_total` | counter | Gen2 GC collections |

## Useful queries

```promql
rate(wasmgate_connections_accepted_total[5m])
rate(wasmgate_relayed_bytes_total[5m])
sum(rate(wasmgate_connections_limit_rejected_total[5m]))
sum(rate(wasmgate_connections_rate_rejected_total[5m]))
sum(rate(wasmgate_connections_plugin_rejected_total[5m]))
wasmgate_upstream_healthy == 0
wasmgate_control_agents{status="Online"}
```

## Scrape configuration

Minimal Prometheus job:

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

For a fleet, add one target per agent or use `file_sd_configs` / Kubernetes
service discovery. Each DP must expose its admin endpoint on the network that
Prometheus can reach.

## Security

- Use two distinct tokens: data-plane `WASMGATE_ADMIN_TOKEN` and control-plane
  `WASMGATE_CP_ADMIN_TOKEN`.
- Do not publish the data-plane admin port to the internet. The observability
  Compose stack keeps it on the internal network.
- Grafana is published on host loopback. Use an SSH tunnel or a TLS reverse proxy
  for remote access.
- Token files are gitignored; keep them `0600` outside the container. The sample
  Prometheus container reads them read-only.
