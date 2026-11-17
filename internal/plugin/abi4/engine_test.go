package abi4

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// The test modules are assembled by hand so the core tests do not need Rust or
// generated adapter artifacts. They export the mandatory v4 surface and one
// on_event body selected by the test.

func uleb(n uint32) []byte {
	var out []byte
	for {
		b := byte(n & 0x7f)
		n >>= 7
		if n != 0 {
			b |= 0x80
		}
		out = append(out, b)
		if n == 0 {
			return out
		}
	}
}

func section(id byte, payload []byte) []byte {
	out := []byte{id}
	out = append(out, uleb(uint32(len(payload)))...)
	return append(out, payload...)
}

func entry(body []byte) []byte {
	out := uleb(uint32(len(body)))
	return append(out, body...)
}

func module(onEvent []byte, abi uint32, caps uint64, ptr, length uint32) []byte {
	types := []byte{
		0x03,
		0x60, 0x05, 0x7e, 0x7f, 0x7f, 0x7f, 0x7f, 0x01, 0x7e,
		0x60, 0x00, 0x01, 0x7f,
		0x60, 0x00, 0x01, 0x7e,
	}
	funcs := []byte{0x05, 0x00, 0x01, 0x02, 0x01, 0x01}
	memory := []byte{0x01, 0x00, 0x01}

	exports := []byte{0x06}
	addExport := func(name string, idx uint32) {
		exports = append(exports, uleb(uint32(len(name)))...)
		exports = append(exports, name...)
		exports = append(exports, 0x00)
		exports = append(exports, uleb(idx)...)
	}
	exports = append(exports, 6, 'm', 'e', 'm', 'o', 'r', 'y', 0x02, 0x00)
	addExport("on_event", 0)
	addExport("wg_abi", 1)
	addExport("wg_caps", 2)
	addExport("wg_buf_ptr", 3)
	addExport("wg_buf_len", 4)

	intConst := func(op byte, v uint32) []byte {
		body := []byte{0x00, op}
		body = append(body, uleb(v)...)
		return append(body, 0x0b)
	}
	i64Const := func(v uint64) []byte {
		body := []byte{0x00, 0x42}
		body = append(body, sleb(int32(v))...)
		return append(body, 0x0b)
	}

	code := []byte{0x05}
	code = append(code, entry(onEvent)...)
	code = append(code, entry(intConst(0x41, abi))...)
	code = append(code, entry(i64Const(caps))...)
	code = append(code, entry(intConst(0x41, ptr))...)
	code = append(code, entry(intConst(0x41, length))...)

	out := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	out = append(out, section(0x01, types)...)
	out = append(out, section(0x03, funcs)...)
	out = append(out, section(0x05, memory)...)
	out = append(out, section(0x07, exports)...)
	out = append(out, section(0x0a, code)...)
	return out
}

func sleb(n int32) []byte {
	var out []byte
	for {
		b := byte(n & 0x7f)
		n >>= 7
		if (n == 0 && b&0x40 == 0) || (n == -1 && b&0x40 != 0) {
			return append(out, b)
		}
		out = append(out, b|0x80)
	}
}

// outputModule writes an empty valid OUT envelope on every event. It is a
// hand-built conformance guest for M4 state/body dispatch tests.
func outputModule() []byte {
	types := []byte{
		0x03,
		0x60, 0x05, 0x7e, 0x7f, 0x7f, 0x7f, 0x7f, 0x01, 0x7e,
		0x60, 0x00, 0x01, 0x7f,
		0x60, 0x00, 0x01, 0x7e,
	}
	funcs := []byte{0x07, 0x00, 0x01, 0x02, 0x01, 0x01, 0x01, 0x01}
	memory := []byte{0x01, 0x00, 0x01}
	exports := []byte{0x08}
	addExport := func(name string, idx uint32) {
		exports = append(exports, uleb(uint32(len(name)))...)
		exports = append(exports, name...)
		exports = append(exports, 0x00)
		exports = append(exports, uleb(idx)...)
	}
	exports = append(exports, 6, 'm', 'e', 'm', 'o', 'r', 'y', 0x02, 0x00)
	addExport("on_event", 0)
	addExport("wg_abi", 1)
	addExport("wg_caps", 2)
	addExport("wg_buf_ptr", 3)
	addExport("wg_buf_len", 4)
	addExport("wg_out_ptr", 5)
	addExport("wg_out_len", 6)

	constI32 := func(v int32) []byte {
		b := []byte{0x00, 0x41}
		b = append(b, sleb(v)...)
		return append(b, 0x0b)
	}
	constI64 := func(v int64) []byte {
		b := []byte{0x00, 0x42}
		b = append(b, sleb(int32(v))...)
		return append(b, 0x0b)
	}
	// on_event returns allow. The output envelope itself is covered by
	// WriteOutput/ParseOutput conformance tests; a real SDK guest writes it.
	onEvent := []byte{0x00, 0x42, 0x00, 0x0b}

	code := []byte{0x07}
	code = append(code, entry(onEvent)...)
	code = append(code, entry(constI32(0x0400))...)
	code = append(code, entry(constI64(0x37))...)
	code = append(code, entry(constI32(4096))...)
	code = append(code, entry(constI32(49152))...)
	code = append(code, entry(constI32(50000))...)
	code = append(code, entry(constI32(4096))...)

	out := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	out = append(out, section(0x01, types)...)
	out = append(out, section(0x03, funcs)...)
	out = append(out, section(0x05, memory)...)
	out = append(out, section(0x07, exports)...)
	out = append(out, section(0x0a, code)...)
	return out
}

