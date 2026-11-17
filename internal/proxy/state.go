package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"sync/atomic"

	"wasmgate/internal/config"
	"wasmgate/internal/plugin/abi4"
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
	cfg      config.PluginConfig
	hash     [32]byte
	pipeline pluginPipeline
	// All production plugins are ABI v4. L4 lifecycle and L7 events share the
	// same Engine type; the plan decides which hooks are required.
	v4 *abi4.Engine
}

type pluginMode uint8

const (
	pluginLifecycle pluginMode = iota
	pluginHeaders
	pluginOnce
	pluginStream
)

type pluginPipeline struct {
	mode        pluginMode
	body        bool
	replaceBody bool
}

func resolvePluginPlan(pc config.PluginConfig) (pluginPipeline, abi4.Options, error) {
	var p pluginPipeline
	opts := abi4.Options{MaxMemoryPages: pc.MaxMemoryPages, Timeout: pc.Timeout}
	switch pc.DataMode {
	case "":
		p.mode, opts.Plan = pluginLifecycle, abi4.InspectLifecycle
	case config.DataModeHeaders:
		p.mode, opts.Plan = pluginHeaders, abi4.InspectRequestHeaders
	case config.DataModeOnce:
		p.mode, opts.Plan = pluginOnce, abi4.InspectOpaqueStream
	case config.DataModeStream:
		p.mode, opts.Plan = pluginStream, abi4.InspectOpaqueStream
	default:
		return p, opts, fmt.Errorf("unsupported data_mode %q", pc.DataMode)
	}
	if p.mode == pluginHeaders || p.mode == pluginOnce {
		opts.RoutePools = len(pc.RoutePools)
	} else if len(pc.RoutePools) != 0 {
		return p, opts, errors.New("route_pools requires data_mode once or headers")
	}
	if pc.BodyPolicy != "" && p.mode != pluginHeaders {
		return p, opts, errors.New("body_policy requires data_mode headers")
	}
	switch pc.BodyPolicy {
	case "":
	case config.BodyPolicyRequest:
		opts.Plan |= abi4.InspectRequestBody
	case config.BodyPolicyResponse:
		opts.Plan |= abi4.InspectResponseHeaders | abi4.InspectResponseBody
	case config.BodyPolicyBoth:
		opts.Plan |= abi4.InspectRequestBody | abi4.InspectResponseHeaders | abi4.InspectResponseBody
	default:
		return p, opts, fmt.Errorf("unsupported body_policy %q", pc.BodyPolicy)
	}
	p.body = opts.Plan&(abi4.InspectRequestBody|abi4.InspectResponseBody) != 0
	switch pc.MutationPolicy {
	case "", config.MutationPolicyNone:
	case config.MutationPolicyReplace:
		if !p.body {
			return p, opts, errors.New("mutation_policy replace requires body_policy")
		}
		p.replaceBody = true
		opts.RequiredCaps |= abi4.CapMutation
	default:
		return p, opts, fmt.Errorf("unsupported mutation_policy %q", pc.MutationPolicy)
	}
	return p, opts, nil
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

// preparePlugin compiles pc (or reuses the cached instance when config and
// content hash match) without mutating g.plugins, so a multi-listener reload
// can validate every listener before committing any change to the registry.
func (g *registry) preparePlugin(name string, pc config.PluginConfig) (cachedPlugin, error) {
	wasmBytes := pc.Wasm
	if len(wasmBytes) == 0 {
		b, err := os.ReadFile(pc.Path) // #nosec G304 -- path comes from validated config
		if err != nil {
			return cachedPlugin{}, fmt.Errorf("plugin: read %s: %w", pc.Path, err)
		}
		wasmBytes = b
	}
	hash := sha256.Sum256(wasmBytes)

	g.mu.Lock()
	cur, ok := g.plugins[name]
	g.mu.Unlock()
	if ok && samePluginConfig(cur.cfg, pc) && cur.hash == hash {
		return cur, nil
	}

	pipeline, opts, err := resolvePluginPlan(pc)
	if err != nil {
		return cachedPlugin{}, err
	}
	eng, err := abi4.New(context.Background(), wasmBytes, opts)
	if err != nil {
		return cachedPlugin{}, fmt.Errorf("ABI v4: %w", err)
	}
	return cachedPlugin{cfg: pc, hash: hash, v4: eng, pipeline: pipeline}, nil
}

func (g *registry) commitPlugins(prepared map[string]cachedPlugin) {
	if len(prepared) == 0 {
		return
	}
	g.mu.Lock()
	for name, cp := range prepared {
		g.plugins[name] = cp
	}
	g.mu.Unlock()
}

func samePluginConfig(a, b config.PluginConfig) bool {
	return a.Path == b.Path &&
		a.MaxMemoryPages == b.MaxMemoryPages &&
		a.Timeout == b.Timeout &&
		a.DataMode == b.DataMode &&
		a.BodyPolicy == b.BodyPolicy &&
		a.MutationPolicy == b.MutationPolicy &&
		a.MaxHeaderBytes == b.MaxHeaderBytes &&
		a.InspectTimeout == b.InspectTimeout &&
		slices.Equal(a.RoutePools, b.RoutePools) &&
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
