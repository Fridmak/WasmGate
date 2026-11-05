// Package proxy implements the WasmGate data plane: a fast, lock-free L4 TCP
// reverse proxy.
//
// # Design in one paragraph
//
// Each accepted client connection is handled by exactly one goroutine
// (Proxy.Handle). At the very start of Handle the goroutine captures the
// current configuration snapshot with a single atomic load (Runtime.Load) and
// uses that immutable view for the whole lifetime of the connection.
// Configuration is never mutated in place: a reload builds a brand-new
// Snapshot and publishes it atomically (Runtime.Reload). This
// read-copy-update (RCU) pattern is what delivers Zero-Downtime Hot Reload —
// in-flight connections keep the snapshot they captured, new connections see
// the new one, and no lock is ever taken on the hot path.
//
// # Files
//
//   - snapshot.go — Snapshot (immutable config view) and Runtime (atomic publish).
//   - server.go   — Server: binds/reconciles listeners, runs accept loops, drains on shutdown.
//   - proxy.go    — Proxy.Handle: the per-connection pipeline (the hot path).
//   - pool.go     — pool: a set of upstreams behind one balancing strategy.
//   - upstream.go — upstream endpoint and Balancer strategies (round-robin, least-conn).
//   - state.go    — registry: counters and rate-limit windows that outlive a snapshot.
//   - copy.go     — join: bidirectional byte pump with half-close; zero-copy
//     splice on the Linux fast path, pooled buffers everywhere else, and a
//     connection-wide idle watchdog when idle_timeout > 0.
//
// # Extension seams
//
//   - Per-IP connection-flood rate limiting (internal/ratelimit) runs in
//     Proxy.Handle right after max_conns and before the upstream dial, with its
//     sliding windows kept in the registry across reloads.
//   - WASM plugin hooks (on_accept / on_data / on_close) attach around the
//     accept/data/close stages in Proxy.Handle.
package proxy
