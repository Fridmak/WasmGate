package abi4

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

const (
	DefaultMaxMemoryPages = 16
	DefaultHookTimeout    = time.Second
	DefaultLoadTimeout    = 5 * time.Second
	DefaultInCap          = 40 * 1024
	DefaultOutCap         = 4 * 1024
)

// Options configures the v4 engine. Plan is required: it is the explicit
// contract between the listener and the plugin.
type Options struct {
	MaxMemoryPages uint32
	Timeout        time.Duration
	Plan           Plan
	InCap          int
	OutCap         int
	RoutePools     int
}

type instance struct {
	mod     api.Module
	onEvent api.Function
	mem     api.Memory
	bufPtr  uint32
	bufLen  uint32
	abi     uint32
	caps    Caps
	inCap   uint32
	stack   [5]uint64
}

// Engine is a compiled v4 module plus a pool of reusable instances. It is safe
// for concurrent use. Payload writes and header indexing avoid allocations;
// hook deadlines currently allocate one context per event (tracked as the P1
// wazero follow-up, not part of ABI correctness).
type Engine struct {
	rt          wazero.Runtime
	compiled    wazero.CompiledModule
	plan        Plan
	caps        Caps
	abi         uint32
	inCap       uint32
	outCap      uint32
	bufPtr      uint32
	bufLen      uint32
	timeout     time.Duration
	loadTimeout time.Duration
	routePools  int
	nextID      atomic.Uint64
	discarded   atomic.Int64
	pool        sync.Pool
	idxPool     sync.Pool
	initErr     atomic.Pointer[error]
	closeOnce   sync.Once
}

// LoadFile reads and compiles a v4 module.
func LoadFile(ctx context.Context, path string, opts Options) (*Engine, error) {
	wasmBytes, err := os.ReadFile(path) // #nosec G304 -- path comes from validated config
	if err != nil {
		return nil, fmt.Errorf("abi4: read %s: %w", path, err)
	}
	return New(ctx, wasmBytes, opts)
}

// New compiles a v4 module and validates its ABI, capabilities, window and
// on_event contract before serving any event.
func New(ctx context.Context, wasmBytes []byte, opts Options) (*Engine, error) {
	if len(wasmBytes) == 0 {
		return nil, errors.New("abi4: empty wasm binary")
	}
	if err := opts.Plan.Validate(); err != nil {
		return nil, err
	}
	if opts.InCap == 0 {
		opts.InCap = DefaultInCap
	}
	if opts.OutCap == 0 {
		opts.OutCap = DefaultOutCap
	}
	if opts.InCap < hdrLen || opts.OutCap <= 0 ||
		uint64(opts.InCap)+uint64(opts.OutCap) > maxU32 {
		return nil, errors.New("abi4: invalid window capacities")
	}
	if opts.MaxMemoryPages == 0 {
		opts.MaxMemoryPages = DefaultMaxMemoryPages
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultHookTimeout
	}
	if opts.RoutePools < 0 {
		opts.RoutePools = 0
	}

	loadCtx, cancel := context.WithTimeout(ctx, DefaultLoadTimeout)
	defer cancel()

	rt := wazero.NewRuntimeWithConfig(loadCtx,
		wazero.NewRuntimeConfig().WithMemoryLimitPages(opts.MaxMemoryPages).WithCloseOnContextDone(true))
	compiled, err := rt.CompileModule(loadCtx, wasmBytes)
	if err != nil {
		_ = rt.Close(loadCtx)
		return nil, fmt.Errorf("abi4: compile: %w", err)
	}
	if err := validateExports(compiled); err != nil {
		_ = rt.Close(loadCtx)
		return nil, err
	}

	e := &Engine{
		rt:          rt,
		compiled:    compiled,
		plan:        opts.Plan,
		inCap:       uint32(opts.InCap),  // #nosec G115 -- positive config value
		outCap:      uint32(opts.OutCap), // #nosec G115 -- positive config value
		timeout:     opts.Timeout,
		loadTimeout: DefaultLoadTimeout,
		routePools:  opts.RoutePools,
	}
	warm, err := e.instantiate(loadCtx)
	if err != nil {
		_ = rt.Close(loadCtx)
		return nil, err
	}
	e.abi = warm.abi
	e.caps = warm.caps
	e.bufPtr = warm.bufPtr
	e.bufLen = warm.bufLen
	if !e.caps.Has(CapBlob) {
		_ = warm.close()
		_ = rt.Close(loadCtx)
		return nil, errors.New("abi4: module must declare CapBlob")
	}
	if need := opts.Plan.RequiredCaps(); !e.caps.Has(need) {
		_ = warm.close()
		_ = rt.Close(loadCtx)
		return nil, fmt.Errorf("abi4: plan requires caps %b, module declares %b", need, e.caps)
	}
	if e.bufLen < e.inCap+e.outCap {
		_ = warm.close()
		_ = rt.Close(loadCtx)
		return nil, fmt.Errorf("abi4: window is %d bytes, need %d", e.bufLen, e.inCap+e.outCap)
	}
	if uint64(e.bufPtr)+uint64(e.bufLen) > uint64(warm.mem.Size()) {
		_ = warm.close()
		_ = rt.Close(loadCtx)
		return nil, errors.New("abi4: window is outside linear memory")
	}

	e.pool.New = func() any {
		if e.initErr.Load() != nil {
			return nil
		}
		instCtx, instCancel := context.WithTimeout(context.Background(), e.loadTimeout)
		defer instCancel()
		inst, err := e.instantiate(instCtx)
		if err != nil {
			e.initErr.Store(&err)
			return nil
		}
		return inst
	}
	e.idxPool.New = func() any {
		buf := make([]Ent, 128)
		return &buf
	}
	e.pool.Put(warm)
	runtime.AddCleanup(e, func(rt wazero.Runtime) {
		_ = rt.Close(context.Background())
	}, e.rt)
	return e, nil
}

