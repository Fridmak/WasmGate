package engine

import (
	"context"
	"encoding/binary"
	"strings"
	"sync"
	"testing"
)

func TestEngineEnvelopeTooLarge(t *testing.T) {
	ctx := context.Background()
	e, err := New(ctx, allowModule(), Options{Plan: InspectRequestHeaders, InCap: 4096, OutCap: 1024})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close(ctx)
	block := []byte("GET / HTTP/1.1\r\nX: " + strings.Repeat("a", 4096) + "\r\n\r\n")
	if v, err := e.OnRequestHeaders(ctx, 1, block); err == nil || v != VerdictDeny {
		t.Fatalf("oversized event = (%v, %v), want deny/error", v, err)
	}
}

// outputStores emits i32.store instructions that the guest executes inside
// on_event, after the host clears OUT. The envelope writer itself is tested
// independently in output_test.go.
func outputStores(win []byte) (body []byte) {
	for off := 0; off < len(win); off += 4 {
		body = append(body, 0x41) // i32.const (guest address)
		body = append(body, sleb(int32(testOutPtr+off))...)
		body = append(body, 0x41) // i32.const (word)
		body = append(body, sleb(int32(binary.LittleEndian.Uint32(win[off:])))...)
		body = append(body, 0x36, 0x02, 0x00) // i32.store align=4 offset=0
	}
	return body
}

func outputEvent(t *testing.T, out Output, v Verdict) []byte {
	t.Helper()
	var win [1024]byte
	n, err := WriteOutput(win[:], out)
	if err != nil {
		t.Fatal(err)
	}
	body := append([]byte{0x00}, outputStores(win[:n])...)
	return append(body, 0x42, byte(v), 0x0b) // return verdict
}

const headerCaps = CapBlob | CapHeaderIndex | CapStartLine

func TestEngineRejectsUndeclaredOutput(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps Caps
		plan Plan
		out  Output
	}{
		{"CapMutation", headerCaps | CapStateIn | CapStateOut, InspectRequestHeaders | InspectRequestBody, Output{MutationPresent: true}},
		{"CapStateOut", headerCaps | CapMutation, InspectRequestHeaders, Output{State: []byte("state")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			e, err := New(ctx, outputModule(outputEvent(t, tc.out, VerdictAllow), tc.caps), Options{Plan: tc.plan, InCap: 4096, OutCap: 1024})
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close(ctx)
			result, err := e.OnEventResult(ctx, 1, PhaseRequestHeaders, FlagFirst|FlagLast, testEngineEvent())
			if err == nil || !strings.Contains(err.Error(), tc.name) || result.Verdict != VerdictDeny || result.StatePresent || result.MutationPresent {
				t.Fatalf("result = (%+v, %v), want deny/%s", result, err, tc.name)
			}
		})
	}
}

func TestEngineOwnsOutputAfterReturningInstance(t *testing.T) {
	ctx := context.Background()
	const caps = headerCaps | CapMutation | CapStateOut
	body := outputEvent(t, Output{Mutation: []byte("new"), State: []byte("state")}, VerdictAllow)
	// Grow once before writing OUT to exercise reacquiring the memory view.
	body = append([]byte{0x00, 0x41, 0x01, 0x40, 0x00, 0x1a}, body[1:]...)
	e, err := New(ctx, outputModule(body, caps), Options{Plan: InspectRequestHeaders, InCap: 4096, OutCap: 1024})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close(ctx)
	inst := <-e.pool
	e.pool <- inst
	result, err := e.OnEventResult(ctx, 1, PhaseRequestHeaders, FlagFirst|FlagLast, testEngineEvent())
	if err != nil || result.Verdict != VerdictAllow || !result.MutationPresent || !result.StatePresent {
		t.Fatalf("result = (%+v, %v)", result, err)
	}
	// Simulate the next caller clearing this instance's OUT window.
	out, ok := inst.mem.Read(testOutPtr, e.outCap)
	if !ok {
		t.Fatal("fixture lost OUT window")
	}
	clear(out)
	if string(result.Mutation) != "new" || string(result.State) != "state" {
		t.Fatalf("result aliases guest memory: %+v", result)
	}
}

func TestEngineConcurrentOwnedOutput(t *testing.T) {
	ctx := context.Background()
	const caps = headerCaps | CapMutation | CapStateOut
	// The guest uses a different payload for each connection while the host
	// keeps earlier results alive across many instance reuse cycles.
	var win [128]byte
	n, err := WriteOutput(win[:], Output{State: []byte{0}, Mutation: []byte{0}})
	if err != nil {
		t.Fatal(err)
	}
	body := append([]byte{0}, outputStores(win[:n])...)
	for _, i := range []int{0, 1} {
		off := binary.LittleEndian.Uint32(win[hdrLen+i*secStride+4:])
		body = append(body, 0x41)
		body = append(body, sleb(testOutPtr+int32(off))...)
		body = append(body, 0x20, 0x00, 0xa7, 0x3a, 0x00, 0x00) // i32.store8 of local.get connID
	}
	wasm := outputModule(append(body, 0x42, 0x00, 0x0b), caps)
	e, err := New(ctx, wasm, Options{Plan: InspectRequestHeaders | InspectRequestBody})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close(ctx)
	const events = 64
	var results [events]EventResult
	var wg sync.WaitGroup
	for i := range events {
		wg.Go(func() {
			var err error
			results[i], err = e.OnRequestBody(ctx, uint64(i+1), FlagFirst|FlagLast, Event{Block: []byte{1}})
			if err != nil {
				t.Errorf("event %d: %v", i, err)
			}
		})
	}
	wg.Wait()
	for i, r := range results {
		want := string([]byte{byte(i + 1)})
		if string(r.State) != want || string(r.Mutation) != want {
			t.Fatalf("event %d lost host-owned bytes: %+v", i, r)
		}
	}
}

func TestEngineNonAllowDoesNotReturnOutput(t *testing.T) {
	for _, v := range []Verdict{VerdictDeny, 2} {
		ctx := context.Background()
		e, err := New(ctx, outputModule(outputEvent(t, Output{StatePresent: true, Mutation: []byte("new")}, v), headerCaps|CapMutation|CapStateOut), Options{
			Plan: InspectRequestHeaders, RoutePools: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = e.Close(ctx) })
		result, err := e.OnEventResult(ctx, 1, PhaseRequestHeaders, FlagFirst|FlagLast, testEngineEvent())
		if err != nil || result.Verdict != v || result.StatePresent || result.MutationPresent {
			t.Fatalf("result = (%+v, %v), want verdict %d without output", result, err, v)
		}
	}
}
