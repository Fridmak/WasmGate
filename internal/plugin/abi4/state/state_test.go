package state

import (
	"bytes"
	"errors"
	"testing"
)

func TestStateSlotCommitAndReset(t *testing.T) {
	s, err := NewStateSlot(8)
	if err != nil {
		t.Fatal(err)
	}
	if s.Capacity() != 8 {
		t.Fatalf("capacity = %d, want 8", s.Capacity())
	}
	if got := s.Bytes(); got != nil {
		t.Fatalf("new state = %#v, want absent", got)
	}
	if err := s.Commit([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if got := string(s.Bytes()); got != "abc" {
		t.Fatalf("state = %q", got)
	}
	if err := s.Commit([]byte("123456789")); !errors.Is(err, errStateCapacity) {
		t.Fatalf("capacity error = %v", err)
	}
	if err := s.Commit(nil); err != nil || s.Bytes() == nil || len(s.Bytes()) != 0 {
		t.Fatalf("clear state = %v, %v", s.Bytes(), err)
	}
	s.Reset()
	if s.Bytes() != nil || !bytes.Equal(s.buf, make([]byte, 8)) {
		t.Fatalf("reset did not clear state: len=%d bytes=%v", len(s.Bytes()), s.buf)
	}
}

func TestStateSlotCapacityIsBounded(t *testing.T) {
	for _, cap := range []int{-1, MaxStateSlot + 1} {
		if _, err := NewStateSlot(cap); !errors.Is(err, errStateCapacity) {
			t.Fatalf("capacity %d: %v", cap, err)
		}
	}
}
