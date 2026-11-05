package proxy

import (
	"context"
	"io"
	"net"
	"runtime"
	"testing"
	"time"

	"wasmgate/internal/config"
	"wasmgate/internal/observe"
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
	addr := startServer(t, NewServer(testLogger(), NewRuntime(cfg), m))

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
	srv := NewServer(testLogger(), NewRuntime(validated(t, edgeConfig([]string{startEcho(t)}, nil))), m)
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
	srv := NewServer(testLogger(), NewRuntime(edgeConfig([]string{"127.0.0.1:9000"}, nil)), nil)
	startServer(t, srv)

	if err := srv.Start(); err == nil {
		t.Fatal("second Start returned nil, want an error")
	}
}

// TestReloadBeforeStartFails checks that reloading a server that never started
// is refused rather than half-applied.
func TestReloadBeforeStartFails(t *testing.T) {
	cfg := edgeConfig([]string{"127.0.0.1:9000"}, nil)
	srv := NewServer(testLogger(), NewRuntime(cfg), nil)

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

	srv := NewServer(testLogger(), NewRuntime(cfg), nil)
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
	srv := NewServer(testLogger(), NewRuntime(edgeConfig([]string{"127.0.0.1:9000"}, nil)), nil)
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
	srv := NewServer(testLogger(), NewRuntime(cfg), nil)
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

	srv := NewServer(testLogger(), NewRuntime(base(a)), nil)
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

	srv := NewServer(testLogger(), NewRuntime(cfgFor("a", addr)), nil)
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
	srv := NewServer(testLogger(), NewRuntime(cfg), nil)
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

	b.Run("direct", func(b *testing.B) { benchSetup(b, backend) })
	b.Run("through_proxy", func(b *testing.B) { benchSetup(b, addr) })
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
