package conn

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"sync/atomic"
	"time"

	"wasmgate/internal/observe"
	abi4 "wasmgate/internal/plugin/abi4/engine"
	"wasmgate/internal/proxy/httpinspect"
	"wasmgate/internal/proxy/relay"
	pruntime "wasmgate/internal/proxy/runtime"
	"wasmgate/internal/tlspeek"
)

// logWindow and logBurst bound how often per-connection "expected under load"
// events are logged, so a flood cannot turn the log into a denial of service.
const (
	logWindow = time.Second
	logBurst  = 2
)

// Handler is the data-path handler: it turns an accepted client connection into a
// proxied session against an upstream, enforcing the listener's max_conns and
// per-IP sliding-window rate_limit before dialing the backend.
type Handler struct {
	log     *slog.Logger      // structured logger
	rt      *pruntime.Runtime // source of the current snapshot
	sampler *observe.Limiter  // rate-limits per-connection log events
	metrics *observe.Metrics  // per-connection counters; nil = disabled
	nextID  atomic.Uint64     // connection id exposed to ABI v4 plugin hooks
}

// New returns a Handler that reads configuration from rt on every connection.
// metrics may be nil, which disables the per-connection counters (every Metrics
// method is nil-safe).
func New(log *slog.Logger, rt *pruntime.Runtime, metrics *observe.Metrics) *Handler {
	return &Handler{log: log, rt: rt, sampler: observe.NewLimiter(logWindow, logBurst), metrics: metrics}
}

