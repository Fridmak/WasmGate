package server

import "context"

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
