# SQLite storage for the control plane — implementation plan

Status: proposed  
Branch: `feat/cp-sqlite-storage`  
Date: 2026-11-18  
Spike: [`control-plane/spikes/DapperSqliteSpike`](spikes/DapperSqliteSpike)

## 1. Summary

The control plane currently requires PostgreSQL. That is the last heavyweight
runtime dependency after the Go data plane and the Native AOT control-plane
binary. This plan makes the store selectable:

- `postgres` — unchanged, remains the default for existing deployments and fleets;
- `sqlite` — a single-file database for single-node installs, local demos and
  the prebuilt release bundle.

The control plane already assumes exactly one process: live agent sessions are
kept in memory and the operations guide says to run one CP. SQLite's
single-writer model therefore matches the existing architecture instead of
constraining it.

PostgreSQL is not removed. It stays the right choice for shared databases,
operational backups managed outside the host, and any future multi-process
deployment.

## 2. Goals

- Start the control plane with no PostgreSQL, no `psql`, and no external
  database service.
- Keep the existing PostgreSQL path behaviourally compatible.
- Make the choice explicit through configuration; never silently move an
  existing deployment to a different database.
- Preserve the Native AOT build: `dotnet publish -r linux-x64
  -p:PublishAot=true` must keep working with SQLite included.
- Keep the database small: metadata only. Artifact bytes, the agent CA, the
  release-signing key and issued certificates stay on disk exactly as today.
- Cover both providers with the same repository contract tests.

## 3. Non-goals

- Multi-process control planes or high availability. Not supported today, not
  introduced here.
- Replacing PostgreSQL in the compose fleet scenarios. It remains available as
  a profile and is still tested.
- Storing artifacts or certificates as database blobs.
- Automatic PostgreSQL → SQLite migration. New installs choose a store;
  existing installs stay where they are.
- Rewriting the control plane in Go. That is a separate, much larger decision.

## 4. Current state

Facts relevant to the plan:

- `control-plane/src/WasmGate.Control.Api/WasmGate.Control.Api.csproj` already
  sets `<PublishAot>true</PublishAot>`. The Linux AOT binary is about 24 MB and
  depends only on libc/libm.
- Persistence is 10 repository classes in
  `control-plane/src/WasmGate.Control.Infrastructure/Repositories` over
  `NpgsqlDataSource` and Dapper.AOT. The repository interfaces live in
  `WasmGate.Control.Core/Abstractions/Repositories`.
- `AssemblyInfo.cs` has `[module: DapperAot]`; this is mandatory for the AOT
  interceptors. Do not remove it.
- The schema is applied manually from `control-plane/migrations/001..006.sql`.
  There is no migration runner.
- PostgreSQL-specific SQL is limited to:
  - `jsonb` casts (`@Labels::jsonb`, `@Details::jsonb`);
  - `TEXT[]` for `agents.abi_versions`;
  - `= ANY(@ids)` in release/deployment queries;
  - `pg_advisory_xact_lock` / `pg_advisory_lock`;
  - `now()`, `timestamptz`, `bigserial`;
  - `count(*)::int`;
  - `PostgresException` handling for foreign-key violations.
- The CP keeps live sessions in memory. The deployment guide already says to run
  exactly one control-plane process.

## 5. Spike results

The spike is a standalone console app in
`control-plane/spikes/DapperSqliteSpike`. It is deliberately outside the
solution and outside production code.

Environment:

| Component | Version |
|---|---|
| .NET SDK | 10.0.404 |
| .NET runtime | 10.0.15 |
| Dapper | 2.1.89 |
| Dapper.AOT | 1.1.0 |
| Microsoft.Data.Sqlite | 10.0.15 |
| Bundled SQLite | 3.53.3 |

What was proven:

- **JIT**: all 24 checks pass. Dapper.AOT interceptors are generated for
  `SqliteConnection`.
- **Native AOT**: `dotnet publish -c Release -r linux-x64
  -p:PublishAot=true -p:StripSymbols=true` succeeds. The binary runs on
  `gcr.io/distroless/cc-debian12`, loads `libe_sqlite3.so`, and passes all
  checks.
