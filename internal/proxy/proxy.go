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

	"wasmgate/internal/observe"
	"wasmgate/internal/plugin"
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
	if rl.rateLimiter != nil || rl.connGuard != nil || rl.plugin != nil {
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
	if rl.plugin != nil {
		connID = p.nextID.Add(1)
		verdict, err := rl.plugin.OnAccept(ctx, peerIP, peerPort)
		if err != nil {
			p.metrics.PluginDiscard()
		}
		if err != nil || verdict != plugin.VerdictAllow {
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

	// L7 first-packet inspection ("once" mode, the default when on_data is
	// exported): inspect (and optionally mutate or route) the initial client
	// request in the WASM sandbox BEFORE dialing the upstream, then DrainTo
	// replays the packet and hands the raw socket back to splice(2) for the
	// rest of the connection.
	var streamPlugin *plugin.Plugin
	if rl.plugin != nil && rl.plugin.HasData() {
		if rl.cfg.Plugin.DataMode == "stream" {
			streamPlugin = rl.plugin
		} else {
			chunk, wrapped, err := tlspeek.PeekChunk(client, rl.cfg.DialTimeout)
			if err != nil {
				return
			}
			client = wrapped
			verdict, err := rl.plugin.OnDataConn(ctx, connID, 0, chunk)
			if err != nil {
				p.metrics.PluginDiscard()
			}
			if err != nil || verdict == plugin.VerdictDeny {
				p.metrics.RejectPlugin()
				if ok, dropped := p.sampler.Allow("plugin_data:" + listenerName); ok {
					p.log.Warn("payload rejected by wasm on_data hook",
						"listener", listenerName,
						"err", err,
						"suppressed", dropped,
					)
				}
				return
			}
			if verdict >= plugin.Verdict(2) {
				idx := int(verdict) - 2
				if idx < 0 || idx >= len(rl.routePools) || rl.routePools[idx] == nil {
					p.metrics.RejectPlugin()
					p.log.Warn("wasm on_data selected an unknown route pool", "listener", listenerName, "verdict", verdict)
					return
				}
				targetPool = rl.routePools[idx]
			}
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
		if streamPlugin != nil && len(pc.Bytes()) > 0 {
			verdict, err := streamPlugin.OnDataConn(ctx, connID, 0, pc.Bytes())
			if err != nil {
				p.metrics.PluginDiscard()
			}
			if err != nil || verdict != plugin.VerdictAllow {
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

	rx, tx := join(ctx, client, server, rl.cfg.IdleTimeout, streamPlugin, p.metrics, connID)
	tx += prefix
	p.metrics.AddBytes(rx + tx)
	if rl.plugin != nil {
		if err := rl.plugin.OnCloseConn(ctx, connID, peerIP, rx, tx); err != nil {
			p.metrics.PluginDiscard()
		}
	}
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
