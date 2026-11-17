// Command wasmgate is the WasmGate data plane: a fast, L4 TCP reverse proxy
// with zero-downtime configuration reload, per-IP sliding-window rate limiting
// and a Prometheus/pprof admin endpoint.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"time"

	"wasmgate/internal/config"
	"wasmgate/internal/controlplane"
	"wasmgate/internal/observe"
	"wasmgate/internal/proxy"
)

// version is overridden at build time with
// -ldflags "-X main.version=<tag>".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

// run wires the graph (config -> runtime -> server), starts accepting and then
// blocks until a shutdown signal arrives. On Unix, SIGHUP reloads the config
// without dropping connections; the returned error is reported by main.
func run() error {
	var (
		configPath    = flag.String("config", "configs/wasmgate.yaml", "path to the configuration file")
		logLevel      = flag.String("log-level", "info", "log level: debug|info|warn|error")
		shutdownGrace = flag.Duration("shutdown-grace", 15*time.Second, "graceful shutdown timeout")
		validateOnly  = flag.Bool("validate", false, "validate the configuration (and any WASM plugins) and exit")
		versionOnly   = flag.Bool("version", false, "print the build version and exit")

		cpEndpoint   = flag.String("cp", "", "control-plane gRPC endpoint (https:// for mTLS); empty = standalone")
		cpToken      = flag.String("cp-token", os.Getenv("WASMGATE_ENROLL_TOKEN"), "one-time enrollment token used when no agent certificate exists")
		cpState      = flag.String("cp-state", "state", "directory for the agent identity and applied-release state")
		cpName       = flag.String("cp-name", "", "agent name reported to the control plane (default: host name)")
		cpAgentID    = flag.String("cp-agent-id", "", "agent id assigned by the control plane (per-agent enrollment tokens)")
		cpEnv        = flag.String("cp-environment", "", "environment this agent belongs to; releases for another environment are rejected")
		cpLabels     = flag.String("cp-labels", "", "comma-separated k=v labels used by deployment selectors")
		cpServerCA   = flag.String("cp-server-ca", "", "PEM file with the CA that signed the control-plane server certificate")
		cpHeartbeat  = flag.Duration("cp-heartbeat", controlplane.DefaultHeartbeat, "heartbeat interval")
		cpInsecure   = flag.Bool("cp-insecure", false, "allow a plaintext http:// control-plane endpoint (local development)")
		cpReleaseKey = flag.String(
			"cp-release-key",
			"",
			"PEM file with the control-plane release-signing public key (release-signing.key.pub); required")
		cpRenewBefore = flag.Duration(
			"cp-renew-before",
			controlplane.DefaultRenewBefore,
			"renew the agent certificate this long before it expires")
	)
	flag.Parse()

	if *versionOnly {
		fmt.Println(version)
		return nil
	}

	level, err := observe.ParseLevel(*logLevel)
	if err != nil {
		return err
	}
	log := observe.NewLogger(level)
	slog.SetDefault(log)

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	for _, lc := range cfg.Listeners {
		if lc.MaxConns == 0 {
			log.Warn("listener has unlimited max_conns; slow clients can exhaust the process", "listener", lc.Name)
		}
		if lc.RateLimit.MaxConcurrent == 0 {
			log.Warn("listener has no per-IP concurrent connection limit", "listener", lc.Name)
		}
	}

	metrics := observe.NewMetrics()
	metrics.SetBuildVersion(version)
	rt, err := proxy.NewRuntimeChecked(cfg)
	if err != nil {
		return err
	}

	// -validate is a configuration/plugin check, not a runtime start. Do it
	// before resolving runtime secrets so a valid cluster config can be
	// validated without WASMGATE_ADMIN_TOKEN.
	if *validateOnly {
		log.Info("configuration is valid", "path", *configPath)
		return nil
	}

	var adminToken string
	if cfg.AdminAddr != "" {
		adminToken = os.Getenv("WASMGATE_ADMIN_TOKEN")
		if adminToken == "" && cfg.AdminTokenFile != "" {
			b, err := os.ReadFile(cfg.AdminTokenFile) // #nosec G304 -- operator-configured path
			if err != nil {
				return fmt.Errorf("admin token: %w", err)
			}
			adminToken = strings.TrimSpace(string(b))
			if adminToken == "" {
				return fmt.Errorf("admin token: %s is empty", cfg.AdminTokenFile)
			}
		}
		if cfg.AdminAllowRemote && adminToken == "" {
			return fmt.Errorf("admin_allow_remote requires admin_token_file or WASMGATE_ADMIN_TOKEN")
		}
	}

	srv := proxy.NewServer(log, rt, metrics)

	if err := srv.Start(); err != nil {
		return err
	}
	log.Info("wasmgate started", "config_version", rt.Load().Version)

	reloadFromDisk := func(ctx context.Context) (uint64, error) {
		newCfg, err := config.Load(*configPath)
		if err != nil {
			return 0, err
		}
		v, err := srv.Reload(ctx, newCfg)
		if err != nil {
			return 0, err
		}
		log.Info("configuration reloaded", "config_version", v)
		return v, nil
	}

	// The admin endpoint (metrics, healthz, reload, pprof) is optional: it runs
	// on its own listener only when admin_addr is configured, so the data-plane
	// accept path never handles an HTTP request. Binding happens here, before
	// Serve, so a busy port fails startup instead of surfacing later from a
	// goroutine.
	var admin *observe.Admin
	if cfg.AdminAddr != "" {
		admin, err = observe.NewAdmin(cfg.AdminAddr, metrics, rt.UpstreamStats, log, observe.AdminOptions{
			Token:       adminToken,
			Pprof:       cfg.AdminPprof,
			AllowRemote: cfg.AdminAllowRemote,
		})
		if err != nil {
			return err
		}
		admin.SetReload(reloadFromDisk)
		go func() {
			if err := admin.Serve(); err != nil {
				log.Error("admin endpoint stopped", "err", err)
			}
		}()
		endpoints := "/metrics /healthz /reload"
		if cfg.AdminPprof {
			endpoints += " /debug/pprof/"
		}
		log.Info("admin endpoint listening",
			"addr", admin.Addr().String(),
			"endpoints", endpoints,
		)
	}

	// The control-plane client is optional. When -cp is set the data plane
	// enrolls (once, if it has no certificate), opens Session and applies the
	// releases the control plane pushes. The configuration file becomes
	// control-plane managed: every applied release is written there.
	cpCtx, cpCancel := context.WithCancel(context.Background())
	defer cpCancel()
	if *cpEndpoint != "" {
		labels, err := parseLabels(*cpLabels)
		if err != nil {
			return err
		}

		client, err := controlplane.NewAgent(controlplane.Options{
			Endpoint:    *cpEndpoint,
			EnrollToken: *cpToken,
			StateDir:    *cpState,
			ConfigPath:  *configPath,
			Name:        *cpName,
			AgentID:     *cpAgentID,
			Environment: *cpEnv,
			Labels:      labels,
			Heartbeat:   *cpHeartbeat,
			ServerCA:    *cpServerCA,
			ReleaseKey:  *cpReleaseKey,
			RenewBefore: *cpRenewBefore,
			Insecure:    *cpInsecure,
		}, srv, log)
		if err != nil {
			return err
		}

		go func() {
			if err := client.Run(cpCtx); err != nil {
				log.Error("control-plane client stopped", "err", err)
			}
		}()
		log.Info("control-plane client started", "endpoint", *cpEndpoint, "state_dir", *cpState)
	}

	// Build the set of signals: shutdown signals plus (on Unix) SIGHUP for reload.
	reload := reloadSignal()
	sigs := shutdownSignals()
	if reload != nil {
		sigs = append(sigs, reload)
	}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, sigs...)
	defer signal.Stop(sigCh)

	for s := range sigCh {
		if reload != nil && s == reload {
			if _, err := reloadFromDisk(context.Background()); err != nil {
				log.Error("reload failed, keeping previous config", "err", err)
			}
			continue
		}

		log.Info("shutting down", "signal", s.String())
		cpCancel()
		shutCtx, shutCancel := context.WithTimeout(context.Background(), *shutdownGrace)
		err := srv.Shutdown(shutCtx)
		if admin != nil {
			_ = admin.Shutdown(shutCtx)
		}
		shutCancel()
		if err != nil {
			log.Warn("connections force-closed after grace expired", "err", err)
		}
		return nil
	}
	return nil
}

// parseLabels parses "region=eu,role=edge" into a map. Empty input yields nil.
func parseLabels(raw string) (map[string]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	labels := make(map[string]string)
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}

		key, value, ok := strings.Cut(pair, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, fmt.Errorf("invalid label %q (want key=value)", pair)
		}
		labels[key] = strings.TrimSpace(value)
	}
	return labels, nil
}