- **Artifact size**: spike binary 4.4 MB, `libe_sqlite3.so` 1.5 MB, distroless
  image 46.2 MB (a CP image will additionally carry the 24 MB CP binary, the
  17 MB Go validator and `wwwroot`).
- **Mappings**: `Guid` ↔ TEXT, enum ↔ TEXT, `bool` from `EXISTS`,
  `IN @ids` list expansion, JSON text, nullable values, `ON CONFLICT`,
  `json_extract`, quoted `"index"`, transactions, rollback, WAL.
- **Concurrency**: 4 workers × 50 inserts through separate connections pass
  with WAL and `busy_timeout`.
- **Transactions**: `BeginTransaction(IsolationLevel.Serializable,
  deferred: false)` works and maps to `BEGIN IMMEDIATE`.

Important findings:

1. **`[module: DapperAot]` is required.** Without it Dapper emits DAP005 and
   falls back to reflection; under Native AOT that throws
   `PlatformNotSupportedException` from
   `Dapper.SqlMapper.CreateParamInfoGenerator`. The CP already has the attribute
   in `Infrastructure/AssemblyInfo.cs`.
2. **Do not map SQLite timestamps directly to `DateTime` or `DateTimeOffset`.**
   - `DateTimeOffset` throws `InvalidCastException` (same limitation as the
     existing PostgreSQL mapping, documented in `PostgresTimestamps`).
   - `DateTime` parses, but reports `DateTimeKind.Local`; on a non-UTC host the
     clock value shifts with the local timezone. This makes comparisons and
     round-trips host-dependent.
   - Decision: store timestamps as `INTEGER` Unix milliseconds (UTC) and convert
     explicitly with `DateTimeOffset.FromUnixTimeMilliseconds` /
     `ToUnixTimeMilliseconds`. Millisecond precision is enough for the control
     plane; queries already use `id` as a tie-breaker where ordering matters.
3. **Windows Native AOT needs the MSVC C++ workload.** The local Windows
   machine lacks the linker. Linux AOT in Docker is the canonical build; CI's
   `windows-latest` runner has the workload and can produce the `win-x64`
   release binary.
4. **Distroless works without extra packages.** The AOT binary only needs
   libc/libm; `libe_sqlite3.so` is copied next to it. No shell is required.

Reproduce:

```bash
cd control-plane/spikes/DapperSqliteSpike
dotnet run -c Release

docker build -t dapper-sqlite-spike .
docker run --rm dapper-sqlite-spike
```

## 6. Target architecture

### 6.1 Configuration

```text
WASMGATE_CP_STORAGE=postgres|sqlite
WASMGATE_CP_SQLITE_PATH=/var/lib/wasmgate-control/control.db
WASMGATE_CP_POSTGRES=Host=...;Port=...;Database=...;Username=...;Password=...
```

Rules:

| `STORAGE` | `POSTGRES` | `SQLITE_PATH` | Result |
|---|---|---|---|
| unset | set | — | postgres (backward compatible) |
| unset | unset | — | postgres with the existing default DSN |
| `postgres` | set | — | postgres |
| `postgres` | unset | — | fail fast: connection string required |
| `sqlite` | unset | path or default `data/control.db` | sqlite |
| `sqlite` | set | any | fail fast: contradictory configuration |
| anything else | — | — | fail fast with the accepted values |

`WASMGATE_CP_SQLITE_PATH` is created if missing. Relative paths are relative to
the process working directory, matching the other path options.

### 6.2 Composition root

`Program.cs` currently registers `NpgsqlDataSource` directly. Replace that with
one storage registration block:

```text
StartUp.GetCpOptions()
StartUp.RegisterServices()        // chooses provider-specific registrations
IStorageInitializer.Initialize()  // sqlite: directory, lock, schema, WAL
```

Provider-specific registrations:

- PostgreSQL: `NpgsqlDataSource`, `Postgres*Repository`,
  `PostgresEnvironmentOperationLock`, `PostgresStorageProbe`.
