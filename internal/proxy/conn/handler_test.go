package conn

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	"wasmgate/internal/config"
	"wasmgate/internal/proxy/backend"
	pruntime "wasmgate/internal/proxy/runtime"
)

type testSource struct{ upstreams map[string]*backend.Upstream }

func (s *testSource) Upstream(addr string) *backend.Upstream {
	if s.upstreams == nil {
		s.upstreams = make(map[string]*backend.Upstream)
	}
	if u := s.upstreams[addr]; u != nil {
		return u
	}
	u := backend.NewUpstream(addr)
	s.upstreams[addr] = u
	return u
}

type peerConn struct {
	net.Conn
	remote net.Addr
}

func (c peerConn) RemoteAddr() net.Addr { return c.remote }

func testConfig(mutate func(*config.ListenerConfig)) *config.Config {
	lc := config.ListenerConfig{Name: "edge", Addr: "127.0.0.1:0", UpstreamPool: "web", DialTimeout: 2 * time.Second}
	if mutate != nil {
		mutate(&lc)
	}
	return &config.Config{Version: 1, Listeners: []config.ListenerConfig{lc}, Pools: map[string]config.PoolConfig{"web": {Servers: []string{"127.0.0.1:1"}}}}
}

func handleAndObserveClose(t *testing.T, h *Handler, listener string) {
	t.Helper()
	client, peer := net.Pipe()
	t.Cleanup(func() { _ = peer.Close() })
	done := make(chan struct{})
	go func() {
		h.Handle(context.Background(), listener, peerConn{Conn: client, remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 1234}})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not return")
	}
}

func TestHandlerMissingListenerClosesClient(t *testing.T) {
	rt := pruntime.New(testConfig(nil))
	handleAndObserveClose(t, New(slog.Default(), rt, nil), "missing")
}

func TestHandlerMissingPoolClosesClient(t *testing.T) {
	cfg := testConfig(nil)
	cfg.Listeners[0].UpstreamPool = "missing"
	handleAndObserveClose(t, New(slog.Default(), pruntime.New(cfg), nil), "edge")
}

func TestHandlerRejectsMaxConnsBeforeDial(t *testing.T) {
	rt := pruntime.New(testConfig(func(lc *config.ListenerConfig) { lc.MaxConns = 1 }))
	rl, ok := rt.Load().Listener("edge")
	if !ok || rl.Limiter == nil || !rl.Limiter.Acquire() {
		t.Fatal("failed to reserve the only connection slot")
	}
	defer rl.Limiter.Release()
	handleAndObserveClose(t, New(slog.Default(), rt, nil), "edge")
}

func TestHandlerRejectsRateAndConnGuard(t *testing.T) {
	t.Run("rate", func(t *testing.T) {
		rt := pruntime.New(testConfig(func(lc *config.ListenerConfig) {
			lc.RateLimit = config.RateLimit{Max: 1, Window: time.Minute}
		}))
		rl, _ := rt.Load().Listener("edge")
		ip := net.ParseIP("192.0.2.10")
		if !rl.RateLimiter.Allow(ip, time.Now()) {
			t.Fatal("initial rate-limit reservation failed")
		}
		handleAndObserveClose(t, New(slog.Default(), rt, nil), "edge")
	})
	t.Run("concurrency", func(t *testing.T) {
		rt := pruntime.New(testConfig(func(lc *config.ListenerConfig) {
			lc.RateLimit = config.RateLimit{MaxConcurrent: 1}
		}))
		rl, _ := rt.Load().Listener("edge")
		ip := net.ParseIP("192.0.2.10")
		if !rl.ConnGuard.Acquire(ip) {
			t.Fatal("initial concurrency reservation failed")
		}
		defer rl.ConnGuard.Release(ip)
		handleAndObserveClose(t, New(slog.Default(), rt, nil), "edge")
	})
}

func TestSelectRoutePoolUsesVerdictIndex(t *testing.T) {
	source := &testSource{}
	first := backend.NewPool("first", []string{"127.0.0.1:1"}, "round_robin", source)
	second := backend.NewPool("second", []string{"127.0.0.1:2"}, "round_robin", source)
	rl := pruntime.ResolvedListener{RoutePools: []*backend.Pool{first, second}}
	if got, ok := selectRoutePool(rl, 3); !ok || got != second {
		t.Fatalf("route verdict 3 = (%v, %v), want second pool", got, ok)
	}
}

func TestSelectRoutePoolRejectsInvalidVerdict(t *testing.T) {
	source := &testSource{}
	pool := backend.NewPool("first", []string{"127.0.0.1:1"}, "round_robin", source)
	if got, ok := selectRoutePool(pruntime.ResolvedListener{RoutePools: []*backend.Pool{pool}}, 4); ok || got != nil {
		t.Fatalf("invalid route = (%v, %v), want (nil, false)", got, ok)
	}
}

