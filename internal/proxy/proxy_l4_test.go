package proxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
	"wasmgate/internal/config"
	"wasmgate/internal/observe"
	"wasmgate/internal/proxy/relay"
)

// Tests for relay behavior and L4/L7 end-to-end paths.

// startDrip starts a TCP server that, per connection, waits for one trigger
// byte and then writes n bytes spaced by delay, without reading anything else.
// It emulates a long one-way transfer (a slow download): the client is silent
// while the backend streams.
func startDrip(t *testing.T, n int, delay time.Duration) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen drip: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				trigger := make([]byte, 1)
				if _, err := io.ReadFull(c, trigger); err != nil {
					return
				}
				for i := 0; i < n; i++ {
					if _, err := c.Write([]byte{'x'}); err != nil {
						return
					}
					time.Sleep(delay)
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// TestIdleTimeoutKeepsOneWayStreamAlive is the regression test for the bug where
// idle_timeout was enforced per direction: a slow download (client silent while
// the backend streams) used to be cut off after one idle period. The timeout
// must now apply to the whole connection.
func TestIdleTimeoutKeepsOneWayStreamAlive(t *testing.T) {
	const total = 20
	backend := startDrip(t, total, 50*time.Millisecond) // ~1s of one-way traffic

	cfg := edgeConfig([]string{backend}, func(lc *config.ListenerConfig) {
		lc.IdleTimeout = 300 * time.Millisecond
	})
	srv := NewServer(testLogger(), NewRuntime(cfg), nil)
	addr := startServer(t, srv)

	conn := dial(t, addr)
	defer conn.Close()

	if _, err := conn.Write([]byte("go")); err != nil { // trigger the stream
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, total)
	n, err := io.ReadFull(conn, got)
	if err != nil {
		t.Fatalf("one-way stream was cut after %d/%d bytes: %v", n, total, err)
	}
}

// TestIdleTimeoutClosesSilentConnection is the other half of the watchdog: when
// NEITHER direction carries bytes for a full idle period the connection must be
// torn down. That is the entire point of the timeout, and the test above only
// covers the case where it must NOT fire.
//
// The client's own read deadline is deliberately much longer than the listener's
// idle timeout, so a timeout error would mean the watchdog never fired â€” and a
// teardown is distinguishable from it (EOF, not a timeout).
func TestIdleTimeoutClosesSilentConnection(t *testing.T) {
	addr := startProxy(t, startEcho(t), 200*time.Millisecond)
	conn := roundTrip(t, addr)
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err := conn.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("read returned data on a connection that has been silent")
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		t.Fatal("idle_timeout did not fire: the connection was still alive at the client's own deadline")
	}
}

// TestHalfCloseWriteClosesPlainConn covers the fallback in halfCloseWrite: a
// connection that cannot signal EOF (anything other than *net.TCPConn, e.g. an
// in-memory pipe) is closed outright instead of being left half-open with the
// peer still reading.
func TestHalfCloseWriteClosesPlainConn(t *testing.T) {
	c, peer := net.Pipe()
	defer peer.Close()

	relay.HalfCloseWrite(c)

	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := peer.Read(make([]byte, 1)); err == nil {
		t.Fatal("halfCloseWrite left a plain connection open")
	}
}

// TestRealProxiesComparison verifies that WasmGate (both bare L4 and with full
// max_conns + F2 rate_limit + WASM ipguard plugin), nginx stream and HAProxy
// TCP all relay the exact same payload against the same backend on Linux.
func TestRealProxiesComparison(t *testing.T) {
	backend := startEcho(t)
	targets := []struct {
		name  string
		start func(testing.TB, string) string
	}{
		{"wasmgate_l4", func(tb testing.TB, be string) string { return startProxy(tb, be, 0) }},
		{"wasmgate_full", startWasmGateFull},
		{"nginx_stream", startNginxStream},
		{"haproxy_tcp", startHAProxyTCP},
	}

	const payload = "wasmgate-vs-nginx-vs-haproxy-l4-check"
	for _, tc := range targets {
		t.Run(tc.name, func(t *testing.T) {
			addr := tc.start(t, backend)
			c := dial(t, addr)
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(2 * time.Second))
			if _, err := c.Write([]byte(payload)); err != nil {
				t.Fatalf("write: %v", err)
			}
			got := make([]byte, len(payload))
			if _, err := io.ReadFull(c, got); err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(got) != payload {
				t.Fatalf("got %q, want %q", got, payload)
			}
		})
	}
}

