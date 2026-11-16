#!/usr/bin/env bash
# Creates the bearer-token files Prometheus mounts, from the current shell env.
#
#   export WASMGATE_ADMIN_TOKEN=...       # data-plane admin token
#   export WASMGATE_CP_ADMIN_TOKEN=...    # control-plane admin token
#   ./deployments/observability/setup.sh
set -euo pipefail

: "${WASMGATE_ADMIN_TOKEN:?set WASMGATE_ADMIN_TOKEN to the data-plane admin token}"
: "${WASMGATE_CP_ADMIN_TOKEN:?set WASMGATE_CP_ADMIN_TOKEN to the control-plane admin token}"

dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/secrets"
mkdir -p "$dir"

printf '%s' "$WASMGATE_ADMIN_TOKEN" > "$dir/wasmgate-admin-token"
printf '%s' "$WASMGATE_CP_ADMIN_TOKEN" > "$dir/cp-admin-token"

# Prometheus runs as an unprivileged user in its container. The files are
# read-only inside the container; keep them out of git via .gitignore.
chmod 0644 "$dir/wasmgate-admin-token" "$dir/cp-admin-token"
echo "wrote $dir/wasmgate-admin-token and $dir/cp-admin-token"
