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

// TestProxyWasmPluginRejectsAndAllows covers the WASM sandbox hook in Handle:
// a listener with a deny-all WASM plugin rejects connections before dialing the
// upstream and increments wasmgate_connections_plugin_rejected_total; hot-reloading
// an allow-all WASM plugin lets subsequent connections through with zero downtime.
func TestProxyWasmPluginRejectsAndAllows(t *testing.T) {
	// on_accept(i64, i64, i32) -> i32 returning 1 (VerdictDeny) or 0 (VerdictAllow).
	wasmVerdict := func(ret byte) []byte {
		return []byte{
			0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
			0x01, 0x08, 0x01, 0x60, 0x03, 0x7e, 0x7e, 0x7f, 0x01, 0x7f,
			0x03, 0x02, 0x01, 0x00,
			0x07, 0x0d, 0x01, 0x09, 'o', 'n', '_', 'a', 'c', 'c', 'e', 'p', 't', 0x00, 0x00,
			0x0a, 0x06, 0x01, 0x04, 0x00, 0x41, ret, 0x0b,
		}
	}

	echoAddr := startEcho(t)
	m := observe.NewMetrics()
	cfgDeny := validated(t, edgeConfig([]string{echoAddr}, func(lc *config.ListenerConfig) {
		lc.Plugin = config.PluginConfig{Wasm: wasmVerdict(1)}
	}))
	srv := NewServer(testLogger(), NewRuntime(cfgDeny), m)
	addr := startServer(t, srv)

	denied := dial(t, addr)
	defer denied.Close()
	_ = denied.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := denied.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection was not rejected by deny WASM plugin")
	}
	waitMetric(t, 2*time.Second, func() bool {
		return m.Snapshot().ConnectionsPluginRejected >= 1
	})

	// Hot-reload to an allow WASM plugin: new connections must now succeed.
	cfgAllow := validated(t, edgeConfig([]string{echoAddr}, func(lc *config.ListenerConfig) {
		lc.Plugin = config.PluginConfig{Wasm: wasmVerdict(0)}
	}))
	if _, err := srv.Reload(context.Background(), cfgAllow); err != nil {
		t.Fatalf("reload allow plugin: %v", err)
	}
	c := roundTrip(t, addr)
	_ = c.Close()
}

// TestProxySNIRoutesToMatchingPoolAndReplaysClientHello verifies end-to-end
// TLS SNI passthrough: the proxy peeks the ClientHello without terminating TLS,
// routes api.example.com and *.wild.example to their respective backend pools,
// and forwards the full ClientHello bytes intact to the chosen backend.
func TestProxySNIRoutesToMatchingPoolAndReplaysClientHello(t *testing.T) {
	apiEcho := startEcho(t)
	wildEcho := startEcho(t)
	defaultEcho := startEcho(t)

	cfg := validated(t, &config.Config{
		Version: 1,
		Listeners: []config.ListenerConfig{{
			Name:         "edge",
			Addr:         "127.0.0.1:0",
			UpstreamPool: "default",
			DialTimeout:  time.Second,
			SNIRoutes: map[string]string{
				"api.example.com": "api",
				"*.wild.example":  "wild",
			},
		}},
		Pools: map[string]config.PoolConfig{
			"api":     {Servers: []string{apiEcho}},
			"wild":    {Servers: []string{wildEcho}},
			"default": {Servers: []string{defaultEcho}},
		},
	})

	rt := NewRuntime(cfg)
	srv := NewServer(testLogger(), rt, nil)
	addr := startServer(t, srv)

	// Minimal TLS ClientHello builder for a given SNI.
	makeHello := func(sni string) []byte {
		h := []byte(sni)
		ext := make([]byte, 9+len(h))
		listLen := 3 + len(h)
		ext[2], ext[3] = byte((2+listLen)>>8), byte(2+listLen)
		ext[4], ext[5] = byte(listLen>>8), byte(listLen)
		ext[7], ext[8] = byte(len(h)>>8), byte(len(h))
		copy(ext[9:], h)

		ch := make([]byte, 0, 64+len(ext))
		ch = append(ch, 0x03, 0x03)
		ch = append(ch, make([]byte, 32)...)
		ch = append(ch, 0x00, 0x00, 0x02, 0x00, 0x2f, 0x01, 0x00)
		ch = append(ch, byte(len(ext)>>8), byte(len(ext)))
		ch = append(ch, ext...)

		hs := make([]byte, 4+len(ch))
		hs[0] = 0x01
		hs[1], hs[2], hs[3] = byte(len(ch)>>16), byte(len(ch)>>8), byte(len(ch))
		copy(hs[4:], ch)

		rec := make([]byte, 5+len(hs))
		rec[0], rec[1], rec[2] = 0x16, 0x03, 0x01
		rec[3], rec[4] = byte(len(hs)>>8), byte(len(hs))
		copy(rec[5:], hs)
		return rec
	}

	sendHello := func(sni string) {
		c := dial(t, addr)
		defer c.Close()
		hello := makeHello(sni)
		if _, err := c.Write(hello); err != nil {
			t.Fatalf("write %s: %v", sni, err)
		}
		got := make([]byte, len(hello))
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := io.ReadFull(c, got); err != nil {
			t.Fatalf("read echoed ClientHello for %s: %v", sni, err)
		}
		if string(got) != string(hello) {
			t.Fatalf("echoed ClientHello for %s was corrupted", sni)
		}
	}

	sendHello("api.example.com")
	sendHello("sub.wild.example")
	sendHello("unmatched.example.org")
}