- SQLite: `SqliteConnectionFactory`, `Sqlite*Repository`,
  `SqliteEnvironmentOperationLock`, `SqliteStorageProbe`,
  `SqliteInstanceLock`, `SqliteSchemaInitializer`.

`IStorageProbe` is new and replaces the direct `NpgsqlDataSource` dependency in
`HealthEndpoints`. Both probes run `SELECT 1 FROM agents LIMIT 1` and translate
provider exceptions to HTTP 503.

### 6.3 Schema

SQLite uses the final logical schema, not the six PostgreSQL migration steps.
The schema is a raw string constant in `SqliteSchema.cs`, applied in one
transaction, with `PRAGMA user_version = 1`. Future changes append versioned
steps; `CREATE TABLE IF NOT EXISTS` alone is not a migration strategy.

Type mapping:

| PostgreSQL | SQLite |
|---|---|
| `UUID` | `TEXT` (`Guid` round-trips through Microsoft.Data.Sqlite) |
| `TIMESTAMPTZ` | `INTEGER` Unix milliseconds UTC |
| `JSONB` | `TEXT` + `CHECK (json_valid(...))` where the column is never `*` |
| `TEXT[]` | `TEXT` JSON array |
| `BIGSERIAL` | `INTEGER PRIMARY KEY AUTOINCREMENT` |
| `BOOLEAN` | none in the current schema |
| `= ANY(@ids)` | `IN @ids` with Dapper list expansion |
| `pg_advisory_*` | in-process semaphores plus a single-process file lock |
| `now()` | parameter or application clock |
| `count(*)::int` | `COUNT(*)` mapped to `long`/`int` |

Tables and constraints to preserve exactly:

- `environments` (PK `name`, seeded `prod` and `staging`);
- `config_versions` (unique `(environment, sha256)`);
- `artifacts` (PK `id`, `size >= 0`);
- `releases` + `release_artifacts` (cascade on release delete);
- `agents` (unique `name`, FK `environment`, `labels` JSON, `abi_versions`
  JSON array);
- `deployments` (FK `environment`, `message` column from migration 005);
- `deployment_waves` (PK `(deployment_id, "index")`, `selector TEXT DEFAULT '*'`);
- `enrollment_tokens` (unique `token_hash`, FK agent/environment, cascade);
- `agent_certificates` (PK `(agent_id, serial_number)`, cascade);
- `audit_events` (autoincrement id, JSON details);
- `deleted_agents` tombstones.

Indexes must match the PostgreSQL set: configuration/release/deployment
`(environment, created_at)`, agent status/last_seen, agent environment,
certificate thumbprint, enrollment expiry, audit `at`.

Foreign keys are enabled per connection with `Foreign Keys=True`. Do not rely on
the default.

### 6.4 Timestamps

New helper `SqliteTimestamps`:

```csharp
long ToParameter(DateTimeOffset value) => value.ToUnixTimeMilliseconds();
DateTimeOffset FromColumn(long value) =>
    DateTimeOffset.FromUnixTimeMilliseconds(value);
long? ToParameter(DateTimeOffset? value);
DateTimeOffset? FromColumn(long? value);
```

Sub-millisecond precision is intentionally dropped. Ordering queries that can
tie must add `id` as a secondary sort key. The existing deployment queries
already do this; audit ordering gets `id DESC` for determinism.

### 6.5 Concurrency and locking

- `PRAGMA journal_mode = WAL` once at schema initialization.
- Connection string: `Data Source=...;Mode=ReadWriteCreate;Pooling=True;Foreign
  Keys=True;Default Timeout=5`.
- No `Cache=Shared`; private caches plus WAL are the SQLite-recommended mode.
- All multi-statement writes use `BeginTransaction(IsolationLevel.Serializable,
  deferred: false)` (`BEGIN IMMEDIATE`) so two writers cannot deadlock while
  upgrading a read lock.
- `busy_timeout` (5 s) absorbs short write bursts. Do not add blind retries; log
  and surface `SQLITE_BUSY` if it still happens.
- `synchronous` stays at SQLite's default `FULL` initially. Measure before
  considering `NORMAL`; the CP write rate is low and durability is more
  important.
