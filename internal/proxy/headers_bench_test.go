package proxy

import (
	"io"
	"testing"
	"time"

	"wasmgate/internal/config"
)

// benchmarkRequest is a valid request for the http_guard example: it requires
// Basic auth and a Host header. The benchmark measures the body path after the
// header decision, which is the path this feature is meant to keep fast.
const benchmarkRequest = "GET /bench HTTP/1.1\r\nHost: bench.test\r\nAuthorization: Basic dXNlcjpwYXNz\r\n\r\n"

// BenchmarkHeadersBody measures body relay throughput for the three plugin
// modes. once/stream use the ABI v3 http_guard module; headers uses an ABI v4
// allow module. The body path after the header decision is what this feature is
// meant to keep fast.
func BenchmarkHeadersBody(b *testing.B) {
	for _, mode := range []string{"", "headers", "stream"} {
		name := mode
		if name == "" {
			name = "once"
		}
		b.Run(name, func(b *testing.B) {
			benchBodyWithMode(b, mode)
		})
	}
}

func benchBodyWithMode(b *testing.B, mode string) {
	const bodyChunk = 64 << 10
	backend := startEcho(b)
	pluginCfg := config.PluginConfig{
		Wasm:           wasmOnDataAllow(),
		DataMode:       mode,
		Timeout:        time.Second,
		MaxHeaderBytes: 1024,
		InspectTimeout: time.Second,
	}
	if mode == config.DataModeHeaders {
		pluginCfg.Wasm = v4Verdict(0)
	}
	cfg := validated(b, edgeConfig([]string{backend}, func(lc *config.ListenerConfig) {
		lc.Plugin = pluginCfg
	}))
	addr := startServer(b, NewServer(testLogger(), NewRuntime(cfg), nil))

	conn := dial(b, addr)
	defer conn.Close()

	if _, err := io.WriteString(conn, benchmarkRequest); err != nil {
		b.Fatalf("write request: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	requestEcho := make([]byte, len(benchmarkRequest))
	if _, err := io.ReadFull(conn, requestEcho); err != nil {
		b.Fatalf("read request echo: %v", err)
	}
	_ = conn.SetReadDeadline(time.Time{})

	want := int64(b.N) * bodyChunk
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 64<<10)
		var got int64
		for got < want {
			n, err := conn.Read(buf)
			got += int64(n)
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()

	chunk := make([]byte, bodyChunk)
	b.SetBytes(bodyChunk)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := conn.Write(chunk); err != nil {
			b.Fatalf("write body: %v", err)
		}
	}
	if err := <-done; err != nil {
		b.Fatalf("read body echo: %v", err)
	}
	b.StopTimer()
}
