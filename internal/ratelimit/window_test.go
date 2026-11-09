package ratelimit

import (
	"net"
	"sync"
	"testing"
	"time"
)

// Tests for window.go: the sliding-window count, per-IP isolation, the
// IPv4/IPv4-mapped key equivalence, and the benchmarks that price the accept
// path.

var t0 = time.Unix(1_000_000, 0)

func TestAllowUpToMaxThenDeny(t *testing.T) {
	l := New(3, time.Second)
	ip := net.ParseIP("192.0.2.1")
	for i := 0; i < 3; i++ {
		if !l.Allow(ip, t0) {
			t.Fatalf("connection %d refused within the limit", i+1)
		}
	}
	if l.Allow(ip, t0) {
		t.Fatal("4th connection in the same window was allowed")
	}
}

func TestWindowSlides(t *testing.T) {
	l := New(2, time.Second)
	ip := net.ParseIP("192.0.2.1")
	l.Allow(ip, t0)
	l.Allow(ip, t0.Add(500*time.Millisecond))
	if l.Allow(ip, t0.Add(900*time.Millisecond)) {
		t.Fatal("third connection inside the window was allowed")
	}
	// The first two fall out of the window one by one; by 1.6s both are old.
	if !l.Allow(ip, t0.Add(1600*time.Millisecond)) {
		t.Fatal("connection after the window passed was refused")
	}
}

func TestIPsAreIndependent(t *testing.T) {
	l := New(1, time.Second)
	if !l.Allow(net.ParseIP("192.0.2.1"), t0) {
		t.Fatal("first IP refused")
	}
	if !l.Allow(net.ParseIP("192.0.2.2"), t0) {
		t.Fatal("second IP refused because of the first")
	}
}

func TestIPv4AndMappedShareKey(t *testing.T) {
	l := New(1, time.Second)
	if !l.Allow(net.ParseIP("192.0.2.9").To4(), t0) {
		t.Fatal("first connection refused")
	}
	if l.Allow(net.ParseIP("192.0.2.9"), t0) { // 16-byte mapped form of the same IP
		t.Fatal("mapped form counted under a different key")
	}
}

func TestDisabledAllowsEverything(t *testing.T) {
	l := New(0, time.Second)
	ip := net.ParseIP("192.0.2.1")
	for i := 0; i < 100; i++ {
		if !l.Allow(ip, t0) {
			t.Fatal("limit 0 must not refuse")
		}
	}
}

func TestSetMaxTakesEffectWithoutReset(t *testing.T) {
	l := New(10, time.Second)
	ip := net.ParseIP("192.0.2.1")
	for i := 0; i < 3; i++ {
		l.Allow(ip, t0)
	}
	l.SetMax(3)
	if l.Allow(ip, t0) {
		t.Fatal("lowered limit did not apply to the connections already counted")
	}
}

func TestShardResetBoundsMemory(t *testing.T) {
	l := New(1, time.Second)
	for i := 0; i < shardCount*shardCap*2; i++ {
		l.Allow(net.IPv4(10, byte(i>>16), byte(i>>8), byte(i)), t0)
	}
	total := 0
	for i := range l.shards {
		total += len(l.shards[i].host) + len(l.shards[i].subnet)
	}
	if total > shardCount*shardCap {
		t.Fatalf("tracked keys = %d, want at most %d", total, shardCount*shardCap)
	}
}

func TestAllowListBypassesLimit(t *testing.T) {
	allow, err := ParseCIDRs([]string{"10.0.0.0/8", "2001:db8::/32"})
	if err != nil {
		t.Fatalf("ParseCIDRs: %v", err)
	}
	l := NewWithOptions(Options{
		Max:    1,
		Window: time.Second,
		Allow:  allow,
	})
	for i := 0; i < 10; i++ {
		if !l.Allow(net.ParseIP("10.1.2.3"), t0) {
			t.Fatalf("allow-listed IPv4 refused on attempt %d", i+1)
		}
		if !l.Allow(net.ParseIP("2001:db8::1"), t0) {
			t.Fatalf("allow-listed IPv6 refused on attempt %d", i+1)
		}
	}
	// Non-allowlisted IP is still limited to 1.
	if !l.Allow(net.ParseIP("192.0.2.1"), t0) {
		t.Fatal("first non-allowlisted connection refused")
	}
	if l.Allow(net.ParseIP("192.0.2.1"), t0) {
		t.Fatal("second non-allowlisted connection allowed")
	}
}

func TestSubnet24AggregationCatchesDistributedFlood(t *testing.T) {
	// Each individual IP stays at 1 (below Max=5), but across 192.0.2.0/24 the
	// fourth distinct IP exceeds SubnetMax=3.
	l := NewWithOptions(Options{
		Max:       5,
		SubnetMax: 3,
		Window:    time.Second,
	})
	for i := byte(1); i <= 3; i++ {
		if !l.Allow(net.IPv4(192, 0, 2, i), t0) {
			t.Fatalf("host .%d refused within /24 limit", i)
		}
	}
	if l.Allow(net.IPv4(192, 0, 2, 4), t0) {
		t.Fatal("4th host in the same /24 was allowed past SubnetMax=3")
	}
	// Another /24 is unaffected.
	if !l.Allow(net.IPv4(198, 51, 100, 1), t0) {
		t.Fatal("unrelated /24 was refused")
	}
}

