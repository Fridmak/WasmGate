package plugin

import (
	"context"
	"testing"
	"time"
)

// wasmOnHeadersDenyX exports:
//   - memory (1 page)
//   - on_data_ptr() -> 0x8000
//   - on_headers(conn_id: i64, ptr: i32, len: i32) -> i32
//
// It returns 1 when the first header byte is 'X', otherwise 0. This is enough to
// exercise hook dispatch, scratch bounds and the verdict path without a Rust
// toolchain.
var wasmOnHeadersDenyX = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	// Type section: (i64, i32, i32) -> i32 and () -> i32.
	0x01, 0x0c, 0x02,
	0x60, 0x03, 0x7e, 0x7f, 0x7f, 0x01, 0x7f,
	0x60, 0x00, 0x01, 0x7f,
	// Function section: func 0 -> type 0, func 1 -> type 1.
	0x03, 0x03, 0x02, 0x00, 0x01,
	// Memory section: min 1 page.
	0x05, 0x03, 0x01, 0x00, 0x01,
	// Export section: memory, on_headers, on_data_ptr.
	0x07, 0x25, 0x03,
	0x06, 'm', 'e', 'm', 'o', 'r', 'y', 0x02, 0x00,
	0x0a, 'o', 'n', '_', 'h', 'e', 'a', 'd', 'e', 'r', 's', 0x00, 0x00,
	0x0b, 'o', 'n', '_', 'd', 'a', 't', 'a', '_', 'p', 't', 'r', 0x00, 0x01,
	// Code section:
	//   on_headers: local.get 1; i32.load8_u; i32.const 'X'; i32.eq; end
	//   on_data_ptr: i32.const 0x8000; end
	0x0a, 0x14, 0x02,
	0x0b, 0x00, 0x20, 0x01, 0x2d, 0x00, 0x00, 0x41, 0xd8, 0x00, 0x46, 0x0b,
	0x06, 0x00, 0x41, 0x80, 0x80, 0x02, 0x0b,
}

// wasmOnHeadersMutate writes 'Z' at the start of the header block and allows it.
var wasmOnHeadersMutate = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	0x01, 0x0c, 0x02,
	0x60, 0x03, 0x7e, 0x7f, 0x7f, 0x01, 0x7f,
	0x60, 0x00, 0x01, 0x7f,
	0x03, 0x03, 0x02, 0x00, 0x01,
	0x05, 0x03, 0x01, 0x00, 0x01,
	0x07, 0x25, 0x03,
	0x06, 'm', 'e', 'm', 'o', 'r', 'y', 0x02, 0x00,
	0x0a, 'o', 'n', '_', 'h', 'e', 'a', 'd', 'e', 'r', 's', 0x00, 0x00,
	0x0b, 'o', 'n', '_', 'd', 'a', 't', 'a', '_', 'p', 't', 'r', 0x00, 0x01,
	0x0a, 0x15, 0x02,
	0x0c, 0x00, 0x20, 0x01, 0x41, 0xda, 0x00, 0x3a, 0x00, 0x00, 0x41, 0x00, 0x0b,
	0x06, 0x00, 0x41, 0x80, 0x80, 0x02, 0x0b,
}

func TestPluginOnHeadersAllowsAndDenies(t *testing.T) {
	ctx := context.Background()
	p, err := New(ctx, wasmOnHeadersDenyX, Options{Timeout: time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close(ctx)

	if !p.HasHeaders() || !p.CanInspectHeaders() {
		t.Fatal("on_headers was not detected")
	}
	if v, err := p.OnHeaders(ctx, 7, []byte("GET / HTTP/1.1\r\n\r\n")); err != nil || v != VerdictAllow {
		t.Fatalf("allow: got (%v, %v), want allow", v, err)
	}
	if v, err := p.OnHeaders(ctx, 7, []byte("XET / HTTP/1.1\r\n\r\n")); err != nil || v != VerdictDeny {
		t.Fatalf("deny: got (%v, %v), want deny", v, err)
	}
}

func TestPluginOnHeadersMutatesInPlace(t *testing.T) {
	ctx := context.Background()
	p, err := New(ctx, wasmOnHeadersMutate, Options{Timeout: time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close(ctx)

	headers := []byte("GET / HTTP/1.1\r\n\r\n")
	if v, err := p.OnHeaders(ctx, 7, headers); err != nil || v != VerdictAllow {
		t.Fatalf("on_headers: got (%v, %v), want allow", v, err)
	}
	if headers[0] != 'Z' {
		t.Fatalf("mutation not copied back: %q", headers)
	}
}

func TestPluginOnHeadersRequiresDataPtr(t *testing.T) {
	// Minimal on_headers module with memory but no on_data_ptr. It must fail at
	// load time instead of using the fixed legacy scratch offset.
	noPtr := []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
		0x01, 0x08, 0x01, 0x60, 0x03, 0x7e, 0x7f, 0x7f, 0x01, 0x7f,
		0x03, 0x02, 0x01, 0x00,
		0x05, 0x03, 0x01, 0x00, 0x01,
		0x07, 0x17, 0x02,
		0x06, 'm', 'e', 'm', 'o', 'r', 'y', 0x02, 0x00,
		0x0a, 'o', 'n', '_', 'h', 'e', 'a', 'd', 'e', 'r', 's', 0x00, 0x00,
		0x0a, 0x06, 0x01, 0x04, 0x00, 0x41, 0x00, 0x0b,
	}
	if _, err := New(context.Background(), noPtr, Options{Timeout: time.Second}); err == nil {
		t.Fatal("module with on_headers but no on_data_ptr was accepted")
	}
}

func TestPluginOnHeadersFallsBackToOnData(t *testing.T) {
	// http_guard exports on_data_ex but no on_headers; headers mode must use the
	// stateless fallback with dir=0 and the complete block.
	p := loadExample(t, "../../plugins/examples/http_guard/http_guard.wasm")
	ctx := context.Background()
	if v, err := p.OnHeaders(ctx, 1, []byte("GET / HTTP/1.1\r\nHost: example.test\r\nAuthorization: Basic dXNlcjpwYXNz\r\n\r\n")); err != nil || v != VerdictAllow {
		t.Fatalf("valid fallback = (%v, %v), want allow", v, err)
	}
	if v, err := p.OnHeaders(ctx, 1, []byte("TRACE / HTTP/1.1\r\nHost: example.test\r\nAuthorization: Basic dXNlcjpwYXNz\r\n\r\n")); err != nil || v != VerdictDeny {
		t.Fatalf("invalid fallback = (%v, %v), want deny", v, err)
	}
}

func BenchmarkOnHeaders(b *testing.B) {
	ctx := context.Background()
	p, err := New(ctx, wasmOnHeadersDenyX, Options{Timeout: time.Second})
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	defer p.Close(ctx)

	headers := []byte("GET /api/login HTTP/1.1\r\nHost: example.test\r\nAuthorization: Basic dXNlcjpwYXNz\r\n\r\n")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		v, err := p.OnHeaders(ctx, 1, headers)
		if err != nil || v != VerdictAllow {
			b.Fatalf("OnHeaders: %v, %v", v, err)
		}
	}
}
