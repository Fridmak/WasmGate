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
	"strings"
	"testing"
	"time"

	"wasmgate/internal/config"
	abi4 "wasmgate/internal/plugin/abi4/engine"
	pruntime "wasmgate/internal/proxy/runtime"
)

// Fixtures shared by the tests and benchmarks in this package.
//
// Layout rule: tests and benchmarks live in the package/file that owns the
// behavior they cover. This file holds only fixtures shared by more than one
// root-package test.

// Sinks stop the compiler from optimising the benchmarked calls away.
var (
	sinkListener pruntime.ResolvedListener
)

// testLogger returns a logger that discards everything, so test output stays
// readable.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// wasmOnHeadersDenyX returns a minimal WASM module exporting:
//   - memory (1 page)
//   - on_data_ptr() -> 0x8000
//   - on_headers(conn_id: i64, ptr: i32, len: i32) -> i32
//
// It denies when the first byte is 'X', otherwise allows. The module is assembled
// by hand so the proxy tests do not need a Rust toolchain.
func wasmOnHeadersDenyX() []byte {
	return []byte{
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
		0x0a, 0x14, 0x02,
		0x0b, 0x00, 0x20, 0x01, 0x2d, 0x00, 0x00, 0x41, 0xd8, 0x00, 0x46, 0x0b,
		0x06, 0x00, 0x41, 0x80, 0x80, 0x02, 0x0b,
	}
}

// startBanner starts a TCP server that writes payload once per connection and
// closes it. It is used to exercise server-to-client stream inspection.
func startBanner(tb testing.TB, payload string) string {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("listen banner: %v", err)
	}
	tb.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = io.WriteString(c, payload)
			time.Sleep(500 * time.Millisecond)
			_ = c.Close()
		}
	}()
	return ln.Addr().String()
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
			Wasm: v4Lifecycle(0),
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

// waitExternalProxy waits until addr accepts a TCP connection. If the child
// process exits first (bad config, missing module, port conflict) the test is
// skipped with its captured log instead of failing on an opaque timeout: these
// helpers exercise optional third-party proxies, not WasmGate itself.
func waitExternalProxy(tb testing.TB, name, addr string, timeout time.Duration, exited <-chan error, logPath string) {
	tb.Helper()
	deadline := time.Now().Add(timeout)
	for {
		c, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		select {
		case werr := <-exited:
			tb.Skipf("%s exited before listening on %s: %v\n%s", name, addr, werr, readLog(logPath))
		default:
		}
		if time.Now().After(deadline) {
			tb.Skipf("%s did not listen on %s within %s\n%s", name, addr, timeout, readLog(logPath))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readLog(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "(no log: " + err.Error() + ")"
	}
	return string(b)
}

// nginxStreamSupport reports whether nginx can serve a stream {} block, either
// statically (--with-stream) or through a loadable module, and returns the
// load_module directive in the latter case. Ubuntu's nginx package is built
// with --with-stream=dynamic, so the module lives in a separate
// libnginx-mod-stream package that is not always installed.
func nginxStreamSupport(bin string) (loadModule string, ok bool) {
	for _, modPath := range []string{
		"/usr/lib/nginx/modules/ngx_stream_module.so",
		"/usr/lib64/nginx/modules/ngx_stream_module.so",
		"/usr/local/libexec/nginx/ngx_stream_module.so",
		"/usr/local/nginx/modules/ngx_stream_module.so",
	} {
		if _, err := os.Stat(modPath); err == nil {
			return fmt.Sprintf("load_module %s;\n", modPath), true
		}
	}
	out, _ := exec.Command(bin, "-V").CombinedOutput()
	return "", nginxHasStaticStream(string(out))
}

// nginxHasStaticStream reports whether nginx -V lists --with-stream as a static
// module. --with-stream=dynamic does not count: it still needs the .so.
func nginxHasStaticStream(versionOutput string) bool {
	for _, f := range strings.Fields(versionOutput) {
		if f == "--with-stream" {
			return true
		}
	}
	return false
}

// startNginxStream launches a real nginx process configured with the L4 stream
// module forwarding to backend. It skips tb when nginx, its stream module, or a
// working start is unavailable on the host.
func startNginxStream(tb testing.TB, backend string) string {
	tb.Helper()
	bin, err := exec.LookPath("nginx")
	if err != nil {
		tb.Skip("nginx not installed")
	}
	loadMod, ok := nginxStreamSupport(bin)
	if !ok {
		tb.Skip("nginx has no stream module (install libnginx-mod-stream)")
	}
	dir := tb.TempDir()
	addr := freeBindAddr(tb)

	confPath := filepath.Join(dir, "nginx.conf")
	conf := fmt.Sprintf(`%sdaemon off;
worker_processes 2;
pid %s/nginx.pid;
error_log stderr notice;
events {
    worker_connections 4096;
}
stream {
    server {
        listen %s;
        proxy_pass %s;
    }
}
`, loadMod, dir, addr, backend)

	if err := os.WriteFile(confPath, []byte(conf), 0o600); err != nil {
		tb.Fatalf("write nginx.conf: %v", err)
	}

	logPath := filepath.Join(dir, "nginx.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		tb.Fatalf("create nginx log: %v", err)
	}
	cmd := exec.Command(bin, "-p", dir, "-c", confPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		tb.Skipf("start nginx: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	tb.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-exited:
		case <-time.After(2 * time.Second):
		}
		_ = logFile.Close()
	})
	waitExternalProxy(tb, "nginx", addr, 5*time.Second, exited, logPath)
	return addr
}

// startHAProxyTCP launches a real HAProxy process in L4 TCP mode forwarding to
// backend. It skips tb when haproxy is missing or cannot start.
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

	logPath := filepath.Join(dir, "haproxy.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		tb.Fatalf("create haproxy log: %v", err)
	}
	cmd := exec.Command(bin, "-f", confPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		tb.Skipf("start haproxy: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	tb.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-exited:
		case <-time.After(2 * time.Second):
		}
		_ = logFile.Close()
	})
	waitExternalProxy(tb, "haproxy", addr, 5*time.Second, exited, logPath)
	return addr
}

