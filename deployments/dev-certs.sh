#!/usr/bin/env bash
# Creates a self-signed TLS certificate for the compose demo.
#
#   ./deployments/dev-certs.sh
#
# The control plane serves the agent gRPC endpoint over TLS, and agents trust
# this certificate through -cp-server-ca. The key is development material: never
# use it outside a local environment.
set -euo pipefail

dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/certs"
mkdir -p "$dir"

if [[ -f "$dir/server.crt" && -f "$dir/server.key" ]]; then
    echo "certificates already exist in $dir"
    exit 0
fi

openssl req -x509 -newkey rsa:2048 -sha256 -days 365 -nodes \
    -keyout "$dir/server.key" \
    -out "$dir/server.crt" \
    -subj "/CN=control-plane" \
    -addext "subjectAltName=DNS:control-plane,DNS:localhost,IP:127.0.0.1"

chmod 600 "$dir/server.key"
echo "created $dir/server.crt and $dir/server.key"