// Handle proxies a single accepted client connection. It always closes client.
//
// ctx parents the whole session and is cancelled only on a hard stop (or after
// the shutdown grace expires); it is what force-closes the connection. The
// snapshot is captured once and used for the lifetime of the connection, so a
// configuration reload mid-connection cannot affect it.
func (p *Handler) Handle(ctx context.Context, listenerName string, client net.Conn) {
	defer func() { _ = client.Close() }()

	// Pin the snapshot for the whole connection. A reload may retire it while
	// the connection is still running, but its engines stay alive until this
	// release.
	snap := p.rt.Acquire()
	if snap == nil {
		return
	}
	defer snap.Release()

	rl, ok := snap.Listener(listenerName)
	if !ok {
		p.log.Error("listener missing from snapshot", "listener", listenerName)
		return
	}
	if rl.Pool == nil {
		p.log.Error("pool missing from snapshot", "listener", listenerName, "pool", rl.Cfg.UpstreamPool)
		return
	}

	// Enforce the per-listener connection limit (nil limiter means unlimited).
	if rl.Limiter != nil {
		if !rl.Limiter.Acquire() {
			p.metrics.RejectLimit()
			if ok, dropped := p.sampler.Allow("limit:" + listenerName); ok {
				p.log.Warn("connection limit reached",
					"listener", listenerName,
					"max_conns", rl.Limiter.Limit(),
					"suppressed", dropped,
				)
			}
			return
		}
		defer rl.Limiter.Release()
	}

	var peerIP net.IP
	var peerPort int
	if rl.RateLimiter != nil || rl.ConnGuard != nil || rl.PluginV4 != nil {
		peerIP, peerPort = clientEndpoint(client)
	}

	if rl.RateLimiter != nil {
		if !rl.RateLimiter.Allow(peerIP, time.Now()) {
			p.metrics.RejectRate()
			if ok, dropped := p.sampler.Allow("rate:" + listenerName); ok {
				p.log.Warn("per-IP connection rate exceeded",
					"listener", listenerName,
					"max", rl.RateLimiter.Max(),
					"window", rl.RateLimiter.Window(),
					"suppressed", dropped,
				)
			}
			return
		}
	}

	if rl.ConnGuard != nil {
		if !rl.ConnGuard.Acquire(peerIP) {
			p.metrics.RejectRate()
			if ok, dropped := p.sampler.Allow("concurrent:" + listenerName); ok {
				p.log.Warn("per-IP concurrent connection limit reached",
					"listener", listenerName,
					"suppressed", dropped,
				)
			}
			return
		}
		defer rl.ConnGuard.Release(peerIP)
	}

	var connID uint64
	if rl.PluginV4 != nil {
		connID = p.nextID.Add(1)
	}
	var pluginAccepted bool
	var closeRx, closeTx int64
	defer func() {
		if !pluginAccepted || rl.PluginV4 == nil {
			return
		}
		// Pair on_close with an accepted connection even if TLS, inspection, or
		// the upstream dial fails before the relay starts.
		if err := rl.PluginV4.OnCloseConn(context.Background(), connID, peerIP, closeRx, closeTx); err != nil {
			p.metrics.PluginDiscard()
		}
	}()
	if rl.PluginV4 != nil {
		verdict, err := rl.PluginV4.OnAccept(ctx, peerIP, peerPort)
		if err != nil {
			p.metrics.PluginDiscard()
		}
		if err != nil || verdict != abi4.VerdictAllow {
			p.metrics.RejectPlugin()
			if ok, dropped := p.sampler.Allow("plugin:" + listenerName); ok {
				p.log.Warn("connection rejected by wasm plugin",
					"listener", listenerName,
					"err", err,
					"suppressed", dropped,
				)
			}
			return
		}
		pluginAccepted = true
	}

	if rl.TLSConfig != nil {
		tlsConn := tls.Server(client, rl.TLSConfig)
		hsTimeout := rl.Cfg.TLS.HandshakeTimeout
		if hsTimeout <= 0 {
			hsTimeout = 10 * time.Second
		}
		hsCtx, cancel := context.WithTimeout(ctx, hsTimeout)
		err := tlsConn.HandshakeContext(hsCtx)
		cancel()
		if err != nil {
			p.log.Debug("tls handshake failed", "listener", listenerName, "err", err)
			return
		}
		client = tlsConn
	}

	targetPool := rl.Pool
	if len(rl.SNIRoutes) > 0 && rl.TLSConfig == nil {
		sni, wrapped, err := tlspeek.Peek(client, rl.Cfg.DialTimeout)
		if err != nil {
			return
		}
		client = wrapped
		if sni != "" {
			if matched, ok := tlspeek.MatchRoute(rl.SNIRoutes, sni); ok && matched != nil {
				targetPool = matched
			}
		}
	}

	// L7 inspection before dialing. The HTTP exchange path is opt-in and does
	// not participate in the opaque relay or the header-only splice path.
	var streamHook relay.Inspector
	var exchange *httpinspect.Exchange
	if rl.PluginV4 != nil {
		switch rl.Pipeline.Mode {
		case pruntime.PluginHeaders:
			_, wrapped, err := tlspeek.PeekChunk(client, rl.Cfg.DialTimeout)
			if err != nil {
				return
			}
			pc, ok := wrapped.(*tlspeek.Conn)
			if !ok {
				p.log.Error("headers mode: peek did not return a buffered connection", "listener", listenerName)
				return
			}
			client = pc
			prefix, headers, err := httpinspect.ReadHeaderBlock(pc, rl.Cfg.Plugin.MaxHeaderBytes, rl.Cfg.Plugin.InspectTimeout)
			if err != nil {
				p.metrics.HeaderIncomplete()
				if ok, dropped := p.sampler.Allow("headers:" + listenerName); ok {
					p.log.Warn("request header block rejected",
						"listener", listenerName,
						"err", err,
						"suppressed", dropped,
					)
				}
				return
			}
			var verdict abi4.Verdict
			if rl.Pipeline.Body {
				exchange = httpinspect.NewExchange(rl.PluginV4, connID, rl.Cfg.Plugin, rl.Pipeline.ReplaceBody, p.metrics)
				verdict, err = exchange.PrepareRequest(ctx, headers, prefix[len(headers):])
			} else {
				verdict, err = rl.PluginV4.OnRequestHeaders(ctx, connID, headers)
				if err != nil {
					p.metrics.PluginDiscard()
				}
			}
			if err != nil || verdict == abi4.VerdictDeny {
				p.metrics.RejectPlugin()
				if ok, dropped := p.sampler.Allow("headers_plugin:" + listenerName); ok {
					p.log.Warn("request rejected during HTTP inspection",
						"listener", listenerName,
						"err", err,
						"suppressed", dropped,
					)
				}
				return
			}
			if routed, ok := selectRoutePool(rl, int32(verdict)); !ok {
				p.metrics.RejectPlugin()
				p.log.Warn("wasm on_event selected an unknown route pool", "listener", listenerName, "verdict", verdict)
				return
			} else if routed != nil {
				targetPool = routed
			}

		case pruntime.PluginOnce:
			chunk, wrapped, err := tlspeek.PeekChunk(client, rl.Cfg.DialTimeout)
			if err != nil {
				return
			}
			client = wrapped
			verdict, err := rl.PluginV4.OnOpaqueFirst(ctx, connID, chunk)
			if err != nil {
				p.metrics.PluginDiscard()
			}
			if err != nil || verdict == abi4.VerdictDeny {
				p.metrics.RejectPlugin()
				if ok, dropped := p.sampler.Allow("plugin_data:" + listenerName); ok {
					p.log.Warn("payload rejected by wasm on_event hook",
						"listener", listenerName,
						"err", err,
						"suppressed", dropped,
					)
				}
				return
			}
			if routed, ok := selectRoutePool(rl, int32(verdict)); !ok {
				p.metrics.RejectPlugin()
				p.log.Warn("wasm on_event selected an unknown route pool", "listener", listenerName, "verdict", verdict)
				return
			} else if routed != nil {
				targetPool = routed
			}

		case pruntime.PluginStream:
			eng := rl.PluginV4
			streamHook = func(ctx context.Context, connID uint64, dir int32, chunk []byte) (int32, error) {
				verdict, err := eng.OnOpaqueChunk(ctx, connID, dir, 0, chunk)
				return int32(verdict), err
			}

		case pruntime.PluginLifecycle:
			// L4-only lifecycle listener: on_accept already ran, no L7 phase.

		default:
			p.log.Error("unsupported resolved plugin mode", "listener", listenerName, "mode", rl.Pipeline.Mode)
			return
		}
	}

	up, server, ok := p.dialWithRetry(ctx, listenerName, targetPool, rl.Cfg.DialTimeout)
	if !ok {
		return
	}
	defer targetPool.Release(up)
	defer server.Close()

	// Replay the peeked prefix synchronously, before join starts. This removes
	// the only shared *tlspeek.Conn between the two copy goroutines: after this
	// point both directions use raw net.Conns, so splice(2) is preserved and no
	// Close/Read race is possible.
	var prefix int64
	if pc, ok := client.(*tlspeek.Conn); ok {
		if streamHook != nil && len(pc.Bytes()) > 0 {
			verdict, err := streamHook(ctx, connID, 0, pc.Bytes())
			if err != nil {
				p.metrics.PluginDiscard()
			}
			if err != nil || verdict != 0 {
				p.metrics.RejectPlugin()
				return
			}
		}
		if rl.Cfg.DialTimeout > 0 {
			_ = server.SetWriteDeadline(time.Now().Add(rl.Cfg.DialTimeout))
		}
		raw, n, err := pc.DrainTo(server)
		if rl.Cfg.DialTimeout > 0 {
			_ = server.SetWriteDeadline(time.Time{})
		}
		if err != nil {
			return
		}
		client = raw
		prefix = n
	}

	p.log.Debug("proxying", "listener", listenerName, "pool", targetPool.Name(), "upstream", up.Address())

	var rx, tx int64
	if exchange != nil {
		rx, tx = exchange.Join(ctx, client, server, rl.Cfg.IdleTimeout)
	} else {
		rx, tx = relay.Join(ctx, client, server, rl.Cfg.IdleTimeout, streamHook, p.metrics, connID)
	}
	tx += prefix
	closeRx, closeTx = rx, tx
	p.metrics.AddBytes(rx + tx)
}
