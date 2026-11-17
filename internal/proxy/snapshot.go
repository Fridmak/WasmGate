package proxy

import (
	"crypto/tls"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"wasmgate/internal/config"
	"wasmgate/internal/observe"
	"wasmgate/internal/plugin/abi4"
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
	// connGuard enforces cfg.RateLimit.MaxConcurrent per client IP/subnet; nil
	// means no concurrent limit is configured.
	connGuard *ratelimit.Guard
	// pluginV4 is the ABI v4 engine for this listener; nil means disabled.
	// L4 lifecycle and every L7 data mode use the same Engine type.
	pluginV4 *abi4.Engine
	pipeline pluginPipeline
	// sniRoutes maps lower-cased SNI host patterns to resolved pools; nil when
	// SNI routing is not configured on this listener.
	sniRoutes map[string]*pool
	// routePools are additional pools selectable by a WASM on_event hook return
	// value (verdict >= 2 selects routePools[verdict-2]).
	routePools []*pool
	// tlsConfig terminates TLS on this listener when non-nil.
	tlsConfig *tls.Config
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

	// refs counts the publication reference plus every acquired connection.
	// When the snapshot is retired and the last connection releases it, the
	// engines it holds are released. This is what lets reloads reclaim Wasm
	// runtimes without breaking connections that still use the old config.
	lifecycle snapshotLifecycle
	engines   []*abi4.Engine
}

type snapshotLifecycle struct {
	mu      sync.Mutex
	refs    int
	retired bool
	closed  bool
}

// Acquire pins the snapshot for the lifetime of one connection. It returns nil
// once the snapshot has been retired and drained; callers must fall back to the
// current snapshot.
func (s *Snapshot) Acquire() *Snapshot {
	s.lifecycle.mu.Lock()
	defer s.lifecycle.mu.Unlock()
	if s.lifecycle.closed {
		return nil
	}
	s.lifecycle.refs++
	return s
}

// Release drops one acquired reference.
func (s *Snapshot) Release() {
	s.lifecycle.mu.Lock()
	s.lifecycle.refs--
	closeNow := s.lifecycle.refs == 0 && s.lifecycle.retired && !s.lifecycle.closed
	if closeNow {
		s.lifecycle.closed = true
	}
	s.lifecycle.mu.Unlock()
	if closeNow {
		s.closeEngines()
	}
}

// retire drops the publication reference. It is called after the snapshot has
// been replaced in Runtime.cur.
func (s *Snapshot) retire() {
	s.lifecycle.mu.Lock()
	s.lifecycle.retired = true
	s.lifecycle.refs--
	closeNow := s.lifecycle.refs == 0 && !s.lifecycle.closed
	if closeNow {
		s.lifecycle.closed = true
	}
	s.lifecycle.mu.Unlock()
	if closeNow {
		s.closeEngines()
	}
}

