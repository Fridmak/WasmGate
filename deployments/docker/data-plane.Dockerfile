# syntax=docker/dockerfile:1
#
# Data-plane image: a static, CGO-free Go binary with CA certificates for mTLS
# to the control plane. Build from the repository root:
#   docker build -f deployments/docker/data-plane.Dockerfile -t wasmgate-dp .

FROM golang:1.27-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/wasmgate ./cmd/wasmgate

FROM alpine:3.22
RUN apk add --no-cache ca-certificates \
    && adduser -D -u 10001 wasmgate \
    && mkdir -p /data \
    && chown -R wasmgate:wasmgate /data

COPY --from=build /out/wasmgate /usr/local/bin/wasmgate

USER wasmgate
WORKDIR /data

ENTRYPOINT ["wasmgate"]
