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
	rt := proxy.NewRuntime(cfg)
	srv := proxy.NewServer(log, rt, metrics)

	if err := srv.Start(); err != nil {
		return err
	}
	log.Info("wasmgate started", "config_version", rt.Load().Version)

	// The admin endpoint (metrics, healthz, pprof) is optional: it runs on its
	// own listener only when admin_addr is configured, so the data-plane accept
	// path never handles an HTTP request. Binding happens here, before Serve, so
	// a busy port fails startup instead of surfacing later from a goroutine.
	var admin *observe.Admin
	if cfg.AdminAddr != "" {
		admin, err = observe.NewAdmin(cfg.AdminAddr, metrics, rt.UpstreamStats, log)
		if err != nil {
			return err
		}
		go func() {
			if err := admin.Serve(); err != nil {
				log.Error("admin endpoint stopped", "err", err)
			}
		}()
		log.Info("admin endpoint listening",
			"addr", admin.Addr().String(),
			"endpoints", "/metrics /healthz /debug/pprof/",
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
			newCfg, err := config.Load(*configPath)
			if err != nil {
				log.Error("reload failed, keeping previous config", "err", err)
				continue
			}
			v, err := srv.Reload(context.Background(), newCfg)
			if err != nil {
				log.Error("reload failed, keeping previous config", "err", err)
				continue
			}
			log.Info("configuration reloaded", "config_version", v)
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