- `SqliteInstanceLock` holds `<db>.lock` with `FileShare.None` for the process
  lifetime. A second CP on the same database fails at startup with a clear
  message. On Unix also set `0700` on the directory and `0600` on the database
  where possible.
- `SqliteEnvironmentOperationLock` uses a `ConcurrentDictionary<string,
  SemaphoreSlim>` keyed by the same logical names as `PostgresLockKeys`. This is
  valid because the design requires one CP process per SQLite database.
- Agent upsert/delete keeps the tombstone check but relies on the write
  transaction for serialization instead of an advisory lock.

### 6.6 Readiness and lifecycle

- `/healthz` stays process-only.
- `/readyz` calls `IStorageProbe`. For SQLite, the probe reads one row from
  `agents`; a missing table or unreadable file gives 503.
- SQLite schema initialization happens before the HTTP/gRPC listeners start. A
  failure exits non-zero instead of serving a permanently unready process.
- Graceful shutdown disposes the instance lock after the host stops. WAL
  checkpoints on close; no manual checkpoint is required.

### 6.7 Backup and restore

- Add `wasmgate-control --backup <path>` (or a small `backup` subcommand) that
  opens the database read-only and runs `VACUUM INTO '<path>'`. This keeps a
  consistent copy while the CP is running and works without the `sqlite3` CLI
  in a distroless image.
- Document the full backup set: database file, artifact root, CA files, signing
  key. Losing the CA invalidates every agent certificate.
- Restore: stop the CP, replace the database file, start the CP. Do not copy a
  WAL database without its `-wal`/`-shm` files while it is running; use
  `VACUUM INTO` or stop first.

## 7. Work breakdown

### Phase 0 — spike (done)

- [x] Dapper.AOT + Microsoft.Data.Sqlite under JIT.
- [x] Native AOT publish and run on distroless.
- [x] Type mapping, list expansion, upsert, JSON, transactions, WAL.
- [x] Windows AOT prerequisite identified.

### Phase 1 — storage seam (1–2 days)

- [ ] Add `Microsoft.Data.Sqlite` 10.0.15 to
      `WasmGate.Control.Infrastructure.csproj`.
- [ ] Add `StorageProvider` enum and options to
      `Core/Configuration/ControlPlaneOptions.cs`.
- [ ] Parse `WASMGATE_CP_STORAGE`, `WASMGATE_CP_SQLITE_PATH` in `StartUp.cs`;
      implement the configuration matrix and fail-fast messages.
- [ ] Add `IStorageInitializer`, `IStorageProbe`, `SqliteConnectionFactory`,
      `SqliteInstanceLock`.
- [ ] Replace the direct `NpgsqlDataSource` registration in `Program.cs` with
      provider selection.
- [ ] Make `HealthEndpoints` depend on `IStorageProbe`.
- [ ] Keep PostgreSQL registrations exactly as they are when the provider is
      `postgres`.

### Phase 2 — schema and helpers (1–2 days)

- [ ] `SqliteSchema.cs`: full DDL, seed environments, indexes, `user_version`.
- [ ] `SqliteTimestamps.cs`: epoch milliseconds conversions.
- [ ] `SqliteJson.cs`: shared label/array serialization; reuse the existing
      ordinal dictionary comparer.
- [ ] `SqliteErrors.cs`: map `SqliteException` constraint codes to the same
      domain exceptions as the PostgreSQL repositories (notably environment
      deletion FK violations).
- [ ] Bootstrap order: create directory, acquire lock, open connection, set WAL,
      run DDL in one transaction.
- [ ] Schema tests: all tables/columns/indexes/foreign keys exist; `prod` and
      `staging` are seeded once.

### Phase 3 — repositories (5–7 days)

Implement one repository at a time, with tests, in this order (smallest first):

1. `SqliteArtifactRepository`
2. `SqliteAuditLog`
3. `SqliteEnvironmentRepository`
4. `SqliteEnrollmentTokenRepository`
5. `SqliteAgentCertificateRepository`
6. `SqliteConfigVersionRepository`
7. `SqliteAgentRepository`
8. `SqliteReleaseRepository`
9. `SqliteDeploymentRepository`
10. `SqliteEnvironmentOperationLock`

