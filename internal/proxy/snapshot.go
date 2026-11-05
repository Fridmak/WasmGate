package proxy

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"wasmgate/internal/config"
	"wasmgate/internal/observe"
	"wasmgate/internal/plugin"
	"wasmgate/internal/ratelimit"
)

// resolvedListener is a listener pre-bound to its pool, so the data path
// resolves everything it needs with a single map lookup per connection instead
// of scanning the listener list and then looking up the pool.
type resolvedListener struct {
	cfg  config.ListenerConfig
	pool *pool
	// limiter enforces cfg.MaxConns; nil means "unlimited" (max_conns == 0).
	// The limiter itself survives reloads (see state.go), so the count does not
	// reset when the configuration is re-published.
	limiter *connLimiter
	// rateLimiter enforces cfg.RateLimit per client IP; nil means disabled.
	rateLimiter *ratelimit.Limiter
	// plugin is the compiled WASM sandbox for this listener; nil means disabled.
	plugin *plugin.Plugin
	// sniRoutes maps lower-cased SNI host patterns to resolved pools; nil when
	// SNI routing is not configured on this listener.
	sniRoutes map[string]*pool
}

// Snapshot is an immutable, ready-to-use view of the configuration together
// with the runtime objects compiled from it.
//
// "Immutable" means its structure never changes after it is published. The
// pools inside carry live counters, but the map and the pool set are frozen —
// this is what makes lock-free reads safe.
type Snapshot struct {
	Version   uint64
	Config    *config.Config
	listeners map[string]resolvedListener
}

// Runtime publishes Snapshots atomically.
//
// This is the core seam for Zero-Downtime Hot Reload:
//   - the data path calls Load() on every connection — a single atomic load,
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

// NewRuntime builds the first snapshot (version 1) from cfg.
func NewRuntime(cfg *config.Config) *Runtime {
	r := &Runtime{registry: newRegistry()}
	r.cur.Store(r.build(1, cfg))
	return r
}

// Load returns the current snapshot. Lock-free and allocation-free.
func (r *Runtime) Load() *Snapshot { return r.cur.Load() }

// UpstreamStats returns the live in-flight connection count per backend
// address, sorted by address. Cold path (the admin /metrics endpoint): it takes
// the registry lock and allocates, which the data path never does. The counts
// come from the registry, so they cover every pool and survive reloads.
func (r *Runtime) UpstreamStats() []observe.UpstreamStat {
	r.registry.mu.Lock()
	defer r.registry.mu.Unlock()
	out := make([]observe.UpstreamStat, 0, len(r.registry.upstreams))
	for addr, u := range r.registry.upstreams {
		out = append(out, observe.UpstreamStat{Server: addr, Active: u.active.Load()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Server < out[j].Server })
	return out
}

// Reload compiles a new snapshot and publishes it, returning the new version.
// Safe for concurrent use.
//
// Precondition: cfg must already be validated (see config.Config.Validate).
func (r *Runtime) Reload(cfg *config.Config) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()

	next := r.build(r.cur.Load().Version+1, cfg)
	r.cur.Store(next)
	return next.Version
}

// ValidatePlugins compiles and caches every listener's WASM plugin before a
// server starts, reloads or runs in -validate mode, so a missing file or
// invalid module fails fast before any socket is bound or any snapshot is swapped.
func (r *Runtime) ValidatePlugins(cfg *config.Config) error {
	for _, lc := range cfg.Listeners {
		if !lc.Plugin.Enabled() {
			continue
		}
		if _, err := r.registry.plugin(lc.Name, lc.Plugin); err != nil {
			return fmt.Errorf("listener %q: %w", lc.Name, err)
		}
	}
	return nil
}

// build compiles cfg into a ready-to-use Snapshot: it turns every configured
// pool into a live pool (upstreams + balancer), pre-resolves every listener to
// its pool (and its connection limiter), and tags the result with the given
// version. All allocation happens here, up front, so the data path only ever
// reads.
//
// Caller must hold r.mu (or be the single-threaded constructor).
func (r *Runtime) build(version uint64, cfg *config.Config) *Snapshot {
	pools := make(map[string]*pool, len(cfg.Pools))
	for name, pc := range cfg.Pools {
		pools[name] = newPool(name, pc.Servers, pc.Balance, r.registry)
	}

	listeners := make(map[string]resolvedListener, len(cfg.Listeners))
	for _, lc := range cfg.Listeners {
		rl := resolvedListener{cfg: lc, pool: pools[lc.UpstreamPool]}
		// Always fetch the registry limiter (this also (re)sets its limit);
		// expose it only when a limit is actually configured.
		lim := r.registry.limiter(lc.Name, int64(lc.MaxConns))
		if lc.MaxConns > 0 {
			rl.limiter = lim
		}
		if lc.RateLimit.Enabled() {
			rl.rateLimiter = r.registry.rateLimiter(lc.Name, lc.RateLimit)
		}
		if lc.Plugin.Enabled() {
			pl, err := r.registry.plugin(lc.Name, lc.Plugin)
			if err == nil {
				rl.plugin = pl
			}
		}
		if len(lc.SNIRoutes) > 0 {
			rl.sniRoutes = make(map[string]*pool, len(lc.SNIRoutes))
			for host, poolName := range lc.SNIRoutes {
				rl.sniRoutes[strings.ToLower(host)] = pools[poolName]
			}
		}
		listeners[lc.Name] = rl
	}

	r.registry.prune(cfg)
	return &Snapshot{Version: version, Config: cfg, listeners: listeners}
}
