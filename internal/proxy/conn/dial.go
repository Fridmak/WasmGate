package conn

import (
	"context"
	"errors"
	"net"
	"strconv"
	"syscall"
	"time"

	"wasmgate/internal/proxy/backend"
	pruntime "wasmgate/internal/proxy/runtime"
)

// selectRoutePool maps an ABI v4 verdict to route_pools. It accepts the raw
// low-32-bit verdict so once/headers callers share one implementation. It
// returns (nil, true) for allow/deny and (nil, false) for an invalid route index.
func selectRoutePool(rl pruntime.ResolvedListener, verdict int32) (*backend.Pool, bool) {
	if verdict < 2 {
		return nil, true
	}
	idx := int(verdict) - 2
	if idx < 0 || idx >= len(rl.RoutePools) || rl.RoutePools[idx] == nil {
		return nil, false
	}
	return rl.RoutePools[idx], true
}

func (p *Handler) dialWithRetry(ctx context.Context, listenerName string, targetPool *backend.Pool, timeout time.Duration) (*backend.Upstream, net.Conn, bool) {
	attempts := 1 + targetPool.DialRetries()
	var d net.Dialer
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			p.metrics.UpstreamRetry()
		}
		up := targetPool.Next()
		dialCtx, cancel := context.WithTimeout(ctx, timeout)
		server, err := d.DialContext(dialCtx, "tcp", up.Address())
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
		if ok, dropped := p.sampler.Allow("dial:" + listenerName + ":" + up.Address()); ok {
			p.log.Warn("dial upstream failed",
				"listener", listenerName,
				"upstream", up.Address(),
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