Use the existing interfaces; do not change the domain contracts unless a bug is
found. Each SQLite class opens its own pooled connection and keeps Dapper calls
in the method body so Dapper.AOT can intercept them.

### Phase 4 — concurrency and lifecycle (2–3 days)

- [ ] Single-process lock and startup failure message.
- [ ] `BEGIN IMMEDIATE` on every write transaction.
- [ ] Tombstone/upsert/delete race test.
- [ ] Enrollment `used_count` conditional update under parallel consume.
- [ ] WAL concurrency test with multiple connections.
- [ ] Graceful shutdown and lock release.
- [ ] `/readyz` provider behaviour.

### Phase 5 — scenarios and docs (2–3 days)

- [ ] `deployments/scenarios/01-local-demo`: SQLite by default; PostgreSQL as a
      compose profile/overlay.
- [ ] `03-control-plane`: SQLite by default; PostgreSQL overlay.
- [ ] `04-single-node-systemd`: remove PostgreSQL install/`Requires=`; add
      SQLite path and backup notes.
- [ ] `05-dp-fleet`: SQLite is acceptable for the single CP; document that
      Postgres is optional.
- [ ] `docs/en|ru/control-plane.md`: storage section, env vars, limitations.
- [ ] `docs/en|ru/deployment.md`: binary bundle + SQLite quickstart.
- [ ] `control-plane/README.md`: development commands for both providers.

### Phase 6 — CI and AOT smoke (2–3 days)

- [ ] `dotnet` job: run SQLite repository tests; add a PostgreSQL service
      container for contract tests.
- [ ] New `cp-aot-smoke` job: publish `linux-x64` AOT, start with SQLite, poll
      `/readyz`, shut down.
- [ ] Optional container smoke: distroless image starts and answers `/readyz`.
- [ ] Ensure `dotnet format` covers the new files.
- [ ] Keep the existing Go/Rust jobs untouched.

### Phase 7 — release packaging (separate, can run in parallel)

Tracked separately in `feat/cp-release-bundles`, but the SQLite work depends on
its decisions:

- AOT CP binary + Go validator + `wwwroot` + migrations + sample env.
- Distroless image built from published artifacts.
- `WASMGATE_CP_WASMGATE_BIN` defaults to the sibling binary.
- Windows `win-x64` binary from CI (MSVC workload is present on GitHub runners).

## 8. Repository notes

| Repository | PostgreSQL specifics | SQLite approach |
|---|---|---|
| Agent | `labels::jsonb`, `TEXT[] abi_versions`, advisory locks, `CASE/NULLIF` touch, tombstone | JSON text, `BEGIN IMMEDIATE`, tombstone in same transaction, same `CASE/NULLIF` semantics |
| Environment | `UNION` list, advisory lock, `count(*)::int`, FK exception | Same `UNION`/`COUNT`, in-process gate, `SqliteException` FK mapping |
| Enrollment token | Conditional `UPDATE used_count` | Identical SQL; atomic under SQLite's single writer |
| Config version | `ON CONFLICT (environment, sha256) DO NOTHING` | Identical |
| Artifact | `ON CONFLICT (id) DO NOTHING` | Identical |
| Release | Header + artifact rows in one transaction, `ANY(@ids)` | Header + rows in one `BEGIN IMMEDIATE` transaction, `IN @ids` |
| Deployment | Header + waves in one transaction, `ANY(@ids)`, stale `Running` cutoff | Same with epoch-ms cutoff and `IN @ids` |
| Audit | `BIGSERIAL`, JSONB details | `INTEGER PRIMARY KEY AUTOINCREMENT`, JSON text |
| Certificates | PK `(agent_id, serial)`, thumbprint index, revoke timestamp | Identical |
| Environment lock | Session advisory lock on a dedicated connection | `SemaphoreSlim` per environment for the process lifetime |

## 9. File-by-file change list

New files (expected):

