package proxy

import (
	"context"
	"log/slog"
	"net"
	"time"

	"wasmgate/internal/observe"
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
	defer client.Close()

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

	if rl.rateLimiter != nil {
		if !rl.rateLimiter.Allow(clientIP(client), time.Now()) {
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

	up := rl.pool.Next()
	defer rl.pool.Release(up)

	dialCtx, cancel := context.WithTimeout(ctx, rl.cfg.DialTimeout)
	defer cancel()

	var d net.Dialer
	server, err := d.DialContext(dialCtx, "tcp", up.addr)
	if err != nil {
		p.metrics.DialFail()
		if ok, dropped := p.sampler.Allow("dial:" + listenerName + ":" + up.addr); ok {
			p.log.Warn("dial upstream failed",
				"listener", listenerName,
				"upstream", up.addr,
				"err", err,
				"suppressed", dropped,
			)
		}
		return
	}
	defer server.Close()

	p.log.Debug("proxying", "listener", listenerName, "pool", rl.pool.Name(), "upstream", up.addr)

	rx, tx := join(ctx, client, server, rl.cfg.IdleTimeout)
	p.metrics.AddBytes(rx + tx)
}

// clientIP returns the peer IP without allocating on the TCP path.
func clientIP(c net.Conn) net.IP {
	if ta, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		return ta.IP
	}
	host, _, err := net.SplitHostPort(c.RemoteAddr().String())
	if err != nil {
		return nil
	}
	return net.ParseIP(host)
}
