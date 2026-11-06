package config

import (
	"errors"
	"fmt"
	"time"
)

// DefaultHealthCheckTimeout is the TCP probe timeout used when a pool enables
// health_check.interval without setting health_check.timeout.
const (
	DefaultHealthCheckTimeout = time.Second
	DefaultCircuitOpen        = 10 * time.Second
)

// HealthCheckConfig configures active TCP health probing for a backend pool.
// Interval 0 disables active probing (passive mark-unhealthy on dial failure
// remains active whenever the pool has more than one server).
type HealthCheckConfig struct {
	Interval time.Duration `yaml:"interval"` // active probe period; 0 = disabled
	Timeout  time.Duration `yaml:"timeout"`  // TCP dial timeout per probe (default 1s)
}

// CircuitBreakerConfig configures the three-state (Closed -> Open -> Half-Open)
// passive circuit breaker for each server in a pool.
type CircuitBreakerConfig struct {
	ErrorThreshold   int           `yaml:"error_threshold"`    // consecutive dial failures to open; 0 = immediate (1 failure)
	OpenDuration     time.Duration `yaml:"open_duration"`      // time in Open before Half-Open probe (default 10s when threshold > 0)
	HalfOpenRequests int           `yaml:"half_open_requests"` // consecutive successes in Half-Open to close (default 1)
}

// PoolConfig describes a group of backend endpoints.
type PoolConfig struct {
	Balance string `yaml:"balance"` // "round_robin" (default) or "least_conn"
	// Servers are backend "host:port" endpoints. Order is significant:
	// round-robin visits them in order and least-conn breaks ties by it, so do
	// not sort or reorder them casually.
	Servers        []string             `yaml:"servers"`
	DialRetries    int                  `yaml:"dial_retries"` // extra dial attempts on other servers in the pool (default 0)
	HealthCheck    HealthCheckConfig    `yaml:"health_check"`
	CircuitBreaker CircuitBreakerConfig `yaml:"circuit_breaker"`
}

// Validate rejects invalid pools: an unknown balance strategy, negative
// health-check durations, and malformed or duplicated servers. Balance "" is
// accepted and means round_robin; server order is meaningful and is left
// untouched.
func (p *PoolConfig) Validate() error {
	if len(p.Servers) == 0 {
		return errors.New("at least one server is required")
	}
	switch p.Balance {
	case "", "round_robin", "least_conn":
	default:
		return fmt.Errorf("unknown balance %q (want round_robin or least_conn)", p.Balance)
	}
	if p.HealthCheck.Interval < 0 {
		return errors.New("health_check.interval cannot be negative")
	}
	if p.HealthCheck.Timeout < 0 {
		return errors.New("health_check.timeout cannot be negative")
	}
	if p.HealthCheck.Interval > 0 && p.HealthCheck.Timeout == 0 {
		p.HealthCheck.Timeout = DefaultHealthCheckTimeout
	}
	if p.DialRetries < 0 {
		return errors.New("dial_retries cannot be negative")
	}
	if p.CircuitBreaker.ErrorThreshold < 0 {
		return errors.New("circuit_breaker.error_threshold cannot be negative")
	}
	if p.CircuitBreaker.OpenDuration < 0 {
		return errors.New("circuit_breaker.open_duration cannot be negative")
	}
	if p.CircuitBreaker.HalfOpenRequests < 0 {
		return errors.New("circuit_breaker.half_open_requests cannot be negative")
	}
	if p.CircuitBreaker.ErrorThreshold > 0 {
		if p.CircuitBreaker.OpenDuration == 0 {
			p.CircuitBreaker.OpenDuration = DefaultCircuitOpen
		}
		if p.CircuitBreaker.HalfOpenRequests == 0 {
			p.CircuitBreaker.HalfOpenRequests = 1
		}
	}
	seen := make(map[string]bool, len(p.Servers))
	for _, s := range p.Servers {
		if err := validateAddr(s, true); err != nil {
			return err
		}
		if seen[s] {
			return fmt.Errorf("duplicate server %q", s)
		}
		seen[s] = true
	}
	return nil
}
