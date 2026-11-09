package proxy

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"

	"wasmgate/internal/config"
	"wasmgate/internal/plugin"
	"wasmgate/internal/ratelimit"
)

// This file holds runtime state that must OUTLIVE a configuration snapshot.
//
// Why it exists: a Snapshot is rebuilt from scratch on every reload. Anything
// whose value depends on history — how many connections a listener is currently
// serving, how many a backend is currently serving — must not live inside the
// snapshot, or a reload would silently reset it:
//
//   - max_conns would restart at zero on every reload, so N reloads would let N
//     times the limit through (a hole exactly where flood protection matters);
//   - least_conn would forget in-flight connections and briefly over-send to a
//     backend.
//
// The registry keys this state by a stable identity (listener name, upstream
// address) and hands the same object to every snapshot, so counters keep adding
// up across reloads. The data path only ever touches the atomics inside these
// objects — never the registry map — so the hot path stays lock-free.
type registry struct {
	mu         sync.Mutex
	limiters   map[string]*connLimiter
	rateLimits map[string]*ratelimit.Limiter
	guards     map[string]*ratelimit.Guard
	plugins    map[string]cachedPlugin
	upstreams  map[string]*upstream
}

type cachedPlugin struct {
	cfg config.PluginConfig
	pl  *plugin.Plugin
}

func newRegistry() *registry {
	return &registry{
		limiters:   make(map[string]*connLimiter),
		rateLimits: make(map[string]*ratelimit.Limiter),
		guards:     make(map[string]*ratelimit.Guard),
		plugins:    make(map[string]cachedPlugin),
		upstreams:  make(map[string]*upstream),
	}
}

// plugin returns the compiled WASM sandbox for a listener, reusing the existing
// compiled module and instance pool when the plugin configuration has not changed.
func (g *registry) plugin(name string, pc config.PluginConfig) (*plugin.Plugin, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if cur, ok := g.plugins[name]; ok && samePluginConfig(cur.cfg, pc) {
		return cur.pl, nil
	}
	opts := plugin.Options{
		MaxMemoryPages: pc.MaxMemoryPages,
		Timeout:        pc.Timeout,
	}
	var (
		pl  *plugin.Plugin
		err error
	)
	if len(pc.Wasm) > 0 {
		pl, err = plugin.New(context.Background(), pc.Wasm, opts)
	} else {
		pl, err = plugin.LoadFile(context.Background(), pc.Path, opts)
	}
	if err != nil {
		return nil, err
	}
	g.plugins[name] = cachedPlugin{cfg: pc, pl: pl}
	return pl, nil
}

func samePluginConfig(a, b config.PluginConfig) bool {
	return a.Path == b.Path &&
		a.MaxMemoryPages == b.MaxMemoryPages &&
		a.Timeout == b.Timeout &&
		a.DataMode == b.DataMode &&
		bytes.Equal(a.Wasm, b.Wasm)
}

