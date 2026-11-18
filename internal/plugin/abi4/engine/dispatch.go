package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
)

// EventResult is the validated result of one event. Mutation and State, if
// present, are owned by the caller and do not alias guest memory. The presence
// flags distinguish an explicitly empty section from an omitted section.
type EventResult struct {
	Verdict         Verdict
	Mutation        []byte
	MutationPresent bool
	State           []byte
	StatePresent    bool
}

// OnEvent is the read-only entry point: it rejects effects that this caller
// has no policy for, rather than silently dropping successful guest output.
func (e *Engine) OnEvent(ctx context.Context, connID uint64, phase Phase, flags Flags, ev Event) (Verdict, error) {
	result, err := e.OnEventResult(ctx, connID, phase, flags, ev)
	if err == nil && (result.MutationPresent || result.StatePresent) {
		return VerdictDeny, errors.New("abi4: effects are not enabled for this event")
	}
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
	truncatedBody := (phase == PhaseRequestBody || phase == PhaseResponseBody) &&
		flags&(FlagLast|FlagTruncated) == FlagLast|FlagTruncated
	if len(ev.Block) == 0 && !truncatedBody {
		return deny(errors.New("abi4: empty event"))
	}
	if err := ctx.Err(); err != nil {
		return deny(err)
	}
	if ev.State != nil && !e.caps.Has(CapStateIn) {
		return deny(errors.New("abi4: input state requires CapStateIn"))
	}
	inst, err := e.borrow(ctx)
	if err != nil {
		return deny(err)
	}
	win, ok := inst.mem.Read(inst.bufPtr, e.inCap)
	if !ok {
		e.discard(inst)
		return deny(errors.New("abi4: IN window out of bounds"))
	}
	total, err := Write(win, ev)
	if err != nil {
		e.release(inst)
		return deny(err)
	}
	if inst.outLen != 0 {
		out, outputOK := inst.mem.Read(inst.outPtr, e.outCap)
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
	inst.stack[4] = uint64(total) // #nosec G115 -- Write validated a nonnegative envelope size
	callCtx, cancel := context.WithTimeout(ctx, e.timeout)
	callErr := inst.onEvent.CallWithStack(callCtx, inst.stack[:])
	cancel()
	if callErr != nil {
		e.discard(inst)
		return deny(callErr)
	}
	v := Verdict(int32(uint32(inst.stack[0]))) // #nosec G115 -- low i32 is the verdict
	if err := ValidateVerdict(v, e.routePools); err != nil {
		e.release(inst)
		return deny(err)
	}
	result := EventResult{Verdict: v}
	if inst.outLen != 0 {
		// A guest can grow its memory during the call, invalidating the old view.
		out, ok := inst.mem.Read(inst.outPtr, e.outCap)
		if !ok {
			e.discard(inst)
			return deny(errors.New("abi4: OUT window out of bounds after on_event"))
		}
		// Only a completely untouched (zeroed) window means no output.
		if binary.LittleEndian.Uint32(out) != 0 || bytes.Count(out, []byte{0}) != len(out) {
			parsed, parseErr := ParseOutput(out)
			if parseErr != nil {
				e.discard(inst)
				return deny(parseErr)
			}
			if parsed.MutationPresent && !inst.caps.Has(CapMutation) {
				e.discard(inst)
				return deny(errors.New("abi4: output mutation requires CapMutation"))
			}
			if parsed.StatePresent && !inst.caps.Has(CapStateOut) {
				e.discard(inst)
				return deny(errors.New("abi4: output state requires CapStateOut"))
			}
			if v == VerdictAllow && !truncatedBody {
				// Copy while the instance is still exclusively held by this call.
				// Do not clone empty views: they can still reference guest memory.
				result.Mutation = append([]byte(nil), parsed.Mutation...)
				result.MutationPresent = parsed.MutationPresent
				result.State = append([]byte(nil), parsed.State...)
				result.StatePresent = parsed.StatePresent
			}
		}
	}
	e.release(inst)
	return result, nil
}

// OnRequestBody dispatches an opt-in request-body event and returns validated
// host-owned effects when the guest allows it.
func (e *Engine) OnRequestBody(ctx context.Context, connID uint64, flags Flags, ev Event) (EventResult, error) {
	return e.OnEventResult(ctx, connID, PhaseRequestBody, flags, ev)
}

// OnResponseHeaders dispatches an opt-in response-header event.
func (e *Engine) OnResponseHeaders(ctx context.Context, connID uint64, flags Flags, ev Event) (EventResult, error) {
	return e.OnEventResult(ctx, connID, PhaseResponseHeaders, flags, ev)
}

// OnResponseBody dispatches an opt-in response-body event and returns validated
// host-owned effects when the guest allows it.
func (e *Engine) OnResponseBody(ctx context.Context, connID uint64, flags Flags, ev Event) (EventResult, error) {
	return e.OnEventResult(ctx, connID, PhaseResponseBody, flags, ev)
}
