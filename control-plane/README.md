# WasmGate Control Plane (skeleton)

C# control plane for WasmGate. The MVP is functional end-to-end on in-memory
stores: mTLS agent sessions, automatic certificate enrollment, artifact upload,
configuration versions, validated/signed releases, deployments and rollback.
The PostgreSQL repository adapters are intentionally left as
`NotImplementedException` practice stubs; the rest of the service works without
a database.

The control plane is optional. A standalone WasmGate data plane keeps working
from a local YAML file and `SIGHUP`; the CP adds versioning, validation,
distribution, rollback and audit for a fleet.

## Stack

| Area | Choice | Why |
|---|---|---|
| Runtime | .NET 10 LTS | .NET 8 LTS is out of support since 10 Nov 2026 |
| API | ASP.NET Core gRPC + Minimal API | gRPC is officially supported with Native AOT |
| Serialization | protobuf + `System.Text.Json` source generation where needed | no reflection at runtime |
| Database | PostgreSQL for production; in-memory adapters for the MVP | real transactions, JSONB in PostgreSQL; fast local bring-up |
| Data access | Dapper.AOT + Npgsql | Dapper.AOT generates mapping code at build time (AOT-safe); Npgsql is Native AOT compatible since 8.0; EF Core is not AOT-compatible |
| AOT | `PublishAot=true` | small, fast-starting native executable |
| Artifacts | content-addressed SHA-256 storage | immutable releases and verifiable distribution |
| Signing | ECDSA P-256 manifest signature | the data plane does not trust the CP blindly |

No EF Core, no reflection-based JSON. Dapper.AOT is used instead of vanilla Dapper:
vanilla Dapper emits IL at runtime and fails under Native AOT. Dapper.AOT is a
build-time interceptor generator, so repository code stays natural
`QueryAsync<T>` / `ExecuteAsync` while remaining AOT-safe.

Dapper.AOT restrictions to keep in mind:

- SQL must be visible at build time; `CommandDefinition` is not supported
  (DAP057). Use the overload that takes the SQL string directly.
- Generic APIs (`QueryAsync<T>`, `ExecuteAsync`) are supported; non-generic
  `Query(Type, ...)` is not.
- Runtime `SqlMapper.AddTypeHandler`/`SqlMapper.Settings` are ignored; type
  handlers are declared with attributes.
- The generated mapping code is C#-only, which is fine for this project.

## Layout

```
control-plane/
├── Directory.Build.props
├── global.json
├── migrations/001_init.sql        PostgreSQL schema
└── src/
    ├── WasmGate.Control.Core/     entities + interfaces (no infrastructure)
    │   ├── Entities/
    │   ├── Abstractions/
    │   └── Configuration/
    ├── WasmGate.Control.Infrastructure/
    │   ├── Postgres/              Npgsql repository stubs (practice area)
    │   ├── Artifacts/             local content-addressed store
    │   ├── Validation/            Go validator adapter
    │   ├── Security/              ECDSA signer and agent CA
    │   └── Agents/                in-memory stores + session registry
    └── WasmGate.Control.Api/
        ├── Protos/agent_control.proto
        ├── Services/AgentControlService.cs
        ├── Endpoints/AdminEndpoints.cs
        └── Program.cs
```

## What is already wired

- Two Kestrel endpoints: HTTP/2 for agent gRPC, HTTP/1.1 for admin/health.
- `http://`, `https://` and `unix:///absolute/path.sock` listen addresses.
  Ports are explicit; URI paths, queries and fragments are rejected. `https://`
  supports mutual TLS with a custom agent CA.
- The gRPC `Session` accepts `Hello`, validates the mTLS certificate identity,
  handles session takeover, reads `Heartbeat`/`ApplyResult`/`Health` and closes
  displaced sessions.
- An agent certificate authority (`PkiCertificateAuthority`) generates or loads
  the agent CA and signs data-plane certificate requests.
- The `AgentEnrollment.Enroll` gRPC service issues the first certificate from a
  one-time enrollment token; no manual `openssl` is needed.
