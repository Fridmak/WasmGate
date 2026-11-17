package proxy

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"wasmgate/internal/config"
)

const validHTTPGuardRequest = "GET / HTTP/1.1\r\nHost: example.test\r\nAuthorization: Basic dXNlcjpwYXNz\r\n\r\n"

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

func TestProxyHeadersModeRejectsStateOnlyPlugin(t *testing.T) {
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
	if _, err := NewRuntimeChecked(cfg); err == nil {
		t.Fatal("headers mode accepted a state-only plugin")
	}
}

func TestProxyHeadersModeCollectsSplitHeaders(t *testing.T) {
	addr := startHeadersProxy(t, config.PluginConfig{
		Path: "../../plugins/examples/http_guard/http_guard.wasm",
	})
	conn := dial(t, addr)
	defer conn.Close()

	// The Authorization header is in the second write. once mode would expose
	// only the first part to the stateless plugin and reject the request.
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
		Path: "../../plugins/examples/http_guard/http_guard.wasm",
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
		Wasm: wasmOnHeadersDenyX(),
	})
	conn := dial(t, addr)
	defer conn.Close()
	writeSplit(t, conn, "XET / HTTP/1.1\r\nHost: example.test\r\n\r\n")
	expectClosed(t, conn)
}

func TestProxyHeadersModeRejectsOversizedBlock(t *testing.T) {
	addr := startHeadersProxy(t, config.PluginConfig{
		Wasm:           wasmOnHeadersDenyX(),
		MaxHeaderBytes: 32,
	})
	conn := dial(t, addr)
	defer conn.Close()
	writeSplit(t, conn, "GET / HTTP/1.1\r\nHost: example.test\r\nX-Pad: ", string(bytes.Repeat([]byte("a"), 64)), "\r\n\r\n")
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
				Wasm:           wasmOnHeadersDenyX(),
				InspectTimeout: 100 * time.Millisecond,
			})
			conn := dial(t, addr)
			defer conn.Close()
			writeSplit(t, conn, tc.data)
			expectClosed(t, conn)
		})
	}
}
