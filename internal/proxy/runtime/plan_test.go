package runtime

import (
	"net"
	"testing"
	"time"
	"wasmgate/internal/config"
	abi4 "wasmgate/internal/plugin/abi4/engine"
)

func TestV3L7ModulesRejected(t *testing.T) {
	backend := startEcho(t)
	for _, mode := range []string{"", config.DataModeOnce, config.DataModeStream, config.DataModeHeaders} {
		t.Run("mode="+mode, func(t *testing.T) {
			cfg := validated(t, edgeConfig([]string{backend}, func(lc *config.ListenerConfig) {
				lc.Plugin = config.PluginConfig{
					Wasm:           wasmOnHeadersDenyX(),
					DataMode:       mode,
					Timeout:        time.Second,
					MaxHeaderBytes: 1024,
					InspectTimeout: time.Second,
				}
			}))
			if _, err := NewChecked(cfg); err == nil {
				t.Fatal("ABI v3 L7 module was accepted")
			}
		})
	}
}

func TestV4L4ModuleAccepted(t *testing.T) {
	backend := startEcho(t)
	cfg := validated(t, edgeConfig([]string{backend}, func(lc *config.ListenerConfig) {
		lc.Plugin = config.PluginConfig{Wasm: v4Lifecycle(0), Timeout: time.Second}
	}))
	rt, err := NewChecked(cfg)
	if err != nil {
		t.Fatalf("NewChecked: %v", err)
	}
	rl := rt.Load().listeners["edge"]
	if rl.PluginV4 == nil {
		t.Fatal("v4 L4 listener did not resolve an engine")
	}
}

func TestV3L4ModuleRejected(t *testing.T) {
	backend := startEcho(t)
	cfg := validated(t, edgeConfig([]string{backend}, func(lc *config.ListenerConfig) {
		lc.Plugin = config.PluginConfig{Wasm: legacyOnAcceptModule(), Timeout: time.Second}
	}))
	if _, err := NewChecked(cfg); err == nil {
		t.Fatal("ABI v3 L4 module was accepted")
	}
}

func legacyOnAcceptModule() []byte {
	return []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
		0x01, 0x08, 0x01, 0x60, 0x03, 0x7e, 0x7e, 0x7f, 0x01, 0x7f,
		0x03, 0x02, 0x01, 0x00,
		0x07, 0x0d, 0x01, 0x09, 'o', 'n', '_', 'a', 'c', 'c', 'e', 'p', 't', 0x00, 0x00,
		0x0a, 0x06, 0x01, 0x04, 0x00, 0x41, 0x00, 0x0b,
	}
}

func TestV4OpaqueModesResolveEngine(t *testing.T) {
	backend := startEcho(t)
	for _, mode := range []string{config.DataModeOnce, config.DataModeStream} {
		t.Run(mode, func(t *testing.T) {
			var routePools []string
			if mode == config.DataModeOnce {
				routePools = []string{"web"}
			}
			cfg := validated(t, edgeConfig([]string{backend}, func(lc *config.ListenerConfig) {
				lc.Plugin = config.PluginConfig{
					Wasm:       v4Verdict(0),
					DataMode:   mode,
					Timeout:    time.Second,
					RoutePools: routePools,
				}
			}))
			rt, err := NewChecked(cfg)
			if err != nil {
				t.Fatalf("NewChecked: %v", err)
			}
			rl := rt.Load().listeners["edge"]
			if rl.PluginV4 == nil {
				t.Fatalf("mode %s did not resolve an engine", mode)
			}
		})
	}
}

func TestV4StreamRejectsRoutePools(t *testing.T) {
	backend := startEcho(t)
	cfg := edgeConfig([]string{backend}, func(lc *config.ListenerConfig) {
		lc.Plugin = config.PluginConfig{
			Wasm:       v4Verdict(0),
			DataMode:   config.DataModeStream,
			RoutePools: []string{"web"},
		}
	})
	if err := cfg.Validate(); err == nil {
		t.Fatal("stream mode accepted route_pools")
	}
}

