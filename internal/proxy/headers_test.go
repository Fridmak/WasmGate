package proxy

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"wasmgate/internal/config"
)

const validHTTPGuardRequest = "GET / HTTP/1.1\r\nHost: example.test\r\nAuthorization: Basic dXNlcjpwYXNz\r\n\r\n"

func TestProxyHeadersModeRejectsV4MissingCaps(t *testing.T) {
	backend := startEcho(t)
	cfg := validated(t, edgeConfig([]string{backend}, func(lc *config.ListenerConfig) {
		lc.Plugin = config.PluginConfig{
			Wasm:           v4ModuleCaps([]byte{0x00, 0x42, 0x00, 0x0b}, 1),
			DataMode:       config.DataModeHeaders,
			Timeout:        time.Second,
			MaxHeaderBytes: 1024,
			InspectTimeout: time.Second,
		}
	}))
	if _, err := NewRuntimeChecked(cfg); err == nil {
		t.Fatal("v4 module without header caps was accepted")
	}
}

func TestProxyHeadersModeResolvesV4Engine(t *testing.T) {
	backend := startEcho(t)
	cfg := validated(t, edgeConfig([]string{backend}, func(lc *config.ListenerConfig) {
		lc.Plugin = config.PluginConfig{
			Wasm:           v4Verdict(0),
			DataMode:       config.DataModeHeaders,
			Timeout:        time.Second,
			MaxHeaderBytes: 1024,
			InspectTimeout: time.Second,
		}
	}))
	rt, err := NewRuntimeChecked(cfg)
	if err != nil {
		t.Fatalf("NewRuntimeChecked: %v", err)
	}
	rl := rt.Load().listeners["edge"]
	if rl.pluginV4 == nil {
		t.Fatal("headers listener did not resolve an ABI v4 engine")
	}
}

func startHeadersProxy(t *testing.T, plugin config.PluginConfig) string {
	t.Helper()
	backend := startEcho(t)
	if plugin.Timeout == 0 {
		plugin.Timeout = time.Second
	}
	if plugin.MaxHeaderBytes == 0 {
		plugin.MaxHeaderBytes = 1024
	}
	if plugin.InspectTimeout == 0 {
		plugin.InspectTimeout = time.Second
	}
	plugin.DataMode = config.DataModeHeaders
	cfg := edgeConfig([]string{backend}, func(lc *config.ListenerConfig) {
		lc.Plugin = plugin
	})
	return startServer(t, NewServer(testLogger(), NewRuntime(validated(t, cfg)), nil))
}

func writeSplit(t *testing.T, conn net.Conn, parts ...string) {
	t.Helper()
	for _, part := range parts {
		if _, err := io.WriteString(conn, part); err != nil {
			t.Fatalf("write %q: %v", part, err)
		}
	}
}

func readAllEcho(t *testing.T, conn net.Conn, want int) string {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, want)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	return string(buf)
}

func expectClosed(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var buf [1]byte
	if _, err := conn.Read(buf[:]); err == nil {
		t.Fatal("connection was not closed")
	}
}

func TestProxyHeadersModeRejectsInertHeadersPlugin(t *testing.T) {
	backend := startEcho(t)
	for _, mode := range []string{"once", "stream"} {
		cfg := validated(t, edgeConfig([]string{backend}, func(lc *config.ListenerConfig) {
			lc.Plugin = config.PluginConfig{
				Wasm:     wasmOnHeadersDenyX(),
				DataMode: mode,
				Timeout:  time.Second,
			}
		}))
		if _, err := NewRuntimeChecked(cfg); err == nil {
			t.Fatalf("headers-only plugin was inert but accepted in %s mode", mode)
		}
	}
}

