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
//     strings and no heap allocations on the hot path (0 B/op, 0 allocs/op).
//  3. For L7 payload inspection (on_data), a fixed scratch region inside each
//     instance's linear memory receives the packet bytes directly for the
//     duration of the CPU call (never across a blocking network Read, so slow
//     clients cannot starve the instance pool) and invokes on_data(ptr, len) ->
//     verdict with 0 Go heap allocations.
//  4. The sandbox exposes no WASI filesystem, network, environment or clock
//     capabilities, caps linear memory at MaxMemoryPages (64 KiB per page), and
//     optionally enforces a per-call timeout via WithCloseOnContextDone.
package plugin

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// Verdict is the decision returned by an on_accept or on_data hook.
type Verdict int32

const (
	// VerdictAllow lets the connection or packet proceed.
	VerdictAllow Verdict = 0
	// VerdictDeny closes the connection immediately.
	VerdictDeny Verdict = 1
)

const (
	// DefaultMaxMemoryPages caps a plugin instance's linear memory at 16 pages
	// (16 * 64 KiB = 1 MiB) when Options.MaxMemoryPages is 0.
	DefaultMaxMemoryPages = 16

	// ScratchOffset is the byte offset used when a module exports on_data but
	// fails to export on_data_ptr. It is retained only as documentation: L7
	// plugins must export on_data_ptr so the host never overwrites guest data.
	ScratchOffset   = 32 * 1024
	ScratchCapacity = 32 * 1024

	// StateCapacity is the per-direction byte capacity of the host-managed
	// state buffer passed to on_data_state (4 KiB).
	StateCapacity = 4 * 1024

	hookOnAccept    = "on_accept"
	hookOnData      = "on_data"
	hookOnDataEx    = "on_data_ex"
	hookOnDataState = "on_data_state"
	hookOnDataPtr   = "on_data_ptr"
	hookOnStatePtr  = "on_state_ptr"
	hookOnClose     = "on_close"
	hookOnCloseEx   = "on_close_ex"
)

// Options configures the sandbox limits for a compiled plugin.
type Options struct {
	// MaxMemoryPages caps the WASM linear memory in 64 KiB pages (default 16 = 1 MiB).
	MaxMemoryPages uint32
	// Timeout bounds a single hook execution. 0 disables the per-call timer
	// (preserving 0 allocs/op on the fast path). A zero value is an accepted
	// operator risk: a guest loop can freeze the process, so New logs a warning.
	Timeout time.Duration
}

// Plugin is a compiled WASM module together with a pool of reusable sandboxed
// instances. Safe for concurrent use by any number of goroutines; immutable
// from the caller's perspective so it can sit inside a proxy Snapshot and be
// closed once drained.
type Plugin struct {
	rt           wazero.Runtime
	compiled     wazero.CompiledModule
	timeout      time.Duration
	hasAccept    bool
	hasData      bool
	hasDataEx    bool
	hasDataState bool
	hasClose     bool
	hasCloseEx   bool
	nextID       atomic.Uint64
	discarded    atomic.Int64
	pool         sync.Pool
	initErr      atomic.Pointer[error]
	closeOnce    sync.Once
}

// StateBuf holds the host-managed bounded state for one stream direction of a
// single connection (up to StateCapacity bytes).
type StateBuf struct {
	len uint32
	buf [StateCapacity]byte
}

// Bytes returns the currently active state slice.
func (s *StateBuf) Bytes() []byte {
	if s == nil {
		return nil
	}
	return s.buf[:s.len]
}

// Reset zeroes the active bytes and resets the length to 0.
func (s *StateBuf) Reset() {
	if s == nil {
		return
	}
	clear(s.buf[:s.len])
	s.len = 0
}

// ConnState pairs the client->server (dir 0) and server->client (dir 1) state
// buffers for a single proxied connection. Each direction is owned by its own
// pipe goroutine, so no mutex is required on the hot path.
type ConnState struct {
	dir [2]StateBuf
}

var connStatePool = sync.Pool{
	New: func() any {
		return new(ConnState)
	},
}

// AcquireConnState borrows a zeroed ConnState from the pool.
func AcquireConnState() *ConnState {
	return connStatePool.Get().(*ConnState)
}

// ReleaseConnState zeroes any active state bytes and returns s to the pool.
func ReleaseConnState(s *ConnState) {
	if s == nil {
		return
	}
	s.dir[0].Reset()
	s.dir[1].Reset()
	connStatePool.Put(s)
}