// TestV4FeatureMatrix documents the production mapping from every data-plane
// mode to one ABI v4 plan. This is intentionally a snapshot-level test: the
// proxy must reject an unsupported combination before serving a connection.
func TestV4FeatureMatrix(t *testing.T) {
	backend := startEcho(t)
	for _, tc := range []struct {
		name, mode, body string
		plan             abi4.Plan
		pipeline         PluginMode
	}{
		{"lifecycle", "", "", abi4.InspectLifecycle, PluginLifecycle},
		{"once", config.DataModeOnce, "", abi4.InspectOpaqueStream, PluginOnce},
		{"stream", config.DataModeStream, "", abi4.InspectOpaqueStream, PluginStream},
		{"headers", config.DataModeHeaders, "", abi4.InspectRequestHeaders, PluginHeaders},
		{"request-body", config.DataModeHeaders, config.BodyPolicyRequest,
			abi4.InspectRequestHeaders | abi4.InspectRequestBody, PluginHeaders},
		{"response-body", config.DataModeHeaders, config.BodyPolicyResponse,
			abi4.InspectRequestHeaders | abi4.InspectResponseHeaders | abi4.InspectResponseBody, PluginHeaders},
		{"both-body", config.DataModeHeaders, config.BodyPolicyBoth,
			abi4.InspectRequestHeaders | abi4.InspectRequestBody | abi4.InspectResponseHeaders | abi4.InspectResponseBody, PluginHeaders},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validated(t, edgeConfig([]string{backend}, func(lc *config.ListenerConfig) {
				wasm := v4Verdict(0)
				if tc.pipeline == PluginLifecycle {
					wasm = v4Lifecycle(0)
				}
				lc.Plugin = config.PluginConfig{Wasm: wasm, DataMode: tc.mode, BodyPolicy: tc.body, Timeout: time.Second}
			}))
			rt, err := NewChecked(cfg)
			if err != nil {
				t.Fatal(err)
			}
			rl := rt.Load().listeners["edge"]
			if rl.PluginV4 == nil || rl.Pipeline.Mode != tc.pipeline || rl.PluginV4.Plan() != tc.plan {
				t.Fatalf("resolved mode=%v plan=%b, want mode=%v plan=%b", rl.Pipeline.Mode, rl.PluginV4.Plan(), tc.pipeline, tc.plan)
			}
		})
	}
}

var (
	sinkSnapshot *Snapshot
	sinkVersion  uint64
)

func edgeConfig(backends []string, mutate func(*config.ListenerConfig)) *config.Config {
	lc := config.ListenerConfig{
		Name: "edge", Addr: "127.0.0.1:0", UpstreamPool: "web", DialTimeout: time.Second,
	}
	if mutate != nil {
		mutate(&lc)
	}
	return &config.Config{
		Version: 1, Listeners: []config.ListenerConfig{lc},
		Pools: map[string]config.PoolConfig{"web": {Servers: backends}},
	}
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
			go func() {
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
			}()
		}
	}()
	return ln.Addr().String()
}

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

func v4Lifecycle(accept byte) []byte {
	types := []byte{0x04, 0x60, 0x03, 0x7e, 0x7e, 0x7f, 0x01, 0x7f, 0x60, 0x00, 0x01, 0x7f, 0x60, 0x00, 0x01, 0x7e, 0x60, 0x04, 0x7e, 0x7e, 0x7e, 0x7e, 0x00}
	funcs := []byte{0x04, 0x00, 0x01, 0x02, 0x03}
	exports := []byte{0x04}
	add := func(name string, idx uint32) {
		exports = append(exports, v4ULEB(uint32(len(name)))...)
		exports = append(exports, name...)
		exports = append(exports, 0x00)
		exports = append(exports, v4ULEB(idx)...)
	}
	add("on_accept", 0)
	add("wg_abi", 1)
	add("wg_caps", 2)
	add("on_close", 3)
	code := make([]byte, 1, 32)
	code[0] = 0x04
	code = append(code, v4Entry([]byte{0x00, 0x41, accept, 0x0b})...)
	code = append(code, v4Entry([]byte{0x00, 0x41, 0x80, 0x08, 0x0b})...)
	caps := append([]byte{0x00, 0x42}, v4SLEB(0x100)...)
	code = append(code, v4Entry(append(caps, 0x0b))...)
	code = append(code, v4Entry([]byte{0x00, 0x0b})...)
	out := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	out = append(out, v4Section(0x01, types)...)
	out = append(out, v4Section(0x03, funcs)...)
	out = append(out, v4Section(0x07, exports)...)
	return append(out, v4Section(0x0a, code)...)
}
