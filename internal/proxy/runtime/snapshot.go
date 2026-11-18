package runtime

import (
	"crypto/tls"
	"sync"

	"wasmgate/internal/config"
	abi4 "wasmgate/internal/plugin/abi4/engine"
	"wasmgate/internal/proxy/backend"
	"wasmgate/internal/ratelimit"
)

// ResolvedListener is a listener pre-bound to its pool, so the data path
// resolves everything it needs with a single map lookup per connection instead
// of scanning the listener list and then looking up the pool.
type ResolvedListener struct {
	Cfg  config.ListenerConfig
	Pool *backend.Pool
	// Limiter enforces cfg.MaxConns; nil means "unlimited" (max_conns == 0).
	// The limiter itself survives reloads (see limits.go), so the count does not
	// reset when the configuration is re-published.
	Limiter *ConnLimiter
	// RateLimiter enforces cfg.RateLimit per client IP; nil means disabled.
	RateLimiter *ratelimit.Limiter
	// ConnGuard enforces cfg.RateLimit.MaxConcurrent per client IP/subnet; nil
	// means no concurrent limit is configured.
	ConnGuard *ratelimit.Guard
	// PluginV4 is the ABI v4 engine for this listener; nil means disabled.
	// L4 lifecycle and every L7 data mode use the same Engine type.
	PluginV4 *abi4.Engine
	Pipeline PluginPipeline
	// SNIRoutes maps lower-cased SNI host patterns to resolved pools; nil when
	// SNI routing is not configured on this listener.
	SNIRoutes map[string]*backend.Pool
	// RoutePools are additional pools selectable by a WASM on_event hook return
	// value (verdict >= 2 selects RoutePools[verdict-2]).
	RoutePools []*backend.Pool
	// TLSConfig terminates TLS on this listener when non-nil.
	TLSConfig *tls.Config
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
	listeners map[string]ResolvedListener

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

// Listener resolves a listener by name in this immutable snapshot.
func (s *Snapshot) Listener(name string) (ResolvedListener, bool) {
	rl, ok := s.listeners[name]
	return rl, ok
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