func TestLocalDialErrorDoesNotOpenCircuitBreaker(t *testing.T) {
	source := &testSource{}
	u := source.Upstream("127.0.0.1:1")
	u.ConfigureBreaker(config.CircuitBreakerConfig{ErrorThreshold: 1})
	pool := backend.NewPool("web", []string{"127.0.0.1:1"}, "round_robin", source)
	h := New(slog.Default(), pruntime.New(testConfig(nil)), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, ok := h.dialWithRetry(ctx, "edge", pool, time.Second); ok {
		t.Fatal("cancelled dial unexpectedly succeeded")
	}
	if !localDialError(context.Background(), syscall.EMFILE) {
		t.Fatal("EMFILE was not classified as a local dial error")
	}
	if !u.IsHealthy() {
		t.Fatal("local dial error changed upstream health")
	}
}

func TestHandlerRelaysBytesToBackend(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	cfg := &config.Config{Version: 1, Listeners: []config.ListenerConfig{{Name: "edge", UpstreamPool: "web", DialTimeout: time.Second}}, Pools: map[string]config.PoolConfig{"web": {Servers: []string{ln.Addr().String()}}}}
	h := New(slog.Default(), pruntime.New(cfg), nil)
	handlerSide, clientSide := net.Pipe()
	done := make(chan struct{})
	go func() {
		h.Handle(context.Background(), "edge", peerConn{Conn: handlerSide, remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 3333}})
		close(done)
	}()
	if _, err := clientSide.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(clientSide, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("echo = %q, err=%v", buf, err)
	}
	_ = clientSide.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not finish after client close")
	}
}

func TestHandlerOpaqueModesAllowAndRelay(t *testing.T) {
	for _, mode := range []string{config.DataModeOnce, config.DataModeStream, config.DataModeHeaders} {
		t.Run(mode, func(t *testing.T) {
			backendAddr := startConnEcho(t)
			cfg := testConfig(func(lc *config.ListenerConfig) {
				lc.Plugin = config.PluginConfig{Wasm: connV4Verdict(0), DataMode: mode, Timeout: time.Second, MaxHeaderBytes: 4096, InspectTimeout: time.Second}
			})
			cfg.Pools["web"] = config.PoolConfig{Servers: []string{backendAddr}}
			rt, err := pruntime.NewChecked(cfg)
			if err != nil {
				t.Fatal(err)
			}
			h := New(slog.Default(), rt, nil)
			handlerSide, clientSide := net.Pipe()
			done := make(chan struct{})
			go func() {
				h.Handle(context.Background(), "edge", peerConn{Conn: handlerSide, remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.31"), Port: 3334}})
				close(done)
			}()
			want := []byte("hello")
			if mode == config.DataModeHeaders {
				want = []byte("GET / HTTP/1.1\r\nHost: test\r\n\r\n")
			}
			if _, err := clientSide.Write(want); err != nil {
				t.Fatal(err)
			}
			got := make([]byte, len(want))
			if _, err := io.ReadFull(clientSide, got); err != nil || string(got) != string(want) {
				t.Fatalf("relay = %q, err=%v", got, err)
			}
			_ = clientSide.Close()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("handler did not finish")
			}
		})
	}
}

func TestHandlerOnAcceptDenyBeforeDial(t *testing.T) {
	cfg := testConfig(func(lc *config.ListenerConfig) {
		lc.Plugin = config.PluginConfig{Wasm: connV4Lifecycle(1), Timeout: time.Second}
	})
	rt := pruntime.New(cfg)
	handleAndObserveClose(t, New(slog.Default(), rt, nil), "edge")
	if !rt.Upstream("127.0.0.1:1").IsHealthy() {
		t.Fatal("on_accept deny attempted an upstream dial")
	}
}

func TestHandlerOnCloseRunsAfterDialFailure(t *testing.T) {
	cfg := testConfig(func(lc *config.ListenerConfig) {
		lc.Plugin = config.PluginConfig{Wasm: connV4Lifecycle(0), Timeout: time.Second}
	})
	rt := pruntime.New(cfg)
	handleAndObserveClose(t, New(slog.Default(), rt, nil), "edge")
}

func TestHandlerRejectsPluginVerdictsBeforeDial(t *testing.T) {
	for _, tc := range []struct {
		name, mode string
		payload    []byte
	}{
		{name: "once", mode: config.DataModeOnce, payload: []byte("x")},
		{name: "stream", mode: config.DataModeStream, payload: []byte("x")},
		{name: "headers", mode: config.DataModeHeaders, payload: []byte("GET / HTTP/1.1\r\nHost: test\r\n\r\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(func(lc *config.ListenerConfig) {
				lc.Plugin = config.PluginConfig{Wasm: connV4Verdict(1), DataMode: tc.mode, Timeout: time.Second, MaxHeaderBytes: 4096, InspectTimeout: time.Second}
			})
			invokeHandlerPayload(t, New(slog.Default(), pruntime.New(cfg), nil), tc.payload)
		})
	}
}

