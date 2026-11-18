package server

import (
	"context"
	"errors"
	"fmt"
	"net"

	"wasmgate/internal/config"
)

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

	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.started {
		return 0, errors.New("proxy: Server not started")
	}
	if s.closing {
		return 0, errors.New("proxy: Server is shutting down")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := s.rt.ValidatePlugins(cfg); err != nil {
		return 0, err
	}
	// Binding and rollback must not be aborted just because the HTTP caller
	// disconnected after socket changes began: otherwise a cancelled POST
	// /reload could leave a listener missing.
	bindCtx := context.WithoutCancel(ctx)

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
		sock, err := (&net.ListenConfig{}).Listen(bindCtx, "tcp", l.Addr)
		if err != nil {
			for _, name := range opened {
				_ = next[name].ln.Close()
			}
			rerr := s.restoreReleased(bindCtx, released)
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

	v, err := s.rt.ReloadChecked(cfg)
	if err != nil {
		for _, name := range opened {
			_ = next[name].ln.Close()
		}
		rerr := s.restoreReleased(bindCtx, released)
		return 0, errors.Join(fmt.Errorf("reload: build snapshot: %w", err), rerr)
	}
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
