package proxy

import "sync/atomic"

// upstream is a single backend endpoint plus its live connection counter.
//
// 32/64-bit safety note: we deliberately use the *typed* atomic wrappers
// (atomic.Int64, atomic.Uint64) instead of the raw free functions
// (atomic.AddInt64(&x, 1)). The typed wrappers embed an alignment guarantee,
// so they are correct on 32-bit targets (386, arm) where a plain int64 field
// is only 4-byte aligned — calling a raw 64-bit atomic on such a field would
// panic with "unaligned 64-bit atomic operation".
//
// Cache-line padding is intentionally NOT done yet. Every upstream has its own
// cache line only by luck right now; if profiling shows false sharing between
// hot counters, we will pad `active` to a full line. Deferred on purpose.
type upstream struct {
	addr      string
	active    atomic.Int64
	unhealthy atomic.Bool // false (zero value) = healthy
}

// Healthy reports whether the upstream is currently considered reachable.
func (u *upstream) Healthy() bool { return !u.unhealthy.Load() }

// MarkHealthy marks the upstream reachable.
func (u *upstream) MarkHealthy() { u.unhealthy.Store(false) }

// MarkUnhealthy marks the upstream unreachable so balancers skip it while
// healthy peers exist in the pool.
func (u *upstream) MarkUnhealthy() { u.unhealthy.Store(true) }

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