func invokeHandlerPayload(t *testing.T, h *Handler, payload []byte) {
	t.Helper()
	handlerSide, clientSide := net.Pipe()
	done := make(chan struct{})
	go func() {
		h.Handle(context.Background(), "edge", peerConn{Conn: handlerSide, remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.41"), Port: 4041}})
		close(done)
	}()
	go func() { _, _ = clientSide.Write(payload) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not reject the plugin verdict")
	}
	_ = clientSide.Close()
}

func TestHandlerTLSTermination(t *testing.T) {
	certPEM, keyPEM := connTestCertificate(t)
	backend := startConnEcho(t)
	cfg := testConfig(func(lc *config.ListenerConfig) {
		lc.TLS = config.TLSConfig{CertPEM: certPEM, KeyPEM: keyPEM, HandshakeTimeout: time.Second}
	})
	cfg.Pools["web"] = config.PoolConfig{Servers: []string{backend}}
	h := New(slog.Default(), pruntime.New(cfg), nil)
	handlerSide, clientSide := net.Pipe()
	done := make(chan struct{})
	go func() {
		h.Handle(context.Background(), "edge", peerConn{Conn: handlerSide, remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.32"), Port: 3335}})
		close(done)
	}()
	tlsClient := tls.Client(clientSide, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}) // #nosec G402 -- test certificate
	if err := tlsClient.Handshake(); err != nil {
		t.Fatal(err)
	}
	if _, err := tlsClient.Write([]byte("tls")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 3)
	if _, err := io.ReadFull(tlsClient, got); err != nil || string(got) != "tls" {
		t.Fatalf("TLS relay = %q, err=%v", got, err)
	}
	_ = tlsClient.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("TLS handler did not finish")
	}
}

func TestHandlerHTTPBodyExchange(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 4096)
		_, _ = c.Read(buf)
		_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
	}()
	cfg := testConfig(func(lc *config.ListenerConfig) {
		lc.Plugin = config.PluginConfig{Wasm: connV4Verdict(0), DataMode: config.DataModeHeaders, BodyPolicy: config.BodyPolicyRequest, Timeout: time.Second, MaxHeaderBytes: 4096, InspectTimeout: time.Second}
	})
	cfg.Pools["web"] = config.PoolConfig{Servers: []string{ln.Addr().String()}}
	h := New(slog.Default(), pruntime.New(cfg), nil)
	handlerSide, clientSide := net.Pipe()
	done := make(chan struct{})
	go func() {
		h.Handle(context.Background(), "edge", peerConn{Conn: handlerSide, remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.33"), Port: 3336}})
		close(done)
	}()
	request := "POST / HTTP/1.1\r\nHost: test\r\nContent-Length: 5\r\n\r\nhello"
	if _, err := io.WriteString(clientSide, request); err != nil {
		t.Fatal(err)
	}
	_ = clientSide.SetReadDeadline(time.Now().Add(2 * time.Second))
	response, err := io.ReadAll(clientSide)
	if err != nil {
		t.Fatalf("HTTP response read: %v", err)
	}
	if len(response) > 0 && !strings.Contains(string(response), "HTTP/1.1 200 OK") {
		t.Fatalf("unexpected HTTP response = %q", response)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP handler did not finish")
	}
}

type connStringAddr string

func (a connStringAddr) Network() string { return "tcp" }
func (a connStringAddr) String() string  { return string(a) }

func TestClientEndpointFallback(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	gotIP, gotPort := clientEndpoint(peerConn{Conn: a, remote: connStringAddr("192.0.2.40:4040")})
	if gotPort != 4040 || gotIP.String() != "192.0.2.40" {
		t.Fatalf("clientEndpoint = (%v, %d)", gotIP, gotPort)
	}
	if ip, port := clientEndpoint(peerConn{Conn: a, remote: connStringAddr("not-an-endpoint")}); ip != nil || port != 0 {
		t.Fatalf("invalid clientEndpoint = (%v, %d)", ip, port)
	}
}

func connTestCertificate(t *testing.T) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial := big.NewInt(1)
	tmpl := &x509.Certificate{SerialNumber: serial, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, Subject: pkix.Name{CommonName: "test"}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func startConnEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { defer c.Close(); _, _ = io.Copy(c, c) }(c)
		}
	}()
	return ln.Addr().String()
}