```text
control-plane/src/WasmGate.Control.Infrastructure/Storage/
  IStorageInitializer.cs
  IStorageProbe.cs
  StorageProvider.cs
  SqliteConnectionFactory.cs
  SqliteInstanceLock.cs
  SqliteSchema.cs
  SqliteTimestamps.cs
  SqliteJson.cs
  SqliteErrors.cs
  SqliteStorageProbe.cs
control-plane/src/WasmGate.Control.Infrastructure/Repositories/Sqlite*.cs
control-plane/src/WasmGate.Control.Infrastructure/Repositories/SqliteEnvironmentOperationLock.cs
control-plane/tests/WasmGate.Control.Tests/Storage/
  SqliteRepositoryTests.cs
  StorageContractTests.cs
  SqliteConcurrencyTests.cs
  SqliteSchemaTests.cs
```

Changed files:

```text
control-plane/src/WasmGate.Control.Core/Configuration/ControlPlaneOptions.cs
control-plane/src/WasmGate.Control.Api/Program.cs
control-plane/src/WasmGate.Control.Api/StartUp.cs
control-plane/src/WasmGate.Control.Api/Endpoints/Health/HealthEndpoints.cs
control-plane/src/WasmGate.Control.Infrastructure/WasmGate.Control.Infrastructure.csproj
deployments/scenarios/01-local-demo/*
deployments/scenarios/03-control-plane/*
deployments/scenarios/04-single-node-systemd/*
docs/en/control-plane.md
docs/ru/control-plane.md
docs/en/deployment.md
docs/ru/deployment.md
control-plane/README.md
```

## 10. Tests

### Repository contract tests

One parameterized suite runs against both providers:

- SQLite: temp file per test class, `WAL`, foreign keys on.
- PostgreSQL: `WASMGATE_TEST_POSTGRES` environment variable; skipped when
  absent. CI supplies a `postgres:17` service container.

Cases per repository:

- Agent: insert, update, find by id/name, unique name, tombstone prevents
  resurrection, delete cascades, environment move, touch null vs empty,
  record-enrollment does not mark Online.
- Environment: ensure idempotent, list union, default cannot be deleted, agent
  guard, running-deployment guard, cascade delete, unknown environment.
- Enrollment: add/find/consume, max uses, parallel consume exactly once,
  expiry field round-trip, cascade on agent delete.
- Config: duplicate hash same environment is idempotent, same hash in another
  environment is allowed, list order.
- Artifact: duplicate id keeps first row, list/find.
- Release: header + artifacts atomic, find, list order, status update.
- Deployment: header + waves atomic, find, active list, limit, stale cutoff,
  update atomic.
- Audit: append/list limit, JSON/null details, deterministic order.
- Certificates: add/find by thumbprint/revoke.
- Lock: same environment serializes, different environments run in parallel,
  dispose releases, cancellation.

### Concurrency tests

- 4 writers × 100 audit inserts: no `SQLITE_BUSY`, exact count.
- Two concurrent upserts of the same agent: one row, no unique violation leak.
- Delete vs reconnect upsert: tombstone wins.
- Parallel enrollment consume: `used_count` never exceeds `max_uses`.

### AOT and packaging tests

- `dotnet publish -r linux-x64 -p:PublishAot=true -p:StripSymbols=true`.
- Run in `gcr.io/distroless/cc-debian12`; `/readyz` returns 200.
- Confirm `libe_sqlite3.so` is next to the binary and loaded.
- Windows publish smoke on `windows-latest`.

### End-to-end test

Extend the manual CP+DP check into CI (Linux, SQLite):

1. Start CP with SQLite and a temp admin token.
2. `POST /api/v1/agents`, start DP with the enrollment token.
3. Upload a plugin artifact, create a config and release.
4. Create a deployment; wait for `Succeeded`.
5. Curl the proxy: denied and allowed paths behave as expected.
6. Restart DP; applied release survives.
7. Restart CP; agents and releases survive in the SQLite file.

### Backup test

- `VACUUM INTO` a copy while writes are idle; open the copy and verify counts.
- Restore the copy into a clean directory; CP starts and `/readyz` is 200.

## 11. CI changes

