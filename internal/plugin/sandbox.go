// Package plugin runs WebAssembly plugins in a memory-bounded, allocation-free
// wazero sandbox.
//
// Why this design does not hurt memory or throughput:
//
//  1. A naive host instantiates the WASM module per connection, allocating at
//     least one 64 KiB linear-memory page each time. Instead, Plugin compiles
//     the module once (wazero AOT on amd64/arm64) and keeps a pool of
//     pre-instantiated modules (Reactor model), each with its own preallocated
//     call stack.
//  2. Calling api.Function.Call allocates a variadic parameter slice and a
//     result slice on every hook. Each pooled instance here uses
//     api.Function.CallWithStack over its fixed [4]uint64 buffer, passing the
//     canonical 16-byte client IP as two uint64 values plus the port — no
//     strings and no linear-memory copies on the hot path (0 B/op, 0 allocs/op).
//  3. The sandbox exposes no WASI filesystem, network, environment or clock
//     capabilities, caps linear memory at MaxMemoryPages (64 KiB per page), and
//     optionally enforces a per-call timeout via WithCloseOnContextDone.
package plugin

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// Verdict is the decision returned by an on_accept hook.
type Verdict int32

const (
	// VerdictAllow lets the connection proceed to the upstream pool.
	VerdictAllow Verdict = 0
	// VerdictDeny closes the client connection before dialing the upstream.
	VerdictDeny Verdict = 1
)

const (
	// DefaultMaxMemoryPages caps a plugin instance's linear memory at 16 pages
	// (16 * 64 KiB = 1 MiB) when Options.MaxMemoryPages is 0.
	DefaultMaxMemoryPages = 16

	hookOnAccept = "on_accept"
	hookOnClose  = "on_close"
)

// Options configures the sandbox limits for a compiled plugin.
type Options struct {
	// MaxMemoryPages caps the WASM linear memory in 64 KiB pages (default 16 = 1 MiB).
	MaxMemoryPages uint32
	// Timeout bounds a single hook execution. 0 disables the per-call timer
	// (preserving 0 allocs/op on the fast path) and relies on the connection ctx.
	Timeout time.Duration
}

// Plugin is a compiled WASM module together with a pool of reusable sandboxed
// instances. Safe for concurrent use by any number of goroutines; immutable
// from the caller's perspective so it can sit inside a proxy Snapshot and be
// closed once drained.
type Plugin struct {
	rt        wazero.Runtime
	compiled  wazero.CompiledModule
	timeout   time.Duration
	hasAccept bool
	hasClose  bool
	nextID    atomic.Uint64
	pool      sync.Pool
	initErr   error
}

type instance struct {
	mod      api.Module
	onAccept api.Function
	onClose  api.Function
	stack    [4]uint64
}

// LoadFile reads a .wasm binary from path and compiles it into a sandboxed Plugin.
func LoadFile(ctx context.Context, path string, opts Options) (*Plugin, error) {
	wasmBytes, err := os.ReadFile(path) // #nosec G304 -- path comes from the validated server config
	if err != nil {
		return nil, fmt.Errorf("plugin: read %s: %w", path, err)
	}
	return New(ctx, wasmBytes, opts)
}

// New compiles wasmBytes and warms one instance to verify exported signatures
// and memory limits up front (fail-fast on reload, never on the first client).
func New(ctx context.Context, wasmBytes []byte, opts Options) (*Plugin, error) {
	if len(wasmBytes) == 0 {
		return nil, errors.New("plugin: empty wasm binary")
	}
	maxPages := opts.MaxMemoryPages
	if maxPages == 0 {
		maxPages = DefaultMaxMemoryPages
	}

	rtCfg := wazero.NewRuntimeConfig().WithMemoryLimitPages(maxPages)
	if opts.Timeout > 0 {
		rtCfg = rtCfg.WithCloseOnContextDone(true)
	}
	rt := wazero.NewRuntimeWithConfig(ctx, rtCfg)

	compiled, err := rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("plugin: compile: %w", err)
	}

	hasAccept, hasClose, err := validateExports(compiled)
	if err != nil {
		_ = rt.Close(ctx)
		return nil, err
	}

	p := &Plugin{
		rt:        rt,
		compiled:  compiled,
		timeout:   opts.Timeout,
		hasAccept: hasAccept,
		hasClose:  hasClose,
	}
	p.pool.New = func() any {
		inst, err := p.instantiate(context.Background())
		if err != nil {
			p.initErr = err
			return nil
		}
		return inst
	}

	// Warm one instance now so any instantiation failure is caught at load time.
	warm, err := p.instantiate(ctx)
	if err != nil {
		_ = rt.Close(ctx)
		return nil, err
	}
	p.pool.Put(warm)
	return p, nil
}

