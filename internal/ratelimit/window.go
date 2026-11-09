// Package ratelimit limits how many connections a client IP or its subnet
// may open within a sliding window, with an optional CIDR allow-list and
// exponential-backoff ban. It runs on the accept path, after accept() and
// before the upstream dial, so it limits connection floods; it does not stop
// volumetric attacks, which need a layer below user space (see docs/04, F4).
package ratelimit

import (
	"crypto/rand"
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
	shardCap    = 4096 // keys per table before one entry is evicted (bounds memory)
	maxBanShift = 4    // caps exponential backoff at 2^4 = 16x the base ban duration

	defaultIPv6HostPrefix   = 64
	defaultIPv6SubnetPrefix = 56
)

// key is a canonical 16-byte address. Host and subnet entries live in
// separate maps, so a host address can never collide with a subnet key.
type key [16]byte

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

func matchesAllow(k key, list []Prefix) bool {
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
	Max              int           // max connections per IP per Window; 0 = disabled
	SubnetMax        int           // max connections per subnet per Window; 0 = disabled
	Window           time.Duration // sliding window length
	BanDuration      time.Duration // base ban duration on violation (exponentially backed off); 0 = no ban
	Allow            []Prefix      // allow-listed CIDRs bypassed before counting
	IPv6HostPrefix   int           // per-IP prefix for IPv6 (default 64)
	IPv6SubnetPrefix int           // subnet prefix for IPv6 (default 56)
}

// Limiter counts connections per IP and per subnet over a sliding window of
// slotCount buckets, with an allow-list and exponential backoff ban. Limits
// and allow-lists can be updated in place (Update) without resetting the
// windows; the window length and prefixes are fixed at construction.
type Limiter struct {
	limit     atomic.Int64
	subnetMax atomic.Int64
	banNano   atomic.Int64
	allow     atomic.Pointer[[]Prefix]

	window           time.Duration
	bucket           int64 // slot length in nanoseconds
	ipv6HostPrefix   int
	ipv6SubnetPrefix int
	shards           [shardCount]shard
}

