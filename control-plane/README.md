# WasmGate Control Plane

The control plane is the C#/.NET 10 service in this directory. Documentation:

- [English](../docs/en/control-plane.md)
- [Русский](../docs/ru/control-plane.md)

Build and test from here:

```bash
dotnet build WasmGate.Control.sln
dotnet test  WasmGate.Control.sln
```

SQLite is built in and needs no service. Storage design, backup and migration
notes: [`SQLITE-STORAGE-PLAN.md`](SQLITE-STORAGE-PLAN.md).

Agent protocol, admin API, certificates and the admin UI are described in the
guide linked above.
