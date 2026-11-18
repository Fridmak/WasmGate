package engine

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestSplitResponseHeadersRoundTrip(t *testing.T) {
	block := []byte("HTTP/1.1 200 OK\r\nContent-Length: 5\r\nX-Test: value\r\n\r\n")
	idx := make([]Ent, 4)
	n, start, err := SplitResponseHeaders(block, idx)
	if err != nil {
		t.Fatalf("SplitResponseHeaders: %v", err)
	}
	if n != 2 || start.PathLen != 3 || start.ProtoLen != 8 {
		t.Fatalf("headers = %d, start = %+v", n, start)
	}
	if _, _, err := SplitResponseHeaders([]byte("not HTTP\r\n\r\n"), idx); err == nil {
		t.Fatal("malformed response headers were accepted")
	}
}

func TestEngineMetadataAndInvalidFile(t *testing.T) {
	e, err := New(context.Background(), allowModule(), Options{Plan: InspectRequestHeaders, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if e.Plan() != InspectRequestHeaders || e.ABI() == 0 || e.Caps() == 0 {
		t.Fatalf("metadata = abi=%d caps=%x plan=%x", e.ABI(), e.Caps(), e.Plan())
	}
	if verdict, err := e.OnRequestHeaders(context.Background(), 1, []byte("GET / HTTP/1.1\r\nHost: test\r\n\r\n")); err != nil || verdict != VerdictAllow {
		t.Fatalf("OnRequestHeaders = (%v, %v)", verdict, err)
	}
	e.Retain()
	e.Release()
	e.Release()
	if _, err := LoadFile(context.Background(), t.TempDir()+"/missing.wasm", Options{Plan: InspectRequestHeaders}); err == nil {
		t.Fatal("LoadFile accepted a missing module")
	}
	path := t.TempDir() + "/allow.wasm"
	if err := os.WriteFile(path, allowModule(), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadFile(context.Background(), path, Options{Plan: InspectRequestHeaders})
	if err != nil {
		t.Fatal(err)
	}
	loaded.Release()
	lifecycle, err := New(context.Background(), lifecycleModule(0, CapLifecycle), Options{Plan: InspectLifecycle, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.OnAccept(context.Background(), nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.OnCloseConn(context.Background(), 1, nil, 0, 0); err != nil {
		t.Fatal(err)
	}
	lifecycle.Release()
}
