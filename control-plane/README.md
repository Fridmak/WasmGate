# WasmGate Control Plane

The control plane is the C#/.NET 10 service in this directory. Documentation:

- [English](../docs/en/control-plane.md)
- [Русский](../docs/ru/control-plane.md)

Build and test from here:

```bash
dotnet build WasmGate.Control.sln
dotnet test  WasmGate.Control.sln
```

SQLite is the default in the deployment scenarios and needs no service. To run
the gated PostgreSQL integration tests, set `WASMGATE_TEST_POSTGRES` to a
connection string with `control-plane/migrations` applied:

```bash
export WASMGATE_TEST_POSTGRES='Host=127.0.0.1;Port=5432;Database=wasmgate;Username=wasmgate;Password=wasmgate'
dotnet test WasmGate.Control.sln
```

Storage design and migration notes: [`SQLITE-STORAGE-PLAN.md`](SQLITE-STORAGE-PLAN.md).

Agent protocol, admin API, certificates and the admin UI are described in the
guide linked above.