func (s *Snapshot) closeEngines() {
	for _, eng := range s.engines {
		eng.Release()
	}
	s.engines = nil
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

// NewRuntime builds the first snapshot (version 1) from cfg. It panics if cfg
// cannot be built; production code should use NewRuntimeChecked.
func NewRuntime(cfg *config.Config) *Runtime {
	r, err := NewRuntimeChecked(cfg)
	if err != nil {
		panic(err)
	}
	return r
}

// NewRuntimeChecked builds the first snapshot and returns a build error instead
// of publishing a partially configured (fail-open) runtime.
func NewRuntimeChecked(cfg *config.Config) (*Runtime, error) {
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
			Active:  u.active.Load(),
			Healthy: u.IsHealthy(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Server < out[j].Server })
	return out
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

// resolvePaths anchors a listener's relative artifact paths at the configuration
// directory so the same document works from any working directory.
func resolvePaths(cfg *config.Config, lc config.ListenerConfig) config.ListenerConfig {
	lc.Plugin.Path = cfg.Resolve(lc.Plugin.Path)
	lc.TLS.CertFile = cfg.Resolve(lc.TLS.CertFile)
	lc.TLS.KeyFile = cfg.Resolve(lc.TLS.KeyFile)
	return lc
}

func buildTLSConfig(tc config.TLSConfig) (*tls.Config, error) {
	var (
		cert tls.Certificate
		err  error
	)
	if len(tc.CertPEM) > 0 || len(tc.KeyPEM) > 0 {
		cert, err = tls.X509KeyPair(tc.CertPEM, tc.KeyPEM)
	} else {
		cert, err = tls.LoadX509KeyPair(tc.CertFile, tc.KeyFile)
	}
	if err != nil {
		return nil, err
	}
	var minVer uint16 = tls.VersionTLS12
	if tc.MinVersion == "1.3" {
		minVer = tls.VersionTLS13
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   minVer,
	}, nil
}

// build compiles cfg into a ready-to-use Snapshot: it turns every configured
// pool into a live pool (upstreams + balancer), pre-resolves every listener to
// its pool (and its connection limiter), and tags the result with the given
// version. All allocation happens here, up front, so the data path only ever
// reads.
//
// Caller must hold r.mu (or be the single-threaded constructor).
func (r *Runtime) build(version uint64, cfg *config.Config) (*Snapshot, error) {
	// Validate and compile all fallible per-listener artifacts (WASM plugins and
	// TLS certificates) BEFORE mutating any shared registry state (breaker
	// parameters, connection limiters, rate limiters, concurrency guards). That
	// way a failed reload is truly atomic and leaves live state untouched.
	var preparedPlugins map[string]cachedPlugin
	committed := false
	defer func() {
		if !committed {
			releasePrepared(preparedPlugins)
		}
	}()
	var preparedTLS map[string]*tls.Config
	for _, lc := range cfg.Listeners {
		lc = resolvePaths(cfg, lc)
		if lc.Plugin.Enabled() {
			cp, err := r.registry.preparePlugin(lc.Name, lc.Plugin)
			if err != nil {
				return nil, fmt.Errorf("listener %q: plugin: %w", lc.Name, err)
			}
			if preparedPlugins == nil {
				preparedPlugins = make(map[string]cachedPlugin, len(cfg.Listeners))
			}
			preparedPlugins[lc.Name] = cp
		}
		if lc.TLS.Enabled() {
			tcfg, err := buildTLSConfig(lc.TLS)
			if err != nil {
				return nil, fmt.Errorf("listener %q: tls: %w", lc.Name, err)
			}
			if preparedTLS == nil {
				preparedTLS = make(map[string]*tls.Config, len(cfg.Listeners))
			}
			preparedTLS[lc.Name] = tcfg
		}
	}
	r.registry.commitPlugins(preparedPlugins)
	committed = true

	pools := make(map[string]*pool, len(cfg.Pools))
	for name, pc := range cfg.Pools {
		p := newPool(name, pc.Servers, pc.Balance, r.registry)
		p.dialRetries = pc.DialRetries
		for _, u := range p.servers {
			u.ConfigureBreaker(pc.CircuitBreaker)
		}
		pools[name] = p
	}

	listeners := make(map[string]resolvedListener, len(cfg.Listeners))
	for _, lc := range cfg.Listeners {
		lc = resolvePaths(cfg, lc)
		rl := resolvedListener{cfg: lc, pool: pools[lc.UpstreamPool]}
		// Always fetch the registry limiter (this also (re)sets its limit);
		// expose it only when a limit is actually configured.
		lim := r.registry.limiter(lc.Name, int64(lc.MaxConns))
		if lc.MaxConns > 0 {
			rl.limiter = lim
		}
		if lc.RateLimit.WindowEnabled() {
			rl.rateLimiter = r.registry.rateLimiter(lc.Name, lc.RateLimit)
		}
		if lc.RateLimit.ConcurrencyEnabled() {
			rl.connGuard = r.registry.connGuard(lc.Name, lc.RateLimit)
		}
		if cp, ok := preparedPlugins[lc.Name]; ok {
			rl.pluginV4 = cp.v4
			rl.pipeline = cp.pipeline
		}
		if len(lc.SNIRoutes) > 0 {
			rl.sniRoutes = make(map[string]*pool, len(lc.SNIRoutes))
			for host, poolName := range lc.SNIRoutes {
				rl.sniRoutes[strings.ToLower(host)] = pools[poolName]
			}
		}
		if len(lc.Plugin.RoutePools) > 0 {
			rl.routePools = make([]*pool, 0, len(lc.Plugin.RoutePools))
			for _, poolName := range lc.Plugin.RoutePools {
				rl.routePools = append(rl.routePools, pools[poolName])
			}
		}
		if tcfg, ok := preparedTLS[lc.Name]; ok {
			rl.tlsConfig = tcfg
		}
		listeners[lc.Name] = rl
	}

	// Pin every engine used by this snapshot before it becomes publishable.
	// Snapshot.Release drops these references once the snapshot is retired and
	// no connection can still reach it.
	unique := make(map[*abi4.Engine]struct{}, len(preparedPlugins))
	for _, cp := range preparedPlugins {
		if cp.v4 != nil {
			unique[cp.v4] = struct{}{}
		}
	}
	engines := make([]*abi4.Engine, 0, len(unique))
	for eng := range unique {
		eng.Retain()
		engines = append(engines, eng)
	}

	r.registry.prune(cfg)
	return &Snapshot{
		Version:   version,
		Config:    cfg,
		listeners: listeners,
		engines:   engines,
		lifecycle: snapshotLifecycle{refs: 1}, // publication reference
	}, nil
}
