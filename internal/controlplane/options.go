// Package controlplane connects the WasmGate data plane to the WasmGate
// control plane: it enrolls the agent, keeps a long-lived Session open, sends
// heartbeats, applies releases pushed by the control plane and reports the
// result back.
package controlplane

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// Default tuning used when Options leaves a field empty.
const (
	DefaultHeartbeat        = 15 * time.Second
	DefaultReconnectBackoff = time.Second
	DefaultReconnectCeiling = 30 * time.Second
)

// Options configures a control-plane client.
type Options struct {
	// Endpoint is the control-plane gRPC address. https:// enables mTLS with the
	// agent certificate; http:// requires Insecure and is meant for local runs.
	Endpoint string
	// EnrollToken is a one-time enrollment token. It is used only when StateDir
	// has no certificate yet.
	EnrollToken string
	// StateDir holds agent.id, agent.key, agent.crt and agents-ca.crt.
	StateDir string
	// ConfigPath is the YAML file managed by the control plane. Every applied
	// release is written here and artifacts are downloaded next to it.
	ConfigPath string
	// Name is the operator-visible agent name. Empty means the host name.
	Name string
	// AgentID is the identifier assigned by the control plane when the agent was
	// pre-created with a per-agent enrollment token. Empty means the agent picks
	// its own GUID, which is what shared registration tokens expect.
	AgentID string
	// Labels are matched by deployment wave selectors.
	Labels map[string]string
	// Version is the data-plane version reported in Hello. Empty means the
	// module version from the build info.
	Version string
	// AbiVersions are the plugin ABI versions this build supports.
	AbiVersions []string
	// Heartbeat is the interval between Heartbeat messages.
	Heartbeat time.Duration
	// ServerCA is a PEM bundle used to verify the control-plane server
	// certificate. Empty means the system roots.
	ServerCA string
	// ServerName overrides the TLS server name (useful when dialing by IP).
	ServerName string
	// Insecure allows a plaintext http:// endpoint.
	Insecure bool
	// ReconnectMin and ReconnectMax bound the exponential reconnect backoff.
	ReconnectMin time.Duration
	ReconnectMax time.Duration
}

// normalize applies defaults and validates required fields.
func (o *Options) normalize() error {
	if strings.TrimSpace(o.Endpoint) == "" {
		return errors.New("control plane: endpoint is required")
	}

	u, err := url.Parse(o.Endpoint)
	if err != nil {
		return fmt.Errorf("control plane: endpoint: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if !o.Insecure {
			return fmt.Errorf("control plane: %s uses http:// but Insecure is not set", o.Endpoint)
		}
	default:
		return fmt.Errorf("control plane: unsupported endpoint scheme %q (want http or https)", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("control plane: endpoint %q has no host:port", o.Endpoint)
	}

	if strings.TrimSpace(o.ConfigPath) == "" {
		return errors.New("control plane: config path is required")
	}

	if o.StateDir == "" {
		o.StateDir = "state"
	}
	if o.Name == "" {
		if host, err := os.Hostname(); err == nil {
			o.Name = host
		}
	}
	if o.Version == "" {
		o.Version = buildVersion()
	}
	if len(o.AbiVersions) == 0 {
		o.AbiVersions = []string{"v1", "v2", "v3"}
	}
	if o.Heartbeat <= 0 {
		o.Heartbeat = DefaultHeartbeat
	}
	if o.ReconnectMin <= 0 {
		o.ReconnectMin = DefaultReconnectBackoff
	}
	if o.ReconnectMax < o.ReconnectMin {
		o.ReconnectMax = DefaultReconnectCeiling
	}
	if o.ReconnectMax < o.ReconnectMin {
		o.ReconnectMax = o.ReconnectMin
	}
	return nil
}
