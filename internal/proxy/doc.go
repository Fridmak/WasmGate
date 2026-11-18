// Package proxy is the compatibility facade for the WasmGate data plane.
// Runtime snapshots, connection handling and listener lifecycle live in the
// runtime, conn and server subpackages; backend, relay and httpinspect hold
// the lower-level data-path mechanisms.
package proxy