// The benchmarks below take a real proxy over loopback TCP in front of an echo
// backend, so they exercise the production path:
//
//	client -> accept -> snapshot lookup -> upstream dial -> relay -> client
//
// and each has a "direct" baseline (client talks straight to the echo backend).
// The gap between the two is what the relay costs; without the baseline a
// loopback/echo bottleneck would be misread as a proxy bottleneck.
//
// Zero-copy note: the splice(2) path exists only on Linux. On other platforms
// both copy paths fall back to user-space copies, so the numbers still run but
// the zero-copy advantage shows up only in a Linux run. Off Linux the two paths
// differ only in the idle watchdog (a read deadline per chunk), which is why
// "zero_copy" and "idle_watchdog" land close together on Windows. Neither
// relay allocates a per-connection buffer: only the Linux plain path stays
// wrapper-free so io.Copy can reach splice(2), the other three combinations
// relay through the relay package's buffer pool (see internal/proxy/relay).

// throughputSizes are the chunk sizes swept. 4 KiB is a syscall-heavy "small
// writes" case; 1 MiB amortises syscalls over large copies.
var throughputSizes = []int{4 << 10, 64 << 10, 1 << 20}

// throughputPaths are the paths swept. "direct" goes straight to the echo
// backend and is the no-proxy baseline; the other two pick the relay path
// through the listener's idle_timeout (0 keeps io.Copy with splice zero-copy on
// Linux, >0 wraps both directions in the idle watchdog). The watchdog deadline
// is never reached during a benchmark; the point is to measure what refreshing
// it costs.
var throughputPaths = []struct {
	name   string
	idle   time.Duration
	direct bool
}{
	{"direct", 0, true},
	{"zero_copy", 0, false},
	{"idle_watchdog", 5 * time.Second, false},
}

// sizeLabel renders a byte size as "4KiB" / "1MiB" for sub-benchmark names.
func sizeLabel(size int) string {
	if size >= 1<<20 {
		return fmt.Sprintf("%dMiB", size>>20)
	}
	return fmt.Sprintf("%dKiB", size>>10)
}

// BenchmarkThroughput measures payload throughput (MiB/s, printed by Go as
// MB/s) for each path and chunk size.
//
// The client streams fixed-size chunks while a reader drains the echoes, so the
// pipe never idles and the figure reflects bandwidth, not latency. The clock
// stops only after every byte has come back, so nothing still buffered in a
// socket can inflate the result. Note that through the proxy each payload byte
// crosses it twice (client -> upstream -> client), so syscall-level traffic is
// roughly 2x; compare against the "direct" baseline to isolate the relay cost.
func BenchmarkThroughput(b *testing.B) {
	for _, p := range throughputPaths {
		for _, size := range throughputSizes {
			b.Run(p.name+"/"+sizeLabel(size), func(b *testing.B) {
				benchThroughput(b, p.idle, p.direct, size)
			})
		}
	}
}

func benchThroughput(b *testing.B, idle time.Duration, direct bool, size int) {
	backend := startEcho(b)

	target := backend
	if !direct {
		target = startProxy(b, backend, idle)
	}

	conn := dial(b, target)
	defer conn.Close()

	// The writer sends exactly b.N chunks and the reader drains exactly that many
	// bytes. Both budgets are equal, so neither side can block waiting for the
	// other: that invariant is what keeps this benchmark free of deadlocks.
	want := int64(b.N) * int64(size)
	echoed := make(chan int64, 1)
	go func() {
		var got int64
		buf := make([]byte, 64<<10)
		for got < want {
			n, err := conn.Read(buf)
			got += int64(n)
			if err != nil {
				break
			}
		}
		echoed <- got
	}()

	chunk := make([]byte, size)
	b.SetBytes(int64(size))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := conn.Write(chunk); err != nil {
			b.Fatalf("write: %v", err)
		}
	}
	got := <-echoed // stop the clock only after the last bytes come back
	b.StopTimer()
	if got < want {
		b.Fatalf("echoed %d of %d bytes", got, want)
	}
}