func validateExports(compiled wazero.CompiledModule) error {
	exports := compiled.ExportedFunctions()
	required := map[string][]api.ValueType{
		"on_event":   {api.ValueTypeI64, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32},
		"wg_abi":     {},
		"wg_caps":    {},
		"wg_buf_ptr": {},
		"wg_buf_len": {},
	}
	for name, params := range required {
		fn, ok := exports[name]
		if !ok {
			return fmt.Errorf("abi4: missing export %s", name)
		}
		if !equalTypes(fn.ParamTypes(), params) {
			return fmt.Errorf("abi4: %s has wrong parameters", name)
		}
	}
	if !equalTypes(exports["on_event"].ResultTypes(), []api.ValueType{api.ValueTypeI64}) {
		return errors.New("abi4: on_event must return i64")
	}
	if !equalTypes(exports["wg_abi"].ResultTypes(), []api.ValueType{api.ValueTypeI32}) {
		return errors.New("abi4: wg_abi must return i32")
	}
	if !equalTypes(exports["wg_caps"].ResultTypes(), []api.ValueType{api.ValueTypeI64}) {
		return errors.New("abi4: wg_caps must return i64")
	}
	if !equalTypes(exports["wg_buf_ptr"].ResultTypes(), []api.ValueType{api.ValueTypeI32}) ||
		!equalTypes(exports["wg_buf_len"].ResultTypes(), []api.ValueType{api.ValueTypeI32}) {
		return errors.New("abi4: window exports must return i32")
	}
	for _, legacy := range []string{"on_headers", "on_data", "on_data_ex", "on_data_state"} {
		if _, ok := exports[legacy]; ok {
			return fmt.Errorf("abi4: module exports legacy %s; use on_event", legacy)
		}
	}
	return nil
}

