package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"wasmgate/internal/config"
	"wasmgate/internal/observe"
	pruntime "wasmgate/internal/proxy/runtime"
)

// Tests for server.go: the Start / Reload / Shutdown lifecycle, followed by the
// benchmark that measures session establishment.

// TestServerMetricsCountConnectionLifecycle walks one connection through the
// whole metrics path: accepted on Accept(), relayed bytes counted once per
// finished direction (one byte each way here), active back to zero and closed
// counted after the handler returns.
func TestServerMetricsCountConnectionLifecycle(t *testing.T) {
	m := observe.NewMetrics()
	cfg := validated(t, edgeConfig([]string{startEcho(t)}, nil))
	addr := startServer(t, New(testLogger(), pruntime.New(cfg), m))

	conn := roundTrip(t, addr) // writes one byte, reads the echoed byte back
	_ = conn.Close()

	waitMetric(t, 2*time.Second, func() bool {
		s := m.Snapshot()
		return s.ConnectionsAccepted == 1 &&
			s.ConnectionsClosed == 1 &&
			s.ConnectionsActive == 0 &&
			s.RelayedBytes >= 2 // one byte in each direction
	})
	if s := m.Snapshot(); s.ConfigVersion != 1 {
		t.Fatalf("config_version = %d, want 1 after Start", s.ConfigVersion)
	}
}

// TestServerMetricsCountReload checks that a successful Reload bumps the reload
// counter and publishes the new version into the metrics.
func TestServerMetricsCountReload(t *testing.T) {
	m := observe.NewMetrics()
	srv := New(testLogger(), pruntime.New(validated(t, edgeConfig([]string{startEcho(t)}, nil))), m)
	addr := startServer(t, srv)

	v, err := srv.Reload(context.Background(), validated(t, edgeConfig([]string{startEcho(t)}, nil)))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	waitMetric(t, 2*time.Second, func() bool {
		s := m.Snapshot()
		return s.Reloads == 1 && s.ConfigVersion == v
	})

	c := roundTrip(t, addr) // still serving after the reload
	_ = c.Close()
}

// TestShutdownDrainsActiveConnection checks that graceful shutdown does NOT
// tear down an established connection: it must keep working while Shutdown
// waits, and Shutdown must return cleanly once the peer closes.
func TestShutdownDrainsActiveConnection(t *testing.T) {
	addr, srv := newTestServer(t, nil)
	conn := roundTrip(t, addr)

	shutErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutErr <- srv.Shutdown(ctx)
	}()

	// Give Shutdown a moment to close the listener and start draining.
	time.Sleep(100 * time.Millisecond)

	// The connection must still be proxied during the drain.
	if _, err := conn.Write([]byte("y")); err != nil {
		t.Fatalf("write during drain: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("connection was cut during graceful drain: %v", err)
	}

	_ = conn.Close()
	select {
	case err := <-shutErr:
		if err != nil {
			t.Fatalf("Shutdown returned error during clean drain: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return after the connection closed")
	}
}

// TestShutdownForceClosesAfterGrace checks that when the grace period expires
// with a connection still open, Shutdown force-closes it and reports the
// timeout.
func TestShutdownForceClosesAfterGrace(t *testing.T) {
	addr, srv := newTestServer(t, nil)
	conn := roundTrip(t, addr)
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := srv.Shutdown(ctx); err == nil {
		t.Fatal("Shutdown returned nil, want a deadline error after the grace expired")
	}

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("connection still open after the grace expired")
	}
}

// TestServerStartTwiceFails checks that Start is not idempotent: a second call
// must report an error instead of silently rebinding.
func TestServerStartTwiceFails(t *testing.T) {
	srv := New(testLogger(), pruntime.New(edgeConfig([]string{"127.0.0.1:9000"}, nil)), nil)
	startServer(t, srv)

	if err := srv.Start(); err == nil {
		t.Fatal("second Start returned nil, want an error")
	}
}

// TestReloadBeforeStartFails checks that reloading a server that never started
// is refused rather than half-applied.
func TestReloadBeforeStartFails(t *testing.T) {
	cfg := edgeConfig([]string{"127.0.0.1:9000"}, nil)
	srv := New(testLogger(), pruntime.New(cfg), nil)

	if _, err := srv.Reload(context.Background(), cfg); err == nil {
		t.Fatal("Reload before Start returned nil, want an error")
	}
}