// Dir returns the StateBuf for direction dir (0 = client->server, 1 = server->client).
func (s *ConnState) Dir(dir int32) *StateBuf {
	if s == nil || (dir != 0 && dir != 1) {
		return nil
	}
	return &s.dir[dir]
}

// Discarded returns the total number of instances destroyed after a trap or timeout.
func (p *Plugin) Discarded() int64 {
	if p == nil {
		return 0
	}
	return p.discarded.Load()
}

type instance struct {
	mod         api.Module
	onAccept    api.Function
	onData      api.Function
	onDataEx    api.Function
	onDataState api.Function
	onClose     api.Function
	onCloseEx   api.Function
	dataPtr     uint32
	dataCap     uint32
	statePtr    uint32
	stateCap    uint32
	stack       [8]uint64
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

	flags, err := validateExports(compiled)
	if err != nil {
		_ = rt.Close(ctx)
		return nil, err
	}

	p := &Plugin{
		rt:           rt,
		compiled:     compiled,
		timeout:      opts.Timeout,
		hasAccept:    flags.hasAccept,
		hasData:      flags.hasData,
		hasDataEx:    flags.hasDataEx,
		hasDataState: flags.hasDataState,
		hasClose:     flags.hasClose,
		hasCloseEx:   flags.hasCloseEx,
	}
	if p.timeout == 0 {
		// This is an accepted operator risk, not a silent default: a guest loop
		// without WithCloseOnContextDone can freeze the whole process.
		slog.Default().Warn("wasm plugin has no call timeout; a looping hook can freeze the process",
			"hint", "set plugin.timeout or ensure the plugin is trusted")
	}
	p.pool.New = func() any {
		inst, err := p.instantiate(context.Background())
		if err != nil {
			p.initErr.Store(&err)
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
	runtime.AddCleanup(p, func(rt wazero.Runtime) {
		_ = rt.Close(context.Background())
	}, p.rt)
	return p, nil
}

// HasData reports whether the compiled module exports an L7 on_data hook
// (on_data, on_data_ex, or the stateful on_data_state).
func (p *Plugin) HasData() bool {
	return p != nil && (p.hasData || p.hasDataEx || p.hasDataState)
}

// HasState reports whether the compiled module exports the ABI v3 stateful
// on_data_state hook.
func (p *Plugin) HasState() bool {
	return p != nil && p.hasDataState
}

// Timeout reports the per-call timeout (0 = disabled).
func (p *Plugin) Timeout() time.Duration { return p.timeout }

type exportFlags struct {
	hasAccept    bool
	hasData      bool
	hasDataEx    bool
	hasDataState bool
	hasClose     bool
	hasCloseEx   bool
}

// validateExports checks that the module exports at least one hook and that any
// exported hook has the exact ABI signature expected by the host.
func validateExports(compiled wazero.CompiledModule) (exportFlags, error) {
	var f exportFlags
	exports := compiled.ExportedFunctions()
	if fn, ok := exports[hookOnAccept]; ok {
		if !slices.Equal(fn.ParamTypes(), []api.ValueType{api.ValueTypeI64, api.ValueTypeI64, api.ValueTypeI32}) ||
			!slices.Equal(fn.ResultTypes(), []api.ValueType{api.ValueTypeI32}) {
			return f, fmt.Errorf("plugin: %s must have signature (i64, i64, i32) -> i32", hookOnAccept)
		}
		f.hasAccept = true
	}
	if fn, ok := exports[hookOnData]; ok {
		if !slices.Equal(fn.ParamTypes(), []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}) ||
			!slices.Equal(fn.ResultTypes(), []api.ValueType{api.ValueTypeI32}) {
			return f, fmt.Errorf("plugin: %s must have signature (i32, i32) -> i32", hookOnData)
		}
		f.hasData = true
	}
	if fn, ok := exports[hookOnDataEx]; ok {
		if !slices.Equal(fn.ParamTypes(), []api.ValueType{api.ValueTypeI64, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32}) ||
			!slices.Equal(fn.ResultTypes(), []api.ValueType{api.ValueTypeI32}) {
			return f, fmt.Errorf("plugin: %s must have signature (i64, i32, i32, i32) -> i32", hookOnDataEx)
		}
		f.hasDataEx = true
	}
	if fn, ok := exports[hookOnDataState]; ok {
		wantParams := []api.ValueType{
			api.ValueTypeI64, api.ValueTypeI32,
			api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32,
			api.ValueTypeI32, api.ValueTypeI32,
		}
		if !slices.Equal(fn.ParamTypes(), wantParams) ||
			!slices.Equal(fn.ResultTypes(), []api.ValueType{api.ValueTypeI64}) {
			return f, fmt.Errorf("plugin: %s must have signature (i64, i32, i32, i32, i32, i32, i32) -> i64", hookOnDataState)
		}
		f.hasDataState = true
	}
	if fn, ok := exports[hookOnDataPtr]; ok {
		if len(fn.ParamTypes()) != 0 || !slices.Equal(fn.ResultTypes(), []api.ValueType{api.ValueTypeI32}) {
			return f, fmt.Errorf("plugin: %s must have signature () -> i32", hookOnDataPtr)
		}
	}
	if fn, ok := exports[hookOnStatePtr]; ok {
		if len(fn.ParamTypes()) != 0 || !slices.Equal(fn.ResultTypes(), []api.ValueType{api.ValueTypeI32}) {
			return f, fmt.Errorf("plugin: %s must have signature () -> i32", hookOnStatePtr)
		}
	}
	if fn, ok := exports[hookOnClose]; ok {
		if !slices.Equal(fn.ParamTypes(), []api.ValueType{api.ValueTypeI64, api.ValueTypeI64, api.ValueTypeI64, api.ValueTypeI64}) ||
			len(fn.ResultTypes()) != 0 {
			return f, fmt.Errorf("plugin: %s must have signature (i64, i64, i64, i64) -> ()", hookOnClose)
		}
		f.hasClose = true
	}
	if fn, ok := exports[hookOnCloseEx]; ok {
		if !slices.Equal(fn.ParamTypes(), []api.ValueType{api.ValueTypeI64, api.ValueTypeI64, api.ValueTypeI64, api.ValueTypeI64, api.ValueTypeI64}) ||
			len(fn.ResultTypes()) != 0 {
			return f, fmt.Errorf("plugin: %s must have signature (i64, i64, i64, i64, i64) -> ()", hookOnCloseEx)
		}
		f.hasCloseEx = true
	}
	if (f.hasData || f.hasDataEx || f.hasDataState) && exports[hookOnDataPtr] == nil {
		return f, fmt.Errorf("plugin: %s is required when on_data/on_data_ex/on_data_state is exported", hookOnDataPtr)
	}
	if f.hasDataState && exports[hookOnStatePtr] == nil {
		return f, fmt.Errorf("plugin: %s is required when %s is exported", hookOnStatePtr, hookOnDataState)
	}
	if !f.hasAccept && !f.hasData && !f.hasDataEx && !f.hasDataState && !f.hasClose && !f.hasCloseEx {
		return f, fmt.Errorf("plugin: module must export %s, %s, %s, %s, %s or %s",
			hookOnAccept, hookOnData, hookOnDataEx, hookOnDataState, hookOnClose, hookOnCloseEx)
	}
	return f, nil
}

func ensureMemoryRange(mem api.Memory, ptr, size uint32, label string) error {
	end := uint64(ptr) + uint64(size)
	if uint64(mem.Size()) >= end {
		return nil
	}
	neededBytes := end - uint64(mem.Size())
	neededPages := (neededBytes + 65535) / 65536
	if neededPages > 65536 {
		return fmt.Errorf("plugin: linear memory too small for %s buffer", label)
	}
	if _, ok := mem.Grow(uint32(neededPages)); !ok { // #nosec G115 -- bounded by <= 65536 above
		return fmt.Errorf("plugin: linear memory too small for %s buffer", label)
	}
	return nil
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
	inst := &instance{
		mod:         mod,
		onAccept:    mod.ExportedFunction(hookOnAccept),
		onData:      mod.ExportedFunction(hookOnData),
		onDataEx:    mod.ExportedFunction(hookOnDataEx),
		onDataState: mod.ExportedFunction(hookOnDataState),
		onClose:     mod.ExportedFunction(hookOnClose),
		onCloseEx:   mod.ExportedFunction(hookOnCloseEx),
		dataCap:     ScratchCapacity,
		stateCap:    StateCapacity,
	}
	if p.hasData || p.hasDataEx || p.hasDataState {
		mem := mod.Memory()
		if mem == nil {
			_ = mod.Close(ctx)
			return nil, errors.New("plugin: module has no exported linear memory for on_data")
		}
		ptrFn := mod.ExportedFunction(hookOnDataPtr)
		if ptrFn == nil {
			_ = mod.Close(ctx)
			return nil, fmt.Errorf("plugin: %s is required for on_data", hookOnDataPtr)
		}
		if err := ptrFn.CallWithStack(ctx, inst.stack[:1]); err != nil {
			_ = mod.Close(ctx)
			return nil, fmt.Errorf("plugin: call %s: %w", hookOnDataPtr, err)
		}
		inst.dataPtr = uint32(inst.stack[0]) // #nosec G115 -- low 32 bits are the guest pointer
		if err := ensureMemoryRange(mem, inst.dataPtr, inst.dataCap, "on_data scratch"); err != nil {
			_ = mod.Close(ctx)
			return nil, err
		}
		if p.hasDataState {
			statePtrFn := mod.ExportedFunction(hookOnStatePtr)
			if statePtrFn == nil {
				_ = mod.Close(ctx)
				return nil, fmt.Errorf("plugin: %s is required for %s", hookOnStatePtr, hookOnDataState)
			}
			if err := statePtrFn.CallWithStack(ctx, inst.stack[:1]); err != nil {
				_ = mod.Close(ctx)
				return nil, fmt.Errorf("plugin: call %s: %w", hookOnStatePtr, err)
			}
			inst.statePtr = uint32(inst.stack[0]) // #nosec G115 -- low 32 bits are the guest pointer
			if err := ensureMemoryRange(mem, inst.statePtr, inst.stateCap, "on_data_state"); err != nil {
				_ = mod.Close(ctx)
				return nil, err
			}
			dataEnd := uint64(inst.dataPtr) + uint64(inst.dataCap)
			stateEnd := uint64(inst.statePtr) + uint64(inst.stateCap)
			if uint64(inst.statePtr) < dataEnd && uint64(inst.dataPtr) < stateEnd {
				_ = mod.Close(ctx)
				return nil, errors.New("plugin: on_state_ptr and on_data_ptr buffers overlap")
			}
			if stateMem, ok := mem.Read(inst.statePtr, inst.stateCap); ok {
				clear(stateMem)
			}
		}
	}
	runtime.AddCleanup(inst, func(mod api.Module) {
		_ = mod.Close(context.Background())
	}, mod)
	return inst, nil
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
		return VerdictDeny, fmt.Errorf("plugin: instantiate failed: %v", p.initError())
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
		p.discarded.Add(1)
		_ = inst.mod.Close(context.Background())
		return VerdictDeny, err
	}
	v := int32(uint32(inst.stack[0])) // #nosec G115 -- WASM i32 result is the low 32 bits
	p.pool.Put(inst)
	if v != int32(VerdictAllow) {
		return VerdictDeny, nil
	}
	return VerdictAllow, nil
}

