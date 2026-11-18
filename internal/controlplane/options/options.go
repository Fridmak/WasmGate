// Package options defines the shared control-plane client configuration and its
// defaults. It is independent from the Agent, identity, release and session
// packages so those layers can share one options type without cycles.
package options

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"runtime/debug"
	"strings"
	"time"
)

// Default tuning used when Options leaves a field empty.
const (
	DefaultHeartbeat        = 15 * time.Second
	DefaultReconnectBackoff = time.Second
	DefaultReconnectCeiling = 30 * time.Second
	// DefaultRenewBefore is how long before expiry the agent renews its
	// certificate automatically.
	DefaultRenewBefore = 7 * 24 * time.Hour
)

// Options configures a control-plane client.
type Options struct {
	// Endpoint is the control-plane gRPC address. https:// enables mTLS with the
	// agent certificate; http:// is accepted only with Insecure and must stay on
	// a loopback address, because the control plane refuses plaintext elsewhere.
	Endpoint string
	// EnrollToken is a one-time enrollment token. It is used when StateDir has no
	// certificate yet, and again when the certificate has expired: an expired
	// certificate cannot authenticate, so it can only be replaced by enrolling.
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
	// Environment is the logical environment this agent belongs to. When set,
	// signed releases for another environment are rejected before parsing. Empty
	// keeps compatibility with single-environment deployments.
	Environment string
	// Labels are matched by deployment wave selectors. Labels configured by the
	// operator on the control plane take precedence over these values.
	Labels map[string]string
	// Version is the data-plane version reported in Hello. Empty means the
	// module version from the build info.
	Version string
	// AbiVersions are the plugin ABI versions this build supports. It is not
	// the control-plane protocol version: agents and control planes negotiate
	// that separately. The data plane now ships only the ABI v4 runtime.
	AbiVersions []string
	// Heartbeat is the interval between Heartbeat messages.
	Heartbeat time.Duration
	// ServerCA is a PEM bundle used to verify the control-plane server
	// certificate. Empty means the system roots.
	ServerCA string
	// ServerName overrides the TLS server name (useful when dialing by IP).
	ServerName string
	// ReleaseKey is a PEM file with the ECDSA P-256 public key that verifies
	// release manifests. Required: without it the agent cannot tell a genuine
	// release from a row rewritten in the control-plane database.
	ReleaseKey string
	// RenewBefore is how long before expiry the agent renews its certificate.
	RenewBefore time.Duration
	// Insecure allows a plaintext http:// endpoint.
	Insecure bool
	// ReconnectMin and ReconnectMax bound the exponential reconnect backoff.
	ReconnectMin time.Duration
	ReconnectMax time.Duration
}

func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	return "dev"
}

// Normalize applies defaults and validates required fields.
func (o *Options) Normalize() error {
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
		host := strings.ToLower(u.Hostname())
		if host != "localhost" {
			ip := net.ParseIP(host)
			if ip == nil || !ip.IsLoopback() {
				return fmt.Errorf(
					"control plane: http:// is allowed only for loopback; %q is remote",
					u.Hostname())
			}
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

	if strings.TrimSpace(o.ReleaseKey) == "" {
		return errors.New(
			"control plane: release key is required; pass the control-plane public key " +
				"(release-signing.key.pub) so release signatures can be verified")
	}

	if o.StateDir == "" {
		o.StateDir = "state"
	}
	if o.Name == "" {
		if host, err := os.Hostname(); err == nil {
			o.Name = host
		}
	}
	o.Environment = strings.TrimSpace(o.Environment)
	if o.Version == "" {
		o.Version = buildVersion()
	}
	if len(o.AbiVersions) == 0 {
		o.AbiVersions = []string{"v4"}
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
	if o.RenewBefore <= 0 {
		o.RenewBefore = DefaultRenewBefore
	}
	return nil
}
