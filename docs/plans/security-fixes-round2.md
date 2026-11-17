# Security fixes from round 2 — plan and status

- **Branch:** `feat/plugin-http-headers`
- **Source report:** `WasmGate-security-report-round2.md` (review of `main` @ `4d9448f`)
- **Status:** implemented in this branch; DPCP-1 remains a separate design item

## Accepted findings and fixes

| ID | Fix in this branch | Verification |
|---|---|---|
| **WG-02** | Every hook now has a deadline: `Options.Timeout == 0` selects `DefaultHookTimeout` (1s), `WithCloseOnContextDone(true)` is always enabled. Stack logic rejects `timeout: 0` for headers mode. | `TestPluginDefaultTimeoutStopsInfiniteLoop`, existing timeout tests, race run |
| **WG-26** | Compilation, warm-up and every lazy instantiation run under `DefaultLoadTimeout` (5s); `pool.New` no longer uses `context.Background()` without a deadline. | plugin load/instantiation tests; code review |
| **NEW-1** | The full guest state window (`stateCap`) is zeroed before each stateful call; the previous `uMax`-only clear is removed. | state hook tests |
| **NEW-2** | The full 32 KiB data scratch is zeroed whenever a pooled instance is reused for a different connection; headers mode always zeroes it before `on_headers`. | `TestPluginScratchClearedAcrossConnections` |
| **CPSEC-1** | A shared registration token (`AgentId == null`) now rejects enrollment of an existing agent. Existing agents need a per-agent token or renewal. | `AgentProvisioningService` logic; build |
| **CPDATA-1** | `ArtifactDownloadService` verifies the client certificate CN, computes its thumbprint and requires `IsActiveAsync` before streaming any artifact. Revoked/expired certificates are rejected. | build; logic mirrors `AgentControlService` |
| **CPDATA-2** | An apply result is accepted only when the reporting agent exists, belongs to the deployment environment, matches the deployment release id, and matches at least one wave selector. | build; `WaveSelector` tests |
| **DPCP-2** | Agents can be started with `-cp-environment`; a signed manifest for another environment is rejected before parsing/applying the configuration. | build; manifest parser tests |

## Explicitly not changed here

### DPCP-1 — signed release replay/downgrade

The agent cannot distinguish a malicious replay of an old release from an
operator-issued rollback: both are the same signed `ApplyRelease` command. A
correct fix needs a **deployment-level signed envelope**:

- sign `deployment_id`, `release_id`, a monotonic deployment sequence and an
  expiry/not-before with the control-plane key;
- store the highest accepted deployment sequence in the agent state;
- allow rollback by creating a new signed deployment command with a higher
  sequence;
- reject replayed or expired commands before writing the configuration.

This changes the proto/CP/agent contract and rollback semantics, so it should
be a separate design and migration, not a quick patch. Until then, `stream`
and normal CP trust assumptions apply; the report's downgrade scenario remains
documented as a known limitation.

### Lower-priority hardening still open

- WG-20 SNI parsing of split records: SNI is routing, not authentication; fix
  only if SNI is used as an ACL.
- WG-22 listener name snapshot during address-swap reload: operator-controlled
  race; low.
- WG-25 remaining validation bounds (`allow_cidrs 0.0.0.0/0`, second YAML
  document, health-check lower bound, self-loop addresses).
- WG-28 operational reload diagnostics (`ErrorLog`, rejected `admin_addr`
  changes, round-robin reset).
- WG-29a circuit-breaker Half-Open currently admits all traffic; bound it with
  `half_open_requests`.
- WG-30 supply chain: bump `golang.org/x/net`, add `govulncheck`, pin CI
  actions and permissions.
- DPCP-3: when `-cp-server-ca` is empty, HTTPS currently falls back to system
  roots; consider requiring an explicit opt-in for public-CA deployments.

## Required verification before merge

```bash
gofmt -l cmd internal
go vet ./...
go test ./...
CGO_ENABLED=1 go test -race ./internal/plugin ./internal/proxy ./internal/tlspeek
dotnet build control-plane/WasmGate.Control.sln -c Debug
dotnet test control-plane/WasmGate.Control.sln -c Debug
wasmgate -validate -config configs/showcase.yaml
```

## Notes

- `NEW-2` clearing is connection-scoped for the hot `stream` path: the first
  chunk of a new connection zeroes the scratch, not every chunk. Headers mode
  zeroes the whole window before its single call. This keeps the body path fast
  while closing cross-connection leakage.
- `CPDATA-1` and `CPDATA-2` are defense-in-depth at the application layer; mTLS
  certificate revocation still cannot be enforced by the TLS stack itself.
