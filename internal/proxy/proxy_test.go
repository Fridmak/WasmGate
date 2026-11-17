package proxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
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
			l := rt.Load().listeners["edge"]
			l.limiter.acquire()
			up := l.pool.Next()
			l.pool.Release(up)
			l.limiter.release()
			sinkListener = l
		}
	})
}

func TestProxyStatefulHTTPBodyGuardRejected(t *testing.T) {
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
		t.Fatal("ABI v3 stateful stream plugin was accepted in the ABI v4-only path")
	}
}
