# Deployment scenarios

Ready-to-use assets for the five common deployments. Each scenario has its own
README; shared Dockerfiles and scripts stay at the top level.

| # | Scenario | Use when | Assets |
|---|---|---|---|
| 1 | [Local demo](scenarios/01-local-demo/) | try CP + DP + backend on one machine | compose, config, `.env.example` |
| 2 | [Standalone data plane](scenarios/02-standalone-data-plane/) | proxy without a control plane | compose, YAML, systemd unit |
| 3 | [Control plane only](scenarios/03-control-plane/) | agents run elsewhere | compose, `.env.example` |
| 4 | [Single-node systemd](scenarios/04-single-node-systemd/) | one VM runs CP + DP | systemd units, env/YAML examples |
| 5 | [Data-plane fleet](scenarios/05-dp-fleet/) | one CP and many DPs | fleet script, YAML, `.env.example` |

## Shared assets

```text
docker/
  control-plane.Dockerfile    Native self-contained CP image + Go validator
  data-plane.Dockerfile       static Go proxy image
scripts/
  dev-certs.sh                self-signed CP TLS certificate for local Docker
```

Generated certificates go to `deployments/certs/` and are gitignored.

## Observability

[`observability/`](observability/) contains a Prometheus + Grafana stack with a
provisioned **WasmGate overview** dashboard. It scrapes both the data plane and
the control plane; see its [README](observability/README.md) for local-demo and
fleet wiring.

## Conventions

- Copy `.env.example` to `.env` and edit it. `.env` files are ignored by git;
  `.env.example` files are committed.
- Every scenario that uses the control plane needs its server certificate and
  the release-signing public key on the agent.
- The control plane rewrites the managed `wasmgate.yaml` and installs artifacts
  relative to the configuration directory. That directory must be writable for
  a managed agent; a standalone agent can keep it read-only.
- The CP admin API is HTTP with a bearer token. Keep it on loopback/private
  networks; use an SSH tunnel or a TLS reverse proxy for remote operators.

## Product documentation

- [Deployment guide (English)](../docs/en/deployment.md)
- [Руководство по развёртыванию](../docs/ru/deployment.md)
- [Control plane (English)](../docs/en/control-plane.md)
- [Control plane (Русский)](../docs/ru/control-plane.md)
