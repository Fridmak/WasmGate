package config

import (
	"errors"
	"fmt"
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
	Name         string        `yaml:"name"`          // unique name (used for logs and lookup)
	Addr         string        `yaml:"addr"`          // listen address, e.g. "127.0.0.1:8080"
	UpstreamPool string        `yaml:"upstream_pool"` // name of the target pool in Config.Pools
	MaxConns     int           `yaml:"max_conns"`     // max concurrent connections; 0 = unlimited
	DialTimeout  time.Duration `yaml:"dial_timeout"`  // upstream dial timeout (default 5s)
	IdleTimeout  time.Duration `yaml:"idle_timeout"`  // connection idle timeout; 0 disables (default 0)
	RateLimit    RateLimit     `yaml:"rate_limit"`    // per-IP connection rate; max 0 disables
}

// RateLimit caps how many new connections one client IP may open within a
// sliding window. Max 0 disables the check; Window defaults to 1s when Max is set.
type RateLimit struct {
	Max    int           `yaml:"max"`    // connections per IP per window; 0 = disabled
	Window time.Duration `yaml:"window"` // sliding window length (default 1s)
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
	if l.RateLimit.Max > 0 {
		if l.RateLimit.Window == 0 {
			l.RateLimit.Window = time.Second
		}
		if l.RateLimit.Window < MinRateWindow {
			return fmt.Errorf("rate_limit.window must be at least %s", MinRateWindow)
		}
	}
	return nil
}
