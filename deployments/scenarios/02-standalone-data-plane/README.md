# Standalone data plane

Use this when you need only the Go proxy, without a control plane. The proxy is
configured by one YAML file and keeps working forever without external services.

## Docker

```bash
docker compose -f deployments/scenarios/02-standalone-data-plane/compose.yml up --build
curl http://127.0.0.1:8080/
```

The compose file starts an nginx demo backend and a WasmGate listener that
proxies to it. Replace `pools.backend.servers` with your real upstreams.

## systemd

The unit in [`wasmgate.service`](wasmgate.service) runs the binary as an
unprivileged user and reads `/etc/wasmgate/wasmgate.yaml`. It is read-only
because a standalone proxy never rewrites its own configuration.

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin wasmgate 2>/dev/null || true
sudo install -m 0755 wasmgate /usr/local/bin/wasmgate
sudo install -d -m 0750 -o wasmgate -g wasmgate /etc/wasmgate
sudo install -m 0640 -o wasmgate -g wasmgate \
  deployments/scenarios/02-standalone-data-plane/wasmgate.yaml \
  /etc/wasmgate/wasmgate.yaml
sudo cp deployments/scenarios/02-standalone-data-plane/wasmgate.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now wasmgate
```

Validate before starting:

```bash
wasmgate -config /etc/wasmgate/wasmgate.yaml -validate
```

Reload after editing the file:

```bash
sudo systemctl reload wasmgate
```

## Layout

```text
/usr/local/bin/wasmgate
/etc/wasmgate/wasmgate.yaml
/etc/wasmgate/plugins/...       # optional .wasm files
/etc/wasmgate/certs/...         # optional TLS material
```

Relative plugin and TLS paths resolve from the configuration directory, so the
process can start from any working directory.

## Notes

- No control plane means no remote release, rollback, audit, or certificate
  rotation. Change the file and reload.
- Keep the admin endpoint (`admin_addr`, metrics/health/reload) on loopback.
- For a managed fleet with releases and rollback, see [`../03-control-plane`](../03-control-plane)
  and [`../05-dp-fleet`](../05-dp-fleet).