func equalTypes(a, b []api.ValueType) bool {
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

func (e *Engine) instantiate(ctx context.Context) (*instance, error) {
	id := e.nextID.Add(1)
	mod, err := e.rt.InstantiateModule(ctx, e.compiled,
		wazero.NewModuleConfig().WithName(fmt.Sprintf("abi4-%d", id)))
	if err != nil {
		return nil, fmt.Errorf("abi4: instantiate: %w", err)
	}
	mem := mod.Memory()
	if mem == nil {
		_ = mod.Close(ctx)
		return nil, errors.New("abi4: module has no exported memory")
	}
	call := func(name string, stack []uint64) error {
		fn := mod.ExportedFunction(name)
		if fn == nil {
			return fmt.Errorf("abi4: missing %s", name)
		}
		return fn.CallWithStack(ctx, stack)
	}
	var stack [1]uint64
	if err := call("wg_abi", stack[:]); err != nil {
		_ = mod.Close(ctx)
		return nil, err
	}
	abi := uint32(stack[0])
	if abi>>8 != 4 {
		_ = mod.Close(ctx)
		return nil, fmt.Errorf("abi4: unsupported ABI major %d", abi>>8)
	}
	if err := call("wg_caps", stack[:]); err != nil {
		_ = mod.Close(ctx)
		return nil, err
	}
	caps := Caps(stack[0])
	if err := call("wg_buf_ptr", stack[:]); err != nil {
		_ = mod.Close(ctx)
		return nil, err
	}
	ptr := uint32(stack[0]) // #nosec G115 -- module-provided i32
	if err := call("wg_buf_len", stack[:]); err != nil {
		_ = mod.Close(ctx)
		return nil, err
	}
	length := uint32(stack[0]) // #nosec G115 -- module-provided i32
	if !caps.Has(CapBlob) {
		_ = mod.Close(ctx)
		return nil, errors.New("abi4: module must declare CapBlob")
	}
	if need := e.plan.RequiredCaps(); !caps.Has(need) {
		_ = mod.Close(ctx)
		return nil, fmt.Errorf("abi4: plan requires caps %b, module declares %b", need, caps)
	}
	if uint64(length) < uint64(e.inCap)+uint64(e.outCap) {
		_ = mod.Close(ctx)
		return nil, fmt.Errorf("abi4: window is %d bytes, need %d", length, uint64(e.inCap)+uint64(e.outCap))
	}
	if uint64(ptr)+uint64(length) > uint64(mem.Size()) {
		_ = mod.Close(ctx)
		return nil, errors.New("abi4: window is outside linear memory")
	}
	inst := &instance{
		mod:     mod,
		onEvent: mod.ExportedFunction("on_event"),
		mem:     mem,
		bufPtr:  ptr,
		inCap:   e.inCap,
		abi:     abi,
		caps:    caps,
		bufLen:  length,
	}
	if inst.onEvent == nil {
		_ = mod.Close(ctx)
		return nil, errors.New("abi4: missing on_event")
	}
	return inst, nil
}

// OnEvent writes one envelope and invokes on_event. It does not copy the IN
// buffer back: v4.0 is read-only. Mutations are a separate OUT-section feature.
func (e *Engine) OnEvent(ctx context.Context, connID uint64, phase Phase, flags Flags, ev Event) (Verdict, error) {
	if e == nil {
		return VerdictAllow, nil
	}
	if !e.plan.Has(phase) {
		return VerdictDeny, fmt.Errorf("abi4: phase %d is not in the inspection plan", phase)
	}
	if flags&^(FlagFirst|FlagLast|FlagTruncated) != 0 {
		return VerdictDeny, fmt.Errorf("abi4: unknown event flags %b", flags)
	}
	if len(ev.Block) == 0 {
		return VerdictDeny, errors.New("abi4: empty event")
	}
	if err := ctx.Err(); err != nil {
		return VerdictDeny, err
	}
	inst, ok := e.pool.Get().(*instance)
	if !ok || inst == nil {
		return VerdictDeny, fmt.Errorf("abi4: instantiate failed: %v", e.initError())
	}
	win, ok := inst.mem.Read(inst.bufPtr, e.inCap)
	if !ok {
		e.discard(inst)
		return VerdictDeny, errors.New("abi4: IN window out of bounds")
	}
	total, err := Write(win, ev)
	if err != nil {
		e.pool.Put(inst)
		return VerdictDeny, err
	}

	inst.stack[0] = connID
	inst.stack[1] = uint64(phase)
	inst.stack[2] = uint64(flags)
	inst.stack[3] = uint64(inst.bufPtr)
	inst.stack[4] = uint64(total)
	callCtx, cancel := context.WithTimeout(ctx, e.timeout)
	callErr := inst.onEvent.CallWithStack(callCtx, inst.stack[:])
	cancel()
	if callErr != nil {
		e.discard(inst)
		return VerdictDeny, callErr
	}
	v := Verdict(int32(uint32(inst.stack[0]))) // #nosec G115 -- low i32 is the verdict
	if err := ValidateVerdict(v, e.routePools); err != nil {
		e.pool.Put(inst)
		return VerdictDeny, err
	}
	e.pool.Put(inst)
	if v == VerdictDeny {
		return VerdictDeny, nil
	}
	return v, nil
}

// OnRequestHeaders is the typed request-header entry point for M1 integration.
// The header index is pooled; SplitHeaders rejects overlong or malformed
// requests instead of silently truncating the index.
func (e *Engine) OnRequestHeaders(ctx context.Context, connID uint64, block []byte) (Verdict, error) {
	if e == nil {
		return VerdictAllow, nil
	}
	buf, _ := e.idxPool.Get().(*[]Ent)
	if buf == nil {
		b := make([]Ent, 128)
		buf = &b
	}
	defer e.idxPool.Put(buf)
	n, start, err := SplitHeaders(block, *buf)
	if err != nil {
		return VerdictDeny, err
	}
	return e.OnEvent(ctx, connID, PhaseRequestHeaders, FlagFirst|FlagLast, Event{
		Block: block, Index: (*buf)[:n], Start: &start,
	})
}

// OnOpaqueFirst is the pre-dial opaque first-chunk entry point.
func (e *Engine) OnOpaqueFirst(ctx context.Context, connID uint64, chunk []byte) (Verdict, error) {
	return e.OnEvent(ctx, connID, PhaseOpaqueStream, FlagFirst, Event{Block: chunk})
}

// OnStream is the post-dial opaque chunk entry point.
func (e *Engine) OnStream(ctx context.Context, connID uint64, chunk []byte) (Verdict, error) {
	return e.OnEvent(ctx, connID, PhaseOpaqueStream, 0, Event{Block: chunk})
}

func (e *Engine) discard(inst *instance) {
	e.discarded.Add(1)
	_ = inst.mod.Close(context.Background())
}

func (i *instance) close() error {
	return i.mod.Close(context.Background())
}

func (e *Engine) initError() error {
	if err := e.initErr.Load(); err != nil {
		return *err
	}
	return nil
}

// ABI, Caps, Plan and Discarded expose validated engine metadata.
func (e *Engine) ABI() uint32      { return e.abi }
func (e *Engine) Caps() Caps       { return e.caps }
func (e *Engine) Plan() Plan       { return e.plan }
func (e *Engine) Discarded() int64 { return e.discarded.Load() }

// Close stops the runtime. Idempotent.
func (e *Engine) Close(_ context.Context) error {
	if e == nil {
		return nil
	}
	e.closeOnce.Do(func() {
		_ = e.rt.Close(context.Background())
	})
	return nil
}
