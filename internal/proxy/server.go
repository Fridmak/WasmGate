package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"runtime/debug"
	"sort"
	"sync"
	"time"

	"wasmgate/internal/config"
	"wasmgate/internal/observe"
)

// Server binds the configured listeners and dispatches accepted connections to
// the Proxy.
//
// Lifecycle: create with NewServer, call Start exactly once, then Shutdown (or
// Reload to apply a new configuration while running). Start is not idempotent —
// a second call returns an error.
//
// Concurrency model:
//   - acceptWG tracks accept loops; connWG tracks per-connection handlers.
//     Shutdown waits for accept loops to finish BEFORE waiting on handlers, so
//     no handler can be registered after the drain has begun (a WaitGroup Add
//     racing a Wait is undefined behaviour).
//   - connCtx parents every connection. It is NOT cancelled on graceful
//     shutdown, so established connections drain on their own snapshot. It is
//     cancelled only to force-close what is left after the grace expires.
//   - mu guards the binding map and the started/closing flags. It is never
//     touched on the accept/fast path.
type Server struct {
	log     *slog.Logger     // structured logger
	rt      *Runtime         // source of the current snapshot
	proxy   *Proxy           // shared by all accept loops
	metrics *observe.Metrics // per-connection counters; nil = disabled

	mu      sync.Mutex
	started bool
	closing bool
	ln      map[string]boundListener // listener name -> bound socket

	acceptWG sync.WaitGroup // accept loops
	connWG   sync.WaitGroup // in-flight connection handlers
	probeWG  sync.WaitGroup // active health-check loops

	connCtx     context.Context // parents every connection; cancelled = hard stop
	connCancel  context.CancelFunc
	probeCancel context.CancelFunc
}

// boundListener pairs a bound socket with the address string it was created
// from. The config address (not ln.Addr()) is what we compare on reload: it is
// stable and does not depend on how the OS renders the bound address (e.g.
// ":8080" vs "[::]:8080").
type boundListener struct {
	cfgAddr string
	ln      net.Listener
}

// NewServer builds a Server bound to the runtime rt. metrics may be nil, which
// disables the per-connection counters.
func NewServer(log *slog.Logger, rt *Runtime, metrics *observe.Metrics) *Server {
	connCtx, connCancel := context.WithCancel(context.Background())
	return &Server{
		log:        log,
		rt:         rt,
		proxy:      New(log, rt, metrics),
		metrics:    metrics,
		ln:         make(map[string]boundListener),
		connCtx:    connCtx,
		connCancel: connCancel,
	}
}

// Start binds every listener in the current snapshot and begins accepting.
// It may be called only once per Server; a second call returns an error.
func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		return errors.New("proxy: Server already started")
	}

	snap := s.rt.Load()
	if err := s.rt.ValidatePlugins(snap.Config); err != nil {
		return err
	}
	var lc net.ListenConfig

	ln := make(map[string]boundListener, len(snap.Config.Listeners))
	for _, l := range snap.Config.Listeners {
		sock, err := lc.Listen(context.Background(), "tcp", l.Addr)
		if err != nil {
			for _, b := range ln {
				_ = b.ln.Close()
			}
			return fmt.Errorf("listen %q on %s: %w", l.Name, l.Addr, err)
		}
		ln[l.Name] = boundListener{cfgAddr: l.Addr, ln: sock}
	}
	for _, l := range snap.Config.Listeners {
		s.startAccept(l.Name, ln[l.Name].ln, snap)
	}
	s.started = true
	s.ln = ln
	s.restartProbes(snap)
	s.metrics.SetVersion(snap.Version)
	return nil
}

