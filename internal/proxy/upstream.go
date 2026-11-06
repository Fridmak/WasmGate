package proxy

import (
	"sync/atomic"
	"time"

	"wasmgate/internal/config"
)

// upstream is a single backend endpoint plus its live connection counter and
// lock-free three-state circuit breaker (Closed -> Open -> Half-Open).
//
// 32/64-bit safety note: we deliberately use the *typed* atomic wrappers
// (atomic.Int64, atomic.Uint64) instead of the raw free functions
// (atomic.AddInt64(&x, 1)). The typed wrappers embed an alignment guarantee,
// so they are correct on 32-bit targets (386, arm) where a plain int64 field
// is only 4-byte aligned — calling a raw 64-bit atomic on such a field would
// panic with "unaligned 64-bit atomic operation".
type upstream struct {
	addr      string
	active    atomic.Int64
	unhealthy atomic.Bool // false (zero value) = healthy

	cbThreshold   atomic.Int32 // error_threshold; <= 1 opens immediately on 1st failure
	cbOpenNano    atomic.Int64 // open_duration in ns; 0 means stay unhealthy until probe/success
	cbHalfOpenMax atomic.Int32 // half_open_requests needed to close the breaker

	consecFails  atomic.Int32
	openUntil    atomic.Int64 // >0: Open until UnixNano; -1: Half-Open; 0: Closed
	halfOpenSucc atomic.Int32
}

// ConfigureBreaker updates the circuit-breaker parameters in place.
func (u *upstream) ConfigureBreaker(cb config.CircuitBreakerConfig) {
	u.cbThreshold.Store(int32(cb.ErrorThreshold)) // #nosec G115 -- small threshold from config
	u.cbOpenNano.Store(int64(cb.OpenDuration))
	u.cbHalfOpenMax.Store(int32(cb.HalfOpenRequests)) // #nosec G115 -- small probe count from config
}

// Healthy reports whether the upstream is currently eligible for traffic. When
// an Open circuit breaker's timeout expires, Healthy transitions the upstream
// to Half-Open and returns true so probe requests can test recovery.
func (u *upstream) Healthy() bool {
	if !u.unhealthy.Load() {
		return true
	}
	until := u.openUntil.Load()
	if until > 0 && time.Now().UnixNano() >= until {
		if u.openUntil.CompareAndSwap(until, -1) {
			u.halfOpenSucc.Store(0)
			u.unhealthy.Store(false)
		}
		return true
	}
	return false
}

// MarkHealthy records a successful dial or probe. In Half-Open state, the
// circuit closes once cbHalfOpenMax consecutive successes have landed.
func (u *upstream) MarkHealthy() {
	u.consecFails.Store(0)
	if u.openUntil.Load() == -1 {
		need := u.cbHalfOpenMax.Load()
		if need <= 1 || u.halfOpenSucc.Add(1) >= need {
			u.openUntil.Store(0)
			u.halfOpenSucc.Store(0)
			u.unhealthy.Store(false)
		}
		return
	}
	u.openUntil.Store(0)
	u.unhealthy.Store(false)
}

// MarkUnhealthy records a failed dial or probe. When consecFails reaches the
// configured error_threshold (or immediately when error_threshold <= 1 or while
// in Half-Open), the circuit opens for open_duration.
func (u *upstream) MarkUnhealthy() {
	fails := u.consecFails.Add(1)
	thresh := u.cbThreshold.Load()
	inHalfOpen := u.openUntil.Load() == -1
	if thresh <= 1 || fails >= thresh || inHalfOpen {
		u.halfOpenSucc.Store(0)
		if openNano := u.cbOpenNano.Load(); openNano > 0 {
			u.openUntil.Store(time.Now().UnixNano() + openNano)
		} else {
			u.openUntil.Store(0)
		}
		u.unhealthy.Store(true)
	}
}

// Balancer selects the next upstream for a new connection.
//
// This is an extension point: new strategies (weighted RR, hash by client IP,
// least-latency, ...) implement this interface without touching the data path.
//
// Next must be called with a non-empty server slice (guaranteed by config
// validation), so selection itself cannot fail. When health-checks arrive, a
// balancer may need to skip unhealthy upstreams and this may grow an error
// return.
type Balancer interface {
	Next(servers []*upstream) *upstream
	Name() string
}

// newBalancer maps a config balance value to a strategy. Anything other than
// "least_conn" yields round-robin. Config validation is the real gatekeeper: an
// unknown value (e.g. "least_con") is rejected by Load long before reaching
// here, so the default branch is only a safety net.
func newBalancer(kind string) Balancer {
	if kind == "least_conn" {
		return &leastConn{}
	}
	return &roundRobin{}
}

// roundRobin cycles through the upstreams.
type roundRobin struct {
	next atomic.Uint64
}

// Name identifies the strategy (see Balancer).
func (b *roundRobin) Name() string { return "round_robin" }

// Next advances the cursor and returns the next healthy upstream in
// servers[cursor % len]. If every upstream in the pool is currently marked
// unhealthy, it fails open and returns the cursor's pick so traffic can still
// probe recovery.
func (b *roundRobin) Next(servers []*upstream) *upstream {
	n := uint64(len(servers))
	i := b.next.Add(1) - 1
	first := servers[i%n]
	if first.Healthy() {
		return first
	}
	for step := uint64(1); step < n; step++ {
		if u := servers[(i+step)%n]; u.Healthy() {
			return u
		}
	}
	return first
}

// leastConn picks the upstream with the fewest active connections.
type leastConn struct{}

// Name identifies the strategy (see Balancer).
func (b *leastConn) Name() string { return "least_conn" }

// Next returns the healthy upstream with the smallest active count; ties go to
// the first in config order. Fails open across all servers if none are healthy.
func (b *leastConn) Next(servers []*upstream) *upstream {
	var best *upstream
	var bestActive int64
	for _, s := range servers {
		if !s.Healthy() {
			continue
		}
		a := s.active.Load()
		if best == nil || a < bestActive {
			best, bestActive = s, a
		}
	}
	if best != nil {
		return best
	}
	best = servers[0]
	bestActive = best.active.Load()
	for _, s := range servers[1:] {
		if a := s.active.Load(); a < bestActive {
			best, bestActive = s, a
		}
	}
	return best
}
