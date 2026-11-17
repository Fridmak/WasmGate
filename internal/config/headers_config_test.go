package config_test

import (
	"testing"
	"time"

	"wasmgate/internal/config"
)

func TestHeadersModeDefaults(t *testing.T) {
	l := config.ListenerConfig{
		Name:         "edge",
		Addr:         "127.0.0.1:0",
		UpstreamPool: "web",
		Plugin: config.PluginConfig{
			Wasm:     []byte{0x00},
			DataMode: config.DataModeHeaders,
			Timeout:  time.Second,
		},
	}
	if err := l.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if l.Plugin.MaxHeaderBytes != config.DefaultMaxHeaderBytes {
		t.Fatalf("max_header_bytes = %d, want %d", l.Plugin.MaxHeaderBytes, config.DefaultMaxHeaderBytes)
	}
	if l.Plugin.InspectTimeout != config.DefaultInspectTimeout {
		t.Fatalf("inspect_timeout = %s, want %s", l.Plugin.InspectTimeout, config.DefaultInspectTimeout)
	}
}

func TestHeadersModeRejects(t *testing.T) {
	valid := config.ListenerConfig{
		Name:         "edge",
		Addr:         "127.0.0.1:0",
		UpstreamPool: "web",
		Plugin: config.PluginConfig{
			Wasm:     []byte{0x00},
			DataMode: config.DataModeHeaders,
			Timeout:  time.Second,
		},
	}

	noPlugin := valid
	noPlugin.Plugin.Wasm = nil

	noTimeout := valid
	noTimeout.Plugin.Timeout = 0

	tooLarge := valid
	tooLarge.Plugin.MaxHeaderBytes = config.MaxHeaderBytesLimit + 1

	zeroBytes := valid
	zeroBytes.Plugin.MaxHeaderBytes = -1

	negTimeout := valid
	negTimeout.Plugin.InspectTimeout = -time.Second

	passthrough := valid
	passthrough.SNIRoutes = map[string]string{"example.com": "web"}

	badMode := valid
	badMode.Plugin.DataMode = "sometimes"

	tests := []struct {
		name string
		cfg  config.ListenerConfig
	}{
		{"no plugin", noPlugin},
		{"no timeout", noTimeout},
		{"too large", tooLarge},
		{"negative bytes", zeroBytes},
		{"negative inspect timeout", negTimeout},
		{"tls passthrough", passthrough},
		{"unknown mode", badMode},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cfg.Validate(); err == nil {
				t.Fatalf("Validate accepted %+v", tt.cfg)
			}
		})
	}
}

func TestHeadersModeAllowsSNIRoutesWithTLS(t *testing.T) {
	l := config.ListenerConfig{
		Name:         "edge",
		Addr:         "127.0.0.1:0",
		UpstreamPool: "web",
		SNIRoutes:    map[string]string{"example.com": "web"},
		TLS: config.TLSConfig{
			CertFile: "server.crt",
			KeyFile:  "server.key",
		},
		Plugin: config.PluginConfig{
			Wasm:     []byte{0x00},
			DataMode: config.DataModeHeaders,
			Timeout:  time.Second,
		},
	}
	if err := l.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}
