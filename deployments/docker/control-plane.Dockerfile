# syntax=docker/dockerfile:1
#
# Control-plane image: Native AOT build of the C# service plus the Go data-plane
# binary, which the control plane invokes as `wasmgate -validate`.
# Build from the repository root:
#   docker build -f deployments/docker/control-plane.Dockerfile -t wasmgate-cp .

# The validator binary must come from the same repository revision as the
# service, so it is built in its own stage.
FROM golang:1.27-alpine AS dataplane
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/wasmgate ./cmd/wasmgate

FROM mcr.microsoft.com/dotnet/sdk:10.0 AS build
ARG VERSION=0.0.0-dev
RUN apt-get update \
    && apt-get install -y --no-install-recommends clang zlib1g-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY control-plane/ ./control-plane/
RUN dotnet publish control-plane/src/WasmGate.Control.Api \
    -c Release -r linux-x64 --self-contained \
    -p:Version="${VERSION#v}" -p:InformationalVersion="${VERSION}" \
    -p:StripSymbols=true -p:DebugType=None \
    -o /app

FROM mcr.microsoft.com/dotnet/runtime-deps:10.0
WORKDIR /app

COPY --from=build /app ./
COPY --from=dataplane /out/wasmgate /app/wasmgate

RUN mkdir -p /data && chown -R 1654:1654 /data
VOLUME ["/data"]

# Paths below point into the writable volume. The default listeners stay on
# loopback, which makes a standalone container safe by default; the compose
# scenarios override them to https://0.0.0.0 and http://0.0.0.0 behind a token.
ENV WASMGATE_CP_GRPC_LISTEN="http://127.0.0.1:9096" \
    WASMGATE_CP_HTTP_LISTEN="http://127.0.0.1:9097" \
    WASMGATE_CP_SQLITE_PATH="/data/control.db" \
    WASMGATE_CP_ARTIFACT_ROOT="/data/artifacts" \
    WASMGATE_CP_BACKUP_TMP="/data/backups" \
    WASMGATE_CP_AGENT_CA_CERT="/data/ca/agents-ca.crt" \
    WASMGATE_CP_AGENT_CA_KEY="/data/ca/agents-ca.key" \
    WASMGATE_CP_GRPC_CLIENT_CA="/data/ca/agents-ca.crt" \
    WASMGATE_CP_SIGNING_KEY="/data/keys/release-signing.key" \
    WASMGATE_CP_WASMGATE_BIN="/app/wasmgate"

USER 1654
EXPOSE 9096 9097
ENTRYPOINT ["/app/wasmgate-control"]
