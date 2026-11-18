# SQLite storage for the control plane

Status: implemented; PostgreSQL removed
Branch: `feat/cp-sqlite-storage-impl`
Date: 2026-11-18  
Spike: [`control-plane/spikes/DapperSqliteSpike`](spikes/DapperSqliteSpike)

## 1. Decision

The control plane uses one local SQLite database. PostgreSQL support was
removed in the same branch because the current architecture does not benefit
from it:

- the control plane must run as exactly one process: live agent sessions are in
  memory;
- PostgreSQL would not provide HA or multi-instance by itself;
- the write volume is low (enrollment, deployments, audit), so SQLite's single
  writer is not a bottleneck;
- artifacts, the agent CA and the release-signing key already require a
  persistent local volume, so PostgreSQL would not make the CP stateless;
- maintaining two repository implementations doubles the code, tests, schema
  and deployment surface without a current product need.

If a future release needs multiple CP instances, a managed external database or
`ReadWriteMany` storage, reintroduce a storage interface and an external
provider behind it. The SQLite implementation remains the single-node default.

## 2. What is stored where

| Storage | Content |
|---|---|
| SQLite (`/data/control.db` or `data/control.db`) | agents, certificates, enrollment tokens, configuration versions, artifact metadata, releases, deployments, waves, audit |
| `/data/artifacts` | artifact bytes, addressed by SHA-256 |
| `/data/ca` | agent CA certificate and private key |
| `/data/keys` | release signing key (ECDSA P-256) and public key |
| Process memory | live agent sessions |

## 3. Configuration

```text
WASMGATE_CP_SQLITE_PATH=/var/lib/wasmgate-control/data/control.db
```

Default: `data/control.db`, relative to the process working directory. The
parent directory is created on startup. The database file and lock file are
`0600` on Unix; the parent directory is left at its existing permissions so a
data plane running as a different UID can read the public release key from a
read-only mount.

## 4. Schema and type mapping

The schema is a raw string in `SqliteSchema.cs`, applied in one transaction and
versioned with `PRAGMA user_version`. The logical tables match the previous
control-plane model.

| Logical type | SQLite representation |
|---|---|
| UUID | `TEXT` |
| timestamp | `INTEGER` Unix milliseconds UTC |
| JSON object/array | `TEXT` with `json_valid` checks where appropriate |
| `BIGSERIAL` audit id | `INTEGER PRIMARY KEY AUTOINCREMENT` |
| lists (`abi_versions`) | JSON array text |
| advisory locks | in-process semaphores + single-process lock file |

Timestamps use Unix milliseconds because direct `DateTimeOffset` mapping throws
and direct `DateTime` mapping is timezone-sensitive. This was verified in the
spike and is covered by repository tests.

## 5. Concurrency and lifecycle

- `PRAGMA journal_mode = WAL` once at initialization.
- Connection string: `Foreign Keys=True`, `Pooling=True`, `Default Timeout=5`.
- All multi-statement writes use `BEGIN IMMEDIATE`
  (`BeginTransaction(IsolationLevel.Serializable, deferred: false)`).
- `busy_timeout` absorbs short write bursts.
- `SqliteInstanceLock` holds `<db>.lock` with `FileShare.None`; a second CP exits
  with a clear error.
- `SqliteEnvironmentOperationLock` serializes environment deletion against
  enrollment in the single process.
- `/readyz` depends on `SqliteStorageProbe`, which queries the `agents` table.

## 6. Backup

```bash
wasmgate-control --backup /var/backups/wasmgate/control.db
```

`VACUUM INTO` creates a consistent, compact copy while the CP is running. The
destination must not exist. The admin UI also exposes this as
**Dashboard → Maintenance → Download SQLite backup**, which calls
`POST /api/v1/backup` and streams the snapshot. Back up the database together
with the CA, signing key and artifacts.

## 7. Local mode

A control plane and data plane on one machine can run without any TLS or agent
certificates, enrollment token or release key on the data plane:

```text
WASMGATE_CP_LOCAL=true          # control plane: loopback plaintext only
wasmgate -cp-local              # data plane: loopback, no client certificate
```

Local mode disables enrollment and renewal, creates no agent CA, and the data
plane persists only `agent.id` plus applied-release state. Release manifests are
still parsed and matched, but not signature-verified because no release key is
configured on the data plane; the control plane may still keep its own signing
key for normal release creation. This is a single-host development/demo mode, not a fleet mode.

## 8. Verification

Environment: .NET SDK 10.0.404, runtime 10.0.15, Microsoft.Data.Sqlite 10.0.15,
SQLite 3.53.3.

```text
dotnet build -c Release                          pass, 0 warnings
dotnet format --verify-no-changes                pass
dotnet test                                      106 pass
Go tests                                        all pass
Native AOT container + SQLite                    /readyz, agent create, restart
local mode CP + DP + all 9 example plugins      routes and guards verified, no cert/token/release-key
compose TLS: enroll, plugin deploy, restart     mTLS session and policy survive
backup CLI + POST /api/v1/backup                snapshot readable
second CP on same database                      fails with the lock message
```

The SQLite spike proves the Dapper.AOT + Microsoft.Data.Sqlite combination under
Native AOT and in a distroless image.

## 9. Known limitations

- Exactly one control-plane process per database file.
- The database must live on local disk, not NFS.
- No built-in replication, PITR or HA. Use SQLite backups and the on-disk
  artifact/CA backup.
- If the deployment requires an external managed database, this is a deliberate
  future extension, not a supported mode today.

## 10. Completed phases

- [x] Storage abstraction and SQLite connection/schema/initializer.
- [x] SQLite repositories for all control-plane entities.
- [x] WAL, `BEGIN IMMEDIATE`, single-process lock and readiness probe.
- [x] SQLite integration tests, concurrency tests, backup test.
- [x] Local mode: no TLS, agent certificates, enrollment tokens or release key.
- [x] Backup CLI and the admin UI download endpoint.
- [x] Local demo, control-plane-only and single-node systemd scenarios use
      SQLite only.
- [x] EN/RU documentation and CI AOT smoke.
- [x] PostgreSQL code, migrations, tests, compose overlays and CI service
      removed.
- [x] `--backup` command.
