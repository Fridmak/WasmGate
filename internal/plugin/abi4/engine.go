package abi4

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
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
	// DefaultHeaderIndexCap bounds the pooled header index used by
	// OnRequestHeaders. 512 entries is already far above real-world HTTP
	// requests and keeps the per-event buffer at 8 KiB.
	DefaultHeaderIndexCap = 512
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
	// HeaderIndexCap is the maximum number of headers accepted by
	// OnRequestHeaders. Zero selects DefaultHeaderIndexCap.
	HeaderIndexCap int
}

type instance struct {
	mod       api.Module
	onEvent   api.Function
	onAccept  api.Function
	onClose   api.Function
	onCloseEx api.Function
	mem       api.Memory
	bufPtr    uint32
	bufLen    uint32
	outPtr    uint32
	outLen    uint32
	abi       uint32
	caps      Caps
	stack     [5]uint64
}

// Engine is a compiled v4 module plus a pool of reusable instances. It is safe
// for concurrent event calls; Close must not race with in-flight events. After
// warm-up, payload writes and header indexing avoid allocations; hook deadlines
// currently allocate one context per event (tracked as the P1 wazero
// follow-up, not part of ABI correctness).
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
	headerCap   int
	output      bool
	hasAccept   bool
	hasClose    bool
	hasCloseEx  bool
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
	if opts.HeaderIndexCap == 0 {
		opts.HeaderIndexCap = DefaultHeaderIndexCap
	}
	if opts.HeaderIndexCap < 1 || opts.HeaderIndexCap > maxEntries {
		return nil, fmt.Errorf("abi4: header index cap must be between 1 and %d", maxEntries)
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
	if err := validateExports(compiled, opts.Plan); err != nil {
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
		headerCap:   opts.HeaderIndexCap,
	}
	warm, err := e.instantiate(loadCtx)
	if err != nil {
		_ = rt.Close(loadCtx)
		return nil, err
	}
	e.abi = warm.abi
	e.caps = warm.caps
	e.hasAccept = warm.onAccept != nil
	e.hasClose = warm.onClose != nil
	e.hasCloseEx = warm.onCloseEx != nil
	e.output = warm.outLen != 0
	if e.plan.HasEventPhases() {
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
		buf := make([]Ent, e.headerCap)
		return &buf
	}
	e.pool.Put(warm)
	runtime.AddCleanup(e, func(rt wazero.Runtime) {
		_ = rt.Close(context.Background())
	}, e.rt)
	return e, nil
}

