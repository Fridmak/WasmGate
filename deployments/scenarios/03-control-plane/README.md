# Control plane only

Use this scenario when the control plane and PostgreSQL run on one host and the
data planes run somewhere else: separate VMs, Kubernetes, or another compose
project.

## Start

```bash
./deployments/scripts/dev-certs.sh

cp deployments/scenarios/03-control-plane/.env.example \
   deployments/scenarios/03-control-plane/.env
# edit .env and set WASMGATE_CP_ADMIN_TOKEN (openssl rand -hex 24)

docker compose -f deployments/scenarios/03-control-plane/compose.yml up -d --build

curl -fsS http://127.0.0.1:9097/readyz
```

Open `http://127.0.0.1:9097/` and paste the admin token.

## Add a data plane

1. In the UI choose an environment and create an agent.
2. Save the agent id and the one-time enrollment token.
3. Start the data plane on its own host with:

```bash
wasmgate \
  -config /etc/wasmgate/wasmgate.yaml \
  -cp https://<cp-host>:9096 \
  -cp-server-ca /etc/wasmgate/cp-server.crt \
  -cp-release-key /etc/wasmgate/release-signing.key.pub \
  -cp-token <ENROLL_TOKEN> \
  -cp-agent-id <AGENT_ID> \
  -cp-name dp-01 \
  -cp-state /var/lib/wasmgate/agent
```

`cp-server.crt` is `deployments/certs/server.crt` for the compose demo. The
release-signing public key is inside the `cpdata` volume at
`/data/keys/release-signing.key.pub`:

```bash
docker compose -f deployments/scenarios/03-control-plane/compose.yml exec control-plane \
  cat /data/keys/release-signing.key.pub > release-signing.key.pub
```

For many agents, see [`../05-dp-fleet`](../05-dp-fleet).

## Notes

- Migrations are applied by PostgreSQL only when `pgdata` is first initialized.
  For an existing database run `control-plane/migrations/001..006` manually.
- The data plane connects **outbound** to `9096`; no inbound port on the agent is
  needed for management.
- The control plane keeps live sessions in memory. Run exactly one CP instance
  unless the session registry is redesigned.
- Publish `9097` only to operators (loopback or a private network), never to the
  public internet without TLS and an additional proxy.
- For agents on other hosts, regenerate the server certificate with the address
  they actually dial (`IP:<private-ip>` or `DNS:<cp-host>`) in `subjectAltName`.
