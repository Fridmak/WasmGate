package observe

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// AdminOptions hardens the optional admin listener.
type AdminOptions struct {
	// Token, when non-empty, is required as "Authorization: Bearer <token>"
	// for every admin endpoint.
	Token string
	// Pprof enables /debug/pprof/* handlers. Off by default.
	Pprof bool
	// AllowRemote permits a non-loopback Host header (the listener itself is
	// validated in config.Config.Validate).
	AllowRemote bool
}

// Admin serves the observability endpoints — /metrics, /healthz and the
// optional net/http/pprof handlers — on a listener of its own, configured by
// admin_addr. Keeping it separate from the data-plane listeners means the
// proxy's accept path never handles an HTTP request.
type Admin struct {
	ln         net.Listener
	srv        *http.Server
	reload     func(context.Context) (uint64, error)
	opts       AdminOptions
	pprof      atomic.Bool
	reloadBusy atomic.Bool
}

// SetReload registers the callback invoked by POST /reload. Call before Serve.
func (a *Admin) SetReload(fn func(context.Context) (uint64, error)) {
	a.reload = fn
}

// NewAdmin binds addr and returns an Admin ready to Serve. metrics must not be
// nil; upstreams may be nil when per-backend gauges are not wanted.
func NewAdmin(addr string, metrics *Metrics, upstreams func() []UpstreamStat, log *slog.Logger, opts AdminOptions) (*Admin, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("observe: admin listen %s: %w", addr, err)
	}
	a := &Admin{ln: newLimitListener(ln, 64), opts: opts}
	a.srv = &http.Server{
		Handler:           a.handler(metrics, upstreams, log),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
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

	mux.HandleFunc("/reload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if a.reload == nil {
			w.WriteHeader(http.StatusNotImplemented)
			return
		}
		if !a.reloadBusy.CompareAndSwap(false, true) {
			http.Error(w, "reload already in progress", http.StatusConflict)
			return
		}
		defer a.reloadBusy.Store(false)

		w.Header().Set("Content-Type", "application/json")
		v, err := a.reload(r.Context())
		if err != nil {
			// Do not leak filesystem paths or config fragments to the caller.
			log.Error("admin reload failed", "err", err)
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(struct {
				Status string `json:"status"`
				Error  string `json:"error"`
			}{Status: "error", Error: "reload failed; see server logs"})
			return
		}
		_ = json.NewEncoder(w).Encode(struct {
			Status        string `json:"status"`
			ConfigVersion uint64 `json:"config_version"`
		}{Status: "reloaded", ConfigVersion: v})
	})

	if a.opts.Pprof {
		mux.HandleFunc("/debug/pprof/", a.guardPprof(pprof.Index, false))
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", a.guardPprof(pprof.Profile, false))
		mux.HandleFunc("/debug/pprof/symbol", a.guardPprof(pprof.Symbol, false))
		mux.HandleFunc("/debug/pprof/trace", a.guardPprof(pprof.Trace, false))
		mux.HandleFunc("/debug/pprof/goroutine", a.guardPprof(pprof.Handler("goroutine").ServeHTTP, false))
	}

	return logRequests(a.secure(mux, log), log)
}

// secure enforces Host/Origin/token checks before dispatching an admin request.
func (a *Admin) secure(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hostAllowed(r.Host, a.opts.AllowRemote) {
			http.Error(w, "host not allowed", http.StatusMisdirectedRequest)
			return
		}
		if r.URL.Path == "/reload" && !sameOriginRequest(r) {
			http.Error(w, "cross-origin reload rejected", http.StatusForbidden)
			return
		}
		if a.opts.Token != "" && !bearerMatches(r, a.opts.Token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="wasmgate-admin"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func hostAllowed(host string, allowRemote bool) bool {
	if allowRemote {
		return true
	}
	h := host
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		h = parsed
	} else {
		h = strings.TrimPrefix(strings.TrimSuffix(h, "]"), "[")
	}
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func sameOriginRequest(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // CLI/curl: no browser origin to compare.
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

func bearerMatches(r *http.Request, token string) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(h[len(prefix):]), []byte(token)) == 1
}

// guardPprof serializes expensive pprof requests, enforces a response deadline
// and rejects the stop-the-world debug modes unless explicitly requested by the
// operator (currently never: the handler is for safe diagnostics only).
func (a *Admin) guardPprof(h http.HandlerFunc, allowDebug2 bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !allowDebug2 {
			if dbg := r.URL.Query().Get("debug"); dbg != "" {
				n, err := strconv.Atoi(dbg)
				if err != nil || n < 0 || n >= 2 {
					http.Error(w, "unsafe pprof mode disabled", http.StatusBadRequest)
					return
				}
			}
			if gc := r.URL.Query().Get("gc"); gc != "" {
				n, err := strconv.Atoi(gc)
				if err != nil || n != 0 {
					http.Error(w, "unsafe pprof mode disabled", http.StatusBadRequest)
					return
				}
			}
		}
		if q := r.URL.Query().Get("seconds"); q != "" {
			n, err := strconv.Atoi(q)
			if err != nil || n < 0 || n > 30 {
				http.Error(w, "seconds must be between 0 and 30", http.StatusBadRequest)
				return
			}
		}
		if !a.pprof.CompareAndSwap(false, true) {
			http.Error(w, "another pprof request is in progress", http.StatusTooManyRequests)
			return
		}
		defer a.pprof.Store(false)
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Second))
		if r.URL.Path == "/debug/pprof/symbol" {
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		}
		h(w, r)
	}
}

// limitListener bounds the number of concurrent admin connections so keep-alive
// sockets cannot exhaust the process-wide fd limit.
type limitListener struct {
	net.Listener
	sem       chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

func newLimitListener(ln net.Listener, n int) net.Listener {
	return &limitListener{
		Listener: ln,
		sem:      make(chan struct{}, n),
		done:     make(chan struct{}),
	}
}

func (l *limitListener) Accept() (net.Conn, error) {
	select {
	case <-l.done:
		return nil, net.ErrClosed
	case l.sem <- struct{}{}:
	}
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.sem
		return nil, err
	}
	return &limitConn{Conn: c, release: func() { <-l.sem }}, nil
}

func (l *limitListener) Close() error {
	l.closeOnce.Do(func() { close(l.done) })
	return l.Listener.Close()
}

type limitConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

// logRequests logs one line per admin request at debug level; scrapes every
// 15s would flood info logs.
func logRequests(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		log.Debug("admin request", "method", r.Method, "path", r.URL.Path)
	})
}
