package engine

import (
	"context"
	"errors"
	"testing"
)

func TestEngineIdlePoolClosesOverflow(t *testing.T) {
	ctx := context.Background()
	e, err := New(ctx, allowModule(), Options{Plan: InspectRequestHeaders})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close(ctx)
	var instances [maxIdleInstances + 1]*instance
	for i := range instances {
		instances[i], err = e.borrow(ctx)
		if err != nil {
			t.Fatal(err)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := e.borrow(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("borrow on cancelled context = %v", err)
	}
	extra, err := e.borrow(ctx)
	if err != nil {
		t.Fatalf("new borrow after cancellation: %v", err)
	}
	for _, inst := range instances {
		e.release(inst)
	}
	e.release(extra)
	if len(e.pool) != maxIdleInstances || !instances[maxIdleInstances].mod.IsClosed() || !extra.mod.IsClosed() {
		t.Fatal("excess Wasm instances were retained without close")
	}
}

func TestEngineStateFollowsSlotNotInstance(t *testing.T) {
	ctx := context.Background()
	wasm := outputModule(outputEvent(t, Output{State: []byte("next")}, VerdictAllow), headerCaps|CapStateIn|CapStateOut)
	e, err := New(ctx, wasm, Options{Plan: InspectRequestHeaders | InspectRequestBody})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close(ctx)
	a, err := NewStateSlot(16)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewStateSlot(16)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id   uint64
		slot *StateSlot
		want string
	}{
		{1, a, ""},
		{1, a, "next"},
		{2, b, ""},
	} {
		r, err := e.DispatchWithState(ctx, tc.id, PhaseRequestBody, FlagFirst|FlagLast, Event{Block: []byte("body")}, tc.slot)
		if err != nil || r.Verdict != VerdictAllow || string(tc.slot.Bytes()) != "next" {
			t.Fatalf("exchange %d state commit = (%+v, %v, %q)", tc.id, r, err, tc.slot.Bytes())
		}
		inst := <-e.pool
		win, ok := inst.mem.Read(inst.bufPtr, e.inCap)
		if !ok {
			t.Fatal("IN window unavailable")
		}
		event, err := Parse(win)
		if err != nil || string(event.State) != tc.want || (event.State == nil) != (tc.want == "") {
			t.Fatalf("exchange %d IN envelope = (%+v, %v), want state %q", tc.id, event, err, tc.want)
		}
		e.pool <- inst
	}
}