// validateExports checks that the module exports at least one hook and that any
// exported hook has the exact ABI signature expected by the host.
func validateExports(compiled wazero.CompiledModule) (hasAccept, hasClose bool, err error) {
	exports := compiled.ExportedFunctions()
	if fn, ok := exports[hookOnAccept]; ok {
		if !sameTypes(fn.ParamTypes(), []api.ValueType{api.ValueTypeI64, api.ValueTypeI64, api.ValueTypeI32}) ||
			!sameTypes(fn.ResultTypes(), []api.ValueType{api.ValueTypeI32}) {
			return false, false, fmt.Errorf("plugin: %s must have signature (i64, i64, i32) -> i32", hookOnAccept)
		}
		hasAccept = true
	}
	if fn, ok := exports[hookOnClose]; ok {
		if !sameTypes(fn.ParamTypes(), []api.ValueType{api.ValueTypeI64, api.ValueTypeI64, api.ValueTypeI64, api.ValueTypeI64}) ||
			len(fn.ResultTypes()) != 0 {
			return false, false, fmt.Errorf("plugin: %s must have signature (i64, i64, i64, i64) -> ()", hookOnClose)
		}
		hasClose = true
	}
	if !hasAccept && !hasClose {
		return false, false, fmt.Errorf("plugin: module must export %s or %s", hookOnAccept, hookOnClose)
	}
	return hasAccept, hasClose, nil
}

func sameTypes(a, b []api.ValueType) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (p *Plugin) instantiate(ctx context.Context) (*instance, error) {
	id := p.nextID.Add(1)
	// Pure sandbox: empty module config with a unique name, no WASI filesystem,
	// no network, no env vars, no host clock.
	cfg := wazero.NewModuleConfig().WithName(fmt.Sprintf("inst-%d", id))
	mod, err := p.rt.InstantiateModule(ctx, p.compiled, cfg)
	if err != nil {
		return nil, fmt.Errorf("plugin: instantiate: %w", err)
	}
	return &instance{
		mod:      mod,
		onAccept: mod.ExportedFunction(hookOnAccept),
		onClose:  mod.ExportedFunction(hookOnClose),
	}, nil
}

// OnAccept invokes the guest's on_accept(ip_hi, ip_lo, port) -> verdict hook.
// If the module does not export on_accept, VerdictAllow is returned without
// touching the pool.
func (p *Plugin) OnAccept(ctx context.Context, ip net.IP, port int) (Verdict, error) {
	if p == nil || !p.hasAccept {
		return VerdictAllow, nil
	}
	inst, ok := p.pool.Get().(*instance)
	if !ok || inst == nil {
		return VerdictDeny, fmt.Errorf("plugin: instantiate failed: %v", p.initErr)
	}

	if p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}

	hi, lo := encodeIP(ip)
	inst.stack[0] = hi
	inst.stack[1] = lo
	inst.stack[2] = uint64(port) & 0xffffffff // #nosec G115 -- TCP port is 0..65535

	if err := inst.onAccept.CallWithStack(ctx, inst.stack[:3]); err != nil {
		// A trapped or timed-out instance may be left in an inconsistent state:
		// close and discard it instead of returning it to the pool.
		_ = inst.mod.Close(context.Background())
		return VerdictDeny, err
	}
	v := Verdict(inst.stack[0] & 0x7fffffff) // #nosec G115 -- non-zero means VerdictDeny
	p.pool.Put(inst)
	if v != VerdictAllow {
		return VerdictDeny, nil
	}
	return VerdictAllow, nil
}

// OnClose invokes the guest's on_close(ip_hi, ip_lo, rx_bytes, tx_bytes) hook.
// No-op if the module does not export on_close.
func (p *Plugin) OnClose(ctx context.Context, ip net.IP, rxBytes, txBytes int64) error {
	if p == nil || !p.hasClose {
		return nil
	}
	inst, ok := p.pool.Get().(*instance)
	if !ok || inst == nil {
		return fmt.Errorf("plugin: instantiate failed: %v", p.initErr)
	}

	if p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}

	hi, lo := encodeIP(ip)
	inst.stack[0] = hi
	inst.stack[1] = lo
	inst.stack[2] = uint64(rxBytes) // #nosec G115 -- raw i64 bit pattern on WASM stack
	inst.stack[3] = uint64(txBytes) // #nosec G115 -- raw i64 bit pattern on WASM stack

	if err := inst.onClose.CallWithStack(ctx, inst.stack[:4]); err != nil {
		_ = inst.mod.Close(context.Background())
		return err
	}
	p.pool.Put(inst)
	return nil
}

// Close releases the underlying wazero runtime and all compiled/instantiated
// resources.
func (p *Plugin) Close(ctx context.Context) error {
	if p == nil || p.rt == nil {
		return nil
	}
	return p.rt.Close(ctx)
}

// encodeIP packs ip into two big-endian uint64 values using the IPv4-mapped
// IPv6 representation (::ffff:a.b.c.d) for IPv4 addresses. Zero allocations.
func encodeIP(ip net.IP) (hi, lo uint64) {
	if v4 := ip.To4(); v4 != nil {
		lo = 0x0000ffff00000000 | uint64(binary.BigEndian.Uint32(v4))
		return 0, lo
	}
	if len(ip) == net.IPv6len {
		return binary.BigEndian.Uint64(ip[:8]), binary.BigEndian.Uint64(ip[8:])
	}
	return 0, 0
}
