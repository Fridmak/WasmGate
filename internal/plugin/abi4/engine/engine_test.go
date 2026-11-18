package engine

import (
	"context"
	"testing"
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

	code := make([]byte, 1, len(onEvent)+64)
	code[0] = 5
	code = append(code, entry(onEvent)...)
	code = append(code, entry(intConst(0x41, abi))...)
	code = append(code, entry(i64Const(caps))...)
	code = append(code, entry(intConst(0x41, ptr))...)
	code = append(code, entry(intConst(0x41, length))...)

	out := make([]byte, 0, len(code)+len(exports)+64)
	out = append(out, 0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00)
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

const testOutPtr = 50000

// outputModule exports separate IN and OUT windows for conformance tests.
func outputModule(onEvent []byte, caps Caps) []byte {
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
	code := make([]byte, 1, len(onEvent)+64)
	code[0] = 7
	code = append(code, entry(onEvent)...)
	code = append(code, entry(constI32(0x0400))...)
	code = append(code, entry(constI64(int64(caps)))...)
	code = append(code, entry(constI32(4096))...)
	code = append(code, entry(constI32(49152))...)
	code = append(code, entry(constI32(testOutPtr))...)
	code = append(code, entry(constI32(4096))...)

	out := make([]byte, 0, len(code)+len(exports)+64)
	out = append(out, 0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00)
	out = append(out, section(0x01, types)...)
	out = append(out, section(0x03, funcs)...)
	out = append(out, section(0x05, memory)...)
	out = append(out, section(0x07, exports)...)
	out = append(out, section(0x0a, code)...)
	return out
}

func lifecycleModule(accept byte, caps Caps) []byte {
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
	code := make([]byte, 1, 32)
	code[0] = 4
	code = append(code, entry([]byte{0x00, 0x41, accept, 0x0b})...)
	code = append(code, entry(consts([]byte{0x00, 0x41}, append(uleb(0x0400), 0x0b)))...)
	code = append(code, entry(append(append([]byte{0x00, 0x42}, sleb(int32(caps))...), 0x0b))...)
	code = append(code, entry([]byte{0x00, 0x0b})...)

	out := make([]byte, 0, len(code)+len(exports)+64)
	out = append(out, 0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00)
	out = append(out, section(0x01, types)...)
	out = append(out, section(0x03, funcs)...)
	out = append(out, section(0x07, exports)...)
	out = append(out, section(0x0a, code)...)
	return out
}
func allowModule() []byte {
	return module([]byte{0x00, 0x42, 0x00, 0x0b}, 0x0400, uint64(headerCaps), 4096, 49152)
}

func denyModule() []byte {
	return module([]byte{0x00, 0x42, 0x01, 0x0b}, 0x0400, uint64(headerCaps), 4096, 49152)
}

func negativeModule() []byte {
	return module([]byte{0x00, 0x42, 0x7f, 0x0b}, 0x0400, uint64(headerCaps), 4096, 49152)
}

func routeModule() []byte {
	return module([]byte{0x00, 0x42, 0x02, 0x0b}, 0x0400, uint64(headerCaps), 4096, 49152)
}

func magicModule() []byte {
	// if first byte of the envelope != 'W', return deny.
	body := []byte{0x00, 0x20, 0x03, 0x28, 0x02, 0x00, 0x41, 0xc5, 0xe8, 0x9c, 0xba, 0x05, 0x46, 0x45, 0xad, 0x0b}
	return module(body, 0x0400, uint64(headerCaps), 4096, 49152)
}

func infiniteModule() []byte {
	body := []byte{0x00, 0x03, 0x40, 0x0c, 0x00, 0x0b, 0x42, 0x00, 0x0b}
	return module(body, 0x0400, uint64(headerCaps), 4096, 49152)
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
