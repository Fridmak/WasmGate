package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"wasmgate/internal/config"
	"wasmgate/internal/observe"
	"wasmgate/internal/plugin/abi4"
	"wasmgate/internal/tlspeek"
)

// logWindow and logBurst bound how often per-connection "expected under load"
// events are logged, so a flood cannot turn the log into a denial of service.
const (
	logWindow = time.Second
	logBurst  = 2
)

// Proxy is the data-path handler: it turns an accepted client connection into a
// proxied session against an upstream, enforcing the listener's max_conns and
// per-IP sliding-window rate_limit before dialing the backend.
type Proxy struct {
	log     *slog.Logger     // structured logger
	rt      *Runtime         // source of the current snapshot
	sampler *observe.Limiter // rate-limits per-connection log events
	metrics *observe.Metrics // per-connection counters; nil = disabled
	nextID  atomic.Uint64    // connection id exposed to ABI v2 plugin hooks
}

// New returns a Proxy that reads configuration from rt on every connection.
// metrics may be nil, which disables the per-connection counters (every Metrics
// method is nil-safe).
func New(log *slog.Logger, rt *Runtime, metrics *observe.Metrics) *Proxy {
	return &Proxy{log: log, rt: rt, sampler: observe.NewLimiter(logWindow, logBurst), metrics: metrics}
}