// TestReloadRejectsInvalidConfig covers the validation gate: a bad config must
// be refused without disturbing the running listeners.
func TestReloadRejectsInvalidConfig(t *testing.T) {
	addr, srv := newTestServer(t, nil)

	bad := edgeConfig([]string{"127.0.0.1:9000"}, nil)
	bad.Listeners[0].Addr = "not-an-address"
	if _, err := srv.Reload(context.Background(), bad); err == nil {
		t.Fatal("Reload accepted an invalid config")
	}

	c := roundTrip(t, addr) // the old listener must still serve
	_ = c.Close()
}

// freeAddr returns an address that was just free, so a test can bind it and
// afterwards assert that the socket was released again.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen probe: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// assertAddrFree fails if addr cannot be bound, i.e. if an earlier listener on
// it was leaked instead of closed.
func assertAddrFree(t *testing.T, addr string) {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("address %s was not released: %v", addr, err)
	}
	_ = ln.Close()
}

// toppedListener returns a listener config on addr for pool "web".
func toppedListener(name, addr string) config.ListenerConfig {
	return config.ListenerConfig{Name: name, Addr: addr, UpstreamPool: "web", DialTimeout: time.Second}
}

// TestStartRollbackClosesBoundSockets covers the rollback in Start: when a later
// listener cannot be bound, the ones already bound by this Start must be closed
// again instead of leaking their ports.
func TestStartRollbackClosesBoundSockets(t *testing.T) {
	free := freeAddr(t)
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen taken: %v", err)
	}
	defer taken.Close()

	cfg := &config.Config{
		Version:   1,
		Listeners: []config.ListenerConfig{toppedListener("free", free), toppedListener("taken", taken.Addr().String())},
		Pools:     map[string]config.PoolConfig{"web": {Servers: []string{"127.0.0.1:9000"}}},
	}

	srv := New(testLogger(), pruntime.New(cfg), nil)
	if err := srv.Start(); err == nil {
		t.Fatalf("Start succeeded although %s was already in use", taken.Addr())
	}
	if got := len(srv.Addrs()); got != 0 {
		t.Fatalf("failed Start registered %d listeners, want 0", got)
	}
	assertAddrFree(t, free)
}

// TestPanicInHandlerIsContained covers the panic barrier: a handler that panics
// must close only its own connection and leave the proxy running, because a
// panicking connection goroutine would otherwise take down the whole process.
//
// The faulty handler is installed before Start, so no accept goroutine ever
// observes a concurrent write to the field.
func TestPanicInHandlerIsContained(t *testing.T) {
	srv := New(testLogger(), pruntime.New(edgeConfig([]string{"127.0.0.1:9000"}, nil)), nil)
	srv.proxy = nil // every connection now panics inside its handler goroutine
	addr := startServer(t, srv)

	conn := dial(t, addr)
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("panicking handler left the connection open")
	}
}

// TestReloadAfterShutdownFails checks that a reload racing the shutdown is
// refused instead of re-publishing a configuration on a closed server.
func TestReloadAfterShutdownFails(t *testing.T) {
	cfg := edgeConfig([]string{"127.0.0.1:9000"}, nil)
	srv := New(testLogger(), pruntime.New(cfg), nil)
	startServer(t, srv)

	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if _, err := srv.Reload(context.Background(), cfg); err == nil {
		t.Fatal("Reload after Shutdown returned nil, want an error")
	}
}

// TestReloadBindFailureKeepsOldListeners covers the rollback: when a listener in
// the new config cannot be bound, the sockets already bound by this reload are
// released, no snapshot is published, and the existing listeners keep working.
func TestReloadBindFailureKeepsOldListeners(t *testing.T) {
	addr, srv := newTestServer(t, nil)

	free := freeAddr(t)
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen taken: %v", err)
	}
	defer taken.Close()

	// "edge" is unchanged and kept; "fresh" binds during the reload; "taken"
	// then fails, so "fresh" must be rolled back.
	bad := edgeConfig([]string{"127.0.0.1:9000"}, nil)
	bad.Listeners = append(bad.Listeners,
		toppedListener("fresh", free),
		toppedListener("taken", taken.Addr().String()),
	)
	if _, err := srv.Reload(context.Background(), bad); err == nil {
		t.Fatal("Reload succeeded on an address already in use")
	}

	if got := len(srv.Addrs()); got != 1 {
		t.Fatalf("after failed reload: %d listeners, want 1", got)
	}
	assertAddrFree(t, free)
	c := roundTrip(t, addr) // the old listener must still serve
	_ = c.Close()
}

