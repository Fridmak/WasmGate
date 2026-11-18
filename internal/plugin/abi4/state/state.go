package state

import "errors"

var errStateCapacity = errors.New("abi4: state exceeds slot capacity")

// MaxStateSlot bounds allocation even when requested outside the proxy.
const MaxStateSlot = 64 * 1024

// Slot is host-owned bounded state for one connection/direction. It is
// deliberately separate from pooled WASM instances: instance reuse must never
// imply state reuse. Its owner must serialize access; a slot is not thread-safe.
type Slot struct {
	buf     []byte
	len     int
	present bool
}

// NewStateSlot allocates a zeroed bounded state slot.
func NewStateSlot(capacity int) (*Slot, error) {
	if capacity < 0 || capacity > MaxStateSlot {
		return nil, errStateCapacity
	}
	return &Slot{buf: make([]byte, capacity)}, nil
}

// Bytes returns a read-only view, invalidated by Commit or Reset. Nil means
// absent; a non-nil, empty slice is an explicitly committed empty state.
func (s *Slot) Bytes() []byte {
	if s == nil || !s.present {
		return nil
	}
	return s.buf[:s.len:s.len]
}

// Capacity reports the fixed state capacity in bytes.
func (s *Slot) Capacity() int {
	if s == nil {
		return 0
	}
	return len(s.buf)
}

// Commit replaces state after the caller validates a guest's allowed effects.
// Passing nil commits an explicitly empty state, not an absent one.
func (s *Slot) Commit(next []byte) error {
	if s == nil || len(next) > len(s.buf) {
		return errStateCapacity
	}
	copy(s.buf, next)
	if len(next) < s.len {
		clear(s.buf[len(next):s.len])
	}
	s.len = len(next)
	s.present = true
	return nil
}

// Reset clears the slot and prevents state from crossing connection reuse.
func (s *Slot) Reset() {
	if s == nil {
		return
	}
	clear(s.buf[:s.len])
	s.len = 0
	s.present = false
}