- `IAgentProvisioningService` creates agents and issues per-agent or shared
  registration tokens, so hundreds of data planes can be onboarded by CI.
- Admin HTTP endpoints: `POST /api/v1/agents`, `POST /api/v1/registration-tokens`,
  `POST /api/v1/artifacts`, `POST/GET /api/v1/configs`, `POST/GET /api/v1/releases`,
  `POST /api/v1/deployments`, `POST /api/v1/deployments/{id}/rollback`,
  `GET /api/v1/agents`, `GET /api/v1/audit`.
- Artifact store, Go validator adapter, ECDSA release signer and the complete
  release/deploy/rollback workflow.
- Domain entities for agents, config versions, artifacts, releases, deployments,
  waves, audit events, certificates and enrollment tokens.
- Repository, artifact store, validator, signer and session-registry interfaces.
- Dapper.AOT + Npgsql wiring. PostgreSQL adapters are the practice area; the MVP
  runs on in-memory stores.
- PostgreSQL schema with content-addressed artifacts and audit log.
- Remaining admin routes return `501`.

## Dapper.AOT query pattern

```csharp
// AssemblyInfo.cs
[module: DapperAot]

// repository
private sealed class Row
{
    public Guid Id { get; set; }
    public string Name { get; set; } = string.Empty;
}

const string sql = """SELECT id AS "Id", name AS "Name" FROM agents WHERE id = @id""";
await using var connection = await dataSource.OpenConnectionAsync(ct);
var row = await connection.QuerySingleOrDefaultAsync<Row>(sql, new { id });
```

Use the SQL-string overload, not `CommandDefinition`: the generator must see the
SQL at build time.

## What is intentionally not implemented

- SQL in the `Postgres*Repository` adapters (the control plane runs on in-memory
  stores for the MVP)
- `GoConfigValidator` requires a built `wasmgate` binary; it delegates validation
  to the Go data plane and does not duplicate the schema in C#
- Deployment health gates, approvals, OIDC/RBAC
- Heartbeat timeout/reaper and certificate renewal/revocation checks at `Session`
- PostgreSQL persistence for agents, enrollment tokens, certificates, configs,
  releases, deployments and audit

## Run

Requirements: .NET 10 SDK, PostgreSQL 17, and a C toolchain for Native AOT
publish. `global.json` pins SDK `10.0.404`; `dotnet --version` must report `10.*`.
The repository does not build with the older .NET 8/9 SDKs.

If SDK 10 is installed outside `C:\Program Files\dotnet` (for example with
`dotnet-install.ps1`), make sure Rider uses that `dotnet.exe`: add its directory
to `PATH`, set `DOTNET_ROOT`, or point Rider's .NET CLI path at it.

```bash
# 1. PostgreSQL
docker run --rm \
  -e POSTGRES_USER=wasmgate \
  -e POSTGRES_PASSWORD=wasmgate \
  -e POSTGRES_DB=wasmgate \
  -p 5432:5432 postgres:17

# 2. Schema (from the repository root)
psql "postgresql://wasmgate:wasmgate@127.0.0.1:5432/wasmgate" \
  -f control-plane/migrations/001_init.sql
psql "postgresql://wasmgate:wasmgate@127.0.0.1:5432/wasmgate" \
  -f control-plane/migrations/002_enrollment.sql

# 3. Build and run (global.json lives in control-plane/)
cd control-plane
dotnet build WasmGate.Control.sln
dotnet run --project src/WasmGate.Control.Api

# 4. Health (from another terminal)
curl http://127.0.0.1:9097/healthz
```

Environment variables:

| Variable | Default |
|---|---|
| `WASMGATE_CP_POSTGRES` | `Host=127.0.0.1;Port=5432;Database=wasmgate;Username=wasmgate;Password=wasmgate` |
| `WASMGATE_CP_GRPC_LISTEN` | `http://127.0.0.1:9096` — HTTP/2; `http://<ip\|localhost\|*>:<port>`, `https://<ip\|localhost\|*>:<port>` or `unix:///absolute/path.sock` |
| `WASMGATE_CP_HTTP_LISTEN` | `http://127.0.0.1:9097` — HTTP/1.1; `http://<ip\|localhost\|*>:<port>` or `unix:///absolute/path.sock` |
| `WASMGATE_CP_GRPC_TLS_CERT` | unset — PEM server certificate for `https://` |
| `WASMGATE_CP_GRPC_TLS_KEY` | unset — PEM server private key for `https://` |
| `WASMGATE_CP_GRPC_CLIENT_CA` | `data/ca/agents-ca.crt` — PEM CA bundle used to validate agent client certificates |
| `WASMGATE_CP_GRPC_REQUIRE_CLIENT_CERT` | `true` — require mTLS when TLS is configured |
| `WASMGATE_CP_AGENT_CA_CERT` | `data/ca/agents-ca.crt` — PEM certificate of the agent CA; created if missing |
| `WASMGATE_CP_AGENT_CA_KEY` | `data/ca/agents-ca.key` — PEM private key of the agent CA; created if missing |
| `WASMGATE_CP_AGENT_CERT_TTL` | `30.00:00:00` — lifetime of issued agent certificates |
| `WASMGATE_CP_ARTIFACT_ROOT` | `data/artifacts` — absolute, or relative to the CP working directory |
| `WASMGATE_CP_WASMGATE_BIN` | `wasmgate` — absolute, relative to the CP working directory, or a bare command resolved via `PATH` |
| `WASMGATE_CP_SIGNING_KEY` | unset — absolute, or relative to the CP working directory |

Listen addresses are validated strictly:

- a port is always required; `http://localhost` is rejected instead of silently
  binding port 80;
- `https://` requires `WASMGATE_CP_GRPC_TLS_CERT` and `WASMGATE_CP_GRPC_TLS_KEY`;
- `http://` is plaintext; setting TLS files for an `http://` address is rejected;
- `tcp://` is rejected; use `http://`, `https://` or `unix://`;
- path, query, fragment and userinfo are rejected;
- `*` binds all interfaces, `localhost` binds both loopback families;
- a Unix socket path must be absolute, its parent directory must exist, and a
  stale socket is not deleted automatically. Use systemd `RuntimeDirectory=` (or
  remove the stale socket manually).

### Mutual TLS

When TLS is configured and `WASMGATE_CP_GRPC_REQUIRE_CLIENT_CERT=true`, every
agent must present a client certificate signed by the CA in
`WASMGATE_CP_GRPC_CLIENT_CA`. The certificate subject CN must contain the agent
identity in the form:

```
CN=wasmgate-agent:<agent-guid>
```

The `AgentId` inside the gRPC `Hello` must match that CN. A certificate for one
agent cannot register another agent's ID.

TLS files must be PEM. For local development without certificates, use
`http://127.0.0.1` and keep the endpoint on loopback; the service logs a warning
that client certificate authentication is disabled.

### Onboarding many data planes

Onboarding is automated through the admin HTTP API, so CI or deployment tooling
never shells into the control plane.

Create one agent and its one-time token:

```bash
curl -X POST http://127.0.0.1:9097/api/v1/agents \
  -H 'Content-Type: application/json' \
  -d '{"name":"dp-01","labels":{"region":"eu","role":"edge"}}'
```

Create a shared registration token for a rollout of hundreds of agents:

```bash
curl -X POST http://127.0.0.1:9097/api/v1/registration-tokens \
  -H 'Content-Type: application/json' \
  -d '{"name_prefix":null,"max_uses":100,"ttl_seconds":3600}'
```

Each data plane then:

1. generates its RSA key pair and a CSR with
   `CN=wasmgate-agent:<agent-id>`;
2. calls `AgentEnrollment.Enroll` with the token and CSR;
3. stores the returned certificate and CA;
4. connects to `AgentControl.Session` using mTLS.

The data plane implementation is still pending; see `DEVELOPMENT.md` for the
list of DP-side tasks.