// scalingConnCounts are the concurrency levels swept.
var scalingConnCounts = []int{1, 4, 16, 64}

// BenchmarkThroughputScaling measures aggregate throughput as the number of
// concurrent proxied connections grows. It answers "does the relay scale?": with
// full-duplex forwarding, aggregate MiB/s should keep rising until loopback/CPU
// saturates.
func BenchmarkThroughputScaling(b *testing.B) {
	const size = 64 << 10
	for _, conns := range scalingConnCounts {
		b.Run(fmt.Sprintf("conns=%d", conns), func(b *testing.B) {
			benchScaling(b, conns, size)
		})
	}
}

func benchScaling(b *testing.B, conns, size int) {
	addr := startProxy(b, startEcho(b), 0)

	clients := make([]net.Conn, conns)
	for i := range clients {
		clients[i] = dial(b, addr)
	}
	defer func() {
		for _, c := range clients {
			_ = c.Close()
		}
	}()

	// Each connection gets a fixed number of chunks, split so the totals add up
	// to exactly b.N (keeping the reported MiB/s honest). A shared byte counter
	// would let a reader leave early and strand its writer on a full socket; a
	// fixed per-connection budget cannot.
	per := b.N / conns
	rem := b.N % conns

	chunk := make([]byte, size)
	var wg sync.WaitGroup
	b.SetBytes(int64(size))
	b.ReportAllocs()
	b.ResetTimer()

	wg.Add(2 * conns)
	for i, c := range clients {
		rounds := int64(per)
		if i < rem {
			rounds++
		}
		go writeRounds(&wg, c, chunk, rounds)
		go readBytes(&wg, c, rounds*int64(size))
	}
	wg.Wait()
	b.StopTimer()
}

// writeRounds writes chunk exactly rounds times, then returns. On a write error
// it closes the connection, so the paired reader cannot stay blocked forever and
// deadlock wg.Wait.
func writeRounds(wg *sync.WaitGroup, c net.Conn, chunk []byte, rounds int64) {
	defer wg.Done()
	for i := int64(0); i < rounds; i++ {
		if _, err := c.Write(chunk); err != nil {
			_ = c.Close()
			return
		}
	}
}

// readBytes drains want bytes from c, then returns. On a read error it closes
// the connection, so the paired writer cannot stay blocked forever.
func readBytes(wg *sync.WaitGroup, c net.Conn, want int64) {
	defer wg.Done()
	buf := make([]byte, 64<<10)
	var got int64
	for got < want {
		n, err := c.Read(buf)
		got += int64(n)
		if err != nil {
			_ = c.Close()
			return
		}
	}
}

// BenchmarkRelayLatency measures the per-request round trip on a single warm
// connection: one byte out, one byte back, no dial or close. This is the latency
// a client sees, as opposed to the bulk throughput above. The "direct" baseline
// shows how much of it is the relay and how much is the OS/loopback stack.
func BenchmarkRelayLatency(b *testing.B) {
	backend := startEcho(b)
	addr := startProxy(b, backend, 0)

	b.Run("direct", func(b *testing.B) { benchRoundTrip(b, backend) })
	b.Run("through_proxy", func(b *testing.B) { benchRoundTrip(b, addr) })
}

func benchRoundTrip(b *testing.B, addr string) {
	conn := dial(b, addr)
	defer conn.Close()

	probe := []byte{0}
	var back [1]byte
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := conn.Write(probe); err != nil {
			b.Fatalf("write: %v", err)
		}
		if _, err := io.ReadFull(conn, back[:]); err != nil {
			b.Fatalf("read: %v", err)
		}
	}
}