// OnData is the legacy entry point for the client->server direction. New code
// should use OnDataConn so the plugin can correlate chunks and see direction.
func (p *Plugin) OnData(ctx context.Context, payload []byte) (Verdict, error) {
	return p.OnDataConn(ctx, 0, 0, payload)
}

// OnDataConn places payload into the pooled WASM instance's scratch linear
// memory and invokes on_data(ptr, len) or on_data_ex(conn_id, dir, ptr, len).
// Any in-place byte modifications are copied back into payload before the
// instance is returned to the pool.
//
// dir is 0 for client->server and 1 for server->client. The legacy on_data hook
// is only called for dir==0; on_data_ex is called for both directions.
func (p *Plugin) OnDataConn(ctx context.Context, connID uint64, dir int32, payload []byte) (Verdict, error) {
	if p != nil && p.hasDataState {
		return p.OnDataState(ctx, connID, dir, payload, nil)
	}
	if p == nil || (!p.hasData && !p.hasDataEx) || len(payload) == 0 {
		return VerdictAllow, nil
	}
	if !p.hasDataEx && dir != 0 {
		return VerdictAllow, nil
	}
	inst, ok := p.pool.Get().(*instance)
	if !ok || inst == nil {
		return VerdictDeny, fmt.Errorf("plugin: instantiate failed: %v", p.initError())
	}

	if p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}

	n := uint32(len(payload)) // #nosec G115 -- capped by inst.dataCap below
	if n > inst.dataCap {
		n = inst.dataCap
	}
	dst, ok := inst.mod.Memory().Read(inst.dataPtr, n)
	if !ok {
		p.discarded.Add(1)
		_ = inst.mod.Close(context.Background())
		return VerdictDeny, errors.New("plugin: scratch memory out of bounds")
	}
	copy(dst, payload[:n])

	if p.hasDataEx {
		inst.stack[0] = connID
		inst.stack[1] = uint64(uint32(dir)) // #nosec G115 -- dir is 0 or 1
		inst.stack[2] = uint64(inst.dataPtr)
		inst.stack[3] = uint64(n)
		if err := inst.onDataEx.CallWithStack(ctx, inst.stack[:4]); err != nil {
			p.discarded.Add(1)
			_ = inst.mod.Close(context.Background())
			return VerdictDeny, err
		}
	} else {
		inst.stack[0] = uint64(inst.dataPtr)
		inst.stack[1] = uint64(n)
		if err := inst.onData.CallWithStack(ctx, inst.stack[:2]); err != nil {
			p.discarded.Add(1)
			_ = inst.mod.Close(context.Background())
			return VerdictDeny, err
		}
	}

	v := int32(uint32(inst.stack[0])) // #nosec G115 -- strict i32 ABI: 0=allow, 1=deny, >=2=route_pools[v-2]
	if v < int32(VerdictAllow) {
		p.pool.Put(inst)
		return VerdictDeny, nil
	}
	if v != int32(VerdictDeny) {
		// memory.grow inside on_data may have replaced the backing array, so
		// re-read the scratch region instead of using the stale dst slice.
		fresh, ok := inst.mod.Memory().Read(inst.dataPtr, n)
		if !ok {
			p.discarded.Add(1)
			_ = inst.mod.Close(context.Background())
			return VerdictDeny, errors.New("plugin: scratch memory out of bounds after hook")
		}
		copy(payload[:n], fresh)
	}
	p.pool.Put(inst)
	return Verdict(v), nil
}

