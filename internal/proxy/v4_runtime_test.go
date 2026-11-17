package proxy

import (
	"testing"
	"time"

	"wasmgate/internal/config"
	"wasmgate/internal/plugin/abi4"
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
			if _, err := NewRuntimeChecked(cfg); err == nil {
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
	rt, err := NewRuntimeChecked(cfg)
	if err != nil {
		t.Fatalf("NewRuntimeChecked: %v", err)
	}
	rl := rt.Load().listeners["edge"]
	if rl.pluginV4 == nil {
		t.Fatal("v4 L4 listener did not resolve an engine")
	}
}

func TestV3L4ModuleRejected(t *testing.T) {
	backend := startEcho(t)
	cfg := validated(t, edgeConfig([]string{backend}, func(lc *config.ListenerConfig) {
		lc.Plugin = config.PluginConfig{Wasm: legacyOnAcceptModule(), Timeout: time.Second}
	}))
	if _, err := NewRuntimeChecked(cfg); err == nil {
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
			rt, err := NewRuntimeChecked(cfg)
			if err != nil {
				t.Fatalf("NewRuntimeChecked: %v", err)
			}
			rl := rt.Load().listeners["edge"]
			if rl.pluginV4 == nil {
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
		pipeline         pluginMode
	}{
		{"lifecycle", "", "", abi4.InspectLifecycle, pluginLifecycle},
		{"once", config.DataModeOnce, "", abi4.InspectOpaqueStream, pluginOnce},
		{"stream", config.DataModeStream, "", abi4.InspectOpaqueStream, pluginStream},
		{"headers", config.DataModeHeaders, "", abi4.InspectRequestHeaders, pluginHeaders},
		{"request-body", config.DataModeHeaders, config.BodyPolicyRequest,
			abi4.InspectRequestHeaders | abi4.InspectRequestBody, pluginHeaders},
		{"response-body", config.DataModeHeaders, config.BodyPolicyResponse,
			abi4.InspectRequestHeaders | abi4.InspectResponseHeaders | abi4.InspectResponseBody, pluginHeaders},
		{"both-body", config.DataModeHeaders, config.BodyPolicyBoth,
			abi4.InspectRequestHeaders | abi4.InspectRequestBody | abi4.InspectResponseHeaders | abi4.InspectResponseBody, pluginHeaders},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validated(t, edgeConfig([]string{backend}, func(lc *config.ListenerConfig) {
				wasm := v4Verdict(0)
				if tc.pipeline == pluginLifecycle {
					wasm = v4Lifecycle(0)
				}
				lc.Plugin = config.PluginConfig{Wasm: wasm, DataMode: tc.mode, BodyPolicy: tc.body, Timeout: time.Second}
			}))
			rt, err := NewRuntimeChecked(cfg)
			if err != nil {
				t.Fatal(err)
			}
			rl := rt.Load().listeners["edge"]
			if rl.pluginV4 == nil || rl.pipeline.mode != tc.pipeline || rl.pluginV4.Plan() != tc.plan {
				t.Fatalf("resolved mode=%v plan=%b, want mode=%v plan=%b", rl.pipeline.mode, rl.pluginV4.Plan(), tc.pipeline, tc.plan)
			}
		})
	}
}
