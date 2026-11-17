package proxy

import (
	"bytes"
	"testing"

	"wasmgate/internal/config"
	"wasmgate/internal/plugin/abi4"
)

func TestContentLengthFraming(t *testing.T) {
	got, err := contentLength([]byte("POST / HTTP/1.1\r\nContent-Length: 12\r\n\r\n"))
	if err != nil || got != 12 {
		t.Fatalf("content length = (%d, %v)", got, err)
	}
	for _, block := range []string{
		"POST / HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n",
		"POST / HTTP/1.1\r\nContent-Length: 1\r\nContent-Length: 1\r\n\r\n",
		"POST / HTTP/1.1\r\nContent-Length: nope\r\n\r\n",
	} {
		if _, err := contentLength([]byte(block)); err == nil {
			t.Fatalf("invalid framing accepted: %q", block)
		}
	}
}

func TestBodyMutationRequiresSameLength(t *testing.T) {
	chunk := []byte("hello")
	if err := applyMutation(chunk, abi4.EventResult{
		MutationPresent: true,
		Mutation:        []byte("HELLO"),
	}); err != nil {
		t.Fatalf("same-length mutation: %v", err)
	}
	if !bytes.Equal(chunk, []byte("HELLO")) {
		t.Fatalf("chunk = %q", chunk)
	}
	if err := applyMutation(chunk, abi4.EventResult{
		MutationPresent: true,
		Mutation:        []byte("too-long"),
	}); err == nil {
		t.Fatal("length-changing mutation accepted")
	}
}

func TestBodyPolicyRequiresHeaders(t *testing.T) {
	for _, mode := range []string{config.DataModeOnce, config.DataModeStream, ""} {
		cfg := edgeConfig([]string{"127.0.0.1:1"}, func(lc *config.ListenerConfig) {
			lc.Plugin = config.PluginConfig{BodyPolicy: config.BodyPolicyRequest, DataMode: mode}
		})
		if err := cfg.Validate(); err == nil {
			t.Fatalf("body policy accepted with data_mode %q", mode)
		}
	}
}