// OnDataState invokes the ABI v3 stateful hook
// on_data_state(conn_id, dir, state_ptr, state_len, state_cap, data_ptr, data_len) -> i64
// when the module exports it, falling back to OnDataConn otherwise.
//
// The low 32 bits of the return value carry the signed Verdict; the high 32
// bits carry the new state length (0 <= new_len <= StateCapacity). The guest's
// state region inside WASM linear memory is always wiped before the instance
// returns to the pool, so state never leaks across connections.
func (p *Plugin) OnDataState(ctx context.Context, connID uint64, dir int32, payload []byte, state *StateBuf) (Verdict, error) {
	if p == nil {
		return VerdictAllow, nil
	}
	if !p.hasDataState {
		return p.OnDataConn(ctx, connID, dir, payload)
	}
	if len(payload) == 0 {
		return VerdictAllow, nil
	}
	inst, ok := p.pool.Get().(*instance)
	if !ok || inst == nil {
		return VerdictDeny, fmt.Errorf("plugin: instantiate failed: %v", p.initError())
	}

	if p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}

	n := uint32(len(payload)) // #nosec G115 -- capped by inst.dataCap below
	if n > inst.dataCap {
		n = inst.dataCap
	}
	mem := inst.mod.Memory()
	dst, ok := mem.Read(inst.dataPtr, n)
	if !ok {
		p.discarded.Add(1)
		_ = inst.mod.Close(context.Background())
		return VerdictDeny, errors.New("plugin: scratch memory out of bounds")
	}
	copy(dst, payload[:n])

	var stateLen uint32
	if state != nil {
		stateLen = state.len
		if stateLen > inst.stateCap {
			stateLen = inst.stateCap
		}
		if stateLen > 0 {
			stateDst, ok := mem.Read(inst.statePtr, stateLen)
			if !ok {
				p.discarded.Add(1)
				_ = inst.mod.Close(context.Background())
				return VerdictDeny, errors.New("plugin: state memory out of bounds")
			}
			copy(stateDst, state.buf[:stateLen])
		}
	}

	inst.stack[0] = connID
	inst.stack[1] = uint64(uint32(dir)) // #nosec G115 -- dir is 0 or 1
	inst.stack[2] = uint64(inst.statePtr)
	inst.stack[3] = uint64(stateLen)
	inst.stack[4] = uint64(inst.stateCap)
	inst.stack[5] = uint64(inst.dataPtr)
	inst.stack[6] = uint64(n)
	if err := inst.onDataState.CallWithStack(ctx, inst.stack[:7]); err != nil {
		p.discarded.Add(1)
		_ = inst.mod.Close(context.Background())
		return VerdictDeny, err
	}

	raw := inst.stack[0]
	v := int32(uint32(raw))                 // #nosec G115 -- low 32 bits: signed verdict
	newStateLen := int32(uint32(raw >> 32)) // #nosec G115 -- high 32 bits: new state_len

	uMax := stateLen
	if newStateLen > 0 && uint32(newStateLen) <= inst.stateCap && uint32(newStateLen) > uMax {
		uMax = uint32(newStateLen)
	}
	freshState, ok := inst.mod.Memory().Read(inst.statePtr, inst.stateCap)
	if !ok {
		p.discarded.Add(1)
		_ = inst.mod.Close(context.Background())
		return VerdictDeny, errors.New("plugin: state memory out of bounds after hook")
	}
	if newStateLen < 0 || uint32(newStateLen) > inst.stateCap {
		clear(freshState[:uMax])
		p.pool.Put(inst)
		return VerdictDeny, fmt.Errorf("plugin: on_data_state returned invalid state length %d (cap %d)", newStateLen, inst.stateCap)
	}

	if v < int32(VerdictAllow) {
		clear(freshState[:uMax])
		p.pool.Put(inst)
		return VerdictDeny, nil
	}
	if v != int32(VerdictDeny) {
		freshData, ok := inst.mod.Memory().Read(inst.dataPtr, n)
		if !ok {
			p.discarded.Add(1)
			_ = inst.mod.Close(context.Background())
			return VerdictDeny, errors.New("plugin: scratch memory out of bounds after hook")
		}
		copy(payload[:n], freshData)
		if state != nil {
			uLen := uint32(newStateLen) // #nosec G115 -- validated 0 <= newStateLen <= stateCap
			if uLen < state.len {
				clear(state.buf[uLen:state.len])
			}
			copy(state.buf[:uLen], freshState[:uLen])
			state.len = uLen
		}
	}
	clear(freshState[:uMax])
	p.pool.Put(inst)
	return Verdict(v), nil
}

