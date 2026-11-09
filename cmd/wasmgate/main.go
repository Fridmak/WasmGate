// Command wasmgate is the WasmGate data plane: a fast, L4 TCP reverse proxy
// with zero-downtime configuration reload, per-IP sliding-window rate limiting
// and a Prometheus/pprof admin endpoint.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"wasmgate/internal/config"
	"wasmgate/internal/observe"
	"wasmgate/internal/proxy"
)

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
	)
	flag.Parse()

	level, err := observe.ParseLevel(*logLevel)
	if err != nil {
		return err
	}
	log := observe.NewLogger(level)

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	metrics := observe.NewMetrics()
	rt, err := proxy.NewRuntimeChecked(cfg)
	if err != nil {
		return err
	}
	if *validateOnly {
		log.Info("configuration is valid", "path", *configPath)
		return nil
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
		token := os.Getenv("WASMGATE_ADMIN_TOKEN")
		if token == "" && cfg.AdminTokenFile != "" {
			b, err := os.ReadFile(cfg.AdminTokenFile) // #nosec G304 -- operator-configured path
			if err != nil {
				return fmt.Errorf("admin token: %w", err)
			}
			token = strings.TrimSpace(string(b))
		}
		if cfg.AdminAllowRemote && token == "" {
			return fmt.Errorf("admin_allow_remote requires admin_token_file or WASMGATE_ADMIN_TOKEN")
		}
		admin, err = observe.NewAdmin(cfg.AdminAddr, metrics, rt.UpstreamStats, log, observe.AdminOptions{
			Token:       token,
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
		log.Info("admin endpoint listening",
			"addr", admin.Addr().String(),
			"endpoints", "/metrics /healthz /reload /debug/pprof/",
		)
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