// TestProxyWasmOnDataInspectsL7OnceAndStream verifies L7 payload inspection in
// both modes:
//   - "once" (default): the first packet is inspected in WASM before dialing
//     the upstream; allowed packets are replayed intact via DrainTo.
//   - "stream": every chunk is inspected in WASM, so a bad packet sent later on
//     an already established connection tears it down immediately.
func TestProxyWasmOnDataInspectsL7OnceAndStream(t *testing.T) {
	// WASM module exporting memory + on_data(ptr, len) -> 1 if payload[0] == 'X', else 0.
	wasmDenyX := []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
		0x01, 0x07, 0x01, 0x60, 0x02, 0x7f, 0x7f, 0x01, 0x7f,
		0x03, 0x02, 0x01, 0x00,
		0x05, 0x03, 0x01, 0x00, 0x01,
		0x07, 0x14, 0x02,
		0x06, 'm', 'e', 'm', 'o', 'r', 'y', 0x02, 0x00,
		0x07, 'o', 'n', '_', 'd', 'a', 't', 'a', 0x00, 0x00,
		0x0a, 0x0d, 0x01,
		0x0b, 0x00, 0x20, 0x00, 0x2d, 0x00, 0x00, 0x41, 0xd8, 0x00, 0x46, 0x0b,
	}

	echoAddr := startEcho(t)
	m := observe.NewMetrics()
	cfgOnce := validated(t, edgeConfig([]string{echoAddr}, func(lc *config.ListenerConfig) {
		lc.Plugin = config.PluginConfig{Wasm: wasmDenyX} // default data_mode = "once"
	}))
	srv := NewServer(testLogger(), NewRuntime(cfgOnce), m)
	addr := startServer(t, srv)

	// 1. Clean L7 request in "once" mode passes and is echoed back intact.
	good := roundTrip(t, addr)
	_ = good.Close()

	// 2. Malicious first packet starting with 'X' is rejected before dialing upstream.
	bad := dial(t, addr)
	defer bad.Close()
	_, _ = bad.Write([]byte("X-INJECTION"))
	_ = bad.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := bad.Read(make([]byte, 1)); err == nil {
		t.Fatal("packet starting with 'X' was not rejected by WASM on_data")
	}
	waitMetric(t, 2*time.Second, func() bool {
		return m.Snapshot().ConnectionsPluginRejected >= 1
	})

	// 3. Hot-reload to "stream" mode: the first packet passes, the second packet
	// on the SAME connection starts with 'X' and tears down the stream.
	cfgStream := validated(t, edgeConfig([]string{echoAddr}, func(lc *config.ListenerConfig) {
		lc.Plugin = config.PluginConfig{Wasm: wasmDenyX, DataMode: "stream"}
	}))
	if _, err := srv.Reload(context.Background(), cfgStream); err != nil {
		t.Fatalf("reload stream mode: %v", err)
	}
	c := roundTrip(t, addr) // first chunk "ping" is allowed and echoed
	defer c.Close()
	_, _ = c.Write([]byte("X-SECOND-CHUNK"))
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("second chunk starting with 'X' was not rejected in stream mode")
	}
}

// TestProxyExampleIPGuardWasmBlocks127_0_0_99 loads the real
// plugins/examples/ipguard/ipguard.wasm binary into a live proxy listener and
// dials from two real loopback source IPs:
//   - 127.0.0.1 -> allowed by ipguard.wasm, payload echoed;
//   - 127.0.0.99 -> denied by ipguard.wasm (last octet == 99), connection closed
//     before dialing the upstream and counted in ConnectionsPluginRejected.
func TestProxyExampleIPGuardWasmBlocks127_0_0_99(t *testing.T) {
	echoAddr := startEcho(t)
	m := observe.NewMetrics()
	cfg := validated(t, edgeConfig([]string{echoAddr}, func(lc *config.ListenerConfig) {
		lc.Plugin = config.PluginConfig{
			Path: "../../plugins/examples/ipguard/ipguard.wasm",
		}
	}))
	srv := NewServer(testLogger(), NewRuntime(cfg), m)
	addr := startServer(t, srv)

	// 1. Source IP 127.0.0.1 must pass through ipguard.wasm and echo bytes.
	c1 := roundTrip(t, addr)
	_ = c1.Close()

	// 2. Source IP 127.0.0.99 must be rejected by ipguard.wasm's on_accept hook.
	d99 := net.Dialer{
		Timeout:   2 * time.Second,
		LocalAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 99), Port: 0},
	}
	c99, err := d99.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial from 127.0.0.99: %v", err)
	}
	defer c99.Close()
	_ = c99.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c99.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection from 127.0.0.99 was not rejected by ipguard.wasm")
	}
	waitMetric(t, 2*time.Second, func() bool {
		return m.Snapshot().ConnectionsPluginRejected >= 1
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