- `dotnet` job: SQLite tests run by default; PostgreSQL service container for
  the contract suite.
- New `cp-aot-smoke`: publish and start, no Docker required for the basic
  check; optional distroless run.
- Keep Go/Rust jobs as they are. SQLite is a .NET-only change.
- Add the new test project files to the solution so `dotnet test` sees them.

## 12. Compatibility and rollout

- Existing PostgreSQL users: no config change; `WASMGATE_CP_POSTGRES` keeps
  working. `WASMGATE_CP_STORAGE` unset means PostgreSQL.
- New single-node installs: sample env files set `sqlite` explicitly.
- No automatic data migration. Document that a deployment chooses one store at
  install time; a future export/import tool can be added if demand appears.
- Rollback: switch `WASMGATE_CP_STORAGE` back to `postgres` with the original
  DSN. SQLite data written in the meantime is not merged automatically.
- PostgreSQL migrations remain the source of truth for that provider. SQLite
  schema is versioned with `PRAGMA user_version`.

## 13. Risks and mitigations

| Risk | Mitigation |
|---|---|
| Dapper.AOT does not support a SQLite construct | Spike proved the core set; contract tests per repository; fall back to hand-written `SqliteCommand` for a single repository if needed |
| Timestamp drift/timezone bugs | Epoch milliseconds only; explicit converters; tests run in a non-UTC timezone on Windows and UTC in CI |
| SQLite writer contention | WAL, `BEGIN IMMEDIATE`, 5 s busy timeout, concurrency tests; CP write rate is low |
| Two CP processes on one file | Startup lock file with a clear error; documented single-process limitation |
| Native library missing in distroless | Publish copies `libe_sqlite3.so`; AOT smoke runs in distroless |
| Schema drift between providers | Shared contract tests plus a table/column parity test |
| Accidental double configuration | Fail fast when both `STORAGE=sqlite` and `POSTGRES` are set |
| Backup of a live database | `VACUUM INTO` command; document stop-and-copy as the simple fallback |
| Windows AOT build fails for contributors | Document the MSVC C++ workload; CI builds `win-x64`; Linux AOT remains the primary release |
| Duplicate SQL maintenance | Keep PostgreSQL repositories untouched; share only serialization/timestamp/error helpers; contract tests catch drift |

## 14. Estimates

| Phase | Estimate |
|---|---|
| 0 — spike | done |
| 1 — storage seam | 1–2 days |
| 2 — schema/helpers | 1–2 days |
| 3 — repositories | 5–7 days |
| 4 — concurrency/lifecycle | 2–3 days |
| 5 — scenarios/docs | 2–3 days |
| 6 — CI/AOT smoke | 2–3 days |
| Total | ~15–20 working days |

Release packaging (Phase 7) is 3–5 additional days and can run in parallel.

## 15. Definition of done

- [ ] `WASMGATE_CP_STORAGE=sqlite` starts with no PostgreSQL and creates a
      working database.
- [ ] The same repository contract suite passes on SQLite and PostgreSQL.
- [ ] `/readyz` reflects the selected store correctly.
- [ ] AOT publish succeeds and runs in distroless.
- [ ] Concurrent write and tombstone race tests pass.
- [ ] Local demo and single-node systemd scenarios use SQLite by default.
- [ ] PostgreSQL scenarios still pass with the PostgreSQL profile.
- [ ] Backups include the database and the on-disk CA/artifacts/signing key.
- [ ] `dotnet format` and `golangci-lint` stay clean.
- [ ] No changes to the data-plane binary or plugin ABI.

## 16. Open questions

1. Should new installations default to SQLite in the sample env files while the
   code default stays PostgreSQL? Proposed: yes.
2. Do we need a PostgreSQL → SQLite export tool in the first release? Proposed:
   no; document one store per install.
3. Should `synchronous=NORMAL` be enabled after measuring, or stay `FULL`?
   Proposed: measure under the contract/load test, then decide.
4. Should the SQLite database live under `data/` next to artifacts and CA
   material, or in a separate `db/` directory? Proposed: `data/control.db` for
   simplicity; one backup root.