Filesystem paths are resolved against the control-plane process working
directory. Under systemd this is `WorkingDirectory=`; in a container it is
`WORKDIR`; in a shell it is the directory you started the process from.

Production example with mutual TLS (from `control-plane/`):

```bash
WASMGATE_CP_GRPC_LISTEN=https://*:9096 \
WASMGATE_CP_GRPC_TLS_CERT=/etc/wasmgate/tls/server.crt \
WASMGATE_CP_GRPC_TLS_KEY=/etc/wasmgate/tls/server.key \
WASMGATE_CP_GRPC_CLIENT_CA=/etc/wasmgate/tls/agents-ca.crt \
WASMGATE_CP_GRPC_REQUIRE_CLIENT_CERT=true \
WASMGATE_CP_HTTP_LISTEN=http://127.0.0.1:9097 \
WASMGATE_CP_POSTGRES='Host=/run/postgresql;Database=wasmgate;Username=wasmgate' \
dotnet run --project src/WasmGate.Control.Api
```

Local development without certificates (loopback plaintext):

```bash
WASMGATE_CP_GRPC_LISTEN=http://127.0.0.1:9096 \
WASMGATE_CP_HTTP_LISTEN=http://127.0.0.1:9097 \
dotnet run --project src/WasmGate.Control.Api
```

## C# control plane and Go data plane

This is **not** “a C# project running on Go”. These are two independent
processes in one repository:

| Process | Language | Role |
|---|---|---|
| `wasmgate` | Go | Data plane: accepts TCP, applies config, proxies traffic |
| `wasmgate-control` | C# | Control plane: API, PostgreSQL, releases, audit |

They never call each other's functions. The boundary is gRPC (UDS on one host,
mTLS/TCP for remote agents). The control plane never loads Go code, and the Go
proxy never hosts .NET.

**Current state:** the Go data plane has no control-plane client yet. The C#
skeleton exposes the gRPC contract; the two processes run independently.

### Local development

```bash
# Terminal 1: Go data plane, standalone file mode
go run ./cmd/wasmgate -config configs/wasmgate.yaml -log-level debug

# Terminal 2: C# control plane
cd control-plane
dotnet run --project src/WasmGate.Control.Api
```

No interaction happens until the Go agent is implemented. This is intentional:
the data plane must stay useful in standalone mode.

### How to connect them

1. Move the proto to a shared contract path, for example
   `api/proto/wasmgate/control/v1/agent_control.proto`, and reference it from
   both the C# project and the Go module.
2. Generate Go stubs:
   ```bash
   protoc \
     --go_out=. --go-grpc_out=. \
     --proto_path=api/proto \
     api/proto/wasmgate/control/v1/agent_control.proto
   ```
   Add `google.golang.org/grpc` and `google.golang.org/protobuf`.
3. Implement a Go agent in `internal/control`:
   - dial the CP (UDS or TCP/mTLS);
   - open `AgentControl.Session`;
   - send `Hello` with agent ID, version, ABI versions and labels;
   - send `Heartbeat` on a ticker;
   - receive `ApplyRelease`, write artifacts to a temp directory, verify
     SHA-256/signature, run validation, call the existing snapshot reload, then
     send `ApplyResult`;
   - keep the last-known-good release on disk and start from it if the CP is
     unavailable.
4. Add flags such as `-control-plane`, `-agent-id`, `-state-dir`.
5. Implement `AgentControlService.Session` in C#: persist `Hello`, register the
   session in `IAgentSessionRegistry`, dispatch commands and process
   `ApplyResult`.

Do not spawn the Go binary as a child process of the C# service in production.
Use systemd/containers and let the agent dial the CP. A child process is fine
only for integration tests and local demos.

## Build, versioning and logs

### Versioning

- **Git**: feature branches and SemVer tags (`v0.1.0`). Conventional Commits.
- **One product version** for both binaries: a `VERSION` file or
  `git describe --tags --always --dirty`.
