package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"wasmgate/internal/config"
)

// Fixtures shared by the tests and benchmarks in this package.
//
// Layout rule: tests and benchmarks live in the file named after the source file
// they cover (server.go -> server_test.go, copy.go -> copy_test.go and so on),
// tests first, benchmarks below. This file holds only what more than one of them
// needs.

// Sinks stop the compiler from optimising the benchmarked calls away.
var (
	sinkUpstream *upstream
	sinkListener resolvedListener
	sinkSnapshot *Snapshot
	sinkVersion  uint64
)

// testLogger returns a logger that discards everything, so test output stays
// readable.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// startEcho starts a trivial TCP echo server and returns its address; the
// listener is closed when tb's cleanup runs.
func startEcho(tb testing.TB) string {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("listen echo: %v", err)
	}
	tb.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go echo(c)
		}
	}()
	return ln.Addr().String()
}

// echo copies everything it reads back to the client until the peer goes away.
// It uses an explicit read/write loop rather than io.Copy so that splicing a
// socket onto itself can never be attempted.
func echo(c net.Conn) {
	defer c.Close()
	buf := make([]byte, 32<<10)
	for {
		n, err := c.Read(buf)
		if n > 0 {
			if _, werr := c.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// dial opens a client connection to addr, failing tb on error.
func dial(tb testing.TB, addr string) net.Conn {
	tb.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		tb.Fatalf("dial %s: %v", addr, err)
	}
	return conn
}

// edgeConfig builds a config with a single listener "edge" feeding pool "web",
// which holds backends. The listener gets a 1s dial timeout; mutate may adjust it
// further (nil is fine).
func edgeConfig(backends []string, mutate func(*config.ListenerConfig)) *config.Config {
	lc := config.ListenerConfig{
		Name:         "edge",
		Addr:         "127.0.0.1:0",
		UpstreamPool: "web",
		DialTimeout:  time.Second,
	}
	if mutate != nil {
		mutate(&lc)
	}
	return &config.Config{
		Version:   1,
		Listeners: []config.ListenerConfig{lc},
		Pools:     map[string]config.PoolConfig{"web": {Servers: backends}},
	}
}

// validated returns cfg after checking it, failing tb if the config is invalid.
func validated(tb testing.TB, cfg *config.Config) *config.Config {
	tb.Helper()
	if err := cfg.Validate(); err != nil {
		tb.Fatalf("validate: %v", err)
	}
	return cfg
}

// startServer starts srv and registers its shutdown with tb's cleanup.
func startServer(tb testing.TB, srv *Server) string {
	tb.Helper()
	if err := srv.Start(); err != nil {
		tb.Fatalf("start: %v", err)
	}
	tb.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return srv.Addrs()[0].String()
}

// newTestServer starts a proxy in front of an echo server and returns the
// proxy's listen address plus the server handle.
func newTestServer(t *testing.T, mutate func(*config.ListenerConfig)) (string, *Server) {
	t.Helper()
	srv := NewServer(testLogger(), NewRuntime(edgeConfig([]string{startEcho(t)}, mutate)), nil)
	return startServer(t, srv), srv
}

// startProxy starts a Server with one listener feeding pool "web" (backend) and
// returns its address. idle is the listener idle_timeout (0 keeps the zero-copy
// path).
func startProxy(tb testing.TB, backend string, idle time.Duration) string {
	tb.Helper()
	cfg := validated(tb, edgeConfig([]string{backend}, func(lc *config.ListenerConfig) {
		lc.IdleTimeout = idle
	}))
	return startServer(tb, NewServer(testLogger(), NewRuntime(cfg), nil))
}

// waitMetric polls cond until it reports true or the deadline expires, failing
// the test on timeout. Metrics are updated from the connection goroutines, so a
// test that just closed its socket must give them a moment to land.
func waitMetric(t *testing.T, deadline time.Duration, cond func() bool) {
	t.Helper()
	end := time.Now().Add(deadline)
	for !cond() {
		if time.Now().After(end) {
			t.Fatal("metrics did not settle in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// roundTrip opens a connection, sends one byte and waits for the echoed byte.
func roundTrip(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn := dial(t, addr)
	if _, err := conn.Write([]byte("x")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	return conn
}

// startWasmGateFull starts WasmGate with max_conns, F2 sliding-window
// rate_limit and the example ipguard.wasm plugin enabled on the hot path.
func startWasmGateFull(tb testing.TB, backend string) string {
	tb.Helper()
	cfg := validated(tb, edgeConfig([]string{backend}, func(lc *config.ListenerConfig) {
		lc.MaxConns = 10000
		lc.RateLimit = config.RateLimit{
			Max:       1_000_000,
			SubnetMax: 1_000_000,
			Window:    time.Second,
		}
		lc.Plugin = config.PluginConfig{
			Path: "../../plugins/examples/ipguard/ipguard.wasm",
		}
	}))
	return startServer(tb, NewServer(testLogger(), NewRuntime(cfg), nil))
}

// freeBindAddr reserves and immediately releases a loopback TCP port so an
// external proxy (nginx, haproxy) can bind it in its generated config file.
func freeBindAddr(tb testing.TB) string {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("listen free port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// waitListening polls addr until a TCP connection succeeds or deadline expires.
func waitListening(tb testing.TB, addr string, deadline time.Duration) {
	tb.Helper()
	end := time.Now().Add(deadline)
	for {
		c, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		if time.Now().After(end) {
			tb.Fatalf("external proxy on %s did not start listening in time: %v", addr, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startNginxStream launches a real nginx process configured with the L4 stream
// module forwarding to backend. Skips tb if nginx or ngx_stream_module is not
// installed on the host.
func startNginxStream(tb testing.TB, backend string) string {
	tb.Helper()
	bin, err := exec.LookPath("nginx")
	if err != nil {
		tb.Skip("nginx not installed")
	}
	dir := tb.TempDir()
	addr := freeBindAddr(tb)

	var loadMod string
	for _, modPath := range []string{
		"/usr/lib/nginx/modules/ngx_stream_module.so",
		"/usr/lib64/nginx/modules/ngx_stream_module.so",
	} {
		if _, err := os.Stat(modPath); err == nil {
			loadMod = fmt.Sprintf("load_module %s;\n", modPath)
			break
		}
	}

	confPath := filepath.Join(dir, "nginx.conf")
	conf := fmt.Sprintf(`%sdaemon off;
worker_processes 2;
pid %s/nginx.pid;
error_log %s/error.log crit;
events {
    worker_connections 4096;
}
stream {
    server {
        listen %s;
        proxy_pass %s;
    }
}
`, loadMod, dir, dir, addr, backend)

	if err := os.WriteFile(confPath, []byte(conf), 0o600); err != nil {
		tb.Fatalf("write nginx.conf: %v", err)
	}

	cmd := exec.Command(bin, "-p", dir, "-c", confPath)
	if err := cmd.Start(); err != nil {
		tb.Skipf("start nginx: %v", err)
	}
	tb.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	waitListening(tb, addr, 3*time.Second)
	return addr
}

// startHAProxyTCP launches a real HAProxy process in L4 TCP mode forwarding to
// backend. Skips tb if haproxy is not installed on the host.
func startHAProxyTCP(tb testing.TB, backend string) string {
	tb.Helper()
	bin, err := exec.LookPath("haproxy")
	if err != nil {
		tb.Skip("haproxy not installed")
	}
	dir := tb.TempDir()
	addr := freeBindAddr(tb)

	confPath := filepath.Join(dir, "haproxy.cfg")
	conf := fmt.Sprintf(`global
    maxconn 4096

defaults
    mode tcp
    timeout connect 5s
    timeout client 60s
    timeout server 60s

frontend fe
    bind %s
    default_backend be

backend be
    server s1 %s
`, addr, backend)

	if err := os.WriteFile(confPath, []byte(conf), 0o600); err != nil {
		tb.Fatalf("write haproxy.cfg: %v", err)
	}

	cmd := exec.Command(bin, "-f", confPath)
	if err := cmd.Start(); err != nil {
		tb.Skipf("start haproxy: %v", err)
	}
	tb.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	waitListening(tb, addr, 3*time.Second)
	return addr
}
