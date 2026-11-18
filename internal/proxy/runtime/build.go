package runtime

import (
	"crypto/tls"
	"fmt"
	"strings"

	"wasmgate/internal/config"
	abi4 "wasmgate/internal/plugin/abi4/engine"
	"wasmgate/internal/proxy/backend"
)

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

	pools := make(map[string]*backend.Pool, len(cfg.Pools))
	for name, pc := range cfg.Pools {
		p := backend.NewPool(name, pc.Servers, pc.Balance, r.registry)
		p.SetDialRetries(pc.DialRetries)
		p.ConfigureBreaker(pc.CircuitBreaker)
		pools[name] = p
	}

	listeners := make(map[string]ResolvedListener, len(cfg.Listeners))
	for _, lc := range cfg.Listeners {
		lc = resolvePaths(cfg, lc)
		rl := ResolvedListener{Cfg: lc, Pool: pools[lc.UpstreamPool]}
		// Always fetch the registry limiter (this also (re)sets its limit);
		// expose it only when a limit is actually configured.
		lim := r.registry.limiter(lc.Name, int64(lc.MaxConns))
		if lc.MaxConns > 0 {
			rl.Limiter = lim
		}
		if lc.RateLimit.WindowEnabled() {
			rl.RateLimiter = r.registry.rateLimiter(lc.Name, lc.RateLimit)
		}
		if lc.RateLimit.ConcurrencyEnabled() {
			rl.ConnGuard = r.registry.connGuard(lc.Name, lc.RateLimit)
		}
		if cp, ok := preparedPlugins[lc.Name]; ok {
			rl.PluginV4 = cp.v4
			rl.Pipeline = cp.pipeline
		}
		if len(lc.SNIRoutes) > 0 {
			rl.SNIRoutes = make(map[string]*backend.Pool, len(lc.SNIRoutes))
			for host, poolName := range lc.SNIRoutes {
				rl.SNIRoutes[strings.ToLower(host)] = pools[poolName]
			}
		}
		if len(lc.Plugin.RoutePools) > 0 {
			rl.RoutePools = make([]*backend.Pool, 0, len(lc.Plugin.RoutePools))
			for _, poolName := range lc.Plugin.RoutePools {
				rl.RoutePools = append(rl.RoutePools, pools[poolName])
			}
		}
		if tcfg, ok := preparedTLS[lc.Name]; ok {
			rl.TLSConfig = tcfg
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
