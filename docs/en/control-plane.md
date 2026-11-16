# Control plane

The control plane is an optional .NET 10 service that turns a set of WasmGate
nodes into a managed fleet. It stores desired state in PostgreSQL, issues agent
certificates, signs releases and pushes them over a bidirectional gRPC session.
It is **not on the traffic path**: if it is unavailable, agents keep serving the
last applied configuration.

```
operator ──HTTP──► control plane :9097 (admin API + UI)
                          │
                          ├── PostgreSQL (state, audit)
                          └── /data (CA, signing key, artifacts)
agent ────gRPC :9096──────┘   Session / Enroll / Renew / Download
```

## What is stored where

| Storage | Content |
|---|---|
| PostgreSQL | agents, certificates, enrollment tokens, configuration versions, artifacts metadata, releases, deployments, waves, audit |
| `/data/artifacts` | artifact bytes, addressed by SHA-256 |
| `/data/ca` | agent CA certificate and private key |
| `/data/keys` | release signing key (ECDSA P-256) and public key |
| Process memory | live agent sessions; a second control-plane instance would not see them |

## Quick start

With Docker (TLS + admin token + PostgreSQL; see [deployment](deployment.md)):

```bash
./deployments/dev-certs.sh
export WASMGATE_CP_ADMIN_TOKEN=$(openssl rand -hex 24)
docker compose -f deployments/docker-compose.yml up -d --build
```

Manually:

```bash
# PostgreSQL (host port 6767 avoids a local PostgreSQL on 5432)
docker run --rm -e POSTGRES_USER=wasmgate -e POSTGRES_PASSWORD=wasmgate \
  -e POSTGRES_DB=wasmgate -p 127.0.0.1:6767:5432 postgres:17
for f in control-plane/migrations/00*.sql; do
  psql "postgresql://wasmgate:wasmgate@127.0.0.1:6767/wasmgate" -f "$f"
done

cd control-plane
dotnet build WasmGate.Control.sln
dotnet run --project src/WasmGate.Control.Api
```

```bash
curl -fsS http://127.0.0.1:9097/healthz   # liveness
curl -fsS http://127.0.0.1:9097/readyz    # PostgreSQL + schema
```

## Admin UI

Open `http://127.0.0.1:9097/` and paste the admin token. The UI is a static SPA
served by the same process: agents and certificates, artifact upload,
configuration versions, releases, deployments with rollback and the audit trail.
The token is kept in `sessionStorage`.

## Admin API

| Method | Path | Purpose |
|---|---|---|
| POST | `/api/v1/agents` | Create an agent and return a one-time enrollment token |
| GET | `/api/v1/agents` | List agents with status, labels, last seen and active release |
| POST | `/api/v1/agents/{id}/tokens` | Reissue an enrollment token (lost token, re-enroll) |
| GET | `/api/v1/agents/{id}/certificates` | Active certificates (serial, fingerprint, validity) |
| POST | `/api/v1/agents/{id}/certificates/{serial}/revoke` | Revoke one certificate |
| POST | `/api/v1/registration-tokens` | Shared token for a rollout (`namePrefix`, `maxUses`, `ttlSeconds`) |
| POST | `/api/v1/artifacts` | Upload an artifact (base64, ≤64 MiB) |
| GET | `/api/v1/artifacts` | List artifacts |
| POST | `/api/v1/configs` | Store an immutable configuration version |
| GET | `/api/v1/configs/{environment}` | List versions |
| POST | `/api/v1/releases` | Validate and sign a release |
| GET | `/api/v1/releases/{environment}` | List releases |
| POST | `/api/v1/releases/{id}/revoke` | Revoke a release (cannot be deployed) |
| POST | `/api/v1/deployments` | Deploy a release to wave selectors |
| GET | `/api/v1/deployments/{environment}` | List deployments |
| POST | `/api/v1/deployments/{id}/rollback` | Deploy the previous release |
| GET | `/api/v1/audit` | Last 100 audit events |
| GET | `/healthz`, `/readyz` | Probes (no token) |

The API has no user accounts. A bearer token from `WASMGATE_CP_ADMIN_TOKEN` is
required when the admin endpoint is not on loopback.

## Agents and certificates

1. **Create** an agent (`Pending`) or a shared registration token.
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

## Releases and deployments

1. **Configuration version** — immutable YAML; the SHA-256 is unique per
   environment, so identical documents are stored once.
2. **Artifact** — content-addressed by SHA-256; the same bytes are stored once.
3. **Release** — a configuration plus artifacts with install paths. The control
   plane validates it with `wasmgate -validate`, signs a canonical manifest with
   ECDSA P-256, and stores the signature.
4. **Deployment** — wave selectors (JSON label objects or `*`). The release
   signature is verified before dispatch, then every agent with a live session
   receives `ApplyRelease` with the manifest and signature.

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
- Rollback deploys the previous release of the environment with the same selectors.
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

## Security model

- Plaintext gRPC is refused on a non-loopback address: without TLS there is no
  identity, and anyone could open a session as an agent.
- Admin API requires a bearer token outside loopback. The admin UI is static and
  contains no data; the API is protected.
- Agent certificates are issued from CSRs; private keys never leave the node.
  Revocation is enforced at session start and at renewal.
- Release signatures are verified by the control plane before dispatch and by the
  agent before applying, so a rewritten database row is rejected.

## Known limitations

- One control-plane process: the session registry is in memory.
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