// Reload applies cfg to the running server: it publishes a new configuration
// snapshot and reconciles listeners with it.
//
//   - a listener that is unchanged keeps its socket and accept loop untouched;
//   - a listener that is added (or whose address changed) is bound first, and
//     its accept loop starts only after the new snapshot is published, so no
//     connection is ever accepted against a snapshot that does not know it;
//   - a listener that is removed (or whose address changed) has its old socket
//     closed, so its port is released instead of lingering.
//
// Binding happens before the swap: if any new socket cannot be bound, the
// already-bound ones are released and NO snapshot is published — the server
// stays fully on the old configuration. Existing connections are never
// disturbed; they keep draining on the snapshot they captured.
func (s *Server) Reload(ctx context.Context, cfg *config.Config) (uint64, error) {
	if err := cfg.Validate(); err != nil {
		return 0, err
	}
	if err := s.rt.ValidatePlugins(cfg); err != nil {
		return 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.started {
		return 0, errors.New("proxy: Server not started")
	}
	if s.closing {
		return 0, errors.New("proxy: Server is shutting down")
	}

	// An address that a new or changed listener needs may still be held by an
	// old socket under another name (a renamed or removed listener). Such old
	// sockets are released first, so the bind can succeed, and restored if the
	// reload is rolled back.
	want := make(map[string]bool, len(cfg.Listeners))
	for _, l := range cfg.Listeners {
		if cur, ok := s.ln[l.Name]; ok && cur.cfgAddr == l.Addr {
			continue
		}
		if _, port, err := net.SplitHostPort(l.Addr); err == nil && port == "0" {
			continue // port 0 is an ephemeral pick, never a conflict
		}
		want[l.Addr] = true
	}
	released := make(map[string]boundListener)
	for name, cur := range s.ln {
		if want[cur.cfgAddr] {
			_ = cur.ln.Close()
			released[name] = cur
		}
	}

	next := make(map[string]boundListener, len(cfg.Listeners))
	opened := make([]string, 0, len(cfg.Listeners)) // names bound by this reload; their loops start after swap

	for _, l := range cfg.Listeners {
		if cur, ok := s.ln[l.Name]; ok && cur.cfgAddr == l.Addr {
			next[l.Name] = cur // unchanged: keep socket and loop
			continue
		}
		sock, err := (&net.ListenConfig{}).Listen(ctx, "tcp", l.Addr)
		if err != nil {
			for _, name := range opened {
				_ = next[name].ln.Close()
			}
			rerr := s.restoreReleased(ctx, released)
			return 0, errors.Join(fmt.Errorf("reload: listen %q on %s: %w", l.Name, l.Addr, err), rerr)
		}
		next[l.Name] = boundListener{cfgAddr: l.Addr, ln: sock}
		opened = append(opened, l.Name)
	}

	// Close sockets that are gone or whose address changed (their old accept
	// loops exit with net.ErrClosed). Done before the swap so no connection can
	// arrive on an address that is about to disappear. Released sockets are
	// already closed; closing them again is a harmless no-op.
	for name, cur := range s.ln {
		nl, ok := next[name]
		if !ok || nl.cfgAddr != cur.cfgAddr {
			_ = cur.ln.Close()
		}
	}

	v := s.rt.Reload(cfg)
	s.metrics.Reload(v)

	snap := s.rt.Load()
	for _, name := range opened {
		s.startAccept(name, next[name].ln, snap)
	}
	s.ln = next
	s.restartProbes(snap)
	return v, nil
}

// restoreReleased re-binds the sockets that a rolled-back Reload had released
// and restarts their accept loops on the still-published snapshot. Caller must
// hold s.mu. A socket that cannot be re-bound is dropped from s.ln and reported.
func (s *Server) restoreReleased(ctx context.Context, released map[string]boundListener) error {
	var errs []error
	for name, cur := range released {
		sock, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cur.cfgAddr)
		if err != nil {
			delete(s.ln, name)
			errs = append(errs, fmt.Errorf("restore %q on %s: %w", name, cur.cfgAddr, err))
			continue
		}
		s.ln[name] = boundListener{cfgAddr: cur.cfgAddr, ln: sock}
		s.startAccept(name, sock, s.rt.Load())
	}
	return errors.Join(errs...)
}

// Addrs returns the currently bound listener addresses, sorted for determinism
// (useful in tests with port 0). After Shutdown it is empty.
func (s *Server) Addrs() []net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	addrs := make([]net.Addr, 0, len(s.ln))
	for _, b := range s.ln {
		addrs = append(addrs, b.ln.Addr())
	}
	sort.Slice(addrs, func(i, j int) bool { return addrs[i].String() < addrs[j].String() })
	return addrs
}