type shard struct {
	mu     sync.Mutex
	host   map[key]*ipWindow
	subnet map[key]*ipWindow
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

// New returns a limiter with the given window and per-IP limit. Kept as a concise
// constructor for callers that only need the per-IP limit; use NewWithOptions
// for allow-lists, subnet aggregation and backoff bans.
func New(limit int, window time.Duration) *Limiter {
	return NewWithOptions(Options{Max: limit, Window: window})
}

// NewWithOptions returns a limiter configured by opts.
func NewWithOptions(opts Options) *Limiter {
	bucket := int64(opts.Window) / slotCount
	if bucket <= 0 {
		bucket = 1
	}
	hostPrefix, subnetPrefix := normalizePrefixes(opts.IPv6HostPrefix, opts.IPv6SubnetPrefix)
	l := &Limiter{
		window:           opts.Window,
		bucket:           bucket,
		ipv6HostPrefix:   hostPrefix,
		ipv6SubnetPrefix: subnetPrefix,
	}
	for i := range l.shards {
		l.shards[i].host = make(map[key]*ipWindow)
		l.shards[i].subnet = make(map[key]*ipWindow)
	}
	l.Update(opts)
	return l
}

// Update changes the limits, ban duration and allow-list in place without
// resetting the sliding windows.
func (l *Limiter) Update(opts Options) {
	l.limit.Store(int64(opts.Max))
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
func (l *Limiter) SetMax(limit int) { l.limit.Store(int64(limit)) }

// Max returns the current per-IP limit (<= 0 means disabled).
func (l *Limiter) Max() int { return int(l.limit.Load()) }

// SubnetMax returns the current per-subnet limit (<= 0 means disabled).
func (l *Limiter) SubnetMax() int { return int(l.subnetMax.Load()) }

// Window returns the window length configured at construction.
func (l *Limiter) Window() time.Duration { return l.window }

// Prefixes returns the IPv6 host and subnet prefixes fixed at construction.
// A changed prefix requires a new Limiter because it changes the key layout.
func (l *Limiter) Prefixes() (host, subnet int) { return l.ipv6HostPrefix, l.ipv6SubnetPrefix }

// Allow records one connection from ip at time now and reports whether the IP
// (and its subnet, when configured) is within the limit.
//
// Order of checks:
//  1. CIDR allow-list (lock-free; allowed IPs are not counted).
//  2. Per-IP sliding window + exponential-backoff ban.
//  3. Per-subnet sliding window + backoff ban.
//
// Steady state allocates nothing.
func (l *Limiter) Allow(ip net.IP, now time.Time) bool {
	lim := l.limit.Load()
	subLim := l.subnetMax.Load()
	if lim <= 0 && subLim <= 0 {
		return true
	}

	hostKey := keyOf(ip, l.ipv6HostPrefix)
	if al := l.allow.Load(); al != nil && matchesAllow(hostKey, *al) {
		return true
	}

	nowNano := now.UnixNano()
	ban := l.banNano.Load()

	if lim > 0 {
		if !l.allowKey(hostKey, false, lim, nowNano, ban) {
			return false
		}
	}
	if subLim > 0 {
		if !l.allowKey(subnetKeyOf(hostKey, l.ipv6SubnetPrefix), true, subLim, nowNano, ban) {
			return false
		}
	}
	return true
}

func (l *Limiter) allowKey(k key, subnet bool, limit, nowNano, banNano int64) bool {
	e := nowNano / l.bucket
	windowNano := l.bucket * slotCount

	sh := &l.shards[shardOf(k)]
	sh.mu.Lock()
	keys := sh.host
	if subnet {
		keys = sh.subnet
	}
	w := keys[k]
	if w == nil {
		if len(keys) >= shardCap {
			l.evictOne(keys, e, nowNano)
		}
		w = &ipWindow{}
		keys[k] = w
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

	allowed := total <= limit
	if !allowed && banNano > 0 {
		w.strikes++
		shift := w.strikes - 1
		if shift > maxBanShift {
			shift = maxBanShift
		}
		const maxInt64 = int64(^uint64(0) >> 1)
		if banNano > (maxInt64-nowNano)>>shift {
			w.banUntil = maxInt64
		} else {
			w.banUntil = nowNano + (banNano << shift)
		}
	}
	sh.mu.Unlock()
	return allowed
}

// evictOne removes a single entry when a table exceeds its cap. It prefers
// entries whose ban has expired and which have no recent activity, so a flood
// of new keys cannot clear all active bans in one operation.
func (l *Limiter) evictOne(keys map[key]*ipWindow, e, nowNano int64) {
	const sample = 32
	seen := 0
	for k, w := range keys {
		if seen >= sample {
			break
		}
		seen++
		if w.banUntil <= nowNano && !w.hasRecent(e) {
			delete(keys, k)
			return
		}
	}
	// No easily evictable entry: drop one arbitrary key. This is a bounded
	// compromise: one ban may be lost, but the whole table is never cleared.
	for k := range keys {
		delete(keys, k)
		return
	}
}

func (w *ipWindow) hasRecent(e int64) bool {
	for i := range w.slots {
		if w.slots[i].epoch > e-slotCount && w.slots[i].n > 0 {
			return true
		}
	}
	return false
}

// keyOf maps an IP to a fixed 16-byte key. IPv4 keeps the exact address; IPv6
// is masked to ipv6Prefix (default /64), so a single host cannot rotate through
// a whole /64 to bypass the per-IP limit.
func keyOf(ip net.IP, ipv6Prefix int) key {
	var k key
	if v4 := ip.To4(); v4 != nil {
		k[10], k[11] = 0xff, 0xff
		copy(k[12:], v4)
		return k
	}
	if ip16 := ip.To16(); ip16 != nil {
		copy(k[:], ip16)
	}
	maskPrefix((*[16]byte)(k[:]), ipv6Prefix)
	return k
}

// subnetKeyOf turns a host key into its /24 (IPv4) or IPv6 prefix key.
func subnetKeyOf(k key, ipv6Prefix int) key {
	if k[0] == 0 && k[1] == 0 && k[2] == 0 && k[3] == 0 &&
		k[4] == 0 && k[5] == 0 && k[6] == 0 && k[7] == 0 &&
		k[8] == 0 && k[9] == 0 && k[10] == 0xff && k[11] == 0xff {
		k[15] = 0 // /24: keep bytes 12..14
		return k
	}
	maskPrefix((*[16]byte)(k[:]), ipv6Prefix)
	return k
}

func normalizePrefixes(host, subnet int) (int, int) {
	if host <= 0 || host > 128 {
		host = defaultIPv6HostPrefix
	}
	if subnet <= 0 || subnet > 128 {
		subnet = defaultIPv6SubnetPrefix
	}
	if host < subnet {
		host = subnet
	}
	return host, subnet
}

func maskPrefix(a *[16]byte, bits int) {
	if bits <= 0 {
		for i := range a {
			a[i] = 0
		}
		return
	}
	if bits >= 128 {
		return
	}
	full := bits / 8
	rem := bits % 8
	for i := full; i < len(a); i++ {
		a[i] = 0
	}
	if rem > 0 {
		a[full] &= byte(0xff << (8 - rem))
	}
}

// shardSalt makes shard placement hard to predict from a remote address. It is
// generated once per process, so an attacker cannot precompute keys that all
// land in the shard of their banned subnet.
var shardSalt = func() uint64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return binary.LittleEndian.Uint64(b[:])
	}
	return uint64(time.Now().UnixNano())
}()

func shardOf(k key) uint64 {
	hi := binary.BigEndian.Uint64(k[:8])
	lo := binary.BigEndian.Uint64(k[8:])
	h := (hi * 0x9e3779b97f4a7c15) ^ lo ^ shardSalt
	h *= 0x9e3779b97f4a7c15
	return h >> 58 // top 6 bits: 0..63
}
