package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"

	"wasmgate/internal/config"
	abi4 "wasmgate/internal/plugin/abi4/engine"
	"wasmgate/internal/proxy/backend"
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
	limiters   map[string]*ConnLimiter
	rateLimits map[string]*ratelimit.Limiter
	guards     map[string]*ratelimit.Guard
	plugins    map[string]cachedPlugin
	upstreams  map[string]*backend.Upstream
}

type cachedPlugin struct {
	cfg      config.PluginConfig
	hash     [32]byte
	pipeline PluginPipeline
	// All production plugins are ABI v4. L4 lifecycle and L7 events share the
	// same Engine type; the plan decides which hooks are required.
	v4 *abi4.Engine
}

type PluginMode uint8

const (
	PluginLifecycle PluginMode = iota
	PluginHeaders
	PluginOnce
	PluginStream
)

type PluginPipeline struct {
	Mode        PluginMode
	Body        bool
	ReplaceBody bool
}

func resolvePluginPlan(pc config.PluginConfig) (PluginPipeline, abi4.Options, error) {
	var p PluginPipeline
	opts := abi4.Options{MaxMemoryPages: pc.MaxMemoryPages, MaxInstances: pc.MaxInstances, Timeout: pc.Timeout}
	switch pc.DataMode {
	case "":
		p.Mode, opts.Plan = PluginLifecycle, abi4.InspectLifecycle
	case config.DataModeHeaders:
		p.Mode, opts.Plan = PluginHeaders, abi4.InspectRequestHeaders
	case config.DataModeOnce:
		p.Mode, opts.Plan = PluginOnce, abi4.InspectOpaqueStream
	case config.DataModeStream:
		p.Mode, opts.Plan = PluginStream, abi4.InspectOpaqueStream
	default:
		return p, opts, fmt.Errorf("unsupported data_mode %q", pc.DataMode)
	}
	if p.Mode == PluginHeaders || p.Mode == PluginOnce {
		opts.RoutePools = len(pc.RoutePools)
	} else if len(pc.RoutePools) != 0 {
		return p, opts, errors.New("route_pools requires data_mode once or headers")
	}
	if pc.BodyPolicy != "" && p.Mode != PluginHeaders {
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
	p.Body = opts.Plan&(abi4.InspectRequestBody|abi4.InspectResponseBody) != 0
	switch pc.MutationPolicy {
	case "", config.MutationPolicyNone:
	case config.MutationPolicyReplace:
		if !p.Body {
			return p, opts, errors.New("mutation_policy replace requires body_policy")
		}
		p.ReplaceBody = true
		opts.RequiredCaps |= abi4.CapMutation
	default:
		return p, opts, fmt.Errorf("unsupported mutation_policy %q", pc.MutationPolicy)
	}
	return p, opts, nil
}

func newRegistry() *registry {
	return &registry{
		limiters:   make(map[string]*ConnLimiter),
		rateLimits: make(map[string]*ratelimit.Limiter),
		guards:     make(map[string]*ratelimit.Guard),
		plugins:    make(map[string]cachedPlugin),
		upstreams:  make(map[string]*backend.Upstream),
	}
}

// preparePlugin compiles pc (or reuses the cached engine when config and
// content hash match) without mutating g.plugins, so a multi-listener reload
// can validate every listener before committing any change to the registry.
// The returned cachedPlugin owns one engine reference; the caller must either
// commit it or call release.
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
	if ok && samePluginConfig(cur.cfg, pc) && cur.hash == hash {
		cur.v4.Retain()
		g.mu.Unlock()
		return cur, nil
	}
	g.mu.Unlock()

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

// release drops the reference owned by a prepared or cached plugin.
func (cp cachedPlugin) release() {
	if cp.v4 != nil {
		cp.v4.Release()
	}
}

// releasePrepared drops references for plugins that a failed or abandoned
// build did not commit.
func releasePrepared(prepared map[string]cachedPlugin) {
	for _, cp := range prepared {
		cp.release()
	}
}

// commitPlugins transfers ownership of prepared plugins to the registry and
// releases the registry's references to the engines they replace. Retirement
// is deferred until every snapshot that still references an old engine is
// released; an in-flight connection therefore never loses its guest.
func (g *registry) commitPlugins(prepared map[string]cachedPlugin) {
	if len(prepared) == 0 {
		return
	}
	var replaced []cachedPlugin
	g.mu.Lock()
	for name, cp := range prepared {
		if old, ok := g.plugins[name]; ok {
			replaced = append(replaced, old)
		}
		g.plugins[name] = cp
	}
	g.mu.Unlock()

	for _, cp := range replaced {
		cp.release()
	}
}

func samePluginConfig(a, b config.PluginConfig) bool {
	return a.Path == b.Path &&
		a.MaxMemoryPages == b.MaxMemoryPages &&
		a.MaxInstances == b.MaxInstances &&
		a.Timeout == b.Timeout &&
		a.DataMode == b.DataMode &&
		a.BodyPolicy == b.BodyPolicy &&
		a.MutationPolicy == b.MutationPolicy &&
		a.MaxHeaderBytes == b.MaxHeaderBytes &&
		a.InspectTimeout == b.InspectTimeout &&
		slices.Equal(a.RoutePools, b.RoutePools) &&
		bytes.Equal(a.Wasm, b.Wasm)
}
