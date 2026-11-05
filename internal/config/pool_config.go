package config

import (
	"errors"
	"fmt"
	"time"
)

// DefaultHealthCheckTimeout is the TCP probe timeout used when a pool enables
// health_check.interval without setting health_check.timeout.
const DefaultHealthCheckTimeout = time.Second

// HealthCheckConfig configures active TCP health probing for a backend pool.
// Interval 0 disables active probing (passive mark-unhealthy on dial failure
// remains active whenever the pool has more than one server).
type HealthCheckConfig struct {
	Interval time.Duration `yaml:"interval"` // active probe period; 0 = disabled
	Timeout  time.Duration `yaml:"timeout"`  // TCP dial timeout per probe (default 1s)
}

// PoolConfig describes a group of backend endpoints.
type PoolConfig struct {
	Balance string `yaml:"balance"` // "round_robin" (default) or "least_conn"
	// Servers are backend "host:port" endpoints. Order is significant:
	// round-robin visits them in order and least-conn breaks ties by it, so do
	// not sort or reorder them casually.
	Servers     []string          `yaml:"servers"`
	HealthCheck HealthCheckConfig `yaml:"health_check"`
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
