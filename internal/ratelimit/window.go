// Package ratelimit limits how many connections a client IP or its /24 subnet
// may open within a sliding window, with an optional CIDR allow-list and
// exponential-backoff ban. It runs on the accept path, after accept() and
// before the upstream dial, so it limits connection floods; it does not stop
// volumetric attacks, which need a layer below user space (see docs/04, F4).
package ratelimit

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	slotCount   = 10   // buckets per window: the window is slotCount slots long
	shardCount  = 64   // independent tables, so accepts from different IPs rarely contend
	shardCap    = 4096 // keys per shard before the shard is cleared (bounds memory)
	maxBanShift = 4    // caps exponential backoff at 2^4 = 16x the base ban duration
	subnetTagV4 = 0x24 // distinguishes a /24 subnet key from host x.y.z.0 in the same map
	subnetTagV6 = 0x64 // distinguishes a /64 IPv6 subnet key from host :: in the same map
)

// Prefix is a canonical 16-byte IP prefix and mask used by the allow-list.
// Comparing two uint64 halves avoids any allocation on the accept path.
type Prefix struct {
	hiAddr, loAddr uint64
	hiMask, loMask uint64
}

// ParseCIDRs parses CIDR strings into canonical 16-byte prefixes (IPv4 is
// stored in its IPv4-mapped IPv6 form so it matches keyOf).
func ParseCIDRs(cidrs []string) ([]Prefix, error) {
	if len(cidrs) == 0 {
		return nil, nil
	}
	out := make([]Prefix, 0, len(cidrs))
	for _, s := range cidrs {
		_, ipNet, err := net.ParseCIDR(s)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q: %w", s, err)
		}
		ones, bits := ipNet.Mask.Size()
		var addr, mask [16]byte
		if bits == 32 {
			addr[10], addr[11] = 0xff, 0xff
			copy(addr[12:], ipNet.IP.To4())
			// First 12 bytes of a mapped IPv4 address are fixed (::ffff:0:0/96).
			for i := 0; i < 12; i++ {
				mask[i] = 0xff
			}
			copy(mask[12:], ipNet.Mask)
		} else {
			copy(addr[:], ipNet.IP.To16())
			copy(mask[:], ipNet.Mask)
		}
		_ = ones
		hiM := binary.BigEndian.Uint64(mask[:8])
		loM := binary.BigEndian.Uint64(mask[8:])
		out = append(out, Prefix{
			hiAddr: binary.BigEndian.Uint64(addr[:8]) & hiM,
			loAddr: binary.BigEndian.Uint64(addr[8:]) & loM,
			hiMask: hiM,
			loMask: loM,
		})
	}
	return out, nil
}

func matchesAllow(k [16]byte, list []Prefix) bool {
	if len(list) == 0 {
		return false
	}
	hi := binary.BigEndian.Uint64(k[:8])
	lo := binary.BigEndian.Uint64(k[8:])
	for i := range list {
		p := &list[i]
		if (hi&p.hiMask) == p.hiAddr && (lo&p.loMask) == p.loAddr {
			return true
		}
	}
	return false
}

// Options configures a Limiter. Max and SubnetMax may both be active at once;
// setting both to 0 disables rate limiting.
type Options struct {
	Max         int           // max connections per IP per Window; 0 = disabled
	SubnetMax   int           // max connections per /24 (IPv4) or /64 (IPv6) per Window; 0 = disabled
	Window      time.Duration // sliding window length
	BanDuration time.Duration // base ban duration on violation (exponentially backed off); 0 = no ban
	Allow       []Prefix      // allow-listed CIDRs bypassed before counting
}

// Limiter counts connections per IP (and optionally per /24 subnet) over a
// sliding window of slotCount buckets, with an allow-list and exponential
// backoff ban. Limits and allow-lists can be updated in place (Update) without
// resetting the windows; the window length is fixed at construction.
type Limiter struct {
	max       atomic.Int64
	subnetMax atomic.Int64
	banNano   atomic.Int64
	allow     atomic.Pointer[[]Prefix]

	window time.Duration
	bucket int64 // slot length in nanoseconds
	shards [shardCount]shard
}

type shard struct {
	mu   sync.Mutex
	keys map[[16]byte]*ipWindow
}

// ipWindow is the ring of per-slot counters and ban state for one IP or subnet.
// A slot is reused for a new epoch by overwriting it, so the ring never grows.
type ipWindow struct {
	slots    [slotCount]slot
	banUntil int64  // UnixNano until which the key is banned; 0 = not banned
	strikes  uint32 // consecutive violations for exponential backoff (1x, 2x, 4x, 8x, 16x)
}