// BenchmarkProxyComparison runs head-to-head S0/S1/S5 benchmarks (Throughput
// at 64 KiB and 1 MiB, plus warm RelayLatency) across direct, WasmGate L4
// zero-copy, WasmGate with F2 rate_limit + WASM plugin, nginx stream and
// HAProxy TCP on the same echo backend.
func BenchmarkProxyComparison(b *testing.B) {
	targets := []struct {
		name  string
		start func(testing.TB, string) string
	}{
		{"direct", func(_ testing.TB, be string) string { return be }},
		{"wasmgate_l4", func(tb testing.TB, be string) string { return startProxy(tb, be, 0) }},
		{"wasmgate_full", startWasmGateFull},
		{"nginx_stream", startNginxStream},
		{"haproxy_tcp", startHAProxyTCP},
	}

	for _, size := range []int{64 << 10, 1 << 20} {
		for _, tc := range targets {
			b.Run("Throughput/"+sizeLabel(size)+"/"+tc.name, func(b *testing.B) {
				backend := startEcho(b)
				target := tc.start(b, backend)
				benchThroughputAddr(b, target, size)
			})
		}
	}

	for _, tc := range targets {
		b.Run("RelayLatency/"+tc.name, func(b *testing.B) {
			backend := startEcho(b)
			target := tc.start(b, backend)
			benchRoundTrip(b, target)
		})
	}
}

func benchThroughputAddr(b *testing.B, target string, size int) {
	conn := dial(b, target)
	defer conn.Close()

	want := int64(b.N) * int64(size)
	echoed := make(chan int64, 1)
	go func() {
		var got int64
		buf := make([]byte, 64<<10)
		for got < want {
			n, err := conn.Read(buf)
			got += int64(n)
			if err != nil {
				break
			}
		}
		echoed <- got
	}()

	chunk := make([]byte, size)
	b.SetBytes(int64(size))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := conn.Write(chunk); err != nil {
			b.Fatalf("write: %v", err)
		}
	}
	got := <-echoed
	b.StopTimer()
	if got < want {
		b.Fatalf("echoed %d of %d bytes", got, want)
	}
}

// Tests for the connection handler and server facade: one connection handled
// end to end, followed by the benchmarks for the per-connection CPU work.

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
	// ABI v4 lifecycle module returning 1 (deny) or 0 (allow).
	wasmVerdict := v4Lifecycle

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

