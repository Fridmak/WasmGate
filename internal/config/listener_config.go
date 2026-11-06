package config

import (
	"errors"
	"fmt"
	"net"
	"time"
)

// Listener defaults, applied by Validate when the field keeps its zero value.
const (
	// DefaultDialTimeout is the upstream dial timeout used when a listener
	// omits dial_timeout.
	DefaultDialTimeout = 5 * time.Second

	// DefaultIdleTimeout is the connection idle timeout used when a listener
	// omits idle_timeout.
	//
	// NOTE: an idle timeout is enforced on a buffered copy path, which disables
	// the zero-copy splice(2) fast path. Keep the default at 0 (disabled) to
	// preserve zero-copy by default; set idle_timeout explicitly in production
	// to reap half-open connections.
	DefaultIdleTimeout = 0 * time.Second
)

// ListenerConfig describes a single front-facing TCP listener.
type ListenerConfig struct {
	Name         string            `yaml:"name"`          // unique name (used for logs and lookup)
	Addr         string            `yaml:"addr"`          // listen address, e.g. "127.0.0.1:8080"
	UpstreamPool string            `yaml:"upstream_pool"` // name of the target pool in Config.Pools
	MaxConns     int               `yaml:"max_conns"`     // max concurrent connections; 0 = unlimited
	DialTimeout  time.Duration     `yaml:"dial_timeout"`  // upstream dial timeout (default 5s)
	IdleTimeout  time.Duration     `yaml:"idle_timeout"`  // connection idle timeout; 0 disables (default 0)
	RateLimit    RateLimit         `yaml:"rate_limit"`    // per-IP connection rate; max 0 disables
	Plugin       PluginConfig      `yaml:"plugin"`        // optional WASM plugin sandbox for this listener
	SNIRoutes    map[string]string `yaml:"sni_routes"`    // optional TLS SNI host (exact or "*.domain") -> pool name
	TLS          TLSConfig         `yaml:"tls"`           // optional TLS termination for this listener
}

// TLSConfig configures optional TLS termination on a listener. When CertFile
// and KeyFile are empty, the listener operates in raw TCP / TLS passthrough mode.
type TLSConfig struct {
	CertFile   string `yaml:"cert_file"`   // path to PEM certificate chain
	KeyFile    string `yaml:"key_file"`    // path to PEM private key
	MinVersion string `yaml:"min_version"` // "1.2" (default) or "1.3"
	CertPEM    []byte `yaml:"-"`           // inline PEM cert for programmatic/test use
	KeyPEM     []byte `yaml:"-"`           // inline PEM key for programmatic/test use
}

// Enabled reports whether TLS termination is configured.
func (t TLSConfig) Enabled() bool {
	return t.CertFile != "" || t.KeyFile != "" || len(t.CertPEM) > 0 || len(t.KeyPEM) > 0
}

// PluginConfig configures an optional WebAssembly plugin attached to a listener.
type PluginConfig struct {
	Path           string        `yaml:"path"`             // path to .wasm binary; empty = disabled
	MaxMemoryPages uint32        `yaml:"max_memory_pages"` // cap in 64 KiB pages (0 = default 16 pages = 1 MiB)
	Timeout        time.Duration `yaml:"timeout"`          // per-hook timeout; 0 = no per-call timer
	DataMode       string        `yaml:"data_mode"`        // L7 on_data mode: ""/"once" (first packet, preserves splice) or "stream" (every chunk)
	RoutePools     []string      `yaml:"route_pools"`      // optional pools selectable by on_data return value (2 -> index 0, 3 -> index 1, ...)
	Wasm           []byte        `yaml:"-"`                // inline .wasm bytes for programmatic/test use
}

// Enabled reports whether a WASM plugin is configured.
func (p PluginConfig) Enabled() bool {
	return p.Path != "" || len(p.Wasm) > 0
}

