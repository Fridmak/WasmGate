package engine

import "context"

// DispatchWithState sends the current slot as IN state and commits OUT state
// only when the guest allowed the event. A deny/error leaves the previous slot
// untouched.
func (e *Engine) DispatchWithState(ctx context.Context, connID uint64, phase Phase, flags Flags, ev Event, slot *StateSlot) (EventResult, error) {
	if slot == nil {
		return e.OnEventResult(ctx, connID, phase, flags, ev)
	}
	ev.State = slot.Bytes()
	result, err := e.OnEventResult(ctx, connID, phase, flags, ev)
	if err != nil || result.Verdict != VerdictAllow {
		return result, err
	}
	if result.StatePresent {
		if err := slot.Commit(result.State); err != nil {
			return EventResult{Verdict: VerdictDeny}, err
		}
	}
	return result, nil
}
