package server

import (
	"context"
	"net"
	"time"

	"wasmgate/internal/config"
	"wasmgate/internal/proxy/backend"
	pruntime "wasmgate/internal/proxy/runtime"
)

// restartProbes stops any running health-check loops and starts new ones for
// pools in snap that configure health_check.interval > 0. Caller must hold s.mu.
func (s *Server) restartProbes(snap *pruntime.Snapshot) {
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
		ups := make([]*backend.Upstream, 0, len(pc.Servers))
		for _, addr := range pc.Servers {
			ups = append(ups, s.rt.Upstream(addr))
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

func (s *Server) probeLoop(ctx context.Context, poolName string, ups []*backend.Upstream, hc config.HealthCheckConfig) {
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

func probeOne(ctx context.Context, u *backend.Upstream, timeout time.Duration) {
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var d net.Dialer
	c, err := d.DialContext(pctx, "tcp", u.Address())
	if err != nil {
		u.MarkUnhealthy()
		return
	}
	_ = c.Close()
	u.MarkHealthy()
}
