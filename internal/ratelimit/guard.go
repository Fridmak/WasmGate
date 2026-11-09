package ratelimit

import (
	"net"
	"sync"
	"sync/atomic"
)

// Guard limits how many connections may be open concurrently per IP and per
// subnet. It is separate from Limiter because its counters must survive a
// rate-limit window change (Limiter is replaced when Window changes).
//
// The zero value is not usable; use NewGuard.
type Guard struct {
	hostLimit   atomic.Int64
	subnetLimit atomic.Int64

	ipv6HostPrefix   int
	ipv6SubnetPrefix int
	shards           [shardCount]guardShard
}

type guardShard struct {
	mu     sync.Mutex
	host   map[key]*guardCount
	subnet map[key]*guardCount
}

type guardCount struct {
	active int64
}

// NewGuard returns a concurrency guard. A zero prefix uses the same defaults as
// NewWithOptions (IPv6 host /64, subnet /56).
func NewGuard(hostLimit, subnetLimit int, ipv6HostPrefix, ipv6SubnetPrefix int) *Guard {
	g := &Guard{}
	g.ipv6HostPrefix, g.ipv6SubnetPrefix = normalizePrefixes(ipv6HostPrefix, ipv6SubnetPrefix)
	for i := range g.shards {
		g.shards[i].host = make(map[key]*guardCount)
		g.shards[i].subnet = make(map[key]*guardCount)
	}
	g.Update(hostLimit, subnetLimit)
	return g
}

// Update changes the concurrency limits in place without touching the active
// counts. A limit <= 0 disables that dimension.
func (g *Guard) Update(hostLimit, subnetLimit int) {
	g.hostLimit.Store(int64(hostLimit))
	g.subnetLimit.Store(int64(subnetLimit))
}

// Prefixes returns the IPv6 prefixes fixed at construction. A changed prefix
// requires a new Guard because it changes the key layout.
func (g *Guard) Prefixes() (host, subnet int) { return g.ipv6HostPrefix, g.ipv6SubnetPrefix }

// Acquire reserves one concurrent connection slot. It returns false when the
// per-IP or per-subnet limit is reached. Call Release exactly once for every
// successful Acquire.
func (g *Guard) Acquire(ip net.IP) bool {
	hostLimit := g.hostLimit.Load()
	subnetLimit := g.subnetLimit.Load()
	if hostLimit <= 0 && subnetLimit <= 0 {
		return true
	}

	hostKey := keyOf(ip, g.ipv6HostPrefix)
	if hostLimit > 0 && !g.acquireKey(hostKey, false, hostLimit) {
		return false
	}
	if subnetLimit > 0 {
		subnetKey := subnetKeyOf(hostKey, g.ipv6SubnetPrefix)
		if !g.acquireKey(subnetKey, true, subnetLimit) {
			if hostLimit > 0 {
				g.releaseKey(hostKey, false)
			}
			return false
		}
	}
	return true
}

// Release returns a slot reserved by Acquire. It is safe to call even if the
// limits were changed after Acquire: both keys are decremented (releaseKey is
// a no-op for an absent key), so a limit disabled by a reload cannot leave a
// stale active count behind.
func (g *Guard) Release(ip net.IP) {
	hostKey := keyOf(ip, g.ipv6HostPrefix)
	g.releaseKey(hostKey, false)
	g.releaseKey(subnetKeyOf(hostKey, g.ipv6SubnetPrefix), true)
}

func (g *Guard) acquireKey(k key, subnet bool, limit int64) bool {
	sh := &g.shards[shardOf(k)]
	sh.mu.Lock()
	keys := sh.host
	if subnet {
		keys = sh.subnet
	}
	c := keys[k]
	if c == nil {
		if len(keys) >= shardCap {
			evictGuardOne(keys)
		}
		c = &guardCount{}
		keys[k] = c
	}
	if c.active >= limit {
		sh.mu.Unlock()
		return false
	}
	c.active++
	sh.mu.Unlock()
	return true
}

func (g *Guard) releaseKey(k key, subnet bool) {
	sh := &g.shards[shardOf(k)]
	sh.mu.Lock()
	keys := sh.host
	if subnet {
		keys = sh.subnet
	}
	if c := keys[k]; c != nil && c.active > 0 {
		c.active--
	}
	sh.mu.Unlock()
}

func evictGuardOne(keys map[key]*guardCount) {
	for k, c := range keys {
		if c.active == 0 {
			delete(keys, k)
			return
		}
	}
	// All entries are active: drop one arbitrary entry. Its eventual Release
	// is a no-op, which is safer than letting the map grow without bound.
	for k := range keys {
		delete(keys, k)
		return
	}
}
