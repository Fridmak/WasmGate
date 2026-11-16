BINARY := bin/wasmgate
PKG    := ./cmd/wasmgate
PROTO  := control-plane/src/WasmGate.Control.Api/Protos/agent_control.proto
PROTO_INC := control-plane/src/WasmGate.Control.Api/Protos
VERSION ?= dev

.PHONY: build run test race bench tidy lint clean proto

build:
	go build -ldflags "-X main.version=$(VERSION)" -o $(BINARY) $(PKG)

# Regenerate the Go gRPC bindings from the C# project's proto. Requires protoc,
# protoc-gen-go and protoc-gen-go-grpc on PATH.
proto:
	protoc -I $(PROTO_INC) \
		--go_out=. --go_opt=module=wasmgate \
		--go-grpc_out=. --go-grpc_opt=module=wasmgate \
		$(PROTO)

run: build
	$(BINARY) -config configs/wasmgate.yaml

test:
	go test ./...

race:
	go test -race ./...

bench:
	go test -run '^$$' -bench . -benchmem ./...

tidy:
	go mod tidy

lint:
	golangci-lint run

clean:
	go clean
	rm -rf bin