func TestNginxHasStaticStream(t *testing.T) {
	cases := []struct {
		out  string
		want bool
	}{
		{"--with-stream --with-http_ssl_module", true},
		{"--with-stream=dynamic --with-http_ssl_module", false},
		{"--with-http_ssl_module", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := nginxHasStaticStream(tc.out); got != tc.want {
			t.Errorf("nginxHasStaticStream(%q) = %v, want %v", tc.out, got, tc.want)
		}
	}
}

// Hand-assembled ABI v4 modules used by proxy tests and benchmarks. Keeping
// them in Go avoids a Rust toolchain in the hot-path test suite.

func v4ULEB(n uint32) []byte {
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

func v4SLEB(n int32) []byte {
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

func v4Section(id byte, payload []byte) []byte {
	out := []byte{id}
	out = append(out, v4ULEB(uint32(len(payload)))...)
	return append(out, payload...)
}

func v4Entry(body []byte) []byte {
	out := v4ULEB(uint32(len(body)))
	return append(out, body...)
}

// v4Module builds a minimal ABI v4 module exporting memory, on_event, wg_abi,
// wg_caps, wg_buf_ptr and wg_buf_len. on_event returns the supplied i64 and the
// module declares all L7 capabilities (header + opaque).
func v4Module(onEvent []byte) []byte {
	return v4ModuleCaps(onEvent, 0x47)
}

const v4FixtureOutPtr = 50000

func v4ModuleCaps(onEvent []byte, caps uint64) []byte {
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
		exports = append(exports, v4ULEB(uint32(len(name)))...)
		exports = append(exports, name...)
		exports = append(exports, 0x00)
		exports = append(exports, v4ULEB(idx)...)
	}
	exports = append(exports, 6, 'm', 'e', 'm', 'o', 'r', 'y', 0x02, 0x00)
	addExport("on_event", 0)
	addExport("wg_abi", 1)
	addExport("wg_caps", 2)
	addExport("wg_buf_ptr", 3)
	addExport("wg_buf_len", 4)
	addExport("wg_out_ptr", 5)
	addExport("wg_out_len", 6)

	intConst := func(op byte, v uint32) []byte {
		body := []byte{0x00, op}
		body = append(body, v4ULEB(v)...)
		return append(body, 0x0b)
	}
	i64Const := func(v uint64) []byte {
		body := []byte{0x00, 0x42}
		body = append(body, v4SLEB(int32(v))...)
		return append(body, 0x0b)
	}

	code := make([]byte, 1, len(onEvent)+64)
	code[0] = 7
	code = append(code, v4Entry(onEvent)...)
	code = append(code, v4Entry(intConst(0x41, 0x0400))...)
	code = append(code, v4Entry(i64Const(caps))...)
	code = append(code, v4Entry(intConst(0x41, 4096))...)
	code = append(code, v4Entry(intConst(0x41, 49152))...)
	code = append(code, v4Entry(intConst(0x41, v4FixtureOutPtr))...)
	code = append(code, v4Entry(intConst(0x41, 4096))...)

	out := make([]byte, 0, len(code)+len(exports)+64)
	out = append(out, 0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00)
	out = append(out, v4Section(0x01, types)...)
	out = append(out, v4Section(0x03, funcs)...)
	out = append(out, v4Section(0x05, memory)...)
	out = append(out, v4Section(0x07, exports)...)
	out = append(out, v4Section(0x0a, code)...)
	return out
}

// v4Verdict returns a v4 module whose on_event returns verdict.
func v4Verdict(verdict byte) []byte {
	return v4Module([]byte{0x00, 0x42, verdict, 0x0b})
}

// v4Infinite returns a v4 module whose on_event loops forever, used to prove
// the per-call timeout interrupts a hung guest.
func v4Infinite() []byte {
	return v4Module([]byte{0x00, 0x03, 0x40, 0x0c, 0x00, 0x0b, 0x42, 0x00, 0x0b})
}

// v4DenyX inspects the first byte of the envelope blob and denies when it is
// 'X'. It is used by once/stream parity tests.
func v4DenyX() []byte {
	body := []byte{
		0x00,
		0x20, 0x03, // local.get 3 (envelope ptr)
		0x28, 0x02, 0x1c, // i32.load offset=28 (blob section offset)
		0x20, 0x03, // local.get 3 (window base)
		0x6a,             // i32.add -> absolute blob address
		0x2d, 0x00, 0x00, // i32.load8_u offset=0
		0x41, 0xd8, 0x00, // i32.const 'X'
		0x46, // i32.eq
		0xad, // i64.extend_i32_u
		0x0b, // end
	}
	return v4Module(body)
}

// Hand-assembled ABI v4 L4 lifecycle module for proxy tests. It exports
// on_accept/on_close plus wg_abi/wg_caps and no memory or on_event.

func v4Lifecycle(accept byte) []byte {
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
		exports = append(exports, v4ULEB(uint32(len(name)))...)
		exports = append(exports, name...)
		exports = append(exports, 0x00)
		exports = append(exports, v4ULEB(idx)...)
	}
	addExport("on_accept", 0)
	addExport("wg_abi", 1)
	addExport("wg_caps", 2)
	addExport("on_close", 3)

	code := make([]byte, 1, 32)
	code[0] = 4
	code = append(code, v4Entry([]byte{0x00, 0x41, accept, 0x0b})...)
	abi := make([]byte, 0, 6)
	abi = append(abi, 0x00, 0x41)
	abi = append(abi, v4ULEB(0x0400)...)
	abi = append(abi, 0x0b)
	code = append(code, v4Entry(abi)...)
	caps := append([]byte{0x00, 0x42}, v4SLEB(int32(abi4.CapLifecycle))...)
	code = append(code, v4Entry(append(caps, 0x0b))...)
	code = append(code, v4Entry([]byte{0x00, 0x0b})...)

	out := make([]byte, 0, len(code)+len(exports)+64)
	out = append(out, 0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00)
	out = append(out, v4Section(0x01, types)...)
	out = append(out, v4Section(0x03, funcs)...)
	out = append(out, v4Section(0x07, exports)...)
	out = append(out, v4Section(0x0a, code)...)
	return out
}