func TestProxyHeadersModeLoadsBodyGuardWithReadOnlyPlan(t *testing.T) {
	backend := startEcho(t)
	cfg := validated(t, edgeConfig([]string{backend}, func(lc *config.ListenerConfig) {
		lc.Plugin = config.PluginConfig{
			Path:           "../../plugins/examples/http_body_guard/http_body_guard.wasm",
			DataMode:       config.DataModeHeaders,
			Timeout:        time.Second,
			MaxHeaderBytes: 1024,
			InspectTimeout: time.Second,
		}
	}))
	rt, err := NewRuntimeChecked(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if rt.Load().listeners["edge"].pipeline.body {
		t.Fatal("guest capabilities enabled unrequested body inspection")
	}
}

func TestProxyHeadersModeRejectsV3OnHeaders(t *testing.T) {
	backend := startEcho(t)
	cfg := validated(t, edgeConfig([]string{backend}, func(lc *config.ListenerConfig) {
		lc.Plugin = config.PluginConfig{
			Wasm:           wasmOnHeadersDenyX(),
			DataMode:       config.DataModeHeaders,
			Timeout:        time.Second,
			MaxHeaderBytes: 1024,
			InspectTimeout: time.Second,
		}
	}))
	if _, err := NewRuntimeChecked(cfg); err == nil {
		t.Fatal("ABI v3 on_headers module was accepted in headers mode")
	}
}

func TestProxyHeadersModeCollectsSplitHeaders(t *testing.T) {
	addr := startHeadersProxy(t, config.PluginConfig{
		Wasm: v4Verdict(0),
	})
	conn := dial(t, addr)
	defer conn.Close()

	// The Authorization header is in the second write. The v4 engine must see
	// the complete block, including the final empty line.
	writeSplit(t, conn,
		"GET / HTTP/1.1\r\nHost: example.test\r\n",
		"Authorization: Basic dXNlcjpwYXNz\r\n\r\n",
	)
	if got := readAllEcho(t, conn, len(validHTTPGuardRequest)); got != validHTTPGuardRequest {
		t.Fatalf("echo = %q, want %q", got, validHTTPGuardRequest)
	}
}

func TestProxyHeadersModeKeepsBodyBytes(t *testing.T) {
	addr := startHeadersProxy(t, config.PluginConfig{
		Wasm: v4Verdict(0),
	})
	conn := dial(t, addr)
	defer conn.Close()

	body := "payload-with-binary-\x00\xff"
	request := validHTTPGuardRequest + body
	writeSplit(t, conn, validHTTPGuardRequest[:20], validHTTPGuardRequest[20:], body)
	if got := readAllEcho(t, conn, len(request)); got != request {
		t.Fatalf("echo = %q, want %q", got, request)
	}
}

func TestProxyHeadersModeDeniesBeforeDial(t *testing.T) {
	addr := startHeadersProxy(t, config.PluginConfig{
		Wasm: v4Verdict(1),
	})
	conn := dial(t, addr)
	defer conn.Close()
	writeSplit(t, conn, "XET / HTTP/1.1\r\nHost: example.test\r\n\r\n")
	expectClosed(t, conn)
}

func TestProxyHeadersModeRoutesPool(t *testing.T) {
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
				Wasm:           v4Verdict(2),
				DataMode:       config.DataModeHeaders,
				Timeout:        time.Second,
				MaxHeaderBytes: 1024,
				InspectTimeout: time.Second,
				RoutePools:     []string{"canary"},
			},
		}},
		Pools: map[string]config.PoolConfig{
			"default": {Servers: []string{defaultEcho}},
			"canary":  {Servers: []string{canary}},
		},
	})
	addr := startServer(t, NewServer(testLogger(), NewRuntime(cfg), nil))
	conn := dial(t, addr)
	defer conn.Close()
	writeSplit(t, conn, validHTTPGuardRequest)
	expectClosed(t, conn)
}

func TestProxyHeadersModeRejectsMalformedHeaders(t *testing.T) {
	addr := startHeadersProxy(t, config.PluginConfig{
		Wasm: v4Verdict(0),
	})
	conn := dial(t, addr)
	defer conn.Close()
	writeSplit(t, conn, "GET / HTTP/1.1\r\nBadHeader\r\n\r\n")
	expectClosed(t, conn)
}

func TestProxyHeadersModeRejectsTooManyHeaders(t *testing.T) {
	addr := startHeadersProxy(t, config.PluginConfig{
		Wasm:           v4Verdict(0),
		MaxHeaderBytes: 16 * 1024,
	})
	conn := dial(t, addr)
	defer conn.Close()
	block := "GET / HTTP/1.1\r\n"
	for i := 0; i < 513; i++ {
		block += "X: y\r\n"
	}
	block += "\r\n"
	writeSplit(t, conn, block)
	expectClosed(t, conn)
}

func TestProxyHeadersModeTimesOut(t *testing.T) {
	addr := startHeadersProxy(t, config.PluginConfig{
		Wasm:    v4Infinite(),
		Timeout: 20 * time.Millisecond,
	})
	conn := dial(t, addr)
	defer conn.Close()
	start := time.Now()
	writeSplit(t, conn, validHTTPGuardRequest)
	expectClosed(t, conn)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("v4 header timeout took %s", elapsed)
	}
}

