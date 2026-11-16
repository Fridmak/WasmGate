#!/usr/bin/env bash
# Starts N independent data planes against one control plane.
#
# Required environment:
#   CP_URL          control-plane gRPC URL (https://...)
#   CP_SERVER_CRT   CP server certificate
#   RELEASE_PUB_KEY release-signing public key
#   ENROLL_TOKEN    shared registration token
#   CONFIG_FILE     initial wasmgate.yaml
#
# Optional:
#   COUNT=20, NAME_PREFIX=edge, DP_IMAGE=wasmgate-dp, NETWORK=bridge
#   LABELS=role=edge, VOLUME_PREFIX=wasmgate-dp
#
# Each container gets its own named volume and its own agent identity. Do not
# share a state volume between replicas.
set -euo pipefail

: "${CP_URL:?set CP_URL to the CP gRPC endpoint, for example https://control-plane:9096}"
: "${CP_SERVER_CRT:?set CP_SERVER_CRT to the CP server certificate}"
: "${RELEASE_PUB_KEY:?set RELEASE_PUB_KEY to release-signing.key.pub}"
: "${ENROLL_TOKEN:?set ENROLL_TOKEN to a registration token}"
: "${CONFIG_FILE:?set CONFIG_FILE to the initial wasmgate.yaml}"

COUNT="${COUNT:-20}"
NAME_PREFIX="${NAME_PREFIX:-edge}"
DP_IMAGE="${DP_IMAGE:-wasmgate-dp}"
NETWORK="${NETWORK:-bridge}"
LABELS="${LABELS:-role=edge}"
VOLUME_PREFIX="${VOLUME_PREFIX:-wasmgate-dp}"

if ! [[ "$COUNT" =~ ^[1-9][0-9]*$ ]]; then
    echo "COUNT must be a positive integer" >&2
    exit 1
fi

# Docker -v treats a relative source as a named volume, not a host path.
absolute_path() {
    case "$1" in
        /*) printf '%s\n' "$1" ;;
        [A-Za-z]:[\\/]*) printf '%s\n' "$1" ;;
        *) printf '%s/%s\n' "$(pwd)" "$1" ;;
    esac
}

CP_SERVER_CRT="$(absolute_path "$CP_SERVER_CRT")"
RELEASE_PUB_KEY="$(absolute_path "$RELEASE_PUB_KEY")"
CONFIG_FILE="$(absolute_path "$CONFIG_FILE")"

for path in "$CP_SERVER_CRT" "$RELEASE_PUB_KEY" "$CONFIG_FILE"; do
    if [[ ! -f "$path" ]]; then
        echo "file not found: $path" >&2
        exit 1
    fi
done

for i in $(seq -w 1 "$COUNT"); do
    name="${NAME_PREFIX}-${i}"
    volume="${VOLUME_PREFIX}-${i}"

    if docker container inspect "$name" >/dev/null 2>&1; then
        echo "skip: container $name already exists"
        continue
    fi

    docker volume create "$volume" >/dev/null

    # Seed the writable volume once. The control plane rewrites this file on
    # every deployment, so the bind mount must not be read-only.
    docker run --rm \
        -v "$volume:/data" \
        -v "$CONFIG_FILE:/seed.yaml:ro" \
        alpine:3.22 \
        sh -c 'test -f /data/wasmgate.yaml || cp /seed.yaml /data/wasmgate.yaml; chown -R 10001:10001 /data'

    docker run -d \
        --name "$name" \
        --restart unless-stopped \
        --network "$NETWORK" \
        -v "$volume:/data" \
        -v "$CP_SERVER_CRT:/certs/server.crt:ro" \
        -v "$RELEASE_PUB_KEY:/certs/release-signing.key.pub:ro" \
        "$DP_IMAGE" \
        -config /data/wasmgate.yaml \
        -cp "$CP_URL" \
        -cp-server-ca /certs/server.crt \
        -cp-release-key /certs/release-signing.key.pub \
        -cp-token "$ENROLL_TOKEN" \
        -cp-name "$name" \
        -cp-labels "$LABELS" \
        -cp-state /data/agent

    echo "started: $name (volume $volume)"
done

echo
echo "Started $COUNT data planes."
echo "Put an L4 load balancer in front of them; no host ports are published."