// TestReloadReconcilesListeners checks that a reload can add and remove
// listeners: a new address starts accepting, and a removed address is closed.
func TestReloadReconcilesListeners(t *testing.T) {
	echoAddr := startEcho(t)
	base := func(listeners ...config.ListenerConfig) *config.Config {
		return &config.Config{
			Version:   1,
			Listeners: listeners,
			Pools:     map[string]config.PoolConfig{"web": {Servers: []string{echoAddr}}},
		}
	}
	a := config.ListenerConfig{Name: "a", Addr: "127.0.0.1:0", UpstreamPool: "web", DialTimeout: time.Second}
	b := config.ListenerConfig{Name: "b", Addr: "127.0.0.1:0", UpstreamPool: "web", DialTimeout: time.Second}

	srv := New(testLogger(), pruntime.New(base(a)), nil)
	startServer(t, srv)

	if _, err := srv.Reload(context.Background(), base(a, b)); err != nil {
		t.Fatalf("reload add: %v", err)
	}
	addrs := srv.Addrs()
	if len(addrs) != 2 {
		t.Fatalf("after add: %d listeners, want 2", len(addrs))
	}
	for _, ad := range addrs {
		c := roundTrip(t, ad.String())
		_ = c.Close()
	}

	if _, err := srv.Reload(context.Background(), base(a)); err != nil {
		t.Fatalf("reload remove: %v", err)
	}
	if got := len(srv.Addrs()); got != 1 {
		t.Fatalf("after remove: %d listeners, want 1", got)
	}
}

// TestReloadRenameKeepsAddress covers a listener renamed without moving its
// address: the old socket is still held under the old name, so the reload has
// to release it before binding the new name.
func TestReloadRenameKeepsAddress(t *testing.T) {
	echoAddr := startEcho(t)
	cfgFor := func(name, addr string) *config.Config {
		return &config.Config{
			Version:   1,
			Listeners: []config.ListenerConfig{toppedListener(name, addr)},
			Pools:     map[string]config.PoolConfig{"web": {Servers: []string{echoAddr}}},
		}
	}
	addr := freeAddr(t)

	srv := New(testLogger(), pruntime.New(cfgFor("a", addr)), nil)
	startServer(t, srv)

	if _, err := srv.Reload(context.Background(), cfgFor("b", addr)); err != nil {
		t.Fatalf("rename on the same address: %v", err)
	}
	if got := len(srv.Addrs()); got != 1 {
		t.Fatalf("after rename: %d listeners, want 1", got)
	}
	c := roundTrip(t, addr)
	_ = c.Close()
}

// TestReloadRenameRollbackRestoresListener: the renamed listener is released
// and then the reload fails on another address. The old listener must be
// re-bound and keep serving.
func TestReloadRenameRollbackRestoresListener(t *testing.T) {
	echoAddr := startEcho(t)
	addr := freeAddr(t)
	cfg := &config.Config{
		Version:   1,
		Listeners: []config.ListenerConfig{toppedListener("a", addr)},
		Pools:     map[string]config.PoolConfig{"web": {Servers: []string{echoAddr}}},
	}
	srv := New(testLogger(), pruntime.New(cfg), nil)
	startServer(t, srv)

	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen taken: %v", err)
	}
	defer taken.Close()

	bad := &config.Config{
		Version: 1,
		Listeners: []config.ListenerConfig{
			toppedListener("b", addr),
			toppedListener("taken", taken.Addr().String()),
		},
		Pools: map[string]config.PoolConfig{"web": {Servers: []string{echoAddr}}},
	}
	if _, err := srv.Reload(context.Background(), bad); err == nil {
		t.Fatal("Reload succeeded on an address already in use")
	}
	if got := len(srv.Addrs()); got != 1 {
		t.Fatalf("after failed rename: %d listeners, want 1", got)
	}
	c := roundTrip(t, addr)
	_ = c.Close()
}