func TestIPv6HostKeyGroupsBy64(t *testing.T) {
	l := NewWithOptions(Options{Max: 1, Window: time.Second})
	a := net.ParseIP("2001:db8:1:2::1")
	b := net.ParseIP("2001:db8:1:2::2")
	if !l.Allow(a, t0) {
		t.Fatal("first IPv6 host refused")
	}
	if l.Allow(b, t0) {
		t.Fatal("IPv6 host in the same /64 bypassed the per-IP limit")
	}
	// A different /64 is independent.
	if !l.Allow(net.ParseIP("2001:db8:1:3::1"), t0) {
		t.Fatal("unrelated IPv6 /64 was refused")
	}
}

func TestIPv6SubnetKeyDoesNotUseByteHeuristic(t *testing.T) {
	// Bytes 10..11 are 0xffff, but this is a real IPv6 address, not an
	// IPv4-mapped one. It must be grouped by the IPv6 prefix, not by /24.
	l := NewWithOptions(Options{Max: 100, SubnetMax: 1, Window: time.Second})
	if !l.Allow(net.ParseIP("2001:db8::ffff:1:2"), t0) {
		t.Fatal("first connection refused")
	}
	if l.Allow(net.ParseIP("2001:db8::ffff:1:3"), t0) {
		t.Fatal("IPv6 address with ffff bytes was grouped as IPv4")
	}
}

func TestGuardLimitsPerIPAndReleases(t *testing.T) {
	g := NewGuard(2, 0, 64, 56)
	ip := net.ParseIP("192.0.2.1")
	if !g.Acquire(ip) {
		t.Fatal("first slot refused")
	}
	if !g.Acquire(ip) {
		t.Fatal("second slot refused")
	}
	if g.Acquire(ip) {
		t.Fatal("third concurrent connection allowed past max_concurrent")
	}
	g.Release(ip)
	if !g.Acquire(ip) {
		t.Fatal("released slot was not reusable")
	}
}

func TestGuardSubnetRollbackOnReject(t *testing.T) {
	g := NewGuard(10, 1, 64, 56)
	if !g.Acquire(net.ParseIP("2001:db8::1")) {
		t.Fatal("first subnet slot refused")
	}
	if g.Acquire(net.ParseIP("2001:db8::2")) {
		t.Fatal("second host in the same /56 allowed past subnet limit")
	}
	// The rejected attempt must not leak the host slot.
	g.Release(net.ParseIP("2001:db8::1"))
	if !g.Acquire(net.ParseIP("2001:db8::2")) {
		t.Fatal("host slot leaked after subnet rejection")
	}
}

func TestEvictOnePrefersUnbanned(t *testing.T) {
	l := NewWithOptions(Options{Max: 1, Window: time.Second, BanDuration: time.Hour})
	keys := make(map[key]*ipWindow)
	banned := keyOf(net.ParseIP("192.0.2.1"), 64)
	free := keyOf(net.ParseIP("192.0.2.2"), 64)
	keys[banned] = &ipWindow{banUntil: t0.Add(time.Hour).UnixNano()}
	keys[free] = &ipWindow{}
	l.evictOne(keys, t0.UnixNano()/l.bucket, t0.UnixNano())
	if _, ok := keys[banned]; !ok {
		t.Fatal("eviction removed an active ban instead of an unbanned entry")
	}
}

func TestExponentialBackoffBan(t *testing.T) {
	// Max=1, Window=1s, base BanDuration=2s.
	// 1st violation at t0 -> banned for 2s (until t0+2s).
	// Next violation right after ban expires (t0+2.1s, still within 1s after
	// banUntil so strikes do not reset) -> banned for 4s (2x, until t0+6.1s).
	l := NewWithOptions(Options{
		Max:         1,
		Window:      time.Second,
		BanDuration: 2 * time.Second,
	})
	ip := net.ParseIP("192.0.2.1")

	if !l.Allow(ip, t0) {
		t.Fatal("1st connection should be allowed")
	}
	if l.Allow(ip, t0) { // strike 1: ban until t0+2s
		t.Fatal("2nd connection should trigger 1st ban")
	}
	// At t0+1.5s the sliding window has emptied, but the 2s ban is still active.
	if l.Allow(ip, t0.Add(1500*time.Millisecond)) {
		t.Fatal("connection allowed while 1st ban is still active")
	}
	// At t0+2.1s the 1st ban has expired: 1 connection is allowed, the 2nd
	// triggers strike 2 (4s ban, until t0+6.1s).
	t1 := t0.Add(2100 * time.Millisecond)
	if !l.Allow(ip, t1) {
		t.Fatal("connection after 1st ban expired should be allowed")
	}
	if l.Allow(ip, t1) {
		t.Fatal("violation after 1st ban should trigger 2nd ban (4s)")
	}
	if l.Allow(ip, t1.Add(3*time.Second)) {
		t.Fatal("connection allowed 3s into a 4s (2x backoff) ban")
	}
	// After the 4s ban + a full 1s clean window (t1 + 5.1s), strikes reset.
	t2 := t1.Add(5100 * time.Millisecond)
	if !l.Allow(ip, t2) {
		t.Fatal("connection after 2nd ban expired should be allowed")
	}
}

// BenchmarkAllow is one IP hammering the limiter: the single-goroutine cost of
// the check on the accept path.
func BenchmarkAllow(b *testing.B) {
	l := New(1<<30, time.Second)
	ip := net.ParseIP("192.0.2.1")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		l.Allow(ip, t0)
	}
}

// BenchmarkAllowParallel is the same check with all Ps accepting at once, over
// distinct IPs, which is the realistic accept-path mix.
func BenchmarkAllowParallel(b *testing.B) {
	l := New(1<<30, time.Second)
	var seq int
	var mu sync.Mutex
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		mu.Lock()
		seq++
		base := seq
		mu.Unlock()
		ip := net.IPv4(10, byte(base>>8), byte(base), 1)
		for pb.Next() {
			l.Allow(ip, t0)
		}
	})
}
