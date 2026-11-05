package config_test

import (
	"testing"
	"time"

	"wasmgate/internal/config"
)

// Tests for listener_config.go: the listener defaults and ListenerConfig.Validate.

func TestLoadAppliesDefaults(t *testing.T) {
	path := write(t, `
version: 1
listeners:
  - name: edge
    addr: "127.0.0.1:0"
    upstream_pool: web
pools:
  web:
    balance: round_robin
    servers: ["127.0.0.1:9000"]
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Listeners[0].DialTimeout; got != config.DefaultDialTimeout {
		t.Fatalf("dial timeout = %s, want %s", got, config.DefaultDialTimeout)
	}
	if got := cfg.Listeners[0].IdleTimeout; got != config.DefaultIdleTimeout {
		t.Fatalf("idle timeout = %s, want %s", got, config.DefaultIdleTimeout)
	}
}

func TestLoadRejectsMalformedListenerAddr(t *testing.T) {
	path := write(t, `
version: 1
listeners:
  - name: edge
    addr: "not-an-address"
    upstream_pool: web
pools:
  web:
    servers: ["127.0.0.1:9000"]
`)
	if _, err := config.Load(path); err == nil {
		t.Fatal("expected error for malformed listener addr, got nil")
	}
}

func TestLoadRejectsNegativeTimeout(t *testing.T) {
	path := write(t, `
version: 1
listeners:
  - name: edge
    addr: "127.0.0.1:0"
    upstream_pool: web
    dial_timeout: -5s
pools:
  web:
    servers: ["127.0.0.1:9000"]
`)
	if _, err := config.Load(path); err == nil {
		t.Fatal("expected error for negative dial_timeout, got nil")
	}
}

func TestLoadRejectsDuplicateListenerAddr(t *testing.T) {
	path := write(t, `
version: 1
listeners:
  - name: a
    addr: "127.0.0.1:8080"
    upstream_pool: web
  - name: b
    addr: "127.0.0.1:8080"
    upstream_pool: web
pools:
  web:
    servers: ["127.0.0.1:9000"]
`)
	if _, err := config.Load(path); err == nil {
		t.Fatal("expected error for duplicate listener addr, got nil")
	}
}

func TestListenerValidateFillsDefaults(t *testing.T) {
	l := config.ListenerConfig{
		Name:         "edge",
		Addr:         "127.0.0.1:0",
		UpstreamPool: "web",
	}
	if err := l.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if l.DialTimeout != config.DefaultDialTimeout {
		t.Fatalf("dial timeout = %s, want %s", l.DialTimeout, config.DefaultDialTimeout)
	}
	if l.IdleTimeout != config.DefaultIdleTimeout {
		t.Fatalf("idle timeout = %s, want %s", l.IdleTimeout, config.DefaultIdleTimeout)
	}
}

func TestListenerValidateRejects(t *testing.T) {
	valid := config.ListenerConfig{Name: "edge", Addr: "127.0.0.1:0", UpstreamPool: "web"}

	noName := valid
	noName.Name = ""
	noAddr := valid
	noAddr.Addr = ""
	noPool := valid
	noPool.UpstreamPool = ""
	badAddr := valid
	badAddr.Addr = "not-an-address"
	negDial := valid
	negDial.DialTimeout = -time.Second
	negIdle := valid
	negIdle.IdleTimeout = -time.Second
	negConns := valid
	negConns.MaxConns = -1
	negRate := valid
	negRate.RateLimit.Max = -1
	shortWindow := valid
	shortWindow.RateLimit = config.RateLimit{Max: 10, Window: time.Millisecond}

	cases := map[string]config.ListenerConfig{
		"missing name":     noName,
		"missing addr":     noAddr,
		"missing pool":     noPool,
		"malformed addr":   badAddr,
		"negative dial":    negDial,
		"negative idle":    negIdle,
		"negative maxconn": negConns,
		"negative rate":    negRate,
		"short window":     shortWindow,
	}
	for name, l := range cases {
		if err := l.Validate(); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

// TestListenerValidateAcceptsWildcardHost covers the address rule for listeners:
// unlike an upstream server, a listener may omit the host to bind every
// interface.
func TestListenerValidateAcceptsWildcardHost(t *testing.T) {
	l := config.ListenerConfig{Name: "edge", Addr: ":8080", UpstreamPool: "web"}
	if err := l.Validate(); err != nil {
		t.Fatalf("validate %q: %v", l.Addr, err)
	}
}

// TestListenerValidateRejectsBadAddr covers the address checks that do not need
// the whole document in view.
func TestListenerValidateRejectsBadAddr(t *testing.T) {
	cases := map[string]string{
		"missing port":      "127.0.0.1:",
		"non-numeric port":  "127.0.0.1:http",
		"port out of range": "127.0.0.1:70000",
		"not host:port":     "garbage",
	}
	for name, addr := range cases {
		l := config.ListenerConfig{Name: "edge", Addr: addr, UpstreamPool: "web"}
		if err := l.Validate(); err == nil {
			t.Errorf("%s (%q): expected error, got nil", name, addr)
		}
	}
}

func TestListenerValidateDefaultsRateWindow(t *testing.T) {
	l := config.ListenerConfig{
		Name:         "edge",
		Addr:         "127.0.0.1:0",
		UpstreamPool: "web",
		RateLimit:    config.RateLimit{Max: 50},
	}
	if err := l.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if l.RateLimit.Window != time.Second {
		t.Fatalf("rate_limit.window = %s, want 1s", l.RateLimit.Window)
	}
}