// TestReloadRejectsBrokenPlugin checks that a reload pointing at a missing or
// invalid .wasm module fails before touching the running listeners or bumping
// the snapshot version.
func TestReloadRejectsBrokenPlugin(t *testing.T) {
	addr, srv := newTestServer(t, nil)

	bad := edgeConfig([]string{"127.0.0.1:9000"}, func(lc *config.ListenerConfig) {
		lc.Plugin = config.PluginConfig{Path: "does-not-exist.wasm"}
	})
	if _, err := srv.Reload(context.Background(), bad); err == nil {
		t.Fatal("Reload succeeded with a non-existent plugin file")
	}
	c := roundTrip(t, addr)
	_ = c.Close()
}

// TestActiveHealthCheckExcludesDeadAndRecovers checks that an active TCP
// health check marks a dead backend unhealthy so traffic routes to the live
// backend, and marks it healthy again once it starts listening.
func TestActiveHealthCheckExcludesDeadAndRecovers(t *testing.T) {
	live := startEcho(t)
	deadAddr := freeAddr(t)

	cfg := &config.Config{
		Version: 1,
		Listeners: []config.ListenerConfig{{
			Name:         "edge",
			Addr:         "127.0.0.1:0",
			UpstreamPool: "web",
			DialTimeout:  500 * time.Millisecond,
		}},
		Pools: map[string]config.PoolConfig{
			"web": {
				Balance: "round_robin",
				Servers: []string{deadAddr, live},
				HealthCheck: config.HealthCheckConfig{
					Interval: 30 * time.Millisecond,
					Timeout:  100 * time.Millisecond,
				},
			},
		},
	}
	rt := pruntime.New(cfg)
	srv := New(testLogger(), rt, nil)
	proxyAddr := startServer(t, srv)

	deadUp := rt.Upstream(deadAddr)
	waitMetric(t, 2*time.Second, func() bool { return !deadUp.Healthy() })

	// Every connection now lands on the healthy backend instead of failing 50% of the time.
	for i := 0; i < 4; i++ {
		c := roundTrip(t, proxyAddr)
		_ = c.Close()
	}

	// Bring a listener up on deadAddr: the active probe must mark it healthy again.
	recovered, err := net.Listen("tcp", deadAddr)
	if err != nil {
		t.Fatalf("listen recovered: %v", err)
	}
	defer recovered.Close()
	waitMetric(t, 2*time.Second, func() bool { return deadUp.Healthy() })
}

// TestReloadUnderLoadZeroDroppedConnections is the chaos/load test for
// Hypothesis H2: while concurrent clients continuously open connections and
// stream payloads through the proxy, 12 configuration reloads swap pools,
// rate-limit thresholds and WASM plugins underneath them. Zero connections may
// be dropped or corrupted.
func TestReloadUnderLoadZeroDroppedConnections(t *testing.T) {
	echo1 := startEcho(t)
	echo2 := startEcho(t)
	listenAddr := freeAddr(t)

	// Minimal ABI v4 allow-all lifecycle module.
	wasmAllow := v4Lifecycle(0)

	cfgFor := func(step int) *config.Config {
		servers := []string{echo1, echo2}
		balance := "round_robin"
		if step%2 == 1 {
			balance = "least_conn"
		}
		var pl config.PluginConfig
		if step%3 == 0 {
			pl = config.PluginConfig{Wasm: wasmAllow}
		}
		return &config.Config{
			Version: 1,
			Listeners: []config.ListenerConfig{{
				Name:         "edge",
				Addr:         listenAddr,
				UpstreamPool: "web",
				MaxConns:     1000,
				DialTimeout:  2 * time.Second,
				RateLimit:    config.RateLimit{Max: 100000, Window: time.Second},
				Plugin:       pl,
			}},
			Pools: map[string]config.PoolConfig{
				"web": {Balance: balance, Servers: servers},
			},
		}
	}

	srv := New(testLogger(), pruntime.New(validated(t, cfgFor(0))), nil)
	addr := startServer(t, srv)

	// Keep 4 long-lived connections open across all 12 reloads AND hammer with
	// short-lived connections in parallel.
	longConns := make([]net.Conn, 4)
	for i := range longConns {
		longConns[i] = dial(t, addr)
		defer longConns[i].Close()
	}

	stop := make(chan struct{})
	errCh := make(chan error, 16)
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			msg := []byte("chaos-reload-payload")
			buf := make([]byte, len(msg))
			for {
				select {
				case <-stop:
					return
				default:
				}
				c, err := net.DialTimeout("tcp", addr, 2*time.Second)
				if err != nil {
					errCh <- fmt.Errorf("dial: %w", err)
					return
				}
				_ = c.SetDeadline(time.Now().Add(2 * time.Second))
				if _, err := c.Write(msg); err != nil {
					_ = c.Close()
					errCh <- fmt.Errorf("write: %w", err)
					return
				}
				if _, err := io.ReadFull(c, buf); err != nil {
					_ = c.Close()
					errCh <- fmt.Errorf("read: %w", err)
					return
				}
				_ = c.Close()
			}
		}()
	}

	for step := 1; step <= 12; step++ {
		if _, err := srv.Reload(context.Background(), cfgFor(step)); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("reload %d failed: %v", step, err)
		}
		// Verify every long-lived connection still relays bytes across the reload.
		for _, lc := range longConns {
			_ = lc.SetDeadline(time.Now().Add(2 * time.Second))
			if _, err := lc.Write([]byte("ping")); err != nil {
				close(stop)
				wg.Wait()
				t.Fatalf("long-lived write at reload %d: %v", step, err)
			}
			var pong [4]byte
			if _, err := io.ReadFull(lc, pong[:]); err != nil {
				close(stop)
				wg.Wait()
				t.Fatalf("long-lived read at reload %d: %v", step, err)
			}
		}
	}

	close(stop)
	wg.Wait()
	select {
	case err := <-errCh:
		t.Fatalf("dropped connection during reload under load: %v", err)
	default:
	}
}