func validateExports(compiled wazero.CompiledModule, plan Plan) error {
	exports := compiled.ExportedFunctions()
	required := map[string][]api.ValueType{
		"wg_abi":  {},
		"wg_caps": {},
	}
	if plan.HasEventPhases() {
		required["on_event"] = []api.ValueType{
			api.ValueTypeI64, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32,
		}
		required["wg_buf_ptr"] = []api.ValueType{}
		required["wg_buf_len"] = []api.ValueType{}
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
	if !equalTypes(exports["wg_abi"].ResultTypes(), []api.ValueType{api.ValueTypeI32}) {
		return errors.New("abi4: wg_abi must return i32")
	}
	if !equalTypes(exports["wg_caps"].ResultTypes(), []api.ValueType{api.ValueTypeI64}) {
		return errors.New("abi4: wg_caps must return i64")
	}
	for _, name := range []string{"wg_out_ptr", "wg_out_len"} {
		if fn, ok := exports[name]; ok && !equalTypes(fn.ParamTypes(), []api.ValueType{}) {
			return fmt.Errorf("abi4: %s has wrong parameters", name)
		}
		if fn, ok := exports[name]; ok && !equalTypes(fn.ResultTypes(), []api.ValueType{api.ValueTypeI32}) {
			return fmt.Errorf("abi4: %s must return i32", name)
		}
	}
	if plan.HasEventPhases() {
		if !equalTypes(exports["on_event"].ResultTypes(), []api.ValueType{api.ValueTypeI64}) {
			return errors.New("abi4: on_event must return i64")
		}
		if !equalTypes(exports["wg_buf_ptr"].ResultTypes(), []api.ValueType{api.ValueTypeI32}) ||
			!equalTypes(exports["wg_buf_len"].ResultTypes(), []api.ValueType{api.ValueTypeI32}) {
			return errors.New("abi4: window exports must return i32")
		}
	}
	if plan.HasLifecycle() {
		hooks := 0
		if fn, ok := exports["on_accept"]; ok {
			if !equalTypes(fn.ParamTypes(), []api.ValueType{api.ValueTypeI64, api.ValueTypeI64, api.ValueTypeI32}) ||
				!equalTypes(fn.ResultTypes(), []api.ValueType{api.ValueTypeI32}) {
				return errors.New("abi4: on_accept must be (i64, i64, i32) -> i32")
			}
			hooks++
		}
		if fn, ok := exports["on_close"]; ok {
			if !equalTypes(fn.ParamTypes(), []api.ValueType{api.ValueTypeI64, api.ValueTypeI64, api.ValueTypeI64, api.ValueTypeI64}) ||
				len(fn.ResultTypes()) != 0 {
				return errors.New("abi4: on_close must be (i64, i64, i64, i64) -> ()")
			}
			hooks++
		}
		if fn, ok := exports["on_close_ex"]; ok {
			if !equalTypes(fn.ParamTypes(), []api.ValueType{api.ValueTypeI64, api.ValueTypeI64, api.ValueTypeI64, api.ValueTypeI64, api.ValueTypeI64}) ||
				len(fn.ResultTypes()) != 0 {
				return errors.New("abi4: on_close_ex must be (i64, i64, i64, i64, i64) -> ()")
			}
			hooks++
		}
		if hooks == 0 {
			return errors.New("abi4: lifecycle plan requires on_accept, on_close or on_close_ex")
		}
		if !plan.HasEventPhases() {
			if _, ok := exports["on_event"]; ok {
				return errors.New("abi4: lifecycle-only module must not export on_event")
			}
		}
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
	if e.plan.HasEventPhases() && mem == nil {
		_ = mod.Close(ctx)
		return nil, errors.New("abi4: event module has no exported memory")
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
	inst := &instance{
		mod:       mod,
		onEvent:   mod.ExportedFunction("on_event"),
		onAccept:  mod.ExportedFunction("on_accept"),
		onClose:   mod.ExportedFunction("on_close"),
		onCloseEx: mod.ExportedFunction("on_close_ex"),
		mem:       mem,
		abi:       abi,
		caps:      caps,
	}
	if e.plan.HasEventPhases() {
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
		inst.onEvent = mod.ExportedFunction("on_event")
		if inst.onEvent == nil {
			_ = mod.Close(ctx)
			return nil, errors.New("abi4: missing on_event")
		}
		inst.bufPtr = ptr
		inst.bufLen = length
		// Body/response plans require state output. Mutation is enabled by those
		// opt-in plans in M4; opaque/header plans stay read-only until then.
		needsOutput := e.plan.RequiredCaps().Has(CapStateOut) || caps.Has(CapMutation)
		if needsOutput {
			outPtrFn := mod.ExportedFunction("wg_out_ptr")
			outLenFn := mod.ExportedFunction("wg_out_len")
			if outPtrFn == nil || outLenFn == nil {
				_ = mod.Close(ctx)
				return nil, errors.New("abi4: output caps require wg_out_ptr and wg_out_len")
			}
			if err := outPtrFn.CallWithStack(ctx, stack[:]); err != nil {
				_ = mod.Close(ctx)
				return nil, err
			}
			outPtr := uint32(stack[0]) // #nosec G115 -- module-provided i32
			if err := outLenFn.CallWithStack(ctx, stack[:]); err != nil {
				_ = mod.Close(ctx)
				return nil, err
			}
			outLen := uint32(stack[0]) // #nosec G115 -- module-provided i32
			if uint64(outLen) < uint64(e.outCap) || uint64(outPtr)+uint64(outLen) > uint64(mem.Size()) {
				_ = mod.Close(ctx)
				return nil, errors.New("abi4: output window is invalid or too small")
			}
			inst.outPtr = outPtr
			inst.outLen = outLen
		}
	}
	if e.plan.HasLifecycle() && inst.onAccept == nil && inst.onClose == nil && inst.onCloseEx == nil {
		_ = mod.Close(ctx)
		return nil, errors.New("abi4: missing lifecycle hooks")
	}
	return inst, nil
}

// EventResult is the validated result of one event. Mutation and State are
// zero-copy views into the guest OUT window and must be consumed before the
// next event on the same instance.
type EventResult struct {
	Verdict         Verdict
	Mutation        []byte
	MutationPresent bool
	State           []byte
	StatePresent    bool
}

// OnEvent writes one envelope and invokes on_event. It is the compatibility
// convenience API for callers that only need a verdict.
func (e *Engine) OnEvent(ctx context.Context, connID uint64, phase Phase, flags Flags, ev Event) (Verdict, error) {
	result, err := e.OnEventResult(ctx, connID, phase, flags, ev)
	return result.Verdict, err
}

// OnEventResult invokes on_event and, when the module opted into output caps,
// validates and returns the bounded OUT envelope. The host does not apply a
// mutation automatically; callers commit it only after an allow verdict.
func (e *Engine) OnEventResult(ctx context.Context, connID uint64, phase Phase, flags Flags, ev Event) (EventResult, error) {
	deny := func(err error) (EventResult, error) {
		return EventResult{Verdict: VerdictDeny}, err
	}
	if e == nil {
		return EventResult{Verdict: VerdictAllow}, nil
	}
	if !e.plan.Has(phase) {
		return deny(fmt.Errorf("abi4: phase %d is not in the inspection plan", phase))
	}
	if flags&^(FlagFirst|FlagLast|FlagTruncated|FlagClientToServer|FlagServerToClient) != 0 {
		return deny(fmt.Errorf("abi4: unknown event flags %b", flags))
	}
	if flags&(FlagClientToServer|FlagServerToClient) == (FlagClientToServer | FlagServerToClient) {
		return deny(errors.New("abi4: event cannot carry both directions"))
	}
	if len(ev.Block) == 0 {
		return deny(errors.New("abi4: empty event"))
	}
	if err := ctx.Err(); err != nil {
		return deny(err)
	}
	inst, ok := e.pool.Get().(*instance)
	if !ok || inst == nil {
		return deny(fmt.Errorf("abi4: instantiate failed: %v", e.initError()))
	}
	win, ok := inst.mem.Read(inst.bufPtr, e.inCap)
	if !ok {
		e.discard(inst)
		return deny(errors.New("abi4: IN window out of bounds"))
	}
	total, err := Write(win, ev)
	if err != nil {
		e.pool.Put(inst)
		return deny(err)
	}
	var out []byte
	if e.output {
		var outputOK bool
		out, outputOK = inst.mem.Read(inst.outPtr, e.outCap)
		if !outputOK {
			e.discard(inst)
			return deny(errors.New("abi4: OUT window out of bounds"))
		}
		clear(out)
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
		return deny(callErr)
	}
	v := Verdict(int32(uint32(inst.stack[0]))) // #nosec G115 -- low i32 is the verdict
	if err := ValidateVerdict(v, e.routePools); err != nil {
		e.pool.Put(inst)
		return deny(err)
	}
	result := EventResult{Verdict: v}
	if e.output {
		// An all-zero OUT window means the guest produced no mutation/state.
		// This keeps allow-only body policies cheap while still validating any
		// non-empty output envelope strictly.
		if binary.LittleEndian.Uint32(out) != 0 {
			parsed, parseErr := ParseOutput(out)
			if parseErr != nil {
				e.discard(inst)
				return deny(parseErr)
			}
			result.Mutation = parsed.Mutation
			result.MutationPresent = parsed.MutationPresent
			result.State = parsed.State
			result.StatePresent = parsed.StatePresent
		}
	}
	e.pool.Put(inst)
	if v == VerdictDeny {
		result.Mutation = nil
		result.MutationPresent = false
		result.State = nil
		result.StatePresent = false
	}
	return result, nil
}

// OnRequestBody dispatches an opt-in request-body event.
func (e *Engine) OnRequestBody(ctx context.Context, connID uint64, flags Flags, ev Event) (EventResult, error) {
	return e.OnEventResult(ctx, connID, PhaseRequestBody, flags, ev)
}

// OnResponseHeaders dispatches an opt-in response-header event.
func (e *Engine) OnResponseHeaders(ctx context.Context, connID uint64, flags Flags, ev Event) (EventResult, error) {
	return e.OnEventResult(ctx, connID, PhaseResponseHeaders, flags, ev)
}

// OnResponseBody dispatches an opt-in response-body event.
func (e *Engine) OnResponseBody(ctx context.Context, connID uint64, flags Flags, ev Event) (EventResult, error) {
	return e.OnEventResult(ctx, connID, PhaseResponseBody, flags, ev)
}

// OnAccept invokes the ABI v4 L4 on_accept(ip_hi, ip_lo, port) hook. Any
// nonzero result denies; the hook cannot route. If the module does not export
// on_accept, the connection is allowed without touching the instance pool.
func (e *Engine) OnAccept(ctx context.Context, ip net.IP, port int) (Verdict, error) {
	if e == nil || !e.hasAccept {
		return VerdictAllow, nil
	}
	inst, ok := e.pool.Get().(*instance)
	if !ok || inst == nil {
		return VerdictDeny, fmt.Errorf("abi4: instantiate failed: %v", e.initError())
	}
	hi, lo := encodeIP(ip)
	inst.stack[0] = hi
	inst.stack[1] = lo
	inst.stack[2] = uint64(port) & 0xffffffff // #nosec G115 -- TCP port is 0..65535
	callCtx, cancel := context.WithTimeout(ctx, e.timeout)
	callErr := inst.onAccept.CallWithStack(callCtx, inst.stack[:3])
	cancel()
	if callErr != nil {
		e.discard(inst)
		return VerdictDeny, callErr
	}
	v := int32(uint32(inst.stack[0])) // #nosec G115 -- WASM i32 result
	e.pool.Put(inst)
	if v != 0 {
		return VerdictDeny, nil
	}
	return VerdictAllow, nil
}

// OnCloseConn invokes on_close_ex(conn_id, ip_hi, ip_lo, rx, tx) when exported,
// otherwise on_close(ip_hi, ip_lo, rx, tx). It is a best-effort lifecycle hook:
// callers log/discard a failed close but do not resurrect the connection.
func (e *Engine) OnCloseConn(ctx context.Context, connID uint64, ip net.IP, rxBytes, txBytes int64) error {
	if e == nil || (!e.hasClose && !e.hasCloseEx) {
		return nil
	}
	inst, ok := e.pool.Get().(*instance)
	if !ok || inst == nil {
		return fmt.Errorf("abi4: instantiate failed: %v", e.initError())
	}
	hi, lo := encodeIP(ip)
	callCtx, cancel := context.WithTimeout(ctx, e.timeout)
	var callErr error
	if e.hasCloseEx {
		inst.stack[0] = connID
		inst.stack[1] = hi
		inst.stack[2] = lo
		inst.stack[3] = uint64(rxBytes) // #nosec G115 -- raw i64 bit pattern
		inst.stack[4] = uint64(txBytes) // #nosec G115 -- raw i64 bit pattern
		callErr = inst.onCloseEx.CallWithStack(callCtx, inst.stack[:5])
	} else {
		inst.stack[0] = hi
		inst.stack[1] = lo
		inst.stack[2] = uint64(rxBytes) // #nosec G115 -- raw i64 bit pattern
		inst.stack[3] = uint64(txBytes) // #nosec G115 -- raw i64 bit pattern
		callErr = inst.onClose.CallWithStack(callCtx, inst.stack[:4])
	}
	cancel()
	if callErr != nil {
		e.discard(inst)
		return callErr
	}
	e.pool.Put(inst)
	return nil
}

// encodeIP packs an IP into two big-endian uint64 values using the IPv4-mapped
// IPv6 representation (::ffff:a.b.c.d) for IPv4 addresses.
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

// OnRequestHeaders is the typed request-header entry point for M1 integration.
// The header index is pooled and bounded by Options.HeaderIndexCap;
// SplitHeaders rejects overlong or malformed requests instead of silently
// truncating the index.
func (e *Engine) OnRequestHeaders(ctx context.Context, connID uint64, block []byte) (Verdict, error) {
	if e == nil {
		return VerdictAllow, nil
	}
	buf, _ := e.idxPool.Get().(*[]Ent)
	if buf == nil {
		b := make([]Ent, e.headerCap)
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
	return e.OnEvent(ctx, connID, PhaseOpaqueStream, FlagFirst|FlagClientToServer, Event{Block: chunk})
}

// OnOpaqueChunk is the direction-aware opaque entry point. dir 0 is
// client-to-server, dir 1 is server-to-client. The direction is encoded into
// Flags; an event carrying both directions is rejected.
func (e *Engine) OnOpaqueChunk(ctx context.Context, connID uint64, dir int32, flags Flags, chunk []byte) (Verdict, error) {
	switch dir {
	case 0:
		flags |= FlagClientToServer
	case 1:
		flags |= FlagServerToClient
	default:
		return VerdictDeny, fmt.Errorf("abi4: invalid opaque direction %d", dir)
	}
	return e.OnEvent(ctx, connID, PhaseOpaqueStream, flags, Event{Block: chunk})
}

// OnStream is the post-dial client-to-server opaque chunk entry point. Use
// OnOpaqueChunk when the host needs direction-aware stream inspection.
func (e *Engine) OnStream(ctx context.Context, connID uint64, chunk []byte) (Verdict, error) {
	return e.OnOpaqueChunk(ctx, connID, 0, 0, chunk)
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