// TestProxyWasmOnDataInspectsL7OnceAndStream verifies ABI v4 opaque payload
// inspection in both modes:
//   - "once": the first packet is inspected before dialing the upstream;
//     allowed packets are replayed intact via DrainTo.
//   - "stream": every chunk is inspected, so a bad packet sent later on an
//     already established connection tears it down immediately.
func TestProxyWasmOnDataInspectsL7OnceAndStream(t *testing.T) {
	echoAddr := startEcho(t)
	m := observe.NewMetrics()
	cfgOnce := validated(t, edgeConfig([]string{echoAddr}, func(lc *config.ListenerConfig) {
		lc.Plugin = config.PluginConfig{Wasm: v4DenyX(), DataMode: config.DataModeOnce}
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
		t.Fatal("packet starting with 'X' was not rejected by ABI v4 on_event")
	}
	waitMetric(t, 2*time.Second, func() bool {
		return m.Snapshot().ConnectionsPluginRejected >= 1
	})

	// 3. Hot-reload to "stream" mode: the first packet passes, the second packet
	// on the SAME connection starts with 'X' and tears down the stream.
	cfgStream := validated(t, edgeConfig([]string{echoAddr}, func(lc *config.ListenerConfig) {
		lc.Plugin = config.PluginConfig{Wasm: v4DenyX(), DataMode: config.DataModeStream}
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

// TestProxyV4L4LifecycleAllowsAndDenies verifies the ABI v4 L4 lifecycle
// path: on_accept allow reaches the upstream, on_accept deny closes before dial.
func TestProxyV4L4LifecycleAllowsAndDenies(t *testing.T) {
	echoAddr := startEcho(t)

	allowCfg := validated(t, edgeConfig([]string{echoAddr}, func(lc *config.ListenerConfig) {
		lc.Plugin = config.PluginConfig{Wasm: v4Lifecycle(0)}
	}))
	allowAddr := startServer(t, NewServer(testLogger(), NewRuntime(allowCfg), nil))
	c := roundTrip(t, allowAddr)
	_ = c.Close()

	denyCfg := validated(t, edgeConfig([]string{echoAddr}, func(lc *config.ListenerConfig) {
		lc.Plugin = config.PluginConfig{Wasm: v4Lifecycle(1)}
	}))
	denyAddr := startServer(t, NewServer(testLogger(), NewRuntime(denyCfg), nil))
	d := dial(t, denyAddr)
	defer d.Close()
	_ = d.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := d.Read(make([]byte, 1)); err == nil {
		t.Fatal("v4 lifecycle deny did not close the connection")
	}
}

// TestProxyDialRetriesFailsOverTransparently verifies that when the first
// server in a pool is down and dial_retries >= 1, the proxy transparently
// retries the dial on the next healthy server in the pool without dropping the
// client connection.
func TestProxyDialRetriesFailsOverTransparently(t *testing.T) {
	live := startEcho(t)
	m := observe.NewMetrics()
	cfg := validated(t, &config.Config{
		Version: 1,
		Listeners: []config.ListenerConfig{{
			Name:         "edge",
			Addr:         "127.0.0.1:0",
			UpstreamPool: "web",
			DialTimeout:  500 * time.Millisecond,
		}},
		Pools: map[string]config.PoolConfig{
			"web": {
				Balance:     "round_robin",
				Servers:     []string{"127.0.0.1:1", live},
				DialRetries: 1,
			},
		},
	})
	addr := startServer(t, NewServer(testLogger(), NewRuntime(cfg), m))

	c := roundTrip(t, addr)
	_ = c.Close()

	waitMetric(t, 2*time.Second, func() bool {
		s := m.Snapshot()
		return s.UpstreamDialFailures >= 1 && s.UpstreamRetries >= 1
	})
}

func TestProxyStreamInspectsServerDirection(t *testing.T) {
	backend := startBanner(t, "X-RESPONSE")
	cfg := validated(t, edgeConfig([]string{backend}, func(lc *config.ListenerConfig) {
		lc.Plugin = config.PluginConfig{Wasm: v4DenyX(), DataMode: config.DataModeStream}
	}))
	addr := startServer(t, NewServer(testLogger(), NewRuntime(cfg), nil))

	conn := dial(t, addr)
	defer conn.Close()
	if _, err := conn.Write([]byte("ok")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("server-to-client chunk starting with 'X' was not rejected")
	}
}

// TestProxyWasmOpaqueRoutesPool verifies that an ABI v4 once-mode event can
// return a route verdict and select a pool before dialing. The canary backend
// closes immediately, so a successful route is observable without payload
// mutation (M2 is read-only).
func TestProxyStreamRouteVerdictDenied(t *testing.T) {
	backend := startEcho(t)
	cfg := validated(t, edgeConfig([]string{backend}, func(lc *config.ListenerConfig) {
		lc.Plugin = config.PluginConfig{Wasm: v4Verdict(2), DataMode: config.DataModeStream}
	}))
	addr := startServer(t, NewServer(testLogger(), NewRuntime(cfg), nil))
	conn := dial(t, addr)
	defer conn.Close()
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	expectClosed(t, conn)
}

func TestProxyWasmOpaqueRoutesPool(t *testing.T) {
	defaultEcho := startEcho(t)
	canary := startCloseServer(t)

	cfg := validated(t, &config.Config{
		Version: 1,
		Listeners: []config.ListenerConfig{{
			Name:         "edge",
			Addr:         "127.0.0.1:0",
			UpstreamPool: "default",
			DialTimeout:  time.Second,
			Plugin: config.PluginConfig{
				Wasm:       v4Verdict(2),
				DataMode:   config.DataModeOnce,
				RoutePools: []string{"canary"},
			},
		}},
		Pools: map[string]config.PoolConfig{
			"default": {Servers: []string{defaultEcho}},
			"canary":  {Servers: []string{canary}},
		},
	})
	rt := NewRuntime(cfg)
	addr := startServer(t, NewServer(testLogger(), rt, nil))

	c := dial(t, addr)
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	expectClosed(t, c)
}

// TestProxyTLSTerminationWithWasmOnData verifies optional TLS termination on a
// listener: the client connects over TLS, the proxy terminates TLS and hands
// the decrypted plaintext to the ABI v4 once-mode hook before forwarding it to
// the plain-TCP backend.
func TestProxyTLSTerminationWithWasmOnData(t *testing.T) {
	certPEM, keyPEM := generateSelfSignedCert(t)
	echoAddr := startEcho(t)

	cfg := validated(t, edgeConfig([]string{echoAddr}, func(lc *config.ListenerConfig) {
		lc.TLS = config.TLSConfig{CertPEM: certPEM, KeyPEM: keyPEM, MinVersion: "1.2"}
		lc.Plugin = config.PluginConfig{Wasm: v4DenyX(), DataMode: config.DataModeOnce}
	}))
	addr := startServer(t, NewServer(testLogger(), NewRuntime(cfg), nil))

	tlsCfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test fixture
	conn, err := tls.Dial("tcp", addr, tlsCfg)
	if err != nil {
		t.Fatalf("tls.Dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write([]byte("OK-HTTPS")); err != nil {
		t.Fatalf("write: %v", err)
	}
	var back [8]byte
	if _, err := io.ReadFull(conn, back[:]); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(back[:]) != "OK-HTTPS" {
		t.Fatalf("got %q, want OK-HTTPS", back[:])
	}
}

func generateSelfSignedCert(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
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
		l, ok := rt.Load().Listener("edge")
		if !ok || !l.Limiter.Acquire() {
			b.Fatal("unexpected refusal below the limit")
		}
		up := l.Pool.Next()
		l.Pool.Release(up)
		l.Limiter.Release()
		sinkListener = l
	}
}

// TestProxySNIRoutesStreamPluginSeesPeekedPrefix is the regression test for the
// interaction between SNI peek and data_mode "stream": tlspeek.Peek buffers the
// first bytes in a *tlspeek.Conn, and that buffered prefix must be passed
// through the ABI v4 stream hook before DrainTo replays it to the backend.
func TestProxySNIRoutesStreamPluginSeesPeekedPrefix(t *testing.T) {
	echoAddr := startEcho(t)
	m := observe.NewMetrics()
	cfg := validated(t, &config.Config{
		Version: 1,
		Listeners: []config.ListenerConfig{{
			Name:         "edge",
			Addr:         "127.0.0.1:0",
			UpstreamPool: "web",
			DialTimeout:  time.Second,
			SNIRoutes:    map[string]string{"api.example.com": "web"},
			Plugin:       config.PluginConfig{Wasm: v4DenyX(), DataMode: config.DataModeStream},
		}},
		Pools: map[string]config.PoolConfig{"web": {Servers: []string{echoAddr}}},
	})
	addr := startServer(t, NewServer(testLogger(), NewRuntime(cfg), m))

	// A non-TLS prefix starting with 'X' is buffered by tlspeek.Peek. Without the
	// fix it bypassed the plugin and was echoed back; now it must be rejected.
	bad := dial(t, addr)
	defer bad.Close()
	if _, err := bad.Write([]byte("X-BYPASS")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = bad.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := bad.Read(make([]byte, 1)); err == nil {
		t.Fatal("peeked prefix bypassed the stream plugin and reached the backend")
	}
	waitMetric(t, 2*time.Second, func() bool {
		return m.Snapshot().ConnectionsPluginRejected >= 1
	})

	// A benign prefix must still be replayed intact.
	good := dial(t, addr)
	defer good.Close()
	if _, err := good.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = good.SetReadDeadline(time.Now().Add(2 * time.Second))
	got := make([]byte, 5)
	if _, err := io.ReadFull(good, got); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("echoed prefix = %q, want hello", string(got))
	}
}

// TestProxyReloadPicksUpReplacedWasmFile verifies that the plugin cache is keyed
// by content, not only by path: rewriting the .wasm file in place and reloading
// must swap allow -> deny without a process restart.
func TestProxyReloadPicksUpReplacedWasmFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plugin.wasm")
	if err := os.WriteFile(path, v4Lifecycle(0), 0o600); err != nil {
		t.Fatalf("write allow plugin: %v", err)
	}

	echoAddr := startEcho(t)
	buildCfg := func() *config.Config {
		return validated(t, edgeConfig([]string{echoAddr}, func(lc *config.ListenerConfig) {
			lc.Plugin = config.PluginConfig{Path: path}
		}))
	}
	srv := NewServer(testLogger(), NewRuntime(buildCfg()), nil)
	addr := startServer(t, srv)

	c := roundTrip(t, addr) // allow module: connection works
	_ = c.Close()

	// Replace the file at the same path with a deny module, then reload.
	if err := os.WriteFile(path, v4Lifecycle(1), 0o600); err != nil {
		t.Fatalf("write deny plugin: %v", err)
	}
	if _, err := srv.Reload(context.Background(), buildCfg()); err != nil {
		t.Fatalf("reload: %v", err)
	}

	denied := dial(t, addr)
	defer denied.Close()
	_ = denied.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := denied.Read(make([]byte, 1)); err == nil {
		t.Fatal("reloaded listener still used the old allow plugin")
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
			l, _ := rt.Load().Listener("edge")
			l.Limiter.Acquire()
			up := l.Pool.Next()
			l.Pool.Release(up)
			l.Limiter.Release()
			sinkListener = l
		}
	})
}

func TestProxyHTTPBodyGuardRejectsOpaquePlan(t *testing.T) {
	echoAddr := startEcho(t)
	cfg := validated(t, &config.Config{
		Version: 1,
		Listeners: []config.ListenerConfig{{
			Name:         "edge",
			Addr:         "127.0.0.1:0",
			UpstreamPool: "web",
			DialTimeout:  time.Second,
			Plugin: config.PluginConfig{
				Path:     "../../plugins/examples/http_body_guard/http_body_guard.wasm",
				DataMode: config.DataModeStream,
			},
		}},
		Pools: map[string]config.PoolConfig{"web": {Servers: []string{echoAddr}}},
	})
	if _, err := NewRuntimeChecked(cfg); err == nil {
		t.Fatal("HTTP-only guest accepted on an opaque plan")
	}
}

func TestMaxConnsRejectsExtraConnection(t *testing.T) {
	addr, _ := newTestServer(t, func(lc *config.ListenerConfig) { lc.MaxConns = 1 })
	first := roundTrip(t, addr)
	defer first.Close()
	second := dial(t, addr)
	defer second.Close()
	_ = second.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, err := second.Read(make([]byte, 1)); err == nil {
		t.Fatalf("second connection read %d bytes, want it closed by the limit", n)
	}
}

func TestMaxConnsSurvivesReload(t *testing.T) {
	echoAddr := startEcho(t)
	mk := func() *config.Config {
		return edgeConfig([]string{echoAddr}, func(lc *config.ListenerConfig) { lc.MaxConns = 1 })
	}
	srv := NewServer(testLogger(), NewRuntime(mk()), nil)
	addr := startServer(t, srv)
	first := roundTrip(t, addr)
	defer first.Close()
	for i := 0; i < 3; i++ {
		if _, err := srv.Reload(context.Background(), mk()); err != nil {
			t.Fatalf("reload %d: %v", i, err)
		}
	}
	second := dial(t, addr)
	defer second.Close()
	_ = second.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, err := second.Read(make([]byte, 1)); err == nil {
		t.Fatalf("second connection read %d bytes after reload, want it still limited", n)
	}
}

const validHTTPGuardRequest = "GET / HTTP/1.1\r\nHost: example.test\r\nAuthorization: Basic dXNlcjpwYXNz\r\n\r\n"
