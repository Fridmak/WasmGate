package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestEngineWritesEnvelope(t *testing.T) {
	ctx := context.Background()
	e, err := New(ctx, magicModule(), Options{Plan: InspectRequestHeaders, InCap: 4096, OutCap: 1024})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close(ctx)
	if v, err := e.OnEvent(ctx, 2, PhaseRequestHeaders, FlagFirst|FlagLast, testEngineEvent()); err != nil || v != VerdictAllow {
		t.Fatalf("envelope magic: (%v, %v)", v, err)
	}
}

func TestEngineRejectsInvalidModules(t *testing.T) {
	valid := allowModule()
	cases := []struct {
		name string
		wasm []byte
		opts Options
	}{
		{"bad abi", module([]byte{0x00, 0x42, 0x00, 0x0b}, 0x0500, 0x3f, 4096, 49152), Options{Plan: InspectRequestHeaders}},
		{"missing plan caps", module([]byte{0x00, 0x42, 0x00, 0x0b}, 0x0400, uint64(CapBlob), 4096, 49152), Options{Plan: InspectRequestBody | InspectRequestHeaders, InCap: 4096, OutCap: 1024}},
		{"small window", module([]byte{0x00, 0x42, 0x00, 0x0b}, 0x0400, 0x3f, 4096, 1024), Options{Plan: InspectRequestHeaders, InCap: 4096, OutCap: 1024}},
		{"no plan", valid, Options{InCap: 4096, OutCap: 1024}},
		{"empty wasm", nil, Options{Plan: InspectRequestHeaders}},
		{"not wasm", []byte("not a wasm module"), Options{Plan: InspectRequestHeaders}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(context.Background(), tc.wasm, tc.opts); err == nil {
				t.Fatal("invalid module accepted")
			}
		})
	}
}

func TestEngineTimeout(t *testing.T) {
	ctx := context.Background()
	e, err := New(ctx, infiniteModule(), Options{
		Plan: InspectRequestHeaders, Timeout: 20 * time.Millisecond, InCap: 4096, OutCap: 1024,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close(ctx)
	start := time.Now()
	if v, err := e.OnRequestHeaders(ctx, 1, testEngineEvent().Block); err == nil || v != VerdictDeny {
		t.Fatalf("timeout = (%v, %v), want deny/error", v, err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("timeout took too long: %s", time.Since(start))
	}
	if e.Discarded() == 0 {
		t.Fatal("timed-out instance was not discarded")
	}
}

func TestEnginePlanAndRouteValidation(t *testing.T) {
	ctx := context.Background()
	e, err := New(ctx, allowModule(), Options{Plan: InspectRequestHeaders, InCap: 4096, OutCap: 1024, RoutePools: 2})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close(ctx)
	if _, err := e.OnEvent(ctx, 1, PhaseResponseHeaders, 0, testEngineEvent()); err == nil {
		t.Fatal("phase outside the plan was accepted")
	}
	if err := ValidateVerdict(2, 2); err != nil {
		t.Fatalf("route 0: %v", err)
	}
}

func TestEngineRejectsMalformedHeaders(t *testing.T) {
	ctx := context.Background()
	e, err := New(ctx, allowModule(), Options{Plan: InspectRequestHeaders, InCap: 4096, OutCap: 1024})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close(ctx)

	bad := [][]byte{
		[]byte("GET /\r\n"),
		[]byte("GET / HTTP/1.1\r\nBadHeader\r\n\r\n"),
		[]byte("GET / HTTP/1.1\r\nX: a\x00b\r\n\r\n"),
	}
	for _, block := range bad {
		if v, err := e.OnRequestHeaders(ctx, 1, block); err == nil || v != VerdictDeny {
			t.Fatalf("malformed %q = (%v, %v), want deny/error", block, v, err)
		}
	}

	many := []byte("GET / HTTP/1.1\r\n")
	for i := 0; i < 513; i++ {
		many = append(many, []byte("X: y\r\n")...)
	}
	many = append(many, '\r', '\n')
	if v, err := e.OnRequestHeaders(ctx, 1, many); err == nil || v != VerdictDeny {
		t.Fatalf("overflowing header index = (%v, %v), want deny/error", v, err)
	}
}

func TestEngineRejectsUnknownFlags(t *testing.T) {
	ctx := context.Background()
	e, err := New(ctx, allowModule(), Options{Plan: InspectRequestHeaders, InCap: 4096, OutCap: 1024})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close(ctx)
	if v, err := e.OnEvent(ctx, 1, PhaseRequestHeaders, FlagFirst|1<<15, testEngineEvent()); err == nil || v != VerdictDeny {
		t.Fatalf("unknown flags = (%v, %v), want deny/error", v, err)
	}
}

func TestEngineConcurrent(t *testing.T) {
	ctx := context.Background()
	e, err := New(ctx, allowModule(), Options{Plan: InspectRequestHeaders, InCap: 4096, OutCap: 1024})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close(ctx)

	block := []byte("GET / HTTP/1.1\r\nHost: example.test\r\nX-Test: one\r\n\r\n")
	const (
		workers = 8
		iters   = 200
	)
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id uint64) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				v, err := e.OnRequestHeaders(ctx, id<<32|uint64(i), block)
				if err != nil || v != VerdictAllow {
					errs <- err
					return
				}
			}
		}(uint64(w))
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent event: %v", err)
		} else {
			t.Fatal("concurrent event returned deny")
		}
	}
}

