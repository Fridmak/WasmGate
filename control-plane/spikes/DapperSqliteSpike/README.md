# Dapper.AOT + SQLite spike

This is a throwaway verification project, not production code and not part of
`WasmGate.Control.sln`. It answers one question before the SQLite storage work
starts:

> Do Dapper.AOT, `Microsoft.Data.Sqlite` and Native AOT work together, and can
> they represent the types the control plane uses?

The answer is yes. See the results below and
[`../../SQLITE-STORAGE-PLAN.md`](../../SQLITE-STORAGE-PLAN.md) for the full
implementation plan.

## What it checks

- Dapper.AOT interceptors are generated for `SqliteConnection`.
- Native AOT publish succeeds and the binary runs without a .NET runtime.
- The bundled `libe_sqlite3.so` is copied and loaded.
- `Guid` ↔ TEXT, enum ↔ TEXT, `bool` from `EXISTS`, JSON text, nullable values.
- `IN @ids` list expansion, `ON CONFLICT`, `LIMIT`, quoted `"index"`.
- Transactions, rollback, `BEGIN IMMEDIATE` (`deferred: false`).
- WAL and concurrent writers (4 workers × 50 inserts).
- Timestamp behaviour is measured, not assumed: direct `DateTimeOffset`
  mapping fails and direct `DateTime` mapping is timezone-sensitive.

## Run with JIT

```bash
dotnet run -c Release
```

Expected tail:

```text
PASS  concurrent writers (WAL)  [rows=200]
PASS  database file created  [~68 KiB]

SPIKE RESULT: PASS
```

## Run under Native AOT in distroless

```bash
docker build -t dapper-sqlite-spike .
docker run --rm dapper-sqlite-spike
```

The final image is `gcr.io/distroless/cc-debian12`; there is no shell, no .NET
runtime and no SDK. The spike binary is ~4.4 MB and `libe_sqlite3.so` is ~1.5 MB.

## Observed results

Environment: .NET SDK 10.0.404, runtime 10.0.15, Dapper 2.1.89, Dapper.AOT
1.1.0, Microsoft.Data.Sqlite 10.0.15, SQLite 3.53.3.

- JIT: 24/24 checks pass.
- Linux Native AOT: publish succeeds with
  `-p:PublishAot=true -p:StripSymbols=true -p:DebugType=None`; all checks pass
  on distroless.
- Distroless image size for this console app: 46.2 MB.
- Windows Native AOT requires the Visual Studio "Desktop development with C++"
  workload (the MSVC linker). Linux AOT in Docker is the canonical build for
  this spike; GitHub `windows-latest` runners have the workload.

## Critical findings

1. `[module: DapperAot]` is mandatory.
   Without it Dapper emits DAP005, falls back to reflection, and throws
   `PlatformNotSupportedException` under Native AOT. The control plane already
   has this attribute in
   [`Infrastructure/AssemblyInfo.cs`](../../src/WasmGate.Control.Infrastructure/AssemblyInfo.cs).
2. Do not map SQLite timestamps directly.
   - `DateTimeOffset` throws `InvalidCastException`.
   - `DateTime` parses but reports `DateTimeKind.Local`; on a non-UTC host the
     value shifts with the timezone.
   - Store Unix milliseconds in `INTEGER` and convert explicitly. This is the
     decision in the implementation plan.
3. `IN @ids` list expansion works; the PostgreSQL `= ANY(@ids)` queries can be
   rewritten per provider without custom SQL generation.
