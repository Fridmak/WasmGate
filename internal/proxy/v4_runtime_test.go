package proxy

import (
	"testing"
	"time"

	"wasmgate/internal/config"
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