func TestEngineRetainRelease(t *testing.T) {
	ctx := context.Background()
	e, err := New(ctx, allowModule(), Options{Plan: InspectRequestHeaders, InCap: 4096, OutCap: 1024})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	e.Retain()
	e.Release()
	if v, err := e.OnRequestHeaders(ctx, 1, testEngineEvent().Block); err != nil || v != VerdictAllow {
		t.Fatalf("shared engine before final release: (%v, %v)", v, err)
	}
	e.Release() // last reference: closes the runtime
	if _, err := e.OnRequestHeaders(ctx, 2, testEngineEvent().Block); err == nil {
		t.Fatal("event succeeded after the last reference was released")
	}
}

func TestEngineMaxInstances(t *testing.T) {
	ctx := context.Background()
	e, err := New(ctx, infiniteModule(), Options{
		Plan: InspectRequestHeaders, InCap: 4096, OutCap: 1024,
		Timeout: 500 * time.Millisecond, MaxInstances: 1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close(ctx)

	first := make(chan error, 1)
	go func() {
		_, err := e.OnRequestHeaders(context.Background(), 1, testEngineEvent().Block)
		first <- err
	}()

	deadline := time.Now().Add(2 * time.Second)
	for len(e.sem) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("first event never occupied the instance slot")
		}
		time.Sleep(time.Millisecond)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	_, err = e.OnRequestHeaders(waitCtx, 2, testEngineEvent().Block)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second event = %v, want context deadline", err)
	}
	if err := <-first; err == nil {
		t.Fatal("blocking event unexpectedly succeeded")
	}

	deadline = time.Now().Add(2 * time.Second)
	for len(e.sem) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("instance slot was not released")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestEngineCloseIdempotent(t *testing.T) {
	ctx := context.Background()
	e, err := New(ctx, allowModule(), Options{Plan: InspectRequestHeaders, InCap: 4096, OutCap: 1024})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := e.Close(ctx); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := e.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestEngineOpaquePlan(t *testing.T) {
	ctx := context.Background()
	e, err := New(ctx, module([]byte{0x00, 0x42, 0x00, 0x0b}, 0x0400, uint64(CapBlob|CapOpaque), 4096, 49152),
		Options{Plan: InspectOpaqueStream, InCap: 4096, OutCap: 1024})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close(ctx)
	if v, err := e.OnOpaqueFirst(ctx, 1, []byte("first")); err != nil || v != VerdictAllow {
		t.Fatalf("opaque first = (%v, %v)", v, err)
	}
	if v, err := e.OnStream(ctx, 1, []byte("more")); err != nil || v != VerdictAllow {
		t.Fatalf("opaque stream = (%v, %v)", v, err)
	}
	if v, err := e.OnOpaqueChunk(ctx, 1, 1, FlagLast, []byte("response")); err != nil || v != VerdictAllow {
		t.Fatalf("opaque response direction = (%v, %v)", v, err)
	}
	if v, err := e.OnOpaqueChunk(ctx, 1, 2, 0, []byte("bad")); err == nil || v != VerdictDeny {
		t.Fatalf("invalid opaque direction = (%v, %v), want deny/error", v, err)
	}
	if v, err := e.OnEvent(ctx, 1, PhaseOpaqueStream,
		FlagClientToServer|FlagServerToClient, Event{Block: []byte("both")}); err == nil || v != VerdictDeny {
		t.Fatalf("both directions = (%v, %v), want deny/error", v, err)
	}
	if _, err := e.OnEvent(ctx, 1, PhaseRequestHeaders, 0, testEngineEvent()); err == nil {
		t.Fatal("HTTP phase accepted on an opaque-only plan")
	}
	if _, err := e.OnEvent(ctx, 1, PhaseOpaqueStream, 0, Event{}); err == nil {
		t.Fatal("empty opaque event accepted")
	}
}

func TestEngineHeaderIndexCap(t *testing.T) {
	ctx := context.Background()
	e, err := New(ctx, allowModule(), Options{
		Plan: InspectRequestHeaders, InCap: 32 * 1024, OutCap: 1024,
		HeaderIndexCap: 512,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close(ctx)

	block := manyHeaders(512)
	if v, err := e.OnRequestHeaders(ctx, 1, block); err != nil || v != VerdictAllow {
		t.Fatalf("512 headers = (%v, %v)", v, err)
	}
	block = manyHeaders(513)
	if v, err := e.OnRequestHeaders(ctx, 2, block); !errors.Is(err, errTooManyHeaders) || v != VerdictDeny {
		t.Fatalf("513 headers = (%v, %v), want deny/%v", v, err, errTooManyHeaders)
	}
}

func TestEngineBodyAndResponseDispatch(t *testing.T) {
	ctx := context.Background()
	e, err := New(ctx, outputModule([]byte{0x00, 0x42, 0x00, 0x0b}, CapBlob|CapHeaderIndex|CapStartLine|CapStateIn|CapStateOut), Options{
		Plan:  InspectRequestHeaders | InspectRequestBody | InspectResponseHeaders | InspectResponseBody,
		InCap: 4096, OutCap: 1024,
	})
	if err != nil {
		t.Fatalf("New body engine: %v", err)
	}
	defer e.Close(ctx)
	slot, err := NewStateSlot(64)
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range []func() (EventResult, error){
		func() (EventResult, error) {
			return e.DispatchWithState(ctx, 1, PhaseRequestBody, FlagFirst, Event{Block: []byte("request")}, slot)
		},
		func() (EventResult, error) {
			return e.OnResponseHeaders(ctx, 1, FlagFirst|FlagLast, Event{Block: []byte("HTTP/1.1 200 OK\r\n\r\n")})
		},
		func() (EventResult, error) {
			return e.OnResponseBody(ctx, 1, FlagLast, Event{Block: []byte("response")})
		},
	} {
		result, err := call()
		if err != nil || result.Verdict != VerdictAllow {
			t.Fatalf("phase result = (%+v, %v)", result, err)
		}
	}
}

func TestEngineMutationCapabilityRequiresOutputWindow(t *testing.T) {
	// The fixture declares CapMutation but does not export output-window
	// accessors; loading must fail before any event can run.
	if _, err := New(context.Background(), module([]byte{0x00, 0x42, 0x00, 0x0b}, 0x0400, 0xb7, 4096, 49152), Options{
		Plan: InspectRequestHeaders, InCap: 4096, OutCap: 1024,
	}); err == nil {
		t.Fatal("mutation-capable module without output window was accepted")
	}
}

func manyHeaders(n int) []byte {
	block := []byte("GET / HTTP/1.1\r\n")
	for i := 0; i < n; i++ {
		block = append(block, []byte("X: y\r\n")...)
	}
	return append(block, '\r', '\n')
}

func TestEngineOpaqueDirectionReachesGuest(t *testing.T) {
	// on_event returns 1 when FlagClientToServer is set, 0 otherwise. This
	// proves the direction bit is actually part of the envelope call.
	body := []byte{
		0x00,       // no locals
		0x20, 0x02, // local.get 2 (flags)
		0x41, 0x08, // i32.const FlagClientToServer (1 << 3)
		0x71, // i32.and
		0x45, // i32.eqz
		0x45, // i32.eqz (normalize non-zero to 1)
		0xad, // i64.extend_i32_u
		0x0b, // end
	}
	ctx := context.Background()
	e, err := New(ctx, module(body, 0x0400, uint64(CapBlob|CapOpaque), 4096, 49152), Options{
		Plan: InspectOpaqueStream, InCap: 4096, OutCap: 1024,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close(ctx)
	if v, err := e.OnOpaqueChunk(ctx, 1, 0, 0, []byte("request")); err != nil || v != VerdictDeny {
		t.Fatalf("client direction = (%v, %v), want deny", v, err)
	}
	if v, err := e.OnOpaqueChunk(ctx, 1, 1, 0, []byte("response")); err != nil || v != VerdictAllow {
		t.Fatalf("server direction = (%v, %v), want allow", v, err)
	}
}