func TestProxyHeadersModeRejectsOversizedBlock(t *testing.T) {
	addr := startHeadersProxy(t, config.PluginConfig{
		Wasm:           v4Verdict(0),
		MaxHeaderBytes: 32,
	})
	conn := dial(t, addr)
	defer conn.Close()
	block := "GET / HTTP/1.1\r\nHost: example.test\r\nX-Pad: " + string(bytes.Repeat([]byte("a"), 64)) + "\r\n\r\n"
	// The proxy may close the connection while the oversized block is still
	// being written, so a write error is an acceptable outcome here.
	_, _ = io.WriteString(conn, block)
	expectClosed(t, conn)
}

func TestProxyHeadersModeRejectsPartialAndHTTP2(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
	}{
		{"partial", "GET / HTTP/1.1\r\nHost: example.test\r\n"},
		{"http2", "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr := startHeadersProxy(t, config.PluginConfig{
				Wasm:           v4Verdict(0),
				InspectTimeout: 100 * time.Millisecond,
			})
			conn := dial(t, addr)
			defer conn.Close()
			writeSplit(t, conn, tc.data)
			expectClosed(t, conn)
		})
	}
}

func TestProxyHeadersBodyPolicyRequestContentLength(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != "hello" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(backend.Close)
	cfg := validated(t, edgeConfig([]string{backend.Listener.Addr().String()}, func(lc *config.ListenerConfig) {
		lc.Plugin = config.PluginConfig{Wasm: v4Verdict(0), DataMode: config.DataModeHeaders,
			BodyPolicy: config.BodyPolicyRequest, Timeout: time.Second}
	}))
	addr := startServer(t, NewServer(testLogger(), NewRuntime(cfg), nil))
	conn := dial(t, addr)
	defer conn.Close()
	request := "POST / HTTP/1.1\r\nHost: example.test\r\nContent-Length: 5\r\n\r\nhello"
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatalf("write: %v", err)
	}
	const status = "HTTP/1.1 204 No Content\r\n"
	if got := readAllEcho(t, conn, len(status)); got != status {
		t.Fatalf("status = %q, want %q", got, status)
	}
}

func TestProxyHeadersBodyPolicyResponseContentLength(t *testing.T) {
	backend := startBanner(t, "HTTP/1.1 200 OK\r\nContent-Length: 7\r\n\r\nX-body!")
	cfg := validated(t, edgeConfig([]string{backend}, func(lc *config.ListenerConfig) {
		lc.Plugin = config.PluginConfig{
			Wasm:           v4Verdict(0),
			DataMode:       config.DataModeHeaders,
			BodyPolicy:     config.BodyPolicyResponse,
			Timeout:        time.Second,
			MaxHeaderBytes: 1024,
			InspectTimeout: time.Second,
		}
	}))
	addr := startServer(t, NewServer(testLogger(), NewRuntime(cfg), nil))
	conn := dial(t, addr)
	defer conn.Close()
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: example.test\r\n\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	got := make([]byte, len("HTTP/1.1 200 OK\r\nContent-Length: 7\r\n\r\nX-body!"))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read response: %v", err)
	}
	if string(got) != "HTTP/1.1 200 OK\r\nContent-Length: 7\r\n\r\nX-body!" {
		t.Fatalf("response = %q", got)
	}
}

func TestProxyHeadersBodyPolicyRejectsChunkedFraming(t *testing.T) {
	backend := startEcho(t)
	cfg := validated(t, edgeConfig([]string{backend}, func(lc *config.ListenerConfig) {
		lc.Plugin = config.PluginConfig{
			Wasm:           v4Verdict(0),
			DataMode:       config.DataModeHeaders,
			BodyPolicy:     config.BodyPolicyRequest,
			Timeout:        time.Second,
			MaxHeaderBytes: 1024,
			InspectTimeout: time.Second,
		}
	}))
	addr := startServer(t, NewServer(testLogger(), NewRuntime(cfg), nil))
	conn := dial(t, addr)
	defer conn.Close()
	_, _ = io.WriteString(conn, "POST / HTTP/1.1\r\nHost: example.test\r\nTransfer-Encoding: chunked\r\n\r\n")
	expectClosed(t, conn)
}

func startCloseServer(tb testing.TB) string {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("listen close backend: %v", err)
	}
	tb.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return ln.Addr().String()
}