// OnClose is the legacy entry point; new code should use OnCloseConn.
func (p *Plugin) OnClose(ctx context.Context, ip net.IP, rxBytes, txBytes int64) error {
	return p.OnCloseConn(ctx, 0, ip, rxBytes, txBytes)
}

// OnCloseConn invokes on_close(ip_hi, ip_lo, rx_bytes, tx_bytes) or
// on_close_ex(conn_id, ip_hi, ip_lo, rx_bytes, tx_bytes).
func (p *Plugin) OnCloseConn(ctx context.Context, connID uint64, ip net.IP, rxBytes, txBytes int64) error {
	if p == nil || (!p.hasClose && !p.hasCloseEx) {
		return nil
	}
	inst, ok := p.pool.Get().(*instance)
	if !ok || inst == nil {
		return fmt.Errorf("plugin: instantiate failed: %v", p.initError())
	}

	if p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}

	hi, lo := encodeIP(ip)
	if p.hasCloseEx {
		inst.stack[0] = connID
		inst.stack[1] = hi
		inst.stack[2] = lo
		inst.stack[3] = uint64(rxBytes) // #nosec G115 -- raw i64 bit pattern on WASM stack
		inst.stack[4] = uint64(txBytes) // #nosec G115 -- raw i64 bit pattern on WASM stack
		if err := inst.onCloseEx.CallWithStack(ctx, inst.stack[:5]); err != nil {
			p.discarded.Add(1)
			_ = inst.mod.Close(context.Background())
			return err
		}
	} else {
		inst.stack[0] = hi
		inst.stack[1] = lo
		inst.stack[2] = uint64(rxBytes) // #nosec G115 -- raw i64 bit pattern on WASM stack
		inst.stack[3] = uint64(txBytes) // #nosec G115 -- raw i64 bit pattern on WASM stack
		if err := inst.onClose.CallWithStack(ctx, inst.stack[:4]); err != nil {
			p.discarded.Add(1)
			_ = inst.mod.Close(context.Background())
			return err
		}
	}
	p.pool.Put(inst)
	return nil
}

func (p *Plugin) initError() error {
	if e := p.initErr.Load(); e != nil {
		return *e
	}
	return nil
}

// Close releases the underlying wazero runtime and all compiled/instantiated
// resources. It is idempotent.
func (p *Plugin) Close(_ context.Context) error {
	if p == nil || p.rt == nil {
		return nil
	}
	p.closeOnce.Do(func() {
		_ = p.rt.Close(context.Background())
	})
	return nil
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