// RateLimit caps how many new connections a client IP (and optionally its /24
// or /64 subnet) may open within a sliding window, with an optional CIDR
// allow-list and exponential-backoff ban. Setting both Max and SubnetMax to 0
// disables the limiter; Window defaults to 1s when enabled.
type RateLimit struct {
	Max         int           `yaml:"max"`          // connections per IP per window; 0 = disabled
	SubnetMax   int           `yaml:"subnet_max"`   // connections per /24 (IPv4) or /64 (IPv6) per window; 0 = disabled
	Window      time.Duration `yaml:"window"`       // sliding window length (default 1s)
	BanDuration time.Duration `yaml:"ban_duration"` // base ban TTL on violation (exponentially backed off up to 16x); 0 = no ban
	AllowCIDRs  []string      `yaml:"allow_cidrs"`  // CIDRs bypassed before counting
}

// MinRateWindow is the shortest accepted rate-limit window. The limiter splits
// the window into ten slots, so anything shorter than this would give slots
// finer than the clock resolution a counter can reliably use.
const MinRateWindow = 10 * time.Millisecond

// Validate fills in the timeouts a listener may omit and rejects invalid values
// in place. It inspects one listener in isolation: pool references and
// uniqueness across listeners are enforced by Config.Validate.
func (l *ListenerConfig) Validate() error {
	switch {
	case l.Name == "":
		return errors.New("name is required")
	case l.Addr == "":
		return errors.New("addr is required")
	case l.UpstreamPool == "":
		return errors.New("upstream_pool is required")
	}
	if err := validateAddr(l.Addr, false); err != nil {
		return err
	}
	if l.DialTimeout == 0 {
		l.DialTimeout = DefaultDialTimeout
	} else if l.DialTimeout < 0 {
		return errors.New("dial_timeout cannot be negative")
	}
	if l.IdleTimeout == 0 {
		l.IdleTimeout = DefaultIdleTimeout
	} else if l.IdleTimeout < 0 {
		return errors.New("idle_timeout cannot be negative")
	}
	if l.MaxConns < 0 {
		return errors.New("max_conns cannot be negative")
	}
	if l.RateLimit.Max < 0 {
		return errors.New("rate_limit.max cannot be negative")
	}
	if l.RateLimit.SubnetMax < 0 {
		return errors.New("rate_limit.subnet_max cannot be negative")
	}
	if l.RateLimit.BanDuration < 0 {
		return errors.New("rate_limit.ban_duration cannot be negative")
	}
	for _, cidr := range l.RateLimit.AllowCIDRs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("rate_limit.allow_cidrs: invalid CIDR %q: %w", cidr, err)
		}
	}
	if l.RateLimit.Enabled() {
		if l.RateLimit.Window == 0 {
			l.RateLimit.Window = time.Second
		}
		if l.RateLimit.Window < MinRateWindow {
			return fmt.Errorf("rate_limit.window must be at least %s", MinRateWindow)
		}
	}
	if l.Plugin.Timeout < 0 {
		return errors.New("plugin.timeout cannot be negative")
	}
	switch l.Plugin.DataMode {
	case "", "once", "stream":
	default:
		return fmt.Errorf("unknown plugin.data_mode %q (want once or stream)", l.Plugin.DataMode)
	}
	if l.TLS.Enabled() {
		hasFiles := l.TLS.CertFile != "" && l.TLS.KeyFile != ""
		hasInline := len(l.TLS.CertPEM) > 0 && len(l.TLS.KeyPEM) > 0
		if !hasFiles && !hasInline {
			return errors.New("tls requires both cert_file and key_file")
		}
		switch l.TLS.MinVersion {
		case "", "1.2", "1.3":
		default:
			return fmt.Errorf("unknown tls.min_version %q (want 1.2 or 1.3)", l.TLS.MinVersion)
		}
	}
	return nil
}

// Enabled reports whether either the per-IP or the per-subnet limit is active.
func (r RateLimit) Enabled() bool {
	return r.Max > 0 || r.SubnetMax > 0
}
