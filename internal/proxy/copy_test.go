package proxy

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"wasmgate/internal/config"
)

// Tests for copy.go: the relay itself, in particular the idle-timeout watchdog,
// followed by benchmarks for what it sustains once a session is up.

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
// idle timeout, so a timeout error would mean the watchdog never fired — and a
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

	halfCloseWrite(c)

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
// relay through bufferPool (see copy.go).

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
