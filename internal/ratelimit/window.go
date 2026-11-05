// Package ratelimit limits how many connections one client IP may open within a
// sliding window. It runs on the accept path, after accept() and before the
// upstream dial, so it limits connection floods; it does not stop volumetric
// attacks, which need a layer below user space (see docs/04, F4).
package ratelimit

import (
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	slotCount  = 10   // buckets per window: the window is slotCount slots long
	shardCount = 64   // independent tables, so accepts from different IPs rarely contend
	shardCap   = 4096 // keys per shard before the shard is cleared (bounds memory)
)

// Limiter counts connections per IP over a sliding window of slotCount buckets.
// The limit can change without a reset (SetMax); the window length is fixed at
// construction, because the bucket layout depends on it.
type Limiter struct {
	max    atomic.Int64
	window time.Duration
	bucket int64 // slot length in nanoseconds
	shards [shardCount]shard
}

type shard struct {
	mu   sync.Mutex
	keys map[[16]byte]*ipWindow
}

// ipWindow is the ring of per-slot counters for one IP. A slot is reused for a
// new epoch by overwriting it, so the ring never grows.
type ipWindow struct {
	slots [slotCount]slot
}

type slot struct {
	epoch int64 // slot index in units of bucket length
	n     int64 // connections counted in this slot
}

// New returns a limiter allowing max connections per IP per window. max <= 0
// disables the check. window must be at least slotCount nanoseconds (the config
// layer enforces a far larger minimum).
func New(max int, window time.Duration) *Limiter {
	bucket := int64(window) / slotCount
	if bucket <= 0 {
		bucket = 1
	}
	l := &Limiter{window: window, bucket: bucket}
	l.max.Store(int64(max))
	for i := range l.shards {
		l.shards[i].keys = make(map[[16]byte]*ipWindow)
	}
	return l
}

// SetMax changes the limit at runtime. Counted connections are kept, so a
// lower limit takes effect immediately without resetting the window.
func (l *Limiter) SetMax(max int) { l.max.Store(int64(max)) }

// Max returns the current limit (<= 0 means disabled).
func (l *Limiter) Max() int { return int(l.max.Load()) }

// Window returns the window length configured at construction.
func (l *Limiter) Window() time.Duration { return l.window }

// Allow records one connection from ip at time now and reports whether the IP
// is still within the limit. A refused connection is counted too, so a client
// that keeps hammering stays refused until its rate drops.
//
// Steady state allocates nothing. A new IP allocates one window record; that is
// the only allocation and it happens once per IP per shard reset.
func (l *Limiter) Allow(ip net.IP, now time.Time) bool {
	max := l.max.Load()
	if max <= 0 {
		return true
	}
	key := keyOf(ip)
	e := now.UnixNano() / l.bucket

	sh := &l.shards[shardOf(key)]
	sh.mu.Lock()
	w := sh.keys[key]
	if w == nil {
		if len(sh.keys) >= shardCap {
			clear(sh.keys)
		}
		w = &ipWindow{}
		sh.keys[key] = w
	}
	s := &w.slots[e%slotCount]
	if s.epoch != e {
		s.epoch = e
		s.n = 0
	}
	s.n++
	var total int64
	for i := range w.slots {
		if w.slots[i].epoch > e-slotCount {
			total += w.slots[i].n
		}
	}
	sh.mu.Unlock()
	return total <= max
}

// keyOf maps an IP to a fixed 16-byte key. IPv4 and its IPv4-mapped IPv6 form
// get the same key. To4 does not allocate.
func keyOf(ip net.IP) [16]byte {
	var k [16]byte
	if v4 := ip.To4(); v4 != nil {
		k[10], k[11] = 0xff, 0xff
		copy(k[12:], v4)
		return k
	}
	copy(k[:], ip)
	return k
}

func shardOf(k [16]byte) int {
	h := uint64(0)
	for i := 0; i < 16; i += 8 {
		v := uint64(0)
		for j := 0; j < 8; j++ {
			v = v<<8 | uint64(k[i+j])
		}
		h = (h ^ v) * 0x9e3779b97f4a7c15
	}
	return int(h >> 58) // top 6 bits: 64 shards
}