// startAccept registers a new accept loop. Caller must hold s.mu (so the
// acceptWG.Add cannot race Shutdown's acceptWG.Wait).
func (s *Server) startAccept(name string, ln net.Listener, snap *Snapshot) {
	s.acceptWG.Add(1)
	go s.acceptLoop(name, ln)

	s.log.Info("listening",
		"listener", name,
		"addr", ln.Addr().String(),
		"pool", snap.listeners[name].cfg.UpstreamPool,
		"balance", snap.listeners[name].pool.balancer.Name(),
	)
}

// acceptLoop accepts connections on ln until the listener is closed, handing
// each connection to its own goroutine.
func (s *Server) acceptLoop(name string, ln net.Listener) {
	defer s.acceptWG.Done()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return // listener closed (shutdown or reload)
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue // transient, retry immediately
			}
			// Back off briefly so a persistent error cannot spin the CPU.
			s.log.Warn("accept error", "listener", name, "err", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}

		s.metrics.Accept()
		s.connWG.Add(1)
		go func() {
			defer s.connWG.Done()
			defer s.metrics.Close()
			// A panic in a handler (a bug, or later a WASM plugin) must not take
			// down the whole proxy: contain it to this connection.
			defer func() {
				if r := recover(); r != nil {
					s.log.Error("connection handler panicked",
						"listener", name,
						"panic", r,
						"stack", string(debug.Stack()),
					)
				}
			}()
			s.proxy.Handle(s.connCtx, name, conn)
		}()
	}
}

// Shutdown stops accepting and waits for in-flight connections to drain
// gracefully, up to the deadline in ctx. When the deadline expires the
// remaining connections are force-closed and ctx.Err() is returned, so the
// caller can tell a clean drain from a forced one.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil
	}
	s.closing = true
	toClose := s.ln
	s.ln = make(map[string]boundListener)
	s.mu.Unlock()

	// Stop accepting and stop active health probes. Wait for the accept loops
	// to finish before touching connWG: after they return, no new handlers can
	// be registered.
	for _, b := range toClose {
		_ = b.ln.Close()
	}
	s.stopProbes()
	s.acceptWG.Wait()

	done := make(chan struct{})
	go func() {
		s.connWG.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil // drained within the grace period
	case <-ctx.Done():
		s.connCancel() // grace expired: force-close what is left
		<-done
		return ctx.Err()
	}
}

// restartProbes stops any running health-check loops and starts new ones for
// pools in snap that configure health_check.interval > 0. Caller must hold s.mu.
func (s *Server) restartProbes(snap *Snapshot) {
	if s.probeCancel != nil {
		s.probeCancel()
		s.probeWG.Wait()
		s.probeCancel = nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.probeCancel = cancel
	for name, pc := range snap.Config.Pools {
		if pc.HealthCheck.Interval <= 0 {
			continue
		}
		ups := make([]*upstream, 0, len(pc.Servers))
		for _, addr := range pc.Servers {
			ups = append(ups, s.rt.registry.upstream(addr))
		}
		s.probeWG.Add(1)
		go s.probeLoop(ctx, name, ups, pc.HealthCheck)
	}
}

func (s *Server) stopProbes() {
	s.mu.Lock()
	cancel := s.probeCancel
	s.probeCancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		s.probeWG.Wait()
	}
}

func (s *Server) probeLoop(ctx context.Context, poolName string, ups []*upstream, hc config.HealthCheckConfig) {
	defer s.probeWG.Done()
	t := time.NewTicker(hc.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, u := range ups {
				probeOne(ctx, u, hc.Timeout)
			}
			_ = poolName
		}
	}
}

func probeOne(ctx context.Context, u *upstream, timeout time.Duration) {
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var d net.Dialer
	c, err := d.DialContext(pctx, "tcp", u.addr)
	if err != nil {
		u.MarkUnhealthy()
		return
	}
	_ = c.Close()
	u.MarkHealthy()
}
