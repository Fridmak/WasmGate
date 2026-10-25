package observe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"time"
)

// Admin serves the observability endpoints — /metrics, /healthz and the
// net/http/pprof handlers — on a listener of its own, configured by
// admin_addr. Keeping it separate from the data-plane listeners means the
// proxy's accept path never handles an HTTP request, and the admin port can be
// bound to loopback only even when listeners face the internet.
//
// Lifecycle: NewAdmin binds synchronously (so a busy port fails startup with a
// clear error instead of surfacing later from a goroutine), Serve blocks in its
// own goroutine until Shutdown.
type Admin struct {
	ln  net.Listener
	srv *http.Server
}

// NewAdmin binds addr and returns an Admin ready to Serve. metrics must not be
// nil; upstreams may be nil when per-backend gauges are not wanted.
func NewAdmin(addr string, metrics *Metrics, upstreams func() []UpstreamStat, log *slog.Logger) (*Admin, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("observe: admin listen %s: %w", addr, err)
	}
	a := &Admin{ln: ln}
	a.srv = &http.Server{
		Handler:           a.handler(metrics, upstreams, log),
		ReadHeaderTimeout: 5 * time.Second, // pprof's profile endpoint runs long; no WriteTimeout
	}
	return a, nil
}

// Addr reports the bound address (useful when the config asked for port 0).
func (a *Admin) Addr() net.Addr { return a.ln.Addr() }

// Serve accepts connections until Shutdown. It blocks, so run it in its own
// goroutine; it returns nil after a graceful Shutdown.
func (a *Admin) Serve() error {
	err := a.srv.Serve(a.ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown gracefully stops the admin server, waiting for in-flight scrapes up
// to the deadline in ctx.
func (a *Admin) Shutdown(ctx context.Context) error {
	return a.srv.Shutdown(ctx)
}

// handler builds the admin mux. The pprof handlers are registered explicitly on
// this mux (rather than imported for their DefaultServeMux side effect) so the
// process keeps exactly one HTTP surface: this one.
func (a *Admin) handler(m *Metrics, upstreams func() []UpstreamStat, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		var ups []UpstreamStat
		if upstreams != nil {
			ups = upstreams()
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		m.WritePrometheus(w, ups)
	})

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		s := m.Snapshot()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Status          string  `json:"status"`
			ConfigVersion   uint64  `json:"config_version"`
			ConnectionsLive int64   `json:"connections_active"`
			UptimeSeconds   float64 `json:"uptime_seconds"`
		}{
			Status:          "ok",
			ConfigVersion:   s.ConfigVersion,
			ConnectionsLive: s.ConnectionsActive,
			UptimeSeconds:   s.Uptime.Seconds(),
		})
	})

	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	return logRequests(mux, log)
}

// logRequests logs one line per admin request at debug level; scrapes every
// 15s would flood info logs.
func logRequests(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		log.Debug("admin request", "method", r.Method, "path", r.URL.Path)
	})
}
