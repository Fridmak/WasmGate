package plugin

import (
	"context"
	"net"
	"testing"
	"time"
)

// Minimal hand-assembled WebAssembly binaries used as self-contained test
// fixtures (no external Rust/TinyGo toolchain required to run `go test`).
var (
	// wasmAllowAndClose exports:
	//   - on_accept(i64, i64, i32) -> i32  { return 0 }
	//   - on_close(i64, i64, i64, i64)     { nop }
	wasmAllowAndClose = []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
		// Type section: 2 function types (1 + 7 + 7 = 15 bytes = 0x0f)
		0x01, 0x0f, 0x02,
		0x60, 0x03, 0x7e, 0x7e, 0x7f, 0x01, 0x7f, // (i64, i64, i32) -> i32
		0x60, 0x04, 0x7e, 0x7e, 0x7e, 0x7e, 0x00, // (i64, i64, i64, i64) -> ()
		// Function section: func 0 -> type 0, func 1 -> type 1
		0x03, 0x03, 0x02, 0x00, 0x01,
		// Export section: "on_accept" -> func 0, "on_close" -> func 1
		0x07, 0x18, 0x02,
		0x09, 'o', 'n', '_', 'a', 'c', 'c', 'e', 'p', 't', 0x00, 0x00,
		0x08, 'o', 'n', '_', 'c', 'l', 'o', 's', 'e', 0x00, 0x01,
		// Code section:
		//   func 0: i32.const 0; end
		//   func 1: end
		0x0a, 0x09, 0x02,
		0x04, 0x00, 0x41, 0x00, 0x0b,
		0x02, 0x00, 0x0b,
	}

	// wasmDenyHost99 exports on_accept(ip_hi, ip_lo, port) -> i32:
	// returns 1 (VerdictDeny) when the low byte of ip_lo equals 99 (e.g. 192.0.2.99),
	// and 0 (VerdictAllow) otherwise:
	//   local.get 1; i64.const 255; i64.and; i64.const 99; i64.eq; end
	wasmDenyHost99 = []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
		0x01, 0x08, 0x01, 0x60, 0x03, 0x7e, 0x7e, 0x7f, 0x01, 0x7f,
		0x03, 0x02, 0x01, 0x00,
		0x07, 0x0d, 0x01, 0x09, 'o', 'n', '_', 'a', 'c', 'c', 'e', 'p', 't', 0x00, 0x00,
		0x0a, 0x0e, 0x01,
		0x0c, 0x00,
		0x20, 0x01, // local.get 1 (ip_lo)
		0x42, 0xff, 0x01, // i64.const 255
		0x83,             // i64.and
		0x42, 0xe3, 0x00, // i64.const 99
		0x51, // i64.eq (produces i32 1 or 0)
		0x0b, // end
	}

	// wasmInfiniteLoop exports on_accept that spins forever: (loop (br 0)).
	wasmInfiniteLoop = []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
		0x01, 0x08, 0x01, 0x60, 0x03, 0x7e, 0x7e, 0x7f, 0x01, 0x7f,
		0x03, 0x02, 0x01, 0x00,
		0x07, 0x0d, 0x01, 0x09, 'o', 'n', '_', 'a', 'c', 'c', 'e', 'p', 't', 0x00, 0x00,
		0x0a, 0x0b, 0x01,
		0x09, 0x00,
		0x03, 0x40, // loop (void)
		0x0c, 0x00, // br 0
		0x0b,       // end loop
		0x41, 0x00, // i32.const 0
		0x0b, // end func
	}

	// wasm32Pages declares a linear memory with min=32 pages (2 MiB).
	wasm32Pages = []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
		0x01, 0x08, 0x01, 0x60, 0x03, 0x7e, 0x7e, 0x7f, 0x01, 0x7f,
		0x03, 0x02, 0x01, 0x00,
		0x05, 0x03, 0x01, 0x00, 0x20, // memory section: min = 32 pages
		0x07, 0x0d, 0x01, 0x09, 'o', 'n', '_', 'a', 'c', 'c', 'e', 'p', 't', 0x00, 0x00,
		0x0a, 0x06, 0x01, 0x04, 0x00, 0x41, 0x00, 0x0b,
	}
)

func TestPluginAllowAndDenyByIP(t *testing.T) {
	ctx := context.Background()
	p, err := New(ctx, wasmDenyHost99, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close(ctx)

	v, err := p.OnAccept(ctx, net.ParseIP("192.0.2.10"), 12345)
	if err != nil || v != VerdictAllow {
		t.Fatalf("192.0.2.10: got (%v, %v), want VerdictAllow", v, err)
	}

	v, err = p.OnAccept(ctx, net.ParseIP("192.0.2.99"), 12345)
	if err != nil || v != VerdictDeny {
		t.Fatalf("192.0.2.99: got (%v, %v), want VerdictDeny", v, err)
	}
}

func TestPluginMemoryLimitRejectsOversizedModule(t *testing.T) {
	ctx := context.Background()
	_, err := New(ctx, wasm32Pages, Options{MaxMemoryPages: 4}) // 4 pages = 256 KiB < 32 pages
	if err == nil {
		t.Fatal("expected error for module exceeding MaxMemoryPages, got nil")
	}
}

func TestPluginTimeoutStopsInfiniteLoop(t *testing.T) {
	ctx := context.Background()
	p, err := New(ctx, wasmInfiniteLoop, Options{Timeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close(ctx)

	v, err := p.OnAccept(ctx, net.ParseIP("192.0.2.1"), 8080)
	if err == nil {
		t.Fatal("infinite loop returned nil error")
	}
	if v != VerdictDeny {
		t.Fatalf("verdict on timeout = %v, want VerdictDeny", v)
	}
}

func TestPluginRejectsBadSignature(t *testing.T) {
	// Export named "on_accept" with 0 params -> must be rejected at compile time.
	bad := []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
		0x01, 0x04, 0x01, 0x60, 0x00, 0x00,
		0x03, 0x02, 0x01, 0x00,
		0x07, 0x0d, 0x01, 0x09, 'o', 'n', '_', 'a', 'c', 'c', 'e', 'p', 't', 0x00, 0x00,
		0x0a, 0x04, 0x01, 0x02, 0x00, 0x0b,
	}
	if _, err := New(context.Background(), bad, Options{}); err == nil {
		t.Fatal("expected error for bad on_accept signature, got nil")
	}
}

// BenchmarkOnAccept measures the steady-state cost of calling a sandboxed WASM
// on_accept hook from a single goroutine. Goal: 0 allocs/op.
func BenchmarkOnAccept(b *testing.B) {
	ctx := context.Background()
	p, err := New(ctx, wasmAllowAndClose, Options{})
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	defer p.Close(ctx)

	ip := net.ParseIP("192.0.2.1")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		v, err := p.OnAccept(ctx, ip, 443)
		if err != nil || v != VerdictAllow {
			b.Fatalf("OnAccept: %v, %v", v, err)
		}
	}
}

// BenchmarkOnAcceptParallel measures the WASM sandbox when all Ps invoke
// on_accept concurrently through the instance pool. Goal: 0 allocs/op.
func BenchmarkOnAcceptParallel(b *testing.B) {
	ctx := context.Background()
	p, err := New(ctx, wasmAllowAndClose, Options{})
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	defer p.Close(ctx)

	ip := net.ParseIP("192.0.2.1")
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = p.OnAccept(ctx, ip, 443)
		}
	})
}
