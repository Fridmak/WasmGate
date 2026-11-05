package proxy

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"wasmgate/internal/config"
	"wasmgate/internal/observe"
)

// Tests for proxy.go: one connection handled end to end, followed by the
// benchmark for the per-connection CPU work.

func TestProxyForwardsBytes(t *testing.T) {
	srv := NewServer(testLogger(), NewRuntime(edgeConfig([]string{startEcho(t)}, nil)), nil)
	addr := startServer(t, srv)

	conn := dial(t, addr)
	defer conn.Close()

	const payload = "hello wasmgate"
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.(*net.TCPConn).CloseWrite() // half-close must be forwarded

	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != payload {
		t.Fatalf("got %q, want %q", buf, payload)
	}
}

// TestProxyClosesConnectionWhenUpstreamIsDown covers the dial-failure path: the
// client connection must be closed instead of hanging.
func TestProxyClosesConnectionWhenUpstreamIsDown(t *testing.T) {
	// Port 1 is not listening, so the dial fails fast with a connection refused.
	srv := NewServer(testLogger(), NewRuntime(edgeConfig([]string{"127.0.0.1:1"}, nil)), nil)
	addr := startServer(t, srv)

	conn := dial(t, addr)
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection was not closed after the upstream dial failed")
	}
}

// TestHandleUnknownListener covers the defensive lookup in Handle: a connection
// accepted by a listener that a reload has just removed (the old accept loop can
// still hand one over before it observes the closed socket) is closed instead of
// panicking.
func TestHandleUnknownListener(t *testing.T) {
	p := New(testLogger(), NewRuntime(edgeConfig([]string{"127.0.0.1:9000"}, nil)), nil)
	client, peer := net.Pipe()
	defer peer.Close()

	p.Handle(context.Background(), "gone", client)

	// Handle must always close the client side.
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := peer.Read(make([]byte, 1)); err == nil {
		t.Fatal("Handle left the connection open for an unknown listener")
	}
}

// TestProxyMetricsCountRejections covers the max_conns reject path: a refused
// connection must show up as accepted (the socket WAS accepted) and then closed,
// with the limit-rejected counter incremented.
func TestProxyMetricsCountRejections(t *testing.T) {
	m := observe.NewMetrics()
	cfg := validated(t, edgeConfig([]string{startEcho(t)}, func(lc *config.ListenerConfig) {
		lc.MaxConns = 1
	}))
	addr := startServer(t, NewServer(testLogger(), NewRuntime(cfg), m))

	held := roundTrip(t, addr) // occupies the single slot
	defer held.Close()

	over := dial(t, addr)
	defer over.Close()
	_ = over.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := over.Read(make([]byte, 1)); err == nil {
		t.Fatal("second connection was not rejected by max_conns")
	}

	waitMetric(t, 2*time.Second, func() bool {
		return m.Snapshot().ConnectionsLimitRejected >= 1
	})
}

// TestProxyMetricsCountDialFailures covers the upstream-dial failure path.
func TestProxyMetricsCountDialFailures(t *testing.T) {
	m := observe.NewMetrics()
	// Port 1 is not listening, so the dial fails fast with a connection refused.
	srv := NewServer(testLogger(), NewRuntime(edgeConfig([]string{"127.0.0.1:1"}, nil)), m)
	addr := startServer(t, srv)

	conn := dial(t, addr)
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection was not closed after the upstream dial failed")
	}

	waitMetric(t, 2*time.Second, func() bool {
		return m.Snapshot().UpstreamDialFailures >= 1
	})
}

// TestProxyRateLimitsConnections covers the per-IP sliding-window limiter on
// the accept path: connections within the window's max pass, the next one is
// closed before dialing the upstream and counted under rate rejections.
func TestProxyRateLimitsConnections(t *testing.T) {
	m := observe.NewMetrics()
	cfg := validated(t, edgeConfig([]string{startEcho(t)}, func(lc *config.ListenerConfig) {
		lc.RateLimit = config.RateLimit{Max: 2, Window: time.Minute}
	}))
	addr := startServer(t, NewServer(testLogger(), NewRuntime(cfg), m))

	for i := 0; i < 2; i++ {
		c := roundTrip(t, addr)
		_ = c.Close()
	}

	over := dial(t, addr)
	defer over.Close()
	_ = over.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := over.Read(make([]byte, 1)); err == nil {
		t.Fatal("third connection was not rejected by rate_limit")
	}

	waitMetric(t, 2*time.Second, func() bool {
		return m.Snapshot().ConnectionsRateRejected >= 1
	})
}

// BenchmarkConnHotPath measures everything Handle does per connection before it
// touches a socket: load the snapshot, resolve the listener, take a connection
// slot, pick an upstream, then release both. This is the CPU overhead the proxy
// adds on top of the raw relay, which the end-to-end benchmarks in copy_test.go
// measure as the direct-vs-proxied gap.
func BenchmarkConnHotPath(b *testing.B) {
	cfg := validated(b, edgeConfig([]string{"127.0.0.1:9000"}, func(lc *config.ListenerConfig) {
		lc.MaxConns = 10000
	}))
	rt := NewRuntime(cfg)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l := rt.Load().listeners["edge"]
		if !l.limiter.acquire() {
			b.Fatal("unexpected refusal below the limit")
		}
		up := l.pool.Next()
		l.pool.Release(up)
		l.limiter.release()
		sinkListener = l
	}
}

// BenchmarkConnHotPathParallel is the same work with every P sharing one
// Runtime: the snapshot pointer, the listener's counters and the pool cursor are
// read and mutated by all of them, so the cache lines are genuinely shared. The
// single-goroutine case above measures the instruction count of the path; this
// one measures what a burst of connections actually pays.
//
// MaxConns is 10000 here and RunParallel never has that many goroutines in
// flight, so acquire always succeeds and the result is not checked: b.Fatal is
// not callable from a RunParallel goroutine.
func BenchmarkConnHotPathParallel(b *testing.B) {
	cfg := validated(b, edgeConfig([]string{"127.0.0.1:9000"}, func(lc *config.ListenerConfig) {
		lc.MaxConns = 10000
	}))
	rt := NewRuntime(cfg)

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			l := rt.Load().listeners["edge"]
			l.limiter.acquire()
			up := l.pool.Next()
			l.pool.Release(up)
			l.limiter.release()
			sinkListener = l
		}
	})
}