func connV4ULEB(n uint32) []byte {
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

func connV4SLEB(n int32) []byte {
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

func connV4Section(id byte, payload []byte) []byte {
	out := []byte{id}
	out = append(out, connV4ULEB(uint32(len(payload)))...)
	return append(out, payload...)
}

func connV4Entry(body []byte) []byte {
	out := connV4ULEB(uint32(len(body)))
	return append(out, body...)
}

func connV4Module(onEvent []byte) []byte {
	types := []byte{0x03, 0x60, 0x05, 0x7e, 0x7f, 0x7f, 0x7f, 0x7f, 0x01, 0x7e, 0x60, 0x00, 0x01, 0x7f, 0x60, 0x00, 0x01, 0x7e}
	funcs := []byte{0x07, 0x00, 0x01, 0x02, 0x01, 0x01, 0x01, 0x01}
	memory := []byte{0x01, 0x00, 0x01}
	exports := []byte{0x08, 0x06, 'm', 'e', 'm', 'o', 'r', 'y', 0x02, 0x00}
	for i, name := range []string{"on_event", "wg_abi", "wg_caps", "wg_buf_ptr", "wg_buf_len", "wg_out_ptr", "wg_out_len"} {
		exports = append(exports, connV4ULEB(uint32(len(name)))...)
		exports = append(exports, name...)
		exports = append(exports, 0x00)
		exports = append(exports, connV4ULEB(uint32(i))...)
	}
	const outPtr = 50000
	intConst := func(op byte, v uint32) []byte { return append(append([]byte{0x00, op}, connV4ULEB(v)...), 0x0b) }
	i64Const := func(v uint64) []byte { return append(append([]byte{0x00, 0x42}, connV4SLEB(int32(v))...), 0x0b) }
	code := make([]byte, 1, len(onEvent)+64)
	code[0] = 0x07
	code = append(code, connV4Entry(onEvent)...)
	code = append(code, connV4Entry(intConst(0x41, 0x0400))...)
	code = append(code, connV4Entry(i64Const(0x47))...)
	code = append(code, connV4Entry(intConst(0x41, 4096))...)
	code = append(code, connV4Entry(intConst(0x41, 49152))...)
	code = append(code, connV4Entry(intConst(0x41, outPtr))...)
	code = append(code, connV4Entry(intConst(0x41, 4096))...)
	out := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	out = append(out, connV4Section(0x01, types)...)
	out = append(out, connV4Section(0x03, funcs)...)
	out = append(out, connV4Section(0x05, memory)...)
	out = append(out, connV4Section(0x07, exports)...)
	return append(out, connV4Section(0x0a, code)...)
}

func connV4Verdict(verdict byte) []byte { return connV4Module([]byte{0x00, 0x42, verdict, 0x0b}) }

func connV4Lifecycle(accept byte) []byte {
	types := []byte{0x04, 0x60, 0x03, 0x7e, 0x7e, 0x7f, 0x01, 0x7f, 0x60, 0x00, 0x01, 0x7f, 0x60, 0x00, 0x01, 0x7e, 0x60, 0x04, 0x7e, 0x7e, 0x7e, 0x7e, 0x00}
	funcs := []byte{0x04, 0x00, 0x01, 0x02, 0x03}
	exports := []byte{0x04}
	for i, name := range []string{"on_accept", "wg_abi", "wg_caps", "on_close"} {
		exports = append(exports, connV4ULEB(uint32(len(name)))...)
		exports = append(exports, name...)
		exports = append(exports, 0x00)
		exports = append(exports, connV4ULEB(uint32(i))...)
	}
	code := make([]byte, 1, 32)
	code[0] = 0x04
	code = append(code, connV4Entry([]byte{0x00, 0x41, accept, 0x0b})...)
	code = append(code, connV4Entry([]byte{0x00, 0x41, 0x80, 0x08, 0x0b})...)
	caps := append([]byte{0x00, 0x42}, connV4SLEB(0x100)...)
	caps = append(caps, 0x0b)
	code = append(code, connV4Entry(caps)...)
	code = append(code, connV4Entry([]byte{0x00, 0x0b})...)
	out := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	out = append(out, connV4Section(0x01, types)...)
	out = append(out, connV4Section(0x03, funcs)...)
	out = append(out, connV4Section(0x07, exports)...)
	return append(out, connV4Section(0x0a, code)...)
}
