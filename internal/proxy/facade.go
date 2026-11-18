package proxy

import (
	"log/slog"

	"wasmgate/internal/config"
	"wasmgate/internal/observe"
	"wasmgate/internal/proxy/conn"
	pruntime "wasmgate/internal/proxy/runtime"
	"wasmgate/internal/proxy/server"
)

// Runtime is the public runtime facade retained for existing callers.
type Runtime = pruntime.Runtime

// Snapshot is the immutable configuration view published by Runtime.
type Snapshot = pruntime.Snapshot

// Proxy is the public name retained for the connection handler.
type Proxy = conn.Handler

// Server is the public listener lifecycle facade.
type Server = server.Server

// NewRuntime builds a runtime and panics on invalid configuration.
func NewRuntime(cfg *config.Config) *Runtime { return pruntime.New(cfg) }

// NewRuntimeChecked builds a runtime and returns configuration errors.
func NewRuntimeChecked(cfg *config.Config) (*Runtime, error) { return pruntime.NewChecked(cfg) }

// New constructs the connection handler facade.
func New(log *slog.Logger, rt *Runtime, metrics *observe.Metrics) *Proxy {
	return conn.New(log, rt, metrics)
}

// NewServer constructs a listener server facade.
func NewServer(log *slog.Logger, rt *Runtime, metrics *observe.Metrics) *Server {
	return server.New(log, rt, metrics)
}
