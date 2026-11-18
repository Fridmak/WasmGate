# Control plane

The control plane is an optional .NET 10 service that turns a set of WasmGate
nodes into a managed fleet. It stores desired state in SQLite or PostgreSQL,
issues agent certificates, signs releases and pushes them over a bidirectional
gRPC session.
It is **not on the traffic path**: if it is unavailable, agents keep serving the
last applied configuration.

```
operator ──HTTP──► control plane :9097 (admin API + UI)
                          │
                          ├── SQLite or PostgreSQL (state, audit)
                          └── /data (CA, signing key, artifacts)
agent ────gRPC :9096──────┘   Session / Enroll / Renew / Download
```

## What is stored where

| Storage | Content |
|---|---|
| SQLite (`/data/control.db`) or PostgreSQL | agents, certificates, enrollment tokens, configuration versions, artifacts metadata, releases, deployments, waves, audit |
| `/data/artifacts` | artifact bytes, addressed by SHA-256 |
| `/data/ca` | agent CA certificate and private key |
| `/data/keys` | release signing key (ECDSA P-256) and public key |
| Process memory | live agent sessions; a second control-plane instance would not see them |

## Quick start

With Docker (TLS + admin token; SQLite by default; see [deployment](deployment.md)):

```bash
./deployments/scripts/dev-certs.sh
cp deployments/scenarios/03-control-plane/.env.example \
   deployments/scenarios/03-control-plane/.env
# edit .env and set WASMGATE_CP_ADMIN_TOKEN
docker compose -f deployments/scenarios/03-control-plane/compose.yml up -d --build
```

For PostgreSQL add the overlay:

```bash
docker compose -f deployments/scenarios/03-control-plane/compose.yml \
               -f deployments/scenarios/03-control-plane/compose.postgres.yml up -d --build
```

Manually with SQLite (no database service):

```bash
cd control-plane
dotnet build WasmGate.Control.sln
WASMGATE_CP_STORAGE=sqlite dotnet run --project src/WasmGate.Control.Api
```

PostgreSQL remains available by setting `WASMGATE_CP_STORAGE=postgres` and a
`WASMGATE_CP_POSTGRES` connection string after applying
`control-plane/migrations/001..006.sql`.

```bash
curl -fsS http://127.0.0.1:9097/healthz   # liveness
curl -fsS http://127.0.0.1:9097/readyz    # selected store + schema
```

## Admin UI

Open `http://127.0.0.1:9097/` and paste the admin token. The UI is a static SPA
served by the same process: agents and certificates, artifact upload,
configuration versions, releases, deployments with rollback and the audit trail.
The token is kept in `sessionStorage`. The environment picker in the top bar
switches every screen; it also creates new environments.

## Environments

An environment is a hard deployment scope, not a label. Every agent belongs to
exactly one environment, and a deployment can only reach agents of its own
environment; wave selectors then narrow that set. Configurations, releases and
deployments are namespaced by environment as well.

- `prod` and `staging` are created by the schema. The admin picker and
  `POST /api/v1/environments` create more; a new name used by a configuration,
  release or agent is registered automatically.
- The agent environment is chosen when the agent is created, or on enrollment
  with a registration token. An operator can move an existing agent in
  **Agents → Details → Change**; the agent keeps its current release until the
  next deployment in the new environment.
- A per-agent enrollment token inherits the agent environment. A registration
  token carries its own, and enrollment rejects a token from another
  environment. Registration tokens cannot enroll an existing agent; re-enrollment
  uses a per-agent token.
- Start managed agents with `-cp-environment <name>` so they reject a signed
  release whose manifest belongs to another environment before parsing or
  applying it.
- An upgraded installation puts existing agents into `prod`.
- A deployment whose selectors match no agent in its environment is recorded as
  `Failed`, with a message explaining which of the three cases happened: no
  agents in the environment, no selector match, or no live session.
- An environment can be deleted from the picker. `prod` is protected. An
  environment that still has agents or a running deployment is refused;
  otherwise its configurations, releases, deployments and enrollment tokens are
  deleted with it.

Rollback deploys the release that precedes the current one in the same
environment, skipping revoked releases. The API returns `rollbackReleaseId` for
every deployment; the UI disables the button when it is null.

## Deployment details and logs

The deployment list shows a short failure reason. **Details** opens the full
message, wave states, rollback target and the audit events for that deployment.
The `message` field is filled when no command could be sent, when an agent
reports an apply failure (including listener errors), and by the timeout reaper.

The admin panel shows actions and outcomes, not the process log. Engine logs are
in the service journal:

```bash
sudo journalctl -u wasmgate-control -n 200 --no-pager   # control plane
sudo journalctl -u wasmgate -n 200 --no-pager           # data plane
```

## Admin API