- **Go**: inject version metadata at build time:
  ```bash
  go build -trimpath \
    -ldflags "-s -w -X main.version=$VERSION -X main.commit=$GIT_SHA" \
    -o bin/wasmgate ./cmd/wasmgate
  ```
  Add a `-version` flag that prints it.
- **C#**: set `<Version>`, `<AssemblyVersion>` and `<InformationalVersion>` in
  `Directory.Build.props`, or pass `/p:Version=$VERSION` to `dotnet publish`.
  Expose it through an admin `/version` endpoint or a CLI command.
- **gRPC contract**: package `wasmgate.control.v1`; only additive changes, never
  reuse field numbers. Bump to `v2` for a breaking change.
- **Config schema**: keep `version: 1` in YAML and validate it on both sides.
- **Releases**: identify by the config SHA-256 plus the artifact digests, not by
  a mutable “latest” pointer.

### Build

```bash
# Go data plane
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/wasmgate ./cmd/wasmgate

# C# control plane
cd control-plane
dotnet publish src/WasmGate.Control.Api -c Release -r linux-x64 --self-contained
```

Build both in CI, upload two artifacts, and attach them to the same Git tag.
The C# project does not build the Go binary; a Makefile, Taskfile or CI job
orchestrates both.

### Logs

Both services write structured logs to stdout/stderr; the platform collects
them.

- **Go**: `log/slog`. The current handler is text; add a JSON handler for
  production (`WASMGATE_LOG_FORMAT=json`) so journald/Vector/Filebeat can parse
  fields.
- **C#**: `Microsoft.Extensions.Logging`. `AddSimpleConsole` is fine for
  development; use the built-in JSON console formatter in production.
- **Useful fields**: `service`, `version`, `agent_id`, `release_id`,
  `deployment_id`, `request_id`, `listener`, `upstream`, `plugin`.
- **Correlation**: generate `deployment_id`/`request_id` in the CP, include them
  in `ApplyRelease`, and log the same values in the Go agent and in
  `ApplyResult`.
- **Audit vs logs**: audit events belong in PostgreSQL (`audit_events`) and are
  immutable; operational logs go to the log pipeline. Do not treat log files as
  an audit trail.
- **Collection**: journald → Vector/Promtail → Loki/ELK; Docker logs for
  containers; OpenTelemetry traces can be added later.
- **Never log** private keys, admin tokens, client payloads or full configs.

CI build/test logs are a separate concern: keep them as CI artifacts and use
`go test -v` / `dotnet build -v n` when a failure needs investigation.

## Native AOT publish

From `control-plane/`:

```bash
dotnet publish src/WasmGate.Control.Api \
  -c Release \
  -r linux-x64 \
  --self-contained
```

On Linux install `clang` and `zlib1g-dev`; on Windows the Native AOT toolchain
needs the MSVC build tools.

## Suggested next steps

Control plane:

1. Replace the in-memory agent/token/certificate stores with `Postgres*Repository`.
2. Implement `FileSystemArtifactStore` and `PostgresArtifactRepository`.
3. Implement `GoConfigValidator` by materializing a temporary release tree and
   running `wasmgate -config <file> -validate`.
4. Implement `IReleaseService`: config → validate → release → deployment waves.
5. Add `ApplyRelease` dispatch through `IAgentSession` and collect `ApplyResult`.
6. Add `Renew` RPC, certificate revocation check at `Session`, heartbeat
   timeout/reaper.
7. Add health-gated promotion and rollback, OIDC/RBAC, audit writes.

Data plane (Go, not in this branch yet):

1. Add the shared proto generation for Go.
2. Implement the control-plane client: dial `https://`, open `Session`, send
   `Hello`, heartbeat, receive `ApplyRelease`, send `ApplyResult`.
3. Generate an RSA key pair, build a CSR with
   `CN=wasmgate-agent:<agent-id>`, call `AgentEnrollment.Enroll`, store
   `agent.key`/`agent.crt`/`agents-ca.crt` in the state directory.
4. Reconnect using mTLS and renew the certificate before expiry.
5. Keep a last-known-good release on disk and start from it when the control
   plane is unavailable.
