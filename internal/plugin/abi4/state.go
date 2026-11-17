package abi4

import (
	"context"
	"errors"
)

var errStateCapacity = errors.New("abi4: state exceeds slot capacity")

// StateSlot is host-owned bounded state for one connection/direction. It is
// deliberately separate from pooled WASM instances: instance reuse must never
// imply state reuse.
type StateSlot struct {
	buf []byte
	len int
}

// NewStateSlot allocates a zeroed bounded state slot.
func NewStateSlot(capacity int) (*StateSlot, error) {
	if capacity < 0 || capacity > maxPayload {
		return nil, errStateCapacity
	}
	return &StateSlot{buf: make([]byte, capacity)}, nil
}

// Bytes returns the current state view. The view is owned by the slot and is
// invalidated by Commit or Reset.
func (s *StateSlot) Bytes() []byte {
	if s == nil {
		return nil
	}
	return s.buf[:s.len]
}

// Capacity reports the fixed state capacity.
func (s *StateSlot) Capacity() int {
	if s == nil {
		return 0
	}
	return len(s.buf)
}

// Commit atomically replaces the state after a successful guest verdict.
func (s *StateSlot) Commit(next []byte) error {
	if s == nil || len(next) > len(s.buf) {
		return errStateCapacity
	}
	copy(s.buf, next)
	clear(s.buf[len(next):])
	s.len = len(next)
	return nil
}

// Reset clears the slot and prevents state from crossing connection reuse.
func (s *StateSlot) Reset() {
	if s == nil {
		return
	}
	clear(s.buf)
	s.len = 0
}

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