type slot struct {
	epoch int64 // slot index in units of bucket length
	n     int64 // connections counted in this slot
}

// New returns a limiter with the given window and per-IP max. Kept as a concise
// constructor for callers that only need the per-IP limit; use NewWithOptions
// for allow-lists, /24 aggregation and backoff bans.
func New(max int, window time.Duration) *Limiter {
	return NewWithOptions(Options{Max: max, Window: window})
}

// NewWithOptions returns a limiter configured by opts.
func NewWithOptions(opts Options) *Limiter {
	bucket := int64(opts.Window) / slotCount
	if bucket <= 0 {
		bucket = 1
	}
	l := &Limiter{window: opts.Window, bucket: bucket}
	for i := range l.shards {
		l.shards[i].keys = make(map[[16]byte]*ipWindow)
	}
	l.Update(opts)
	return l
}

// Update changes the limits, ban duration and allow-list in place without
// resetting the sliding windows.
func (l *Limiter) Update(opts Options) {
	l.max.Store(int64(opts.Max))
	l.subnetMax.Store(int64(opts.SubnetMax))
	l.banNano.Store(int64(opts.BanDuration))
	if len(opts.Allow) == 0 {
		l.allow.Store(nil)
	} else {
		cp := make([]Prefix, len(opts.Allow))
		copy(cp, opts.Allow)
		l.allow.Store(&cp)
	}
}

// SetMax changes the per-IP limit at runtime without resetting the window.
func (l *Limiter) SetMax(max int) { l.max.Store(int64(max)) }

// Max returns the current per-IP limit (<= 0 means disabled).
func (l *Limiter) Max() int { return int(l.max.Load()) }

// SubnetMax returns the current per-subnet limit (<= 0 means disabled).
func (l *Limiter) SubnetMax() int { return int(l.subnetMax.Load()) }

// Window returns the window length configured at construction.
func (l *Limiter) Window() time.Duration { return l.window }

// Allow records one connection from ip at time now and reports whether the IP
// (and its /24 or /64 subnet, when configured) is within the limit.
//
// Order of checks:
//  1. CIDR allow-list (lock-free; allowed IPs are not counted).
//  2. Per-IP sliding window + exponential-backoff ban.
//  3. Per-subnet (/24 for IPv4, /64 for IPv6) sliding window + backoff ban.
//
// Steady state allocates nothing.
func (l *Limiter) Allow(ip net.IP, now time.Time) bool {
	max := l.max.Load()
	subMax := l.subnetMax.Load()
	if max <= 0 && subMax <= 0 {
		return true
	}

	key := keyOf(ip)
	if al := l.allow.Load(); al != nil && matchesAllow(key, *al) {
		return true
	}

	nowNano := now.UnixNano()
	ban := l.banNano.Load()

	if max > 0 && !l.allowKey(key, max, nowNano, ban) {
		return false
	}
	if subMax > 0 && !l.allowKey(subnetKeyOf(key), subMax, nowNano, ban) {
		return false
	}
	return true
}

func (l *Limiter) allowKey(key [16]byte, max, nowNano, banNano int64) bool {
	e := nowNano / l.bucket
	windowNano := l.bucket * slotCount

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

	// Active ban: reject immediately without advancing the window slots.
	if w.banUntil > nowNano {
		sh.mu.Unlock()
		return false
	}
	// If the key has been clean for a full window since its last ban expired,
	// reset the backoff strike counter.
	if w.banUntil > 0 && nowNano-w.banUntil >= windowNano {
		w.strikes = 0
		w.banUntil = 0
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

	allowed := total <= max
	if !allowed && banNano > 0 {
		w.strikes++
		shift := w.strikes - 1
		if shift > maxBanShift {
			shift = maxBanShift
		}
		w.banUntil = nowNano + (banNano << shift)
	}
	sh.mu.Unlock()
	return allowed
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

// subnetKeyOf masks a canonical 16-byte IP key to its /24 (IPv4) or /64 (IPv6)
// prefix and tags it so it cannot collide with a host key in the same map.
func subnetKeyOf(k [16]byte) [16]byte {
	if k[10] == 0xff && k[11] == 0xff {
		k[0] = subnetTagV4
		k[15] = 0 // /24
		return k
	}
	for i := 8; i < 16; i++ {
		k[i] = 0 // /64
	}
	k[15] = subnetTagV6
	return k
}

func shardOf(k [16]byte) int {
	hi := binary.BigEndian.Uint64(k[:8])
	lo := binary.BigEndian.Uint64(k[8:])
	h := (hi * 0x9e3779b97f4a7c15) ^ lo
	h *= 0x9e3779b97f4a7c15
	return int(h >> 58) // top 6 bits: 64 shards
}