// Handle proxies a single accepted client connection. It always closes client.
//
// ctx parents the whole session and is cancelled only on a hard stop (or after
// the shutdown grace expires); it is what force-closes the connection. The
// snapshot is captured once and used for the lifetime of the connection, so a
// configuration reload mid-connection cannot affect it.
func (p *Proxy) Handle(ctx context.Context, listenerName string, client net.Conn) {
	defer func() { _ = client.Close() }()

	snap := p.rt.Load()

	rl, ok := snap.listeners[listenerName]
	if !ok {
		p.log.Error("listener missing from snapshot", "listener", listenerName)
		return
	}
	if rl.pool == nil {
		p.log.Error("pool missing from snapshot", "listener", listenerName, "pool", rl.cfg.UpstreamPool)
		return
	}

	// Enforce the per-listener connection limit (nil limiter means unlimited).
	if rl.limiter != nil {
		if !rl.limiter.acquire() {
			p.metrics.RejectLimit()
			if ok, dropped := p.sampler.Allow("limit:" + listenerName); ok {
				p.log.Warn("connection limit reached",
					"listener", listenerName,
					"max_conns", rl.limiter.limit(),
					"suppressed", dropped,
				)
			}
			return
		}
		defer rl.limiter.release()
	}

	var peerIP net.IP
	var peerPort int
	if rl.rateLimiter != nil || rl.connGuard != nil || rl.pluginV4 != nil {
		peerIP, peerPort = clientEndpoint(client)
	}

	if rl.rateLimiter != nil {
		if !rl.rateLimiter.Allow(peerIP, time.Now()) {
			p.metrics.RejectRate()
			if ok, dropped := p.sampler.Allow("rate:" + listenerName); ok {
				p.log.Warn("per-IP connection rate exceeded",
					"listener", listenerName,
					"max", rl.rateLimiter.Max(),
					"window", rl.rateLimiter.Window(),
					"suppressed", dropped,
				)
			}
			return
		}
	}

	if rl.connGuard != nil {
		if !rl.connGuard.Acquire(peerIP) {
			p.metrics.RejectRate()
			if ok, dropped := p.sampler.Allow("concurrent:" + listenerName); ok {
				p.log.Warn("per-IP concurrent connection limit reached",
					"listener", listenerName,
					"suppressed", dropped,
				)
			}
			return
		}
		defer rl.connGuard.Release(peerIP)
	}

	var connID uint64
	if rl.pluginV4 != nil {
		connID = p.nextID.Add(1)
	}
	var pluginAccepted bool
	var closeRx, closeTx int64
	defer func() {
		if !pluginAccepted || rl.pluginV4 == nil {
			return
		}
		// Pair on_close with on_accept even when the connection exits before the
		// relay starts (deny, TLS failure, header timeout, dial failure).
		if err := rl.pluginV4.OnCloseConn(context.Background(), connID, peerIP, closeRx, closeTx); err != nil {
			p.metrics.PluginDiscard()
		}
	}()
	if rl.pluginV4 != nil {
		verdict, err := rl.pluginV4.OnAccept(ctx, peerIP, peerPort)
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

	if rl.tlsConfig != nil {
		tlsConn := tls.Server(client, rl.tlsConfig)
		hsTimeout := rl.cfg.TLS.HandshakeTimeout
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

	targetPool := rl.pool
	if len(rl.sniRoutes) > 0 && rl.tlsConfig == nil {
		sni, wrapped, err := tlspeek.Peek(client, rl.cfg.DialTimeout)
		if err != nil {
			return
		}
		client = wrapped
		if sni != "" {
			if matched, ok := tlspeek.MatchRoute(rl.sniRoutes, sni); ok && matched != nil {
				targetPool = matched
			}
		}
	}

	// L7 inspection before dialing. All data modes are ABI v4; ABI v3 is used
	// only for the L4 on_accept/on_close lifecycle path.
	var streamHook streamInspector
	if rl.pluginV4 != nil {
		switch rl.cfg.Plugin.DataMode {
		case config.DataModeHeaders:
			_, wrapped, err := tlspeek.PeekChunk(client, rl.cfg.DialTimeout)
			if err != nil {
				return
			}
			pc, ok := wrapped.(*tlspeek.Conn)
			if !ok {
				p.log.Error("headers mode: peek did not return a buffered connection", "listener", listenerName)
				return
			}
			client = pc
			_, headers, err := readHeaderBlock(pc, rl.cfg.Plugin.MaxHeaderBytes, rl.cfg.Plugin.InspectTimeout)
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
			verdict, err := rl.pluginV4.OnRequestHeaders(ctx, connID, headers)
			if err != nil {
				p.metrics.PluginDiscard()
			}
			if err != nil || verdict == abi4.VerdictDeny {
				p.metrics.RejectPlugin()
				if ok, dropped := p.sampler.Allow("headers_plugin:" + listenerName); ok {
					p.log.Warn("request headers rejected by wasm on_event hook",
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

		case config.DataModeOnce:
			chunk, wrapped, err := tlspeek.PeekChunk(client, rl.cfg.DialTimeout)
			if err != nil {
				return
			}
			client = wrapped
			verdict, err := rl.pluginV4.OnOpaqueFirst(ctx, connID, chunk)
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

		case config.DataModeStream:
			eng := rl.pluginV4
			streamHook = func(ctx context.Context, connID uint64, dir int32, chunk []byte) (int32, error) {
				verdict, err := eng.OnOpaqueChunk(ctx, connID, dir, 0, chunk)
				return int32(verdict), err
			}

		case "":
			// L4-only lifecycle listener: on_accept already ran, no L7 phase.

		default:
			p.log.Error("unsupported ABI v4 data_mode", "listener", listenerName, "data_mode", rl.cfg.Plugin.DataMode)
			return
		}
	}

	up, server, ok := p.dialWithRetry(ctx, listenerName, targetPool, rl.cfg.DialTimeout)
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
		if rl.cfg.DialTimeout > 0 {
			_ = server.SetWriteDeadline(time.Now().Add(rl.cfg.DialTimeout))
		}
		raw, n, err := pc.DrainTo(server)
		if rl.cfg.DialTimeout > 0 {
			_ = server.SetWriteDeadline(time.Time{})
		}
		if err != nil {
			return
		}
		client = raw
		prefix = n
	}

	p.log.Debug("proxying", "listener", listenerName, "pool", targetPool.Name(), "upstream", up.addr)

	rx, tx := join(ctx, client, server, rl.cfg.IdleTimeout, streamHook, p.metrics, connID)
	tx += prefix
	closeRx, closeTx = rx, tx
	p.metrics.AddBytes(rx + tx)
}

// selectRoutePool maps an ABI v4 verdict to route_pools. It accepts the raw
// low-32-bit verdict so once/headers callers share one implementation. It
// returns (nil, true) for allow/deny and (nil, false) for an invalid route index.
func selectRoutePool(rl resolvedListener, verdict int32) (*pool, bool) {
	if verdict < 2 {
		return nil, true
	}
	idx := int(verdict) - 2
	if idx < 0 || idx >= len(rl.routePools) || rl.routePools[idx] == nil {
		return nil, false
	}
	return rl.routePools[idx], true
}

func (p *Proxy) dialWithRetry(ctx context.Context, listenerName string, targetPool *pool, timeout time.Duration) (*upstream, net.Conn, bool) {
	attempts := 1 + targetPool.dialRetries
	var d net.Dialer
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			p.metrics.UpstreamRetry()
		}
		up := targetPool.Next()
		dialCtx, cancel := context.WithTimeout(ctx, timeout)
		server, err := d.DialContext(dialCtx, "tcp", up.addr)
		cancel()
		if err == nil {
			up.MarkHealthy()
			return up, server, true
		}
		if !localDialError(ctx, err) {
			up.MarkUnhealthy()
		}
		targetPool.Release(up)
		p.metrics.DialFail()
		if ok, dropped := p.sampler.Allow("dial:" + listenerName + ":" + up.addr); ok {
			p.log.Warn("dial upstream failed",
				"listener", listenerName,
				"upstream", up.addr,
				"err", err,
				"suppressed", dropped,
			)
		}
	}
	return nil, nil, false
}

// localDialError reports an error caused by the local host rather than by the
// upstream: fd/memory exhaustion, address assignment, or parent cancellation.
// Such failures must not mark a healthy backend unhealthy.
func localDialError(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return true
	}
	return errors.Is(err, syscall.EMFILE) ||
		errors.Is(err, syscall.ENFILE) ||
		errors.Is(err, syscall.ENOBUFS) ||
		errors.Is(err, syscall.ENOMEM) ||
		errors.Is(err, syscall.EADDRNOTAVAIL) ||
		errors.Is(err, context.Canceled)
}

// clientEndpoint returns the peer IP and port without allocating on the TCP path.
func clientEndpoint(c net.Conn) (net.IP, int) {
	if ta, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		return ta.IP, ta.Port
	}
	host, portStr, err := net.SplitHostPort(c.RemoteAddr().String())
	if err != nil {
		return nil, 0
	}
	port, _ := strconv.Atoi(portStr)
	return net.ParseIP(host), port
}
