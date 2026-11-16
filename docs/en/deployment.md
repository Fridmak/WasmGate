# Deployment

WasmGate's data plane is a static, CGO-free binary. A production node needs the
binary, one YAML file, plugin files and TLS material. The control plane is optional
and runs as a separate service with PostgreSQL.

## Build

```bash
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o wasmgate ./cmd/wasmgate

# Linux from any platform
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags="-s -w" -o wasmgate-linux-amd64 ./cmd/wasmgate
```

Always validate before shipping:

```bash
./wasmgate -config /etc/wasmgate/wasmgate.yaml -validate
```

`-validate` parses the YAML, compiles every referenced `.wasm`, checks TLS files
and exits non-zero on any problem. Runtime secrets such as
`WASMGATE_ADMIN_TOKEN` are not required for validation.

## File layout

```
/usr/local/bin/wasmgate
/etc/wasmgate/wasmgate.yaml
/etc/wasmgate/plugins/...        # .wasm files
/etc/wasmgate/certs/...          # certificates and keys
/etc/wasmgate/admin.token        # optional, 0640 root:wasmgate (or WASMGATE_ADMIN_TOKEN)
```

Relative plugin and TLS paths resolve against the configuration directory, so the
data plane can run from any working directory.

## systemd

The unit in
[`deployments/scenarios/02-standalone-data-plane/wasmgate.service`](../../deployments/scenarios/02-standalone-data-plane/wasmgate.service)
runs as an unprivileged user, binds low ports through `CAP_NET_BIND_SERVICE` and
uses `ProtectSystem=strict`. A managed agent uses the writable unit in
[`04-single-node-systemd`](../../deployments/scenarios/04-single-node-systemd/)
instead.

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin wasmgate
sudo install -m 0755 wasmgate /usr/local/bin/wasmgate
sudo install -d -m 0750 -o wasmgate -g wasmgate /etc/wasmgate
sudo install -m 0640 -o wasmgate -g wasmgate wasmgate.yaml /etc/wasmgate/wasmgate.yaml
sudo cp deployments/scenarios/02-standalone-data-plane/wasmgate.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now wasmgate
```

`ExecReload=/bin/kill -HUP $MAINPID` provides zero-downtime configuration reload:
existing connections finish on the old snapshot.

If a control plane manages this node, it rewrites the file passed to `-config`.
That directory must stay writable by the service user; use a systemd drop-in with
`ReadWritePaths=` and point `-config`/`-cp-state` at writable paths.

## Docker (data plane)

```bash
docker build -f deployments/docker/data-plane.Dockerfile -t wasmgate-dp .

docker run -d --name wasmgate --restart unless-stopped --network host \
  -v /etc/wasmgate:/etc/wasmgate:ro \
  -v /var/lib/wasmgate:/var/lib/wasmgate \
  wasmgate-dp -config /etc/wasmgate/wasmgate.yaml
```

`--network host` keeps `splice(2)` and client IPs intact; user-space NAT in front
of the container hides part of the zero-copy gain.

## Control plane + PostgreSQL

[`deployments/scenarios/03-control-plane/compose.yml`](../../deployments/scenarios/03-control-plane/compose.yml)
starts PostgreSQL and the control plane, publishes both ports on host loopback,
requires `WASMGATE_CP_ADMIN_TOKEN` and serves the agent endpoint over TLS.

```bash
./deployments/scripts/dev-certs.sh         # once, local self-signed certificate
cp deployments/scenarios/03-control-plane/.env.example \
   deployments/scenarios/03-control-plane/.env
# edit .env and set WASMGATE_CP_ADMIN_TOKEN
docker compose -f deployments/scenarios/03-control-plane/compose.yml up -d --build

curl -fsS http://127.0.0.1:9097/readyz
# admin UI: http://127.0.0.1:9097/  (paste the token)
```

The complete local stack (control plane + one data plane + nginx backend) is in
[`01-local-demo`](../../deployments/scenarios/01-local-demo/).

Migrations are applied by the PostgreSQL init directory on first start. For an
existing database run them in order:

```bash
for f in control-plane/migrations/00*.sql; do
  psql "$WASMGATE_CP_POSTGRES" -v ON_ERROR_STOP=1 -f "$f"
done
```

Without Docker, see [control plane](control-plane.md) for the environment
variables and the manual run.

## Deployment scenarios

Ready-made assets for five common layouts:

| Scenario | Directory |
|---|---|
| Local demo (CP + one DP + backend) | [`deployments/scenarios/01-local-demo`](../../deployments/scenarios/01-local-demo/) |
| Standalone data plane | [`deployments/scenarios/02-standalone-data-plane`](../../deployments/scenarios/02-standalone-data-plane/) |
| Control plane only | [`deployments/scenarios/03-control-plane`](../../deployments/scenarios/03-control-plane/) |
| Single-node systemd (CP + DP) | [`deployments/scenarios/04-single-node-systemd`](../../deployments/scenarios/04-single-node-systemd/) |
| Data-plane fleet | [`deployments/scenarios/05-dp-fleet`](../../deployments/scenarios/05-dp-fleet/) |

See [`deployments/README.md`](../../deployments/README.md) for the index and shared
assets.

## Metrics

Both processes expose Prometheus metrics: `<admin_addr>/metrics` on the data
plane and the control-plane HTTP address `+ /metrics`. A provisioned Prometheus
and Grafana stack lives in
[`deployments/observability`](../../deployments/observability/). See
[metrics](metrics.md) for the full metric list, tokens and scrape examples.

## Upgrades

1. Build and `-validate` the new binary/config.
2. Replace the binary and `systemctl restart wasmgate` (or upgrade the image).
3. Configuration changes only: SIGHUP/reload, no restart.
4. With a control plane, publish a new release and deploy it; rollback is a normal
   deployment of the previous release.

## Cloud notes

- **L4 only.** An HTTP/L7 load balancer terminates TCP and breaks raw streams,
  SNI passthrough and `splice`. Use an NLB/TCP balancer or a direct public IP.
- **Probes.** Data plane: `GET /healthz` on the admin address (keep it on
  loopback and scrape locally). Control plane: `/healthz` (liveness) and `/readyz`
  (PostgreSQL + schema).
- **Secrets.** `WASMGATE_CP_ADMIN_TOKEN`, the database password and the release
  signing key belong in orchestrator secrets, never in the image.
- **State.** Back up the PostgreSQL database and `/data` (agent CA, signing key,
  artifacts). Losing the CA invalidates every agent certificate.
- **Addressing.** A dynamic public IP changes after a stop/start; reserve a static
  one for a permanent endpoint. Preemptible VMs are fine for labs, not for a
  production edge.
- **Ports.** Data plane listeners must be reachable by clients; the admin port must
  not be. Control-plane gRPC (`9096`) should be reachable only from agents, and the
  admin API (`9097`) only from operators.
