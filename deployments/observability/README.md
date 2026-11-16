# Prometheus and Grafana

A ready-made observability stack for WasmGate. It scrapes both processes:

- **data plane** — `data-plane:9099/metrics`;
- **control plane** — `control-plane:9097/metrics`.

Grafana is provisioned with the Prometheus datasource and the **WasmGate
overview** dashboard; no manual import is needed.

## Quick start with the local demo

Start the local demo first (see [`../scenarios/01-local-demo`](../scenarios/01-local-demo/)):

```bash
docker compose -f deployments/scenarios/01-local-demo/compose.yml --profile dp up -d
```

Export the two admin tokens from the demo `.env` and create the scrape secrets:

```bash
export WASMGATE_ADMIN_TOKEN="$(grep '^WASMGATE_ADMIN_TOKEN=' \
  deployments/scenarios/01-local-demo/.env | cut -d= -f2-)"
export WASMGATE_CP_ADMIN_TOKEN="$(grep '^WASMGATE_CP_ADMIN_TOKEN=' \
  deployments/scenarios/01-local-demo/.env | cut -d= -f2-)"

./deployments/observability/setup.sh

cp deployments/observability/.env.example deployments/observability/.env
# set GRAFANA_ADMIN_PASSWORD; leave WASMGATE_NETWORK=wasmgate-demo_default
docker compose -f deployments/observability/compose.yml up -d
```

Open:

- Grafana: `http://127.0.0.1:3000` (admin / `GRAFANA_ADMIN_PASSWORD`);
- Prometheus: `http://127.0.0.1:9090`.

Prometheus and Grafana bind to host loopback only. The data-plane admin port is
not published; Prometheus reaches it over the compose network.

## Tokens

There are two independent bearer tokens:

| Token | Protects |
|---|---|
| `WASMGATE_ADMIN_TOKEN` | data-plane `/metrics`, `/reload`, pprof |
| `WASMGATE_CP_ADMIN_TOKEN` | control-plane `/metrics` and `/api/v1/*` |

`setup.sh` writes them to `secrets/wasmgate-admin-token` and
`secrets/cp-admin-token`; `prometheus.yml` reads them through
`authorization.credentials_file`. They are gitignored.

The data plane needs `admin_allow_remote: true` and a non-loopback `admin_addr`
when Prometheus runs in another container. In the local demo this is already set
in `config/wasmgate.yaml`. The admin listener is created at process start, so
keep those settings in the seed configuration as well.

## Other layouts

Edit `prometheus/prometheus.yml` targets for the layout you run.

### Standalone data plane on the host

Set in `wasmgate.yaml`:

```yaml
admin_addr: "0.0.0.0:9099"
admin_allow_remote: true
```

Start the proxy with `WASMGATE_ADMIN_TOKEN` in its environment, then change the
Prometheus target to:

```yaml
- targets: ["host.docker.internal:9099"]
```

On Linux, add `extra_hosts: ["host.docker.internal:host-gateway"]` to the
Prometheus service.

### Single-node systemd

Use `host.docker.internal` for both `9099` and `9097`, or run Prometheus
directly on the host and use `127.0.0.1`. Keep the admin listeners on loopback
if Prometheus is local; otherwise set `admin_allow_remote: true` and a token.

### Fleet

Attach the observability stack to the fleet network:

```dotenv
WASMGATE_NETWORK=wasmgate-control_default
```

Then replace the data-plane target with one entry per agent:

```yaml
- targets:
    - edge-01:9099
    - edge-02:9099
    # ...
    - edge-20:9099
```

For dynamic fleets, use Prometheus `file_sd_configs` or Kubernetes service
discovery. Every DP must expose the admin endpoint on the observability network.

### Control plane only

Remove the `wasmgate-data-plane` job. The `wasmgate-control-plane` job continues
to work.

## Metrics

The data plane exposes:

```text
wasmgate_build_info
wasmgate_connections_accepted_total
wasmgate_connections_closed_total
wasmgate_connections_active
wasmgate_connections_limit_rejected_total
wasmgate_connections_rate_rejected_total
wasmgate_connections_plugin_rejected_total
wasmgate_upstream_dial_failures_total
wasmgate_upstream_retries_total
wasmgate_relayed_bytes_total
wasmgate_plugin_instances_discarded_total
wasmgate_reloads_total
wasmgate_config_version
wasmgate_uptime_seconds
wasmgate_upstream_active{server}
wasmgate_upstream_healthy{server}
go_goroutines
go_mem_heap_alloc_bytes
```

The control plane adds:

```text
wasmgate_control_build_info
wasmgate_control_http_requests_total{method,status}
wasmgate_control_agents{environment,status}
wasmgate_control_grpc_sessions_active
wasmgate_control_agent_query_failures_total
wasmgate_control_uptime_seconds
wasmgate_control_working_set_bytes
wasmgate_control_gc_heap_bytes
wasmgate_control_thread_pool_threads
wasmgate_control_gen2_collections_total
```

Useful queries:

```promql
rate(wasmgate_connections_accepted_total[5m])
rate(wasmgate_relayed_bytes_total[5m])
sum(rate(wasmgate_connections_limit_rejected_total[5m]))
sum(rate(wasmgate_connections_rate_rejected_total[5m]))
sum(rate(wasmgate_connections_plugin_rejected_total[5m]))
wasmgate_upstream_healthy == 0
wasmgate_control_agents{status="Online"}
```

## Operations

- Prometheus keeps 15 days of local TSDB data in the `promdata` volume.
- Grafana data lives in `grafanadata`; delete both volumes to reset.
- `promtool check config /etc/prometheus/prometheus.yml` validates the config.
- Prometheus and Grafana are published on `127.0.0.1` only. For remote access,
  use an SSH tunnel or put a TLS reverse proxy in front of Grafana.
