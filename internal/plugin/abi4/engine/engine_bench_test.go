package engine

import (
	"context"
	"testing"
)

func benchmarkEvent() Event {
	block := []byte("GET /v1/items?q=1 HTTP/1.1\r\nHost: example.test\r\nUser-Agent: bench\r\nAccept: */*\r\nX-Test: one\r\n\r\n")
	idx := make([]Ent, 16)
	n, start, err := SplitHeaders(block, idx)
	if err != nil {
		panic(err)
	}
	return Event{Block: block, Index: idx[:n], Start: &start}
}

func BenchmarkSplitHeaders(b *testing.B) {
	ev := benchmarkEvent()
	idx := make([]Ent, 128)
	b.ReportAllocs()
	b.SetBytes(int64(len(ev.Block)))
	for i := 0; i < b.N; i++ {
		if _, _, err := SplitHeaders(ev.Block, idx); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWriteEnvelope(b *testing.B) {
	ev := benchmarkEvent()
	dst := make([]byte, DefaultInCap)
	b.ReportAllocs()
	b.SetBytes(int64(len(ev.Block)))
	for i := 0; i < b.N; i++ {
		if _, err := Write(dst, ev); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkOnRequestHeaders(b *testing.B) {
	ctx := context.Background()
	e, err := New(ctx, allowModule(), Options{Plan: InspectRequestHeaders, InCap: 4096, OutCap: 1024})
	if err != nil {
		b.Fatal(err)
	}
	defer e.Close(ctx)
	ev := benchmarkEvent()
	b.ReportAllocs()
	b.SetBytes(int64(len(ev.Block)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if v, err := e.OnRequestHeaders(ctx, uint64(i), ev.Block); err != nil || v != VerdictAllow {
			b.Fatalf("event = (%v, %v)", v, err)
		}
	}
}
