# Local demo

Everything on one machine:

- PostgreSQL;
- control plane and admin UI;
- one data plane;
- nginx demo backend.

## Start the control plane and backend

```bash
./deployments/scripts/dev-certs.sh

cp deployments/scenarios/01-local-demo/.env.example \
   deployments/scenarios/01-local-demo/.env
# edit .env: set WASMGATE_CP_ADMIN_TOKEN and WASMGATE_ADMIN_TOKEN

docker compose -f deployments/scenarios/01-local-demo/compose.yml up -d --build

curl -fsS http://127.0.0.1:9097/readyz
```

Open `http://127.0.0.1:9097/` and paste the token.

## Create an agent

In the UI:

1. Environment: `prod` (or create `demo`).
2. Agents → New agent.
3. Name: `dp-01`; Environment: `prod`; Labels: `role=demo`.
4. Copy the agent id and the one-time enrollment token.

Put them into `.env`:

```dotenv
WASMGATE_AGENT_ID=<agent-id>
WASMGATE_ENROLL_TOKEN=<token>
```

Then start the data plane:

```bash
docker compose -f deployments/scenarios/01-local-demo/compose.yml --profile dp up -d
```

Check:

```bash
curl http://127.0.0.1:8080/
docker compose -f deployments/scenarios/01-local-demo/compose.yml logs -f data-plane
```

## Create the first release

The data plane already runs the seed config, but the managed path should be
exercised through the control plane:

1. Copy [`config/wasmgate.yaml`](config/wasmgate.yaml) into
   **Configurations → New configuration**.
2. **Releases → New release**, choose that configuration, create it with no
   artifacts (or add plugins if you want).
3. **Deployments → New deployment**, choose the release and selector `*`.
4. Refresh the deployments page; the status should become `Succeeded`.
5. `curl http://127.0.0.1:8080/` still returns the nginx page.

The agent downloads the release, rewrites `/data/wasmgate.yaml` and reloads. From
now on, change the configuration in the UI instead of editing the seed file.

## Metrics and Grafana

The local demo exposes both metric endpoints on the compose network:

- data plane: `data-plane:9099/metrics`;
- control plane: `control-plane:9097/metrics`.

The data-plane admin port is not published to the host. The control-plane admin
port is published on `127.0.0.1:9097` for the UI; Prometheus reaches both over
the compose network. Prometheus and Grafana run in the shared observability
stack:

```bash
export WASMGATE_ADMIN_TOKEN="$(grep '^WASMGATE_ADMIN_TOKEN=' deployments/scenarios/01-local-demo/.env | cut -d= -f2-)"
export WASMGATE_CP_ADMIN_TOKEN="$(grep '^WASMGATE_CP_ADMIN_TOKEN=' deployments/scenarios/01-local-demo/.env | cut -d= -f2-)"
./deployments/observability/setup.sh

cp deployments/observability/.env.example deployments/observability/.env
# set GRAFANA_ADMIN_PASSWORD; WASMGATE_NETWORK is already wasmgate-demo_default
docker compose -f deployments/observability/compose.yml up -d
```

Grafana: `http://127.0.0.1:3000` (admin + `GRAFANA_ADMIN_PASSWORD`).
Prometheus: `http://127.0.0.1:9090`.

The dashboard **WasmGate overview** is provisioned automatically. See
[`deployments/observability/README.md`](../../observability/README.md) for other
deployment layouts. Set `WASMGATE_DP_VERSION=v1.2.3` in `.env` to override the
version reported by `wasmgate_build_info` (the default is `dev`).

## Notes

- The data plane is under the `dp` profile because it needs a token created
  after the control plane starts.
- The control plane data volume `cpdata` is mounted read-only into the data
  plane to read `release-signing.key.pub`.
- `dpdata` is a writable volume; the control plane rewrites the configuration
  there.
- For a CP-only deployment see [`../03-control-plane`](../03-control-plane). For
  many agents see [`../05-dp-fleet`](../05-dp-fleet).
