package server

import (
	"errors"
	"net"
	"runtime/debug"
	"sort"
	"time"

	pruntime "wasmgate/internal/proxy/runtime"
)

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
func (s *Server) startAccept(name string, ln net.Listener, snap *pruntime.Snapshot) {
	s.acceptWG.Add(1)
	go s.acceptLoop(name, ln)
	rl, _ := snap.Listener(name)

	s.log.Info("listening",
		"listener", name,
		"addr", ln.Addr().String(),
		"pool", rl.Cfg.UpstreamPool,
		"balance", rl.Pool.BalancerName(),
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
			if ok, dropped := s.acceptSampler.Allow("accept:" + name); ok {
				s.log.Warn("accept error", "listener", name, "err", err, "suppressed", dropped)
			}
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
