package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

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
		if a[i] != b[i] { // #nosec G602 -- i is bounded by len(a)==len(b)
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
	abi := uint32(stack[0]) // #nosec G115 -- wg_abi returns an i32
	if abi>>8 != 4 {
		_ = mod.Close(ctx)
		return nil, fmt.Errorf("abi4: unsupported ABI major %d", abi>>8)
	}
	if err := call("wg_caps", stack[:]); err != nil {
		_ = mod.Close(ctx)
		return nil, err
	}
	caps := Caps(stack[0])
	if !caps.Has(e.required) {
		_ = mod.Close(ctx)
		return nil, fmt.Errorf("abi4: plan/policy requires caps %#x, module declares %#x", e.required, caps)
	}
	inst := &instance{
		mod: mod, mem: mem, abi: abi, caps: caps,
	}
	if e.plan.HasLifecycle() {
		inst.onAccept = mod.ExportedFunction("on_accept")
		inst.onClose = mod.ExportedFunction("on_close")
		inst.onCloseEx = mod.ExportedFunction("on_close_ex")
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
		if caps&(CapStateOut|CapMutation) != 0 {
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
			outEnd := uint64(outPtr) + uint64(outLen)
			if e.outCap < hdrLen || outLen < e.outCap || outEnd > uint64(mem.Size()) {
				_ = mod.Close(ctx)
				return nil, errors.New("abi4: output window is invalid or too small")
			}
			if uint64(outPtr) < uint64(ptr)+uint64(e.inCap) && uint64(ptr) < outEnd {
				_ = mod.Close(ctx)
				return nil, errors.New("abi4: IN and OUT windows overlap")
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