func lifecycleModule(accept byte) []byte {
	types := []byte{
		0x04,
		0x60, 0x03, 0x7e, 0x7e, 0x7f, 0x01, 0x7f, // on_accept
		0x60, 0x00, 0x01, 0x7f, // wg_abi
		0x60, 0x00, 0x01, 0x7e, // wg_caps
		0x60, 0x04, 0x7e, 0x7e, 0x7e, 0x7e, 0x00, // on_close
	}
	funcs := []byte{0x04, 0x00, 0x01, 0x02, 0x03}
	exports := []byte{0x04}
	addExport := func(name string, idx uint32) {
		exports = append(exports, uleb(uint32(len(name)))...)
		exports = append(exports, name...)
		exports = append(exports, 0x00)
		exports = append(exports, uleb(idx)...)
	}
	addExport("on_accept", 0)
	addExport("wg_abi", 1)
	addExport("wg_caps", 2)
	addExport("on_close", 3)

	consts := func(a, b []byte) []byte { return append(a, b...) }
	code := []byte{0x04}
	code = append(code, entry([]byte{0x00, 0x41, accept, 0x0b})...)
	code = append(code, entry(consts([]byte{0x00, 0x41}, append(uleb(0x0400), 0x0b)))...)
	code = append(code, entry([]byte{0x00, 0x42, 0x00, 0x0b})...)
	code = append(code, entry([]byte{0x00, 0x0b})...)

	out := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	out = append(out, section(0x01, types)...)
	out = append(out, section(0x03, funcs)...)
	out = append(out, section(0x07, exports)...)
	out = append(out, section(0x0a, code)...)
	return out
}
func allowModule() []byte {
	return module([]byte{0x00, 0x42, 0x00, 0x0b}, 0x0400, 0x3f, 4096, 49152)
}

func denyModule() []byte {
	return module([]byte{0x00, 0x42, 0x01, 0x0b}, 0x0400, 0x3f, 4096, 49152)
}

func negativeModule() []byte {
	return module([]byte{0x00, 0x42, 0x7f, 0x0b}, 0x0400, 0x3f, 4096, 49152)
}

func routeModule() []byte {
	return module([]byte{0x00, 0x42, 0x02, 0x0b}, 0x0400, 0x3f, 4096, 49152)
}

func magicModule() []byte {
	// if first byte of the envelope != 'W', return deny.
	body := []byte{0x00, 0x20, 0x03, 0x28, 0x02, 0x00, 0x41, 0xc5, 0xe8, 0x9c, 0xba, 0x05, 0x46, 0x45, 0xad, 0x0b}
	return module(body, 0x0400, 0x3f, 4096, 49152)
}

func infiniteModule() []byte {
	body := []byte{0x00, 0x03, 0x40, 0x0c, 0x00, 0x0b, 0x42, 0x00, 0x0b}
	return module(body, 0x0400, 0x3f, 4096, 49152)
}

func testEngineEvent() Event {
	block := []byte("GET / HTTP/1.1\r\nHost: example.test\r\n\r\n")
	idx := make([]Ent, 8)
	n, start, err := SplitHeaders(block, idx)
	if err != nil {
		panic(err)
	}
	return Event{Block: block, Index: idx[:n], Start: &start}
}

