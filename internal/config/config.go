// Package config defines the on-disk configuration schema and loading.
//
// It is intentionally pure data + validation, with no runtime state. The
// runtime view (compiled objects, live counters) lives in the proxy package.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

// ConfigVersion is the only schema version this build understands.
const ConfigVersion = 1

// Config is the root of the configuration file.
type Config struct {
	Version   int                   `yaml:"version"`    // schema version (must be 1)
	AdminAddr string                `yaml:"admin_addr"` // optional: /metrics, /healthz, pprof listener; empty = disabled
	Listeners []ListenerConfig      `yaml:"listeners"`  // front-facing TCP listeners
	Pools     map[string]PoolConfig `yaml:"pools"`      // backend groups, keyed by pool name
}

// Load reads, parses and validates the configuration file at path.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true) // reject unknown keys instead of silently ignoring typos

	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate applies defaults and rejects invalid configurations in place. It is
// the single entry point for validating a Config: Load calls it, and a control
// plane must call it before handing a configuration to Runtime.Reload, which
// trusts its input.
//
// Each listener and pool validates itself; Validate only adds the rules that
// need the whole document in view: unique listener names, unique listen
// addresses and listener-to-pool references.
func (c *Config) Validate() error {
	if c.Version != ConfigVersion {
		return fmt.Errorf("config: unsupported version %d (want %d)", c.Version, ConfigVersion)
	}
	if len(c.Listeners) == 0 {
		return errors.New("config: no listeners defined")
	}
	if c.AdminAddr != "" {
		// Same shape as a listener address: host optional (binding it to
		// loopback is the operator's choice), numeric port; port 0 is allowed
		// so tests can take an ephemeral one.
		if err := validateAddr(c.AdminAddr, false); err != nil {
			return fmt.Errorf("config: admin_addr: %w", err)
		}
	}
	if err := c.validateListeners(); err != nil {
		return err
	}
	return c.validatePools()
}

// validateListeners validates every listener, then enforces the rules that span
// listeners: unique names, unique (non-ephemeral) addresses and an
// upstream_pool that actually exists.
func (c *Config) validateListeners() error {
	seenName := make(map[string]bool, len(c.Listeners))
	seenAddr := make(map[string]bool, len(c.Listeners))
	for i := range c.Listeners {
		l := &c.Listeners[i]
		label := listenerLabel(i, l.Name)
		if err := l.Validate(); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		if seenName[l.Name] {
			return fmt.Errorf("%s: duplicate name", label)
		}
		seenName[l.Name] = true
		// Two listeners on one address would fight over the same socket. Port 0
		// means "any free port", so it is exempt from the check.
		if !hasZeroPort(l.Addr) {
			if seenAddr[l.Addr] {
				return fmt.Errorf("%s: duplicate addr %q", label, l.Addr)
			}
			seenAddr[l.Addr] = true
		}
		if _, ok := c.Pools[l.UpstreamPool]; !ok {
			return fmt.Errorf("%s: unknown upstream_pool %q", label, l.UpstreamPool)
		}
		for host, poolName := range l.SNIRoutes {
			if host == "" {
				return fmt.Errorf("%s: sni_routes contains an empty host pattern", label)
			}
			if _, ok := c.Pools[poolName]; !ok {
				return fmt.Errorf("%s: sni_routes[%q] references unknown pool %q", label, host, poolName)
			}
		}
	}
	return nil
}

// validatePools validates every pool, naming the offending pool in the error.
func (c *Config) validatePools() error {
	for name := range c.Pools {
		p := c.Pools[name]
		if err := p.Validate(); err != nil {
			return fmt.Errorf("pool %q: %w", name, err)
		}
		c.Pools[name] = p
	}
	return nil
}

// listenerLabel identifies a listener in error messages, falling back to its
// index when the name is missing.
func listenerLabel(i int, name string) string {
	if name == "" {
		return fmt.Sprintf("listener[%d]", i)
	}
	return fmt.Sprintf("listener %q", name)
}

// hasZeroPort reports whether addr uses port 0, the "any free port" marker. The
// address is assumed to be already validated, so SplitHostPort cannot fail.
func hasZeroPort(addr string) bool {
	_, port, err := net.SplitHostPort(addr)
	return err == nil && port == "0"
}

// validateAddr checks that addr is a well-formed "host:port".
//
// When requireHost is true the host part must be present (used for upstream
// servers); listeners may omit it to bind all interfaces (":8080"). Port 0 is
// allowed for listeners (ephemeral port, handy in tests) but not for servers.
// The port must be numeric; service names are rejected to keep behaviour
// predictable.
func validateAddr(addr string, requireHost bool) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid address %q: %w", addr, err)
	}
	if requireHost && host == "" {
		return fmt.Errorf("invalid address %q: host is required", addr)
	}
	if port == "" {
		return fmt.Errorf("invalid address %q: port is required", addr)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("invalid address %q: port %q is not numeric", addr, port)
	}
	if n < 0 || n > 65535 {
		return fmt.Errorf("invalid address %q: port %d out of range 0..65535", addr, n)
	}
	if requireHost && n == 0 {
		return fmt.Errorf("invalid address %q: port 0 is not allowed for servers", addr)
	}
	return nil
}
