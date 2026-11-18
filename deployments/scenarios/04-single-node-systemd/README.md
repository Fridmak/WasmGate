# Single-node systemd: control plane + data plane on one VM

Use this for a small production-like stand: SQLite, the control plane and one
data plane on the same host. The agent connects to the control plane over
loopback; nothing on the management path is public.

## Layout

```text
/opt/wasmgate-control/                 # published control-plane binary and wwwroot
/var/lib/wasmgate-control/             # CP state: SQLite DB, CA, keys, artifacts
/etc/wasmgate-control/cp.env           # CP secrets and paths
/etc/wasmgate-control/tls/             # gRPC server certificate/key

/usr/local/bin/wasmgate                # data-plane binary (also used by CP -validate)
/etc/wasmgate/wasmgate.yaml            # CP-managed config; writable by the service
/etc/wasmgate/agent.env                # enrollment token + agent id
/etc/wasmgate/cp-server.crt            # CP server certificate trusted by the agent
/etc/wasmgate/release-signing.key.pub  # release signature verification key
/var/lib/wasmgate/agent/               # agent certificate and applied state
```

## 1. Storage

SQLite is built in and needs no extra packages. The database is created on first
start at the path from `WASMGATE_CP_SQLITE_PATH` (the example uses
`/var/lib/wasmgate-control/data/control.db`). Back up that file together with the
CA, signing key and artifacts.

## 2. Binaries

Build the Go data plane:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags="-s -w" -o wasmgate ./cmd/wasmgate
```

Publish the control plane self-contained (no .NET SDK on the VM):

```bash
dotnet publish control-plane/src/WasmGate.Control.Api \
  -c Release -r linux-x64 --self-contained true \
  -p:PublishAot=false -p:PublishTrimmed=false \
  -o /tmp/wasmgate-control-publish
```

Install:

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin wasmgate 2>/dev/null || true
sudo install -d -m 0755 /opt/wasmgate-control
sudo cp -a /tmp/wasmgate-control-publish/. /opt/wasmgate-control/
sudo chmod 0755 /opt/wasmgate-control/wasmgate-control

sudo install -m 0755 wasmgate /usr/local/bin/wasmgate

sudo install -d -m 0750 -o wasmgate -g wasmgate /var/lib/wasmgate-control
sudo install -d -m 0755 /etc/wasmgate-control
sudo install -d -m 0750 -o root -g wasmgate /etc/wasmgate-control/tls

sudo install -d -m 0750 -o wasmgate -g wasmgate /etc/wasmgate
sudo install -d -m 0750 -o wasmgate -g wasmgate /var/lib/wasmgate
sudo install -m 0640 -o wasmgate -g wasmgate \
  deployments/scenarios/04-single-node-systemd/wasmgate.yaml \
  /etc/wasmgate/wasmgate.yaml
```

## 3. TLS for the agent endpoint

The certificate must contain **every name/IP agents use**. For a same-VM agent
that is `127.0.0.1` and `localhost`:

```bash
sudo openssl req -x509 -newkey rsa:2048 -sha256 -days 365 -nodes \
  -keyout /etc/wasmgate-control/tls/server.key \
  -out /etc/wasmgate-control/tls/server.crt \
  -subj "/CN=wasmgate-cp" \
  -addext "subjectAltName=IP:127.0.0.1,DNS:localhost"

sudo chown root:wasmgate /etc/wasmgate-control/tls/server.key \
  /etc/wasmgate-control/tls/server.crt
sudo chmod 0640 /etc/wasmgate-control/tls/server.key
sudo chmod 0644 /etc/wasmgate-control/tls/server.crt
```

For agents on other hosts, add the private IP or DNS name to `subjectAltName`
and change `-cp` in the agent unit accordingly.

## 4. Control plane

```bash
sudo install -m 0640 -o root -g wasmgate \
  deployments/scenarios/04-single-node-systemd/cp.env.example \
  /etc/wasmgate-control/cp.env
# edit: database password, admin token, paths

sudo cp deployments/scenarios/04-single-node-systemd/wasmgate-control.service \
  /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now wasmgate-control
sudo systemctl status wasmgate-control

curl -fsS http://127.0.0.1:9097/readyz
```

Open the UI over an SSH tunnel if the host is remote:

```bash
ssh -L 9097:127.0.0.1:9097 yc-user@<host>
```

## 5. Create the agent

In the UI:

1. Environment: `prod` (or create another).
2. Agents → New agent.
3. Name: `dp-01`, labels: `role=demo`.
4. Copy the agent GUID and the one-time token.

Copy the trust material to the agent directory:

```bash
sudo install -m 0644 /etc/wasmgate-control/tls/server.crt \
  /etc/wasmgate/cp-server.crt
sudo install -m 0644 /var/lib/wasmgate-control/data/keys/release-signing.key.pub \
  /etc/wasmgate/release-signing.key.pub
```

## 6. Data plane

```bash
sudo install -m 0640 -o root -g wasmgate \
  deployments/scenarios/04-single-node-systemd/agent.env.example \
  /etc/wasmgate/agent.env
# edit: token, agent id, name, labels

sudo cp deployments/scenarios/04-single-node-systemd/wasmgate-managed.service \
  /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now wasmgate
sudo systemctl status wasmgate
```

The agent should become `Online` in the UI within a few seconds.

## 7. First release

1. Configurations → New configuration: paste
   [`wasmgate.yaml`](wasmgate.yaml) or your own YAML.
2. Releases → New release: choose the configuration; add artifacts if the YAML
   references plugins.
3. Deployments → New deployment: choose the release, selector `*`.

The CP installs artifacts and rewrites `/etc/wasmgate/wasmgate.yaml`; the agent
reloads without dropping established connections. `curl` the listener to check.

## Operations

```bash
sudo systemctl status wasmgate-control wasmgate
sudo journalctl -u wasmgate-control -n 100 --no-pager
sudo journalctl -u wasmgate -n 100 --no-pager
```

- Back up the whole `/var/lib/wasmgate-control` directory (SQLite database, CA,
  signing key, artifacts). For a consistent database copy while the CP runs:
  `wasmgate-control --backup /var/backups/wasmgate/control.db`. Losing the CA
  invalidates every agent certificate.
- The control plane keeps live sessions in memory. Run exactly one CP process
  per SQLite database.
- The admin API is HTTP with a bearer token. Keep it on loopback/private
  networks; for remote operators use an SSH tunnel or a TLS reverse proxy.