func TestEngineLoadAndVerdict(t *testing.T) {
	ctx := context.Background()
	e, err := New(ctx, allowModule(), Options{Plan: InspectRequestHeaders, InCap: 4096, OutCap: 1024})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close(ctx)
	if e.ABI()>>8 != 4 || !e.Caps().Has(CapBlob) {
		t.Fatalf("metadata = abi %x caps %b", e.ABI(), e.Caps())
	}
	v, err := e.OnRequestHeaders(ctx, 1, testEngineEvent().Block)
	if err != nil || v != VerdictAllow {
		t.Fatalf("allow: (%v, %v)", v, err)
	}

	deny, err := New(ctx, denyModule(), Options{Plan: InspectRequestHeaders, InCap: 4096, OutCap: 1024})
	if err != nil {
		t.Fatalf("deny New: %v", err)
	}
	defer deny.Close(ctx)
	if v, err := deny.OnRequestHeaders(ctx, 2, testEngineEvent().Block); err != nil || v != VerdictDeny {
		t.Fatalf("deny: (%v, %v)", v, err)
	}
}

func TestEngineRejectsNegativeVerdict(t *testing.T) {
	ctx := context.Background()
	e, err := New(ctx, negativeModule(), Options{Plan: InspectRequestHeaders, InCap: 4096, OutCap: 1024})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close(ctx)
	if v, err := e.OnRequestHeaders(ctx, 1, testEngineEvent().Block); err == nil || v != VerdictDeny {
		t.Fatalf("negative verdict = (%v, %v), want deny/error", v, err)
	}
	if e.Discarded() != 0 {
		t.Fatal("negative verdict discarded a healthy instance")
	}
}

func TestEngineRouteVerdict(t *testing.T) {
	ctx := context.Background()
	e, err := New(ctx, routeModule(), Options{Plan: InspectRequestHeaders, InCap: 4096, OutCap: 1024, RoutePools: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close(ctx)
	if v, err := e.OnRequestHeaders(ctx, 1, testEngineEvent().Block); err != nil || v != Verdict(2) {
		t.Fatalf("route verdict = (%v, %v), want (2, nil)", v, err)
	}
}

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
		{"missing plan caps", module([]byte{0x00, 0x42, 0x00, 0x0b}, 0x0400, 0x07, 4096, 49152), Options{Plan: InspectRequestBody | InspectRequestHeaders, InCap: 4096, OutCap: 1024}},
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
	e, err := New(ctx, module([]byte{0x00, 0x42, 0x00, 0x0b}, 0x0400, 0x7f, 4096, 49152),
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
	e, err := New(ctx, outputModule(), Options{
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
	e, err := New(ctx, module(body, 0x0400, 0x7f, 4096, 49152), Options{
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

func TestEncodeIP(t *testing.T) {
	hi, lo := encodeIP(net.IPv4(127, 0, 0, 99))
	if hi != 0 || lo != 0x0000ffff7f000063 {
		t.Fatalf("IPv4 encoding = (%#x, %#x)", hi, lo)
	}
	hi, lo = encodeIP(net.ParseIP("2001:db8::1"))
	if hi != 0x20010db800000000 || lo != 1 {
		t.Fatalf("IPv6 encoding = (%#x, %#x)", hi, lo)
	}
}

func TestEngineLifecycle(t *testing.T) {
	ctx := context.Background()
	e, err := New(ctx, lifecycleModule(0), Options{Plan: InspectLifecycle, Timeout: time.Second})
	if err != nil {
		t.Fatalf("New allow: %v", err)
	}
	defer e.Close(ctx)
	if v, err := e.OnAccept(ctx, net.IPv4(127, 0, 0, 1), 1234); err != nil || v != VerdictAllow {
		t.Fatalf("allow lifecycle = (%v, %v)", v, err)
	}
	if err := e.OnCloseConn(ctx, 1, net.IPv4(127, 0, 0, 1), 10, 20); err != nil {
		t.Fatalf("on_close: %v", err)
	}

	deny, err := New(ctx, lifecycleModule(1), Options{Plan: InspectLifecycle, Timeout: time.Second})
	if err != nil {
		t.Fatalf("New deny: %v", err)
	}
	defer deny.Close(ctx)
	if v, err := deny.OnAccept(ctx, net.IPv4(127, 0, 0, 99), 1234); err != nil || v != VerdictDeny {
		t.Fatalf("deny lifecycle = (%v, %v)", v, err)
	}
}

func TestEngineLifecycleRejectsEventModule(t *testing.T) {
	if _, err := New(context.Background(), allowModule(), Options{Plan: InspectLifecycle}); err == nil {
		t.Fatal("event module was accepted for a lifecycle-only plan")
	}
}

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
