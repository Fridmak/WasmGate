# 20 data planes behind one control plane

One control plane manages many independent data planes. The DPs do **not** form
a cluster: they never talk to each other, keep their own certificate and state,
and only connect outbound to the CP.

The control plane can use SQLite (the default for single-node installs) or
PostgreSQL. Fleet size does not require PostgreSQL: the CP is a single process
either way. Use PostgreSQL when the database must be external or backed up by
existing database tooling.

```text
                    ┌── dp-01 ── clients
                    ├── dp-02 ── clients
CP (one process) ───┼── dp-03 ── clients
                    ├── ...
                    └── dp-20 ── clients
```

Use an L4 load balancer (NLB, nginx stream, HAProxy TCP, Kubernetes LoadBalancer)
in front of them. Do not put an HTTP/L7 proxy in front if you need raw TCP, SNI
passthrough or `splice(2)`.

## 1. Create a registration token

The admin panel does not currently have a registration-token form, so use the
API:

```powershell
$cp = 'http://127.0.0.1:9097'
$token = '<admin-token>'
$h = @{ Authorization = "Bearer $token" }

$body = @{
  namePrefix  = 'edge-'
  environment = 'prod'
  maxUses     = 20
  ttlSeconds  = 86400
} | ConvertTo-Json

$reg = Invoke-RestMethod -Method Post `
  -Uri "$cp/api/v1/registration-tokens" `
  -Headers $h -ContentType 'application/json' -Body $body

$reg.token
```

`maxUses` must be at least the number of DPs. Each DP consumes one use when it
first enrolls. A DP with a persistent state volume reuses its certificate and
does not consume another use on restart.

## 2. Build the DP image

```bash
docker build -f deployments/docker/data-plane.Dockerfile -t wasmgate-dp .
```

The script uses `NETWORK` to attach containers to the CP network. If the CP was
started by `deployments/scenarios/03-control-plane/compose.yml`, the project name
is `wasmgate-control`, so the network is `wasmgate-control_default`.

## 3. Configure and run

```bash
cp deployments/scenarios/05-dp-fleet/fleet.env.example \
   deployments/scenarios/05-dp-fleet/.env
# edit CP_URL, certificates, token and CONFIG_FILE

set -a
. deployments/scenarios/05-dp-fleet/.env
set +a

chmod +x deployments/scenarios/05-dp-fleet/run-fleet.sh
./deployments/scenarios/05-dp-fleet/run-fleet.sh
```

The script creates one named volume per DP, seeds the writable configuration,
and starts containers named `edge-01` … `edge-20`. No host ports are published:
traffic goes through the load balancer.

Check in the UI: **Agents** should show 20 agents in the selected environment.
Each has its own GUID, certificate and active release.

## 4. Roll out releases

All DPs in one environment receive the same YAML from a release. Therefore:

- put a DNS name/service name in `pools.*.servers` that every DP can resolve;
- use separate environments for different configs/backends;
- use labels and multiple deployments for canaries.

Example labels from the script: `role=edge`. Split the fleet:

- start 2 DPs with `LABELS=role=canary` and 18 with `LABELS=role=edge`;
- deploy `{"role":"canary"}` first, verify, then deploy `{"role":"edge"}`.

Do not rely on multi-wave deployments for gating: the CP dispatches waves in
order but does not wait for one wave to finish, and the first agent result makes
the whole deployment terminal.

## 5. State and identity

- Every DP needs a **unique** state volume. Never share `-cp-state` between
  replicas; sharing makes them fight over one agent identity.
- Do not use `docker compose --scale dp=20` with one named volume. Either run
  explicit containers as this script does, use 20 services, or use a Kubernetes
  StatefulSet with a PVC per pod.
- If a volume is lost, the DP generates a new GUID and enrolls as a new agent;
  the old agent record remains in the CP. Delete it with **Agents → Details →
  Delete agent**.
- The CP keeps live sessions in memory. Run exactly one CP instance; multiple CP
  replicas do not share sessions and a deployment would only reach agents
  connected to the CP that handled it.

## 6. Kubernetes alternative

Use a StatefulSet with `replicas: 20`, a PVC per pod for `/data`, a headless
Service, and a Secret containing the registration token. The pod name is unique,
so `-cp-name $(POD_NAME)` and a token prefix such as `dp-` work naturally. Put a
TCP LoadBalancer Service in front of the headless service.

## Troubleshooting

```bash
docker ps --filter name=edge-
docker logs -f edge-01
docker volume ls | grep wasmgate-dp
```

If an agent stays `Pending`, the token may be expired/exhausted or the CP URL
may be unreachable from the container network. If it becomes `Offline`, check
the container logs and the CP certificate SAN.