| Method | Path | Purpose |
|---|---|---|
| POST | `/api/v1/agents` | Create an agent and return a one-time enrollment token |
| GET | `/api/v1/agents` | List agents with environment, status, labels, last seen and active release |
| POST | `/api/v1/agents/{id}/tokens` | Reissue an enrollment token (lost token, re-enroll) |
| POST | `/api/v1/agents/{id}/environment` | Move an agent to another environment |
| DELETE | `/api/v1/agents/{id}` | Delete an agent, its certificates and tokens |
| GET | `/api/v1/agents/{id}/certificates` | Active certificates (serial, fingerprint, validity) |
| POST | `/api/v1/agents/{id}/certificates/{serial}/revoke` | Revoke one certificate |
| GET | `/api/v1/environments` | List environments for the picker |
| POST | `/api/v1/environments` | Create an environment (idempotent) |
| DELETE | `/api/v1/environments/{name}` | Delete an environment and its scoped data |
| POST | `/api/v1/registration-tokens` | Shared token for a rollout (`namePrefix`, `environment`, `maxUses`, `ttlSeconds`) |
| POST | `/api/v1/artifacts` | Upload an artifact (base64, ≤64 MiB) |
| GET | `/api/v1/artifacts` | List artifacts |
| POST | `/api/v1/configs` | Store an immutable configuration version |
| GET | `/api/v1/configs/{environment}` | List versions |
| POST | `/api/v1/releases` | Validate and sign a release |
| GET | `/api/v1/releases/{environment}` | List releases |
| POST | `/api/v1/releases/{id}/revoke` | Revoke a release (cannot be deployed) |
| POST | `/api/v1/deployments` | Deploy a release to wave selectors |
| GET | `/api/v1/deployments/{environment}` | List deployments, including the rollback target and failure reason |
| POST | `/api/v1/deployments/{id}/rollback` | Deploy the previous release |
| GET | `/api/v1/audit` | Last 100 audit events |
| GET | `/metrics` | Prometheus scrape (same token as `/api/v1/*`) |
| GET | `/healthz`, `/readyz` | Probes (no token) |

The API has no user accounts. A bearer token from `WASMGATE_CP_ADMIN_TOKEN` is
required when the admin endpoint is not on loopback. It also protects
`/metrics`. See [metrics](metrics.md) for the metric reference and a ready-made
Prometheus + Grafana stack.

## Agents and certificates

1. **Create** an agent (`Pending`) in an environment, or a shared registration
   token bound to an environment.
2. **Enroll**: the agent generates an RSA key and CSR with
   `CN=wasmgate-agent:<guid>` and calls `AgentEnrollment.Enroll` with the token.
   The token is consumed atomically before the certificate is issued.
3. **Session**: the agent opens `AgentControl.Session`; the CN must match
   `Hello.agent_id`, and the certificate fingerprint must still be active.
4. **Renew**: before expiry the agent calls `AgentEnrollment.Renew` over mTLS; a
   new key and certificate replace the old one. No token is needed, and a revoked
   certificate cannot renew.
5. **Revoke** a certificate; the next session is rejected.

Certificate lifetime defaults to 30 days (`WASMGATE_CP_AGENT_CERT_TTL`), renewal
happens 7 days before expiry (`-cp-renew-before` on the agent). The CA files are
created on first start; losing them invalidates every agent certificate.

Operator labels are authoritative: labels reported in `Hello` only fill gaps, so
an agent cannot move itself into another wave.

To remove an agent, use **Agents → Details → Delete agent** (or
`DELETE /api/v1/agents/{id}`). The record, its certificates and enrollment
tokens are deleted, and a live session is closed. The data-plane process is not
stopped: stop the service on the host as well. After deletion the agent cannot
reconnect because its certificate no longer exists; re-enrollment requires a new
agent record.

## Releases and deployments

1. **Configuration version** — immutable YAML; the SHA-256 is unique per
   environment, so identical documents are stored once.
2. **Artifact** — content-addressed by SHA-256; the same bytes are stored once.
3. **Release** — a configuration plus artifacts with install paths. The control
   plane validates it with `wasmgate -validate`, signs a canonical manifest with
   ECDSA P-256, and stores the signature.
4. **Deployment** — wave selectors (JSON label objects or `*`) applied to agents
   of the release environment. The release signature is verified before
   dispatch, then every matching agent with a live session receives
   `ApplyRelease` with the manifest and signature.

Agents verify the signature against the configured public key and cross-check the
manifest (`release_id`, `config_sha256`, artifact ids) before applying anything.

Operational details:

- Artifacts are downloaded over `AgentArtifacts.Download` and verified by SHA-256
  and size; a rejected reload restores the previous configuration **and**
  artifacts.
- A deployment is terminal when the first agent reports a result; waves are
  dispatched in order but are not gated by the previous wave.
- A background reaper fails deployments stuck in `Running` longer than
  `WASMGATE_CP_DEPLOYMENT_TIMEOUT` (15 min).
- Rollback deploys the release that precedes the current one in the same
  environment, skipping revoked releases, with the same selectors.