// TestNoGoroutineLeakPerConnection relays a batch of connections through the
// real server and checks that the goroutine count comes back to its baseline.
//
// The design is goroutines-per-connection (two per relayed connection, plus one
// for the backend's echo), so the invariant is not "goroutines are cheap" but
// "they all go away again": one leaked goroutine per connection is invisible in
// every throughput number here and fatal in production. It spans the whole
// per-connection lifetime — accept loop -> Handle -> join -> teardown — which is
// why it lives with the server lifecycle rather than with one of those files.
//
// Both relay paths are covered, because they own their own goroutine pairs: the
// plain one (idle_timeout unset) and the buffered idle watchdog.
func TestNoGoroutineLeakPerConnection(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*config.ListenerConfig)
	}{
		{"plain", nil},
		{"idle_watchdog", func(lc *config.ListenerConfig) { lc.IdleTimeout = time.Minute }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr, _ := newTestServer(t, tc.mutate)

			// Warm up before taking the baseline: listeners, accept loops, the
			// poller and the pool buffers are set up lazily, so a count taken
			// before the first connection would be too low and fail for the
			// wrong reason.
			roundTrip(t, addr).Close()
			base := settledGoroutines(t)

			const conns = 25
			for i := 0; i < conns; i++ {
				roundTrip(t, addr).Close()
			}

			// The relay goroutines exit as soon as both directions see EOF, which
			// happens right after Close returns; give the scheduler a moment
			// anyway. What is asserted is that the count comes back at all, not
			// how fast.
			deadline := time.Now().Add(3 * time.Second)
			for {
				got := runtime.NumGoroutine()
				if got <= base {
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("goroutines grew from %d to %d over %d connections: something is not unwinding", base, got, conns)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

// settledGoroutines returns the goroutine count once it has stopped falling, so
// a straggler left by an earlier test cannot make the baseline too high.
func settledGoroutines(t *testing.T) int {
	t.Helper()
	prev := runtime.NumGoroutine()
	for i := 0; i < 50; i++ {
		time.Sleep(10 * time.Millisecond)
		cur := runtime.NumGoroutine()
		if cur >= prev {
			return prev
		}
		prev = cur
	}
	return prev
}

// BenchmarkConnectionSetup measures session establishment rate (connections per
// second) for a full handshake: client dial -> proxy accept -> upstream dial ->
// one echoed byte -> close. The "direct" baseline isolates the cost of the extra
// hop and the proxy's per-connection work from the platform's TCP teardown cost
// (which can dominate on Windows).
func BenchmarkConnectionSetup(b *testing.B) {
	backend := startEcho(b)
	addr := startProxy(b, backend, 0)
	fullAddr := startWasmGateFull(b, backend)

	b.Run("direct", func(b *testing.B) { benchSetup(b, backend) })
	b.Run("through_proxy", func(b *testing.B) { benchSetup(b, addr) })
	b.Run("wasmgate_full", func(b *testing.B) { benchSetup(b, fullAddr) })
	b.Run("nginx_stream", func(b *testing.B) { benchSetup(b, startNginxStream(b, backend)) })
	b.Run("haproxy_tcp", func(b *testing.B) { benchSetup(b, startHAProxyTCP(b, backend)) })
}

func benchSetup(b *testing.B, addr string) {
	probe := []byte{0}
	var back [1]byte
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			b.Fatalf("dial: %v", err)
		}
		if _, err := conn.Write(probe); err != nil {
			b.Fatalf("write: %v", err)
		}
		if _, err := io.ReadFull(conn, back[:]); err != nil {
			b.Fatalf("read: %v", err)
		}
		_ = conn.Close()
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func edgeConfig(backends []string, mutate func(*config.ListenerConfig)) *config.Config {
	lc := config.ListenerConfig{Name: "edge", Addr: "127.0.0.1:0", UpstreamPool: "web", DialTimeout: time.Second}
	if mutate != nil {
		mutate(&lc)
	}
	return &config.Config{Version: 1, Listeners: []config.ListenerConfig{lc}, Pools: map[string]config.PoolConfig{"web": {Servers: backends}}}
}

func validated(tb testing.TB, cfg *config.Config) *config.Config {
	tb.Helper()
	if err := cfg.Validate(); err != nil {
		tb.Fatalf("validate: %v", err)
	}
	return cfg
}

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
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()
	return ln.Addr().String()
}

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

func newTestServer(tb testing.TB, mutate func(*config.ListenerConfig)) (string, *Server) {
	tb.Helper()
	srv := New(testLogger(), pruntime.New(edgeConfig([]string{startEcho(tb)}, mutate)), nil)
	return startServer(tb, srv), srv
}

func waitMetric(tb testing.TB, deadline time.Duration, cond func() bool) {
	tb.Helper()
	end := time.Now().Add(deadline)
	for !cond() {
		if time.Now().After(end) {
			tb.Fatal("metrics did not settle in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func dial(tb testing.TB, addr string) net.Conn {
	tb.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		tb.Fatalf("dial %s: %v", addr, err)
	}
	return c
}

func roundTrip(tb testing.TB, addr string) net.Conn {
	tb.Helper()
	c := dial(tb, addr)
	if _, err := c.Write([]byte("x")); err != nil {
		tb.Fatalf("write: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	var b [1]byte
	if _, err := io.ReadFull(c, b[:]); err != nil {
		tb.Fatalf("read echo: %v", err)
	}
	return c
}

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

func v4Lifecycle(accept byte) []byte {
	types := []byte{0x04, 0x60, 0x03, 0x7e, 0x7e, 0x7f, 0x01, 0x7f, 0x60, 0x00, 0x01, 0x7f, 0x60, 0x00, 0x01, 0x7e, 0x60, 0x04, 0x7e, 0x7e, 0x7e, 0x7e, 0x00}
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
	code[0] = 0x04
	code = append(code, v4Entry([]byte{0x00, 0x41, accept, 0x0b})...)
	abi := make([]byte, 2, 8)
	abi[0], abi[1] = 0x00, 0x41
	abi = append(abi, v4ULEB(0x0400)...)
	abi = append(abi, 0x0b)
	code = append(code, v4Entry(abi)...)
	caps := append([]byte{0x00, 0x42}, v4SLEB(0x100)...)
	code = append(code, v4Entry(append(caps, 0x0b))...)
	code = append(code, v4Entry([]byte{0x00, 0x0b})...)
	out := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	out = append(out, v4Section(0x01, types)...)
	out = append(out, v4Section(0x03, funcs)...)
	out = append(out, v4Section(0x07, exports)...)
	return append(out, v4Section(0x0a, code)...)
}

func startProxy(tb testing.TB, backend string, idle time.Duration) string {
	tb.Helper()
	cfg := validated(tb, edgeConfig([]string{backend}, func(lc *config.ListenerConfig) {
		lc.IdleTimeout = idle
	}))
	return startServer(tb, New(testLogger(), pruntime.New(cfg), nil))
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
	return startServer(tb, New(testLogger(), pruntime.New(cfg), nil))
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
