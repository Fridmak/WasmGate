package runtime

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	"wasmgate/internal/config"
	"wasmgate/internal/observe"
	"wasmgate/internal/proxy/backend"
)

// Runtime publishes Snapshots atomically.
//
// This is the core seam for Zero-Downtime Hot Reload:
//   - the data path calls Acquire() on every connection — a single atomic load,
//     no locks, no torn reads;
//   - the control plane (or SIGHUP handler) calls Reload() to publish a new
//     immutable snapshot.
//
// Already-running connections keep using the snapshot they captured, so
// replacing the configuration never interrupts them.
type Runtime struct {
	cur atomic.Pointer[Snapshot]
	// mu serializes Reload. The pointer swap itself is atomic, but computing the
	// next version is a read-modify-write: without this lock two concurrent
	// reloads could publish the same version (a lost update). It also serializes
	// access to the registry, which only the cold path touches.
	mu       sync.Mutex
	registry *registry
}

// New builds the first snapshot (version 1) from cfg. It panics if cfg cannot
// be built; production code should use NewChecked.
func New(cfg *config.Config) *Runtime {
	r, err := NewChecked(cfg)
	if err != nil {
		panic(err)
	}
	return r
}

// NewChecked builds the first snapshot and returns a build error instead
// of publishing a partially configured (fail-open) runtime.
func NewChecked(cfg *config.Config) (*Runtime, error) {
	r := &Runtime{registry: newRegistry()}
	snap, err := r.build(1, cfg)
	if err != nil {
		return nil, err
	}
	r.cur.Store(snap)
	return r, nil
}

// Load returns the current snapshot for read-only inspection. It is lock-free
// and allocation-free, but does not pin the snapshot: callers that use plugin
// engines or keep the snapshot past the current request must use Acquire.
func (r *Runtime) Load() *Snapshot { return r.cur.Load() }

// Acquire returns a pinned snapshot. The caller must call Release when done.
// If a reload retires the snapshot concurrently, Acquire retries against the
// newly published one instead of returning a snapshot whose engines may be
// closing.
func (r *Runtime) Acquire() *Snapshot {
	for {
		snap := r.cur.Load()
		if snap == nil {
			return nil
		}
		if pinned := snap.Acquire(); pinned != nil {
			return pinned
		}
	}
}

// UpstreamStats returns the live in-flight connection count per backend
// address, sorted by address. Cold path (the admin /metrics endpoint): it takes
// the registry lock and allocates, which the data path never does. The counts
// come from the registry, so they cover every pool and survive reloads.
func (r *Runtime) UpstreamStats() []observe.UpstreamStat {
	r.registry.mu.Lock()
	defer r.registry.mu.Unlock()
	out := make([]observe.UpstreamStat, 0, len(r.registry.upstreams))
	for addr, u := range r.registry.upstreams {
		out = append(out, observe.UpstreamStat{
			Server:  addr,
			Active:  u.ActiveCount(),
			Healthy: u.IsHealthy(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Server < out[j].Server })
	return out
}

// Upstream returns the shared endpoint object for an address. The object is
// stable across snapshots, so health and active-connection state survive reloads.
func (r *Runtime) Upstream(addr string) *backend.Upstream {
	return r.registry.Upstream(addr)
}

// Reload compiles a new snapshot and publishes it, returning the new version.
// It is kept as a concise wrapper for tests; production reload paths should use
// ReloadChecked so a build error is never swallowed.
func (r *Runtime) Reload(cfg *config.Config) uint64 {
	v, _ := r.ReloadChecked(cfg)
	return v
}

// ReloadChecked builds a new snapshot and publishes it, or returns an error
// while keeping the previous snapshot active.
func (r *Runtime) ReloadChecked(cfg *config.Config) (uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	next, err := r.build(r.cur.Load().Version+1, cfg)
	if err != nil {
		return 0, err
	}
	prev := r.cur.Swap(next)
	if prev != nil {
		prev.retire()
	}
	return next.Version, nil
}

// ValidatePlugins compiles every listener's WASM plugin (and loads any
// configured TLS certificate pair) before a server starts, reloads or runs in
// -validate mode, so a missing file or invalid module/certificate fails fast
// before any socket is bound or any snapshot is swapped. Prepared engines are
// released immediately; the build that follows compiles or reuses its own
// references.
func (r *Runtime) ValidatePlugins(cfg *config.Config) error {
	for _, lc := range cfg.Listeners {
		lc = resolvePaths(cfg, lc)
		if lc.Plugin.Enabled() {
			cp, err := r.registry.preparePlugin(lc.Name, lc.Plugin)
			if err != nil {
				return fmt.Errorf("listener %q: %w", lc.Name, err)
			}
			cp.release()
		}
		if lc.TLS.Enabled() {
			if _, err := buildTLSConfig(lc.TLS); err != nil {
				return fmt.Errorf("listener %q: tls: %w", lc.Name, err)
			}
		}
	}
	return nil
}