- Limits: 64 MiB per artifact, 1 MiB per configuration, 64 artifacts per release.

## Agent protocol (summary)

| RPC | Direction | Auth |
|---|---|---|
| `AgentEnrollment.Enroll` | unary | one-time enrollment token |
| `AgentEnrollment.Renew` | unary | existing client certificate |
| `AgentControl.Session` | bidirectional stream | client certificate |
| `AgentArtifacts.Download` | server stream | client certificate |

The session starts with `Hello` (id, name, version, ABI, labels, active release);
the control plane answers with `Heartbeat`/`Health` handling, `ApplyRelease`,
`Drain`, `SetLogLevel`, `Ping`, `Rollback`. Only `ApplyRelease` is produced today;
the other commands are logged by the agent. Agents send `Heartbeat` every 15 s and
reconnect with exponential backoff (1→30 s).

## Configuration

| Variable | Default |
|---|---|
| `WASMGATE_CP_STORAGE` | unset; PostgreSQL unless only `WASMGATE_CP_SQLITE_PATH` is set |
| `WASMGATE_CP_SQLITE_PATH` | `data/control.db` when SQLite is selected |
| `WASMGATE_CP_POSTGRES` | `Host=127.0.0.1;Port=6767;Database=wasmgate;Username=wasmgate;Password=wasmgate` |
| `WASMGATE_CP_GRPC_LISTEN` | `http://127.0.0.1:9096` |
| `WASMGATE_CP_HTTP_LISTEN` | `http://127.0.0.1:9097` |
| `WASMGATE_CP_ADMIN_TOKEN` | unset; required when the admin endpoint is not loopback |
| `WASMGATE_CP_ARTIFACT_ROOT` | `data/artifacts` |
| `WASMGATE_CP_AGENT_CA_CERT` / `_KEY` | `data/ca/agents-ca.crt` / `.key` |
| `WASMGATE_CP_SIGNING_KEY` | `data/keys/release-signing.key` |
| `WASMGATE_CP_WASMGATE_BIN` | `wasmgate` from `PATH` |
| `WASMGATE_CP_VALIDATE_TIMEOUT` | `00:01:00` |
| `WASMGATE_CP_AGENT_CERT_TTL` | `30.00:00:00` |
| `WASMGATE_CP_APPLY_TIMEOUT` | `00:05:00` |
| `WASMGATE_CP_DEPLOYMENT_TIMEOUT` | `00:15:00` |
| `WASMGATE_CP_MAX_ARTIFACT_BYTES` | 64 MiB |
| `WASMGATE_CP_MAX_CONFIG_BYTES` | 1 MiB |
| `WASMGATE_CP_GRPC_TLS_CERT` / `_KEY` | unset; enables `https://` |
| `WASMGATE_CP_GRPC_CLIENT_CA` | `data/ca/agents-ca.crt` |
| `WASMGATE_CP_GRPC_REQUIRE_CLIENT_CERT` | `true` |

Durations use the invariant `TimeSpan` format.

`WASMGATE_CP_STORAGE` accepts `postgres` or `sqlite`. Selecting SQLite while a
PostgreSQL connection string is present is rejected as ambiguous. SQLite
supports exactly one control-plane process per database file; it creates the
schema and the single-process lock automatically.

## Security model

- Plaintext gRPC is refused on a non-loopback address: without TLS there is no
  identity, and anyone could open a session as an agent.
- Admin API requires a bearer token outside loopback. The admin UI is static and
  contains no data; the API is protected.
- Agent certificates are issued from CSRs; private keys never leave the node.
  Revocation is enforced at session start, at renewal and before every artifact
  download.
- An apply result is accepted only from an agent in the deployment environment
  whose labels match a wave selector; when present, its release id must match the
  deployment.
- Release signatures are verified by the control plane before dispatch and by the
  agent before applying, so a rewritten database row is rejected.

## Known limitations

- One control-plane process: the session registry is in memory. SQLite storage
  enforces this with a lock file; PostgreSQL still assumes one process too.
- Release commands are replayable. An `ApplyRelease` for an older signed release
  is indistinguishable from an operator rollback; a fix needs a deployment-level
  signed envelope with a monotonic sequence and expiry (DPCP-1), not a
  release-level check.
- Waves are not health-gated; the first `ApplyResult` closes the deployment.
- No heartbeat reaper: a half-open connection keeps an agent `Online` until the
  transport fails. Deployments themselves are reaped.
- No OIDC/RBAC: one static admin token.
- No retention policy for old artifacts, configurations and audit events.
- `Drain` and `SetLogLevel` are accepted but only logged by the data plane.

## Upgrading a fleet

1. Upgrade the control plane (it starts sending the signed manifest).
2. Copy `release-signing.key.pub` to every agent node.
3. Upgrade agents and start them with `-cp-release-key`; they refuse to start
   without it.
4. A new control plane keeps working with old agents until the next release, but
   those agents do not verify signatures. Do not leave such a mix for long.