// rateLimiter returns the per-IP/subnet connection limiter for a listener. The
// window length is part of the limiter's bucket layout, so a changed window
// replaces the limiter (its counts restart); changes to max, subnet_max,
// ban_duration or allow_cidrs are applied in place.
func (g *registry) rateLimiter(name string, rl config.RateLimit) *ratelimit.Limiter {
	allow, _ := ratelimit.ParseCIDRs(rl.AllowCIDRs) // already validated by config.Validate
	hostPrefix, subnetPrefix := ratePrefixes(rl)
	opts := ratelimit.Options{
		Max:              rl.Max,
		SubnetMax:        rl.SubnetMax,
		Window:           rl.Window,
		BanDuration:      rl.BanDuration,
		Allow:            allow,
		IPv6HostPrefix:   hostPrefix,
		IPv6SubnetPrefix: subnetPrefix,
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	l := g.rateLimits[name]
	curHost, curSubnet := 0, 0
	if l != nil {
		curHost, curSubnet = l.Prefixes()
	}
	if l == nil || l.Window() != rl.Window || curHost != hostPrefix || curSubnet != subnetPrefix {
		l = ratelimit.NewWithOptions(opts)
		g.rateLimits[name] = l
		return l
	}
	l.Update(opts)
	return l
}

func ratePrefixes(rl config.RateLimit) (host, subnet int) {
	host, subnet = rl.IPv6HostPrefix, rl.IPv6SubnetPrefix
	if host == 0 {
		host = 64
	}
	if subnet == 0 {
		subnet = 56
	}
	if host < subnet {
		host = subnet
	}
	return host, subnet
}

// connGuard returns the per-IP/subnet concurrent-connection guard for a
// listener. Its counters live outside the snapshot, so a reload or a rate
// window change does not reset active connections. A changed IPv6 prefix
// requires a new guard (and therefore loses counts, like a window change).
func (g *registry) connGuard(name string, rl config.RateLimit) *ratelimit.Guard {
	g.mu.Lock()
	defer g.mu.Unlock()
	hostPrefix, subnetPrefix := ratePrefixes(rl)
	gd := g.guards[name]
	curHost, curSubnet := 0, 0
	if gd != nil {
		curHost, curSubnet = gd.Prefixes()
	}
	if gd == nil || curHost != hostPrefix || curSubnet != subnetPrefix {
		gd = ratelimit.NewGuard(rl.MaxConcurrent, rl.SubnetMaxConcurrent, hostPrefix, subnetPrefix)
		g.guards[name] = gd
		return gd
	}
	gd.Update(rl.MaxConcurrent, rl.SubnetMaxConcurrent)
	return gd
}

// limiter returns the connection limiter for a listener name, creating it on
// first use, and (re)sets its limit. The limit may change freely on reload: a
// lower limit simply starts refusing new connections while the active count
// drains down to it.
func (g *registry) limiter(name string, limit int64) *connLimiter {
	g.mu.Lock()
	defer g.mu.Unlock()
	l := g.limiters[name]
	if l == nil {
		l = &connLimiter{}
		g.limiters[name] = l
	}
	l.max.Store(limit)
	return l
}

// upstream returns the endpoint object for an address, creating it on first
// use. Two pools that reference the same address share the object (and thus the
// active counter), which is what you want: they point at the same backend.
func (g *registry) upstream(addr string) *upstream {
	g.mu.Lock()
	defer g.mu.Unlock()
	u := g.upstreams[addr]
	if u == nil {
		u = &upstream{addr: addr}
		g.upstreams[addr] = u
	}
	return u
}

// prune drops state that is no longer referenced by cfg AND has drained to
// zero. Entries with live users are kept: an in-flight connection still holds
// its object and will release into it, so removing it early would lose that
// decrement (and, if the name came back, reset the count). Such entries are
// collected the next time a reload finds them at zero. This keeps the maps
// bounded without ever resetting a live counter.
func (g *registry) prune(cfg *config.Config) {
	liveListeners := make(map[string]bool, len(cfg.Listeners))
	for _, l := range cfg.Listeners {
		liveListeners[l.Name] = true
	}
	liveUpstreams := make(map[string]bool)
	for _, p := range cfg.Pools {
		for _, s := range p.Servers {
			liveUpstreams[s] = true
		}
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	for name, l := range g.limiters {
		if !liveListeners[name] && l.cur.Load() == 0 {
			delete(g.limiters, name)
		}
	}
	for name := range g.rateLimits {
		if !liveListeners[name] {
			delete(g.rateLimits, name)
		}
	}
	for name := range g.guards {
		if !liveListeners[name] {
			delete(g.guards, name)
		}
	}
	for name := range g.plugins {
		if !liveListeners[name] {
			delete(g.plugins, name)
		}
	}
	for addr, u := range g.upstreams {
		if !liveUpstreams[addr] && u.active.Load() == 0 {
			delete(g.upstreams, addr)
		}
	}
}

// connLimiter is a lock-free, reload-safe concurrent connection limit. A max of
// 0 means unlimited.
type connLimiter struct {
	max atomic.Int64
	cur atomic.Int64
}

// acquire reserves a slot, reporting false when the limit is currently reached.
func (l *connLimiter) acquire() bool {
	for {
		lim := l.max.Load()
		if lim <= 0 {
			return true // unlimited
		}
		cur := l.cur.Load()
		if cur >= lim {
			return false
		}
		if l.cur.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

// release returns a slot. Safe to call once per successful acquire.
func (l *connLimiter) release() {
	for {
		cur := l.cur.Load()
		if cur <= 0 {
			return // defensive: never go negative
		}
		if l.cur.CompareAndSwap(cur, cur-1) {
			return
		}
	}
}

// limit returns the current maximum (0 = unlimited), for logging.
func (l *connLimiter) limit() int64 { return l.max.Load() }
