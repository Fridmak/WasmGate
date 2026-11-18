package runtime

import (
	"sync/atomic"

	"wasmgate/internal/config"
	"wasmgate/internal/proxy/backend"
	"wasmgate/internal/ratelimit"
)

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
func (g *registry) limiter(name string, limit int64) *ConnLimiter {
	g.mu.Lock()
	defer g.mu.Unlock()
	l := g.limiters[name]
	if l == nil {
		l = &ConnLimiter{}
		g.limiters[name] = l
	}
	l.max.Store(limit)
	return l
}

// Upstream returns the shared endpoint object for an address. It implements
// backend.Source so pools can be built without depending on proxy registry
// internals. Two pools that reference the same address share the object (and
// thus the active counter), which is what you want: they point at the same
// backend.
func (g *registry) Upstream(addr string) *backend.Upstream {
	g.mu.Lock()
	defer g.mu.Unlock()
	u := g.upstreams[addr]
	if u == nil {
		u = backend.NewUpstream(addr)
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
	livePlugins := make(map[string]bool, len(cfg.Listeners))
	for _, l := range cfg.Listeners {
		liveListeners[l.Name] = true
		if l.Plugin.Enabled() {
			livePlugins[l.Name] = true
		}
	}
	liveUpstreams := make(map[string]bool)
	for _, p := range cfg.Pools {
		for _, s := range p.Servers {
			liveUpstreams[s] = true
		}
	}

	g.mu.Lock()
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
	var retired []cachedPlugin
	for name, cp := range g.plugins {
		if !livePlugins[name] {
			retired = append(retired, cp)
			delete(g.plugins, name)
		}
	}
	for addr, u := range g.upstreams {
		if !liveUpstreams[addr] && u.ActiveCount() == 0 {
			delete(g.upstreams, addr)
		}
	}
	g.mu.Unlock()

	for _, cp := range retired {
		cp.release()
	}
}

// connLimiter is a lock-free, reload-safe concurrent connection limit. A max of
// 0 means unlimited.
type ConnLimiter struct {
	max atomic.Int64
	cur atomic.Int64
}

// Acquire reserves a slot, reporting false when the limit is currently reached.
func (l *ConnLimiter) Acquire() bool {
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

// Release returns a slot. Safe to call once per successful acquire.
func (l *ConnLimiter) Release() {
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

// Limit returns the current maximum (0 = unlimited).
func (l *ConnLimiter) Limit() int64 { return l.max.Load() }

// Active returns the number of currently reserved slots.
func (l *ConnLimiter) Active() int64 { return l.cur.Load() }
