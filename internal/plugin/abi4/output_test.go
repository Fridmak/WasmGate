package abi4

import (
	"bytes"
	"errors"
	"testing"
)

func TestOutputRoundTrip(t *testing.T) {
	dst := make([]byte, 256)
	n, err := WriteOutput(dst, Output{
		Mutation:        []byte("rewritten"),
		MutationPresent: true,
		State:           []byte("next-state"),
		StatePresent:    true,
	})
	if err != nil {
		t.Fatalf("WriteOutput: %v", err)
	}
	out, err := ParseOutput(dst[:n])
	if err != nil {
		t.Fatalf("ParseOutput: %v", err)
	}
	if !out.MutationPresent || !bytes.Equal(out.Mutation, []byte("rewritten")) {
		t.Fatalf("mutation = (%v, %q)", out.MutationPresent, out.Mutation)
	}
	if !out.StatePresent || !bytes.Equal(out.State, []byte("next-state")) {
		t.Fatalf("state = (%v, %q)", out.StatePresent, out.State)
	}
}

func TestOutputEmptySectionsAreRepresentable(t *testing.T) {
	dst := make([]byte, 128)
	n, err := WriteOutput(dst, Output{StatePresent: true, MutationPresent: true})
	if err != nil {
		t.Fatalf("WriteOutput: %v", err)
	}
	out, err := ParseOutput(dst[:n])
	if err != nil {
		t.Fatalf("ParseOutput: %v", err)
	}
	if !out.StatePresent || !out.MutationPresent || len(out.State) != 0 || len(out.Mutation) != 0 {
		t.Fatalf("empty output = %+v", out)
	}
}

func TestOutputRejectsCorruptionAndCapacity(t *testing.T) {
	dst := make([]byte, 8)
	if _, err := WriteOutput(dst, Output{Mutation: []byte("too large")}); !errors.Is(err, errOutputFull) {
		t.Fatalf("capacity error = %v", err)
	}
	if _, err := ParseOutput([]byte("bad")); err == nil {
		t.Fatal("short output accepted")
	}

	full := make([]byte, 128)
	n, err := WriteOutput(full, Output{Mutation: []byte("x"), MutationPresent: true})
	if err != nil {
		t.Fatal(err)
	}
	full[0] ^= 0xff
	if _, err := ParseOutput(full[:n]); err == nil {
		t.Fatal("corrupt output accepted")
	}
}

func TestStateSlotCommitAndReset(t *testing.T) {
	s, err := NewStateSlot(8)
	if err != nil {
		t.Fatal(err)
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
	s.Reset()
	if len(s.Bytes()) != 0 || !bytes.Equal(s.buf, make([]byte, 8)) {
		t.Fatalf("reset did not clear state: len=%d bytes=%v", len(s.Bytes()), s.buf)
	}
}

func FuzzParseOutput(f *testing.F) {
	dst := make([]byte, 128)
	n, _ := WriteOutput(dst, Output{Mutation: []byte("seed"), MutationPresent: true})
	f.Add(dst[:n])
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ParseOutput(data)
	})
}
