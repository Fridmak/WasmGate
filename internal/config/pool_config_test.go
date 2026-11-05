package config_test

import (
	"testing"
	"time"

	"wasmgate/internal/config"
)

// Tests for pool_config.go: PoolConfig.Validate.

func TestLoadRejectsMalformedServerAddr(t *testing.T) {
	path := write(t, `
version: 1
listeners:
  - name: edge
    addr: "127.0.0.1:0"
    upstream_pool: web
pools:
  web:
    servers: ["garbage"]
`)
	if _, err := config.Load(path); err == nil {
		t.Fatal("expected error for malformed server addr, got nil")
	}
}

func TestLoadRejectsDuplicateServers(t *testing.T) {
	path := write(t, `
version: 1
listeners:
  - name: edge
    addr: "127.0.0.1:0"
    upstream_pool: web
pools:
  web:
    servers: ["127.0.0.1:9000", "127.0.0.1:9000"]
`)
	if _, err := config.Load(path); err == nil {
		t.Fatal("expected error for duplicate servers, got nil")
	}
}

func TestLoadRejectsUnknownBalance(t *testing.T) {
	path := write(t, `
version: 1
listeners:
  - name: edge
    addr: "127.0.0.1:0"
    upstream_pool: web
pools:
  web:
    balance: least_con
    servers: ["127.0.0.1:9000"]
`)
	if _, err := config.Load(path); err == nil {
		t.Fatal("expected error for unknown balance, got nil")
	}
}

func TestPoolValidateRejects(t *testing.T) {
	cases := map[string]config.PoolConfig{
		"no servers":          {},
		"unknown balance":     {Balance: "random", Servers: []string{"127.0.0.1:9000"}},
		"malformed server":    {Servers: []string{"garbage"}},
		"duplicate server":    {Servers: []string{"127.0.0.1:9000", "127.0.0.1:9000"}},
		"negative hc period":  {Servers: []string{"127.0.0.1:9000"}, HealthCheck: config.HealthCheckConfig{Interval: -time.Second}},
		"negative hc timeout": {Servers: []string{"127.0.0.1:9000"}, HealthCheck: config.HealthCheckConfig{Timeout: -time.Second}},
	}
	for name, p := range cases {
		if err := p.Validate(); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
	valid := config.PoolConfig{
		Servers:     []string{"127.0.0.1:9000"},
		HealthCheck: config.HealthCheckConfig{Interval: 2 * time.Second},
	}
	if err := valid.Validate(); err != nil {
		t.Errorf("valid pool: unexpected error %v", err)
	}
	if valid.HealthCheck.Timeout != config.DefaultHealthCheckTimeout {
		t.Errorf("health_check.timeout = %s, want %s", valid.HealthCheck.Timeout, config.DefaultHealthCheckTimeout)
	}
}

// TestPoolValidateRejectsBadServerAddr covers the address checks for upstream
// servers, which are stricter than for listeners: the host is required and port
// 0 is rejected.
func TestPoolValidateRejectsBadServerAddr(t *testing.T) {
	cases := map[string]string{
		"missing host":      ":9000",
		"missing port":      "127.0.0.1:",
		"non-numeric port":  "127.0.0.1:http",
		"port out of range": "127.0.0.1:99999",
		"port zero":         "127.0.0.1:0",
		"not host:port":     "garbage",
	}
	for name, addr := range cases {
		p := config.PoolConfig{Servers: []string{addr}}
		if err := p.Validate(); err == nil {
			t.Errorf("%s (%q): expected error, got nil", name, addr)
		}
	}
}
