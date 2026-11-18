package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"wasmgate/internal/observe"
	"wasmgate/internal/proxy/conn"
	pruntime "wasmgate/internal/proxy/runtime"
)

// Server binds the configured listeners and dispatches accepted connections to
// the Proxy.
//
// Lifecycle: create with New, call Start exactly once, then Shutdown (or
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
	log     *slog.Logger      // structured logger
	rt      *pruntime.Runtime // source of the current snapshot
	proxy   *conn.Handler     // shared by all accept loops
	metrics *observe.Metrics  // per-connection counters; nil = disabled
	// acceptSampler bounds WARN output for persistent accept errors (e.g. EMFILE).
	acceptSampler *observe.Limiter

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

// New builds a Server bound to the runtime rt. metrics may be nil, which
// disables the per-connection counters.
func New(log *slog.Logger, rt *pruntime.Runtime, metrics *observe.Metrics) *Server {
	connCtx, connCancel := context.WithCancel(context.Background())
	return &Server{
		log:           log,
		rt:            rt,
		proxy:         conn.New(log, rt, metrics),
		metrics:       metrics,
		acceptSampler: observe.NewLimiter(time.Second, 2),
		ln:            make(map[string]boundListener),
		connCtx:       connCtx,
		connCancel:    connCancel,
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
